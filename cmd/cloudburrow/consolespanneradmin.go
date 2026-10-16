package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	database "cloud.google.com/go/spanner/admin/database/apiv1"
	databasepb "cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	instance "cloud.google.com/go/spanner/admin/instance/apiv1"
	instancepb "cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// Spanner's schema changes, instance configurations and instance partitions
// in the console (#1207). Each was verified through the official clients
// (#1199) and had no screen:
//
//   - a database's page has Update schema, which sends UpdateDatabaseDdl with
//     the statements typed, confirmed by typing the database's name back;
//   - an instance's page shows its configuration (GetInstanceConfig) and the
//     configurations the project can use (ListInstanceConfigs);
//   - an instance's page has an Instance partitions tab (ListInstancePartitions)
//     whose rows open a partition's page (GetInstancePartition) and delete it
//     (DeleteInstancePartition), a Create instance partition action
//     (CreateInstancePartition), and a Partition operations tab
//     (ListInstancePartitionOperations).

// spannerPartitions is the path segment that puts a partition under its
// instance: [instance, "instancePartitions", partition]. A database ID is
// lowercase, so no database can be named this, and [instance, database,
// table] stays unambiguous.
const spannerPartitions = "instancePartitions"

// maxSpannerDDLBytes bounds one Update schema request. Chosen, not measured:
// far past a schema typed into a form, and under the action route's body.
const maxSpannerDDLBytes = 32 << 10

// isSpannerPartitionPath reports whether path is an instance's partitions
// page ([instance, "instancePartitions"]) or one partition's.
func isSpannerPartitionPath(path []string) bool {
	return len(path) >= 2 && path[1] == spannerPartitions
}

// splitSpannerDDL splits the text typed into Update schema into statements.
//
// UpdateDatabaseDdl takes a list of statements, each without its semicolon.
// A semicolon inside a string literal, a quoted identifier or a comment does
// not end a statement, so the text is walked with the same quoting rules the
// statement classifier's tokenizer uses. Every statement must be DDL: a DML
// statement or a query belongs to the editor, and is refused with where it
// goes rather than sent to fail.
func splitSpannerDDL(sql string) ([]string, error) {
	if len(sql) > maxSpannerDDLBytes {
		return nil, fmt.Errorf("the schema change is %d bytes; Update schema accepts at most %d",
			len(sql), maxSpannerDDLBytes)
	}
	var out []string
	start := 0
	add := func(end int) {
		if s := strings.TrimSpace(sql[start:end]); s != "" {
			out = append(out, s)
		}
	}
	for i := 0; i < len(sql); {
		c := sql[i]
		switch {
		case c == '#' || (c == '-' && strings.HasPrefix(sql[i:], "--")):
			end := strings.IndexByte(sql[i:], '\n')
			if end < 0 {
				i = len(sql)
				continue
			}
			i += end + 1
		case c == '/' && strings.HasPrefix(sql[i:], "/*"):
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				return nil, errors.New("unterminated comment")
			}
			i += 2 + end + 2
		case c == '\'' || c == '"' || c == '`':
			n, err := skipQuoted(sql[i:])
			if err != nil {
				return nil, err
			}
			i += n
		case c == ';':
			add(i)
			i++
			start = i
		default:
			i++
		}
	}
	add(len(sql))
	if len(out) == 0 {
		return nil, errors.New("type at least one DDL statement: CREATE, ALTER or DROP")
	}
	for _, stmt := range out {
		st, err := classifySpannerStatement(stmt)
		if err != nil {
			return nil, err
		}
		kw := st.Keywords[0]
		switch st.Kinds[0] {
		case spannerDDL:
		case spannerDML:
			return nil, fmt.Errorf("%s changes data, not the schema: run it in the query editor's Read-write mode", kw)
		case spannerQuery:
			return nil, fmt.Errorf("%s reads data, not the schema: run it in the query editor", kw)
		default:
			if kw == "" {
				kw = "this statement"
			}
			return nil, fmt.Errorf("Update schema runs DDL (CREATE, ALTER or DROP), not %s", kw)
		}
	}
	return out, nil
}

// spannerDDLAction is the database page's Update schema.
func spannerDDLAction(instanceID, dbID string) console.Action {
	return console.Action{
		ID: "updateddl", Label: "Update schema",
		Confirm: fmt.Sprintf("the schema of database %s in instance %s: the statements are applied "+
			"in order with UpdateDatabaseDdl, and a DROP TABLE takes the table's rows with it", dbID, instanceID),
		Fields: []console.Field{{
			Name: "statements", Label: "DDL statements", Type: "textarea", Required: true,
			Help: "One or more CREATE, ALTER or DROP statements (tables, indexes, views, change streams), " +
				"separated by semicolons. They are applied in order; if one fails, those before it stay applied.",
		}},
	}
}

// spannerCreatePartitionAction is an instance page's Create instance partition.
func spannerCreatePartitionAction() console.Action {
	return console.Action{
		ID: "createpartition", Label: "Create instance partition",
		Fields: []console.Field{
			{
				Name: "partition", Label: "Instance partition ID", Type: "text", Required: true,
				Help:    "Lowercase letters, digits and hyphens, starting with a letter.",
				Pattern: `^[a-z][a-z0-9\-]{0,62}[a-z0-9]$`,
			},
			{Name: "displayName", Label: "Display name", Type: "text", Help: "Optional; the ID when empty."},
			{
				Name: "config", Label: "Configuration", Type: "text", Required: true, Default: "emulator-config",
				Help: "An instance configuration's ID, as the Instance configurations tab lists them.",
			},
			{
				Name: "nodeCount", Label: "Nodes", Type: "number", Default: "1",
				Help: "Accepted and kept; the emulator's one process serves every partition.",
			},
		},
	}
}

// spannerDeletePartitionAction deletes a partition, from its row or its page.
func spannerDeletePartitionAction(onPage bool) console.Action {
	return console.Action{ID: "deletepartition", Label: "Delete instance partition", Destructive: true, Leaves: onPage}
}

// spannerInstanceSections are an instance page's partitions, partition
// operations and configurations, and the group its Configuration tab adds for
// the instance's own configuration.
func (p spannerProvider) spannerInstanceSections(ctx context.Context, instAdmin *instance.InstanceAdminClient,
	project, instanceID, configName string) (partitions, operations, configs console.Section, configGroup *console.PropertyGroup) {
	instName := fmt.Sprintf("projects/%s/instances/%s", project, instanceID)

	partitions = console.Section{ID: "partitions", Label: "Instance partitions",
		Listing: p.spannerPartitionListing(ctx, instAdmin, instanceID, instName)}

	ops := console.Listing{
		Columns: []string{"Partition", "Type", "Done"}, NameColumn: "Operation", Noun: "operations",
	}
	oit := instAdmin.ListInstancePartitionOperations(ctx,
		&instancepb.ListInstancePartitionOperationsRequest{Parent: instName})
	for {
		o, err := oit.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			ops.Unavailable = "listing partition operations: " + err.Error()
			break
		}
		partition := "—"
		if before, _, ok := strings.Cut(o.GetName(), "/operations/"); ok {
			partition = lastSegment(before)
		}
		kind := "—"
		if m := o.GetMetadata(); m != nil {
			kind = lastSegment(strings.ReplaceAll(m.GetTypeUrl(), ".", "/"))
		}
		ops.Items = append(ops.Items, console.Resource{
			Name:   lastSegment(o.GetName()),
			Fields: map[string]string{"Partition": partition, "Type": kind, "Done": strconv.FormatBool(o.GetDone())},
		})
	}
	ops.Total = len(ops.Items)
	operations = console.Section{ID: "partition-operations", Label: "Partition operations", Listing: ops,
		Note: "The long-running operations that created, changed or deleted this instance's partitions."}

	cfgs := console.Listing{
		Columns: []string{"Display name", "Type", "Replicas", "In use"}, NameColumn: "Configuration", Noun: "configurations",
	}
	cit := instAdmin.ListInstanceConfigs(ctx, &instancepb.ListInstanceConfigsRequest{Parent: "projects/" + project})
	for {
		c, err := cit.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			cfgs.Unavailable = "listing instance configurations: " + err.Error()
			break
		}
		inUse := "No"
		if c.GetName() == configName {
			inUse = "Yes"
		}
		cfgs.Items = append(cfgs.Items, console.Resource{
			Name: lastSegment(c.GetName()),
			Fields: map[string]string{
				"Display name": orDash(c.GetDisplayName()),
				"Type":         c.GetConfigType().String(),
				"Replicas":     fmt.Sprint(len(c.GetReplicas())),
				"In use":       inUse,
			},
		})
	}
	sort.SliceStable(cfgs.Items, func(i, j int) bool { return cfgs.Items[i].Name < cfgs.Items[j].Name })
	cfgs.Total = len(cfgs.Items)
	configs = console.Section{ID: "instance-configs", Label: "Instance configurations", Listing: cfgs,
		Note: "The configurations this project's instances and partitions can use. The emulator serves one."}

	if configName != "" {
		group := console.PropertyGroup{Heading: "Instance configuration"}
		if c, err := instAdmin.GetInstanceConfig(ctx, &instancepb.GetInstanceConfigRequest{Name: configName}); err != nil {
			group.Properties = []console.Property{{Label: "Configuration", Value: "cannot read it: " + err.Error()}}
		} else {
			var replicas []string
			for _, r := range c.GetReplicas() {
				replicas = append(replicas, fmt.Sprintf("%s (%s)", r.GetLocation(), r.GetType()))
			}
			group.Properties = []console.Property{
				{Label: "Configuration name", Value: c.GetName()},
				{Label: "Display name", Value: c.GetDisplayName()},
				{Label: "Configuration type", Value: c.GetConfigType().String()},
				{Label: "Replicas", Value: strings.Join(replicas, ", ")},
				{Label: "Leader options", Value: strings.Join(c.GetLeaderOptions(), ", ")},
				{Label: "Base configuration", Value: c.GetBaseConfig()},
				{Label: "State", Value: c.GetState().String()},
			}
		}
		configGroup = &group
	}
	return partitions, operations, configs, configGroup
}

// spannerPartitionListing lists an instance's partitions, each opening its
// own page and deletable from its row.
func (p spannerProvider) spannerPartitionListing(ctx context.Context, instAdmin *instance.InstanceAdminClient,
	instanceID, instName string) console.Listing {
	parts := console.Listing{
		Columns: []string{"Display name", "Configuration", "Nodes", "Created"}, NameColumn: "Instance partition",
		Noun: "instance partitions", AlwaysStatus: true,
	}
	pit := instAdmin.ListInstancePartitions(ctx, &instancepb.ListInstancePartitionsRequest{Parent: instName})
	for {
		ip, err := pit.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			parts.Unavailable = "listing instance partitions: " + err.Error()
			break
		}
		id := lastSegment(ip.GetName())
		at := []string{instanceID, spannerPartitions, id}
		parts.Items = append(parts.Items, console.Resource{
			Name:   id,
			Status: ip.GetState().String(),
			Fields: map[string]string{
				"Display name":  orDash(ip.GetDisplayName()),
				"Configuration": lastSegment(ip.GetConfig()),
				"Nodes":         fmt.Sprint(ip.GetNodeCount()),
				"Created":       spannerTime(ip.GetCreateTime().AsTime(), ip.GetCreateTime() != nil),
			},
			Opens:   at,
			ActsOn:  at,
			Actions: []console.Action{spannerDeletePartitionAction(false)},
		})
	}
	sort.SliceStable(parts.Items, func(i, j int) bool { return parts.Items[i].Name < parts.Items[j].Name })
	parts.Total = len(parts.Items)
	return parts
}

func spannerTime(t time.Time, ok bool) string {
	if !ok {
		return "—"
	}
	return t.Format(time.RFC3339)
}

// spannerPartitionsPage is [instance, "instancePartitions"]: the breadcrumb
// between an instance and one of its partitions.
func (p spannerProvider) spannerPartitionsPage(ctx context.Context, project, instanceID string) (console.Detail, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	instAdmin, err := instance.NewInstanceAdminClient(ctx, localOpts(p.endpoint)...)
	if err != nil {
		return console.Detail{Unavailable: "cannot reach Spanner: " + err.Error()}, nil
	}
	defer instAdmin.Close()
	instName := fmt.Sprintf("projects/%s/instances/%s", project, instanceID)
	parts := p.spannerPartitionListing(ctx, instAdmin, instanceID, instName)
	return console.Detail{
		Summary:  []console.Property{{Label: "Instance", Value: instanceID}, {Label: "Instance partitions", Value: fmt.Sprint(parts.Total)}},
		Sections: []console.Section{{ID: "partitions", Label: "Instance partitions", Listing: parts}},
	}, nil
}

// spannerPartitionDetail is one partition's page, read with
// GetInstancePartition.
func (p spannerProvider) spannerPartitionDetail(ctx context.Context, project, instanceID, partitionID string) (console.Detail, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	instAdmin, err := instance.NewInstanceAdminClient(ctx, localOpts(p.endpoint)...)
	if err != nil {
		return console.Detail{Unavailable: "cannot reach Spanner: " + err.Error()}, nil
	}
	defer instAdmin.Close()
	ip, err := instAdmin.GetInstancePartition(ctx, &instancepb.GetInstancePartitionRequest{
		Name: spannerPartitionName(project, instanceID, partitionID),
	})
	if err != nil {
		return console.Detail{Unavailable: "cannot read the instance partition: " + err.Error()}, nil
	}
	created := spannerTime(ip.GetCreateTime().AsTime(), ip.GetCreateTime() != nil)
	return console.Detail{
		Summary: []console.Property{
			{Label: "State", Value: ip.GetState().String()},
			{Label: "Instance", Value: instanceID},
			{Label: "Configuration", Value: lastSegment(ip.GetConfig())},
			{Label: "Nodes", Value: fmt.Sprint(ip.GetNodeCount())},
		},
		Sections: []console.Section{{
			ID: "properties", Label: "Properties", Kind: console.KindProperties,
			Groups: []console.PropertyGroup{{
				Heading: "Instance partition",
				Properties: []console.Property{
					{Label: "Resource name", Value: ip.GetName()},
					{Label: "Display name", Value: ip.GetDisplayName()},
					{Label: "Configuration", Value: ip.GetConfig()},
					{Label: "Node count", Value: fmt.Sprint(ip.GetNodeCount())},
					{Label: "Processing units", Value: fmt.Sprint(ip.GetProcessingUnits())},
					{Label: "State", Value: ip.GetState().String()},
					{Label: "Created", Value: created},
				},
			}},
		}},
	}, nil
}

func spannerPartitionName(project, instanceID, partitionID string) string {
	return fmt.Sprintf("projects/%s/instances/%s/instancePartitions/%s", project, instanceID, partitionID)
}

// spannerAdminAct performs the #1207 actions; handled is false for any other.
func (p spannerProvider) spannerAdminAct(ctx context.Context, project string, path []string, action string,
	values map[string]string) (handled bool, err error) {
	switch action {
	case "editinstance":
		if len(path) != 1 {
			return true, errors.New("Edit instance is an instance's action")
		}
		return true, p.spannerEditInstance(ctx, project, path[0], values)
	case "updateddl":
		if len(path) != 2 || isSpannerPartitionPath(path) {
			return true, errors.New("Update schema is a database's action")
		}
		statements, err := splitSpannerDDL(values["statements"])
		if err != nil {
			return true, err
		}
		dbAdmin, err := database.NewDatabaseAdminClient(ctx, localOpts(p.endpoint)...)
		if err != nil {
			return true, fmt.Errorf("cannot reach Spanner: %w", err)
		}
		defer dbAdmin.Close()
		op, err := dbAdmin.UpdateDatabaseDdl(ctx, &databasepb.UpdateDatabaseDdlRequest{
			Database:   fmt.Sprintf("projects/%s/instances/%s/databases/%s", project, path[0], path[1]),
			Statements: statements,
		})
		if err != nil {
			return true, err
		}
		return true, op.Wait(ctx)
	case "createpartition":
		if len(path) != 1 {
			return true, errors.New("Create instance partition is an instance's action")
		}
		id := strings.TrimSpace(values["partition"])
		if id == "" {
			return true, errors.New("an instance partition ID is required")
		}
		cfg := strings.TrimSpace(values["config"])
		if cfg == "" {
			cfg = "emulator-config"
		}
		if !strings.Contains(cfg, "/") {
			cfg = fmt.Sprintf("projects/%s/instanceConfigs/%s", project, cfg)
		}
		nodes := int64(1)
		if v := strings.TrimSpace(values["nodeCount"]); v != "" {
			n, err := strconv.ParseInt(v, 10, 32)
			if err != nil || n < 1 {
				return true, fmt.Errorf("nodes must be a whole number of at least 1, not %q", v)
			}
			nodes = n
		}
		display := strings.TrimSpace(values["displayName"])
		if display == "" {
			display = id
		}
		instAdmin, err := instance.NewInstanceAdminClient(ctx, localOpts(p.endpoint)...)
		if err != nil {
			return true, fmt.Errorf("cannot reach Spanner: %w", err)
		}
		defer instAdmin.Close()
		op, err := instAdmin.CreateInstancePartition(ctx, &instancepb.CreateInstancePartitionRequest{
			Parent:              fmt.Sprintf("projects/%s/instances/%s", project, path[0]),
			InstancePartitionId: id,
			InstancePartition: &instancepb.InstancePartition{
				Name: spannerPartitionName(project, path[0], id), Config: cfg, DisplayName: display,
				ComputeCapacity: &instancepb.InstancePartition_NodeCount{NodeCount: int32(nodes)},
			},
		})
		if err != nil {
			return true, err
		}
		_, err = op.Wait(ctx)
		return true, err
	case "deletepartition":
		if len(path) != 3 || !isSpannerPartitionPath(path) {
			return true, errors.New("Delete instance partition is an instance partition's action")
		}
		instAdmin, err := instance.NewInstanceAdminClient(ctx, localOpts(p.endpoint)...)
		if err != nil {
			return true, fmt.Errorf("cannot reach Spanner: %w", err)
		}
		defer instAdmin.Close()
		return true, instAdmin.DeleteInstancePartition(ctx, &instancepb.DeleteInstancePartitionRequest{
			Name: spannerPartitionName(project, path[0], path[2]),
		})
	}
	return false, nil
}

// spannerEditInstanceAction is Edit instance (#1461), prefilled from the
// instance: its display name and compute capacity, which the call front in
// the Spanner pod updates (the emulator refuses UpdateInstance).
func (p spannerProvider) spannerEditInstanceAction(ctx context.Context, project, instanceID string) (console.Action, bool) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	instAdmin, err := instance.NewInstanceAdminClient(ctx, localOpts(p.endpoint)...)
	if err != nil {
		return console.Action{}, false
	}
	defer instAdmin.Close()
	inst, err := instAdmin.GetInstance(ctx, &instancepb.GetInstanceRequest{Name: fmt.Sprintf("projects/%s/instances/%s", project, instanceID)})
	if err != nil {
		return console.Action{}, false
	}
	return console.Action{ID: "editinstance", Label: "Edit instance", Fields: []console.Field{
		{Name: "displayName", Label: "Display name", Type: "text", Required: true, Default: inst.GetDisplayName(), Pattern: `^.{4,30}$`,
			Help: "4 to 30 characters."},
		{Name: "processingUnits", Label: "Processing units", Type: "text", Required: true, Pattern: `^[0-9]+$`,
			Default: strconv.Itoa(int(inst.GetProcessingUnits())),
			Help:    "A multiple of 100 below 1000, and of 1000 (a node) from 1000. The emulator serves every instance the same."},
	}}, true
}

func (p spannerProvider) spannerEditInstance(ctx context.Context, project, instanceID string, values map[string]string) error {
	pu, err := strconv.ParseInt(strings.TrimSpace(values["processingUnits"]), 10, 32)
	if err != nil {
		return fmt.Errorf("processing units %q is not a whole number", values["processingUnits"])
	}
	instAdmin, err := instance.NewInstanceAdminClient(ctx, localOpts(p.endpoint)...)
	if err != nil {
		return fmt.Errorf("cannot reach Spanner: %w", err)
	}
	defer instAdmin.Close()
	op, err := instAdmin.UpdateInstance(ctx, &instancepb.UpdateInstanceRequest{
		Instance: &instancepb.Instance{Name: fmt.Sprintf("projects/%s/instances/%s", project, instanceID),
			DisplayName: strings.TrimSpace(values["displayName"]), ProcessingUnits: int32(pu)},
		FieldMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name", "processing_units"}}})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}
