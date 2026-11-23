package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	alloydbpb "cloud.google.com/go/alloydb/apiv1/alloydbpb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/alloydb"
)

// The console's AlloyDB pages (#2191), as Google's menu has them: Clusters
// (a project's clusters in every region with their instances, Create
// cluster with its primary instance; a cluster's page with its Overview
// (the instances, Add read pool, Edit, Restart, Failover, Delete), Backups
// (Create backup, Restore), Users (Add user account, Delete) and
// Databases), and Backups (the project's backups, Restore and Delete).

const alloyDBNotStarted = "AlloyDB has not started"

func alloyDBAPI(svc *alloyDBService) (*alloydb.Server, error) {
	s := svc.API()
	if s == nil {
		return nil, errors.New(alloyDBNotStarted)
	}
	return s, nil
}

var alloyDBVersions = []string{"POSTGRES_17", "POSTGRES_16", "POSTGRES_15", "POSTGRES_14"}

var alloyDBVersionLabels = map[string]string{"POSTGRES_17": "PostgreSQL 17", "POSTGRES_16": "PostgreSQL 16 (default)",
	"POSTGRES_15": "PostgreSQL 15", "POSTGRES_14": "PostgreSQL 14"}

var alloyDBCPUs = []string{"2", "4", "8", "16", "32", "64", "96", "128"}

func alloyDBCPULabels() map[string]string {
	out := map[string]string{}
	for _, c := range alloyDBCPUs {
		n, _ := strconv.Atoi(c)
		out[c] = fmt.Sprintf("%d vCPU, %d GB", n, n*8)
	}
	return out
}

func alloyDBVersionLabel(v alloydbpb.DatabaseVersion) string {
	return strings.Replace(strings.Replace(v.String(), "POSTGRES_", "PostgreSQL ", 1), "_", ".", 1)
}

func alloyDBState(s fmt.Stringer) string {
	switch s.String() {
	case "READY":
		return "Ready"
	case "CREATING":
		return "Creating"
	case "DELETING":
		return "Deleting"
	case "STOPPED":
		return "Stopped"
	case "FAILED":
		return "Failed"
	case "MAINTENANCE":
		return "Maintenance"
	}
	return s.String()
}

func alloyDBInstanceType(t alloydbpb.Instance_InstanceType) string {
	switch t {
	case alloydbpb.Instance_PRIMARY:
		return "Primary"
	case alloydbpb.Instance_READ_POOL:
		return "Read pool"
	case alloydbpb.Instance_SECONDARY:
		return "Secondary"
	}
	return t.String()
}

// alloyDBWait waits for op, so the page drawn after an action shows its
// outcome, for at most a few minutes: an instance takes longer than a click
// should, and the page shows it Creating until then.
func alloyDBWait(ctx context.Context, s *alloydb.Server, op *longrunningpb.Operation, d time.Duration) error {
	deadline := time.Now().Add(d)
	for op != nil && !op.GetDone() && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
		got, err := s.Operation(op.GetName())
		if err != nil {
			return err
		}
		op = got
	}
	if e := op.GetError(); e != nil {
		return errors.New(e.GetMessage())
	}
	return nil
}

// ---- Clusters ----

type alloyDBProvider struct{ svc *alloyDBService }

func (alloyDBProvider) ID() string    { return "alloydb" }
func (alloyDBProvider) Title() string { return "Clusters" }

func (p alloyDBProvider) List(ctx context.Context, project string) (console.Listing, error) {
	s, err := alloyDBAPI(p.svc)
	if err != nil {
		return console.Listing{}, err
	}
	listing := console.Listing{Columns: []string{"Resource name", "Resource type", "Region/Zone", "Status", "Version", "Instances", "Private IP"},
		NameColumn: "Name", Noun: "clusters"}
	if project == "" {
		listing.Prompt = "AlloyDB lists a project's clusters. Choose one in the toolbar."
		return listing, nil
	}
	resp, err := s.ListClusters(ctx, &alloydbpb.ListClustersRequest{Parent: "projects/" + project + "/locations/-", PageSize: 1000})
	if err != nil {
		return console.Listing{}, err
	}
	for _, c := range resp.GetClusters() {
		ins, _ := s.ListInstances(ctx, &alloydbpb.ListInstancesRequest{Parent: c.GetName(), PageSize: 1000})
		listing.Items = append(listing.Items, console.Resource{Name: c.GetName(), Status: c.GetState().String(), Fields: map[string]string{
			"Resource name": lastSegment(c.GetName()), "Resource type": "Cluster", "Region/Zone": kmsLocation(c.GetName()),
			"Status": alloyDBState(c.GetState()), "Version": alloyDBVersionLabel(c.GetDatabaseVersion()),
			"Instances": strconv.Itoa(len(ins.GetInstances())), "Private IP": "-"}})
	}
	listing.Total = len(listing.Items)
	listing.RowsOpenable = true
	return listing, nil
}

func (p alloyDBProvider) CreateForm() (string, []console.Field) {
	const basics, primary = "Configure your cluster", "Configure your primary instance"
	return "Create cluster", []console.Field{
		{Name: "clusterId", Label: "Cluster ID", Type: "text", Required: true, Section: basics, Pattern: `^[a-z]([a-z0-9\-]{0,61}[a-z0-9])?$`,
			Help: "Lower-case letters, numbers and hyphens, starting with a letter; at most 63 characters."},
		{Name: "password", Label: "Password", Type: "password", Required: true, Section: basics,
			Help: "The password of the postgres user, the cluster's default user."},
		{Name: "version", Label: "Database version", Type: "select", Section: basics, Options: alloyDBVersions, OptionLabels: alloyDBVersionLabels, Default: "POSTGRES_16"},
		{Name: "region", Label: "Region", Type: "select", Section: basics, Options: alloydb.Locations(), Default: "us-central1"},
		{Name: "network", Label: "Network", Type: "text", Section: basics, Default: "default",
			Help: "The VPC network the cluster's instances are reached on, by private services access."},
		{Name: "instanceId", Label: "Instance ID", Type: "text", Required: true, Section: primary, Pattern: `^[a-z]([a-z0-9\-]{0,61}[a-z0-9])?$`},
		{Name: "availability", Label: "Zonal availability", Type: "radio", Section: primary, Options: []string{"REGIONAL", "ZONAL"}, Default: "REGIONAL",
			OptionLabels: map[string]string{"REGIONAL": "Multiple zones (Highly available)", "ZONAL": "Single zone"}},
		{Name: "cpu", Label: "Machine", Type: "select", Section: primary, Options: alloyDBCPUs, OptionLabels: alloyDBCPULabels(), Default: "2"},
	}
}

func (p alloyDBProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	s, err := alloyDBAPI(p.svc)
	if err != nil {
		return "", err
	}
	if project == "" {
		return "", fmt.Errorf("choose a project before creating a cluster")
	}
	parent := "projects/" + project + "/locations/" + strings.TrimSpace(values["region"])
	id := strings.TrimSpace(values["clusterId"])
	network := strings.TrimSpace(values["network"])
	if network != "" && !strings.HasPrefix(network, "projects/") {
		network = "projects/" + project + "/global/networks/" + network
	}
	version := alloydbpb.DatabaseVersion(alloydbpb.DatabaseVersion_value[values["version"]])
	op, err := s.CreateCluster(ctx, &alloydbpb.CreateClusterRequest{Parent: parent, ClusterId: id, Cluster: &alloydbpb.Cluster{
		DatabaseVersion: version, NetworkConfig: &alloydbpb.Cluster_NetworkConfig{Network: network},
		InitialUser: &alloydbpb.UserPassword{User: "postgres", Password: values["password"]}}})
	if err != nil {
		return "", err
	}
	if err := alloyDBWait(ctx, s, op, 30*time.Second); err != nil {
		return "", err
	}
	cpu, _ := strconv.Atoi(values["cpu"])
	avail := alloydbpb.Instance_REGIONAL
	if values["availability"] == "ZONAL" {
		avail = alloydbpb.Instance_ZONAL
	}
	name := parent + "/clusters/" + id
	if _, err := s.CreateInstance(ctx, &alloydbpb.CreateInstanceRequest{Parent: name, InstanceId: strings.TrimSpace(values["instanceId"]),
		Instance: &alloydbpb.Instance{InstanceType: alloydbpb.Instance_PRIMARY, AvailabilityType: avail,
			MachineConfig: &alloydbpb.Instance_MachineConfig{CpuCount: int32(cpu)}}}); err != nil {
		return "", err
	}
	return name, nil
}

func (p alloyDBProvider) cluster(ctx context.Context, project, name string) (*alloydb.Server, *alloydbpb.Cluster, error) {
	s, err := alloyDBAPI(p.svc)
	if err != nil {
		return nil, nil, err
	}
	if err := kmsOwned(project, name); err != nil {
		return nil, nil, err
	}
	c, err := s.GetCluster(ctx, &alloydbpb.GetClusterRequest{Name: name})
	return s, c, err
}

func alloyDBInstances(ctx context.Context, s *alloydb.Server, cluster string) ([]*alloydbpb.Instance, console.Listing) {
	l := console.Listing{Columns: []string{"Instance", "Status", "Type", "Machine", "Availability", "Nodes", "Private IP", "Connectivity", "Connection pooling"},
		NameColumn: "Name", Noun: "instances", TableTitle: "Instances in your cluster"}
	resp, err := s.ListInstances(ctx, &alloydbpb.ListInstancesRequest{Parent: cluster, PageSize: 1000})
	if err != nil {
		l.Unavailable = consoleErr(err)
		return nil, l
	}
	ins := resp.GetInstances()
	sort.SliceStable(ins, func(i, j int) bool { return ins[i].GetInstanceType() < ins[j].GetInstanceType() })
	for _, in := range ins {
		nodes := 1
		avail := "Zonal"
		if in.GetAvailabilityType() == alloydbpb.Instance_REGIONAL {
			avail = "Regional (highly available)"
		}
		if in.GetInstanceType() == alloydbpb.Instance_READ_POOL {
			nodes, avail = int(in.GetReadPoolConfig().GetNodeCount()), "-"
		}
		pooling := "Disabled"
		if in.GetConnectionPoolConfig().GetEnabled() {
			pooling = "Enabled"
		}
		conn := "Private IP"
		if in.GetPscInstanceConfig().GetPscDnsName() != "" {
			conn = "Private Service Connect"
		}
		l.Items = append(l.Items, console.Resource{Name: lastSegment(in.GetName()), Status: in.GetState().String(), Fields: map[string]string{
			"Instance": lastSegment(in.GetName()), "Status": alloyDBState(in.GetState()), "Type": alloyDBInstanceType(in.GetInstanceType()),
			"Machine":      fmt.Sprintf("%d vCPU, %d GB", in.GetMachineConfig().GetCpuCount(), in.GetMachineConfig().GetCpuCount()*8),
			"Availability": avail, "Nodes": strconv.Itoa(nodes), "Private IP": kmsOrDash(in.GetIpAddress()), "Connectivity": conn,
			"Connection pooling": pooling}})
	}
	l.Total = len(l.Items)
	return ins, l
}

func (p alloyDBProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) != 1 {
		return console.DeeperThan(1, path), nil
	}
	s, c, err := p.cluster(ctx, project, path[0])
	if err != nil {
		return console.Detail{Unavailable: "cannot read the cluster: " + consoleErr(err)}, nil
	}
	ins, instances := alloyDBInstances(ctx, s, c.GetName())
	var primary *alloydbpb.Instance
	for _, in := range ins {
		if t := in.GetInstanceType(); t == alloydbpb.Instance_PRIMARY || t == alloydbpb.Instance_SECONDARY {
			primary = in
		}
	}
	cb := "Disabled"
	if c.GetContinuousBackupConfig().GetEnabled() {
		cb = fmt.Sprintf("Enabled, %d days", c.GetContinuousBackupConfig().GetRecoveryWindowDays())
	}
	earliest := "-"
	if t := c.GetContinuousBackupInfo().GetEarliestRestorableTime(); t != nil {
		earliest = kmsTime(t)
	}
	auto := "Disabled"
	if c.GetAutomatedBackupPolicy().GetEnabled() {
		auto = "Enabled"
	}
	enc := "Google-managed"
	if k := c.GetEncryptionConfig().GetKmsKeyName(); k != "" {
		enc = "Customer-managed (" + k + ")"
	}
	overview := []console.PropertyGroup{{Heading: "Cluster", Properties: []console.Property{
		{Label: "Cluster ID", Value: lastSegment(c.GetName())}, {Label: "Status", Value: alloyDBState(c.GetState())},
		{Label: "Region", Value: kmsLocation(c.GetName())}, {Label: "Database version", Value: alloyDBVersionLabel(c.GetDatabaseVersion())},
		{Label: "Network", Value: kmsOrDash(c.GetNetworkConfig().GetNetwork())}, {Label: "Encryption", Value: enc},
		{Label: "Continuous backup and recovery", Value: cb}, {Label: "Earliest restore point", Value: earliest},
		{Label: "Automated backups", Value: auto}, {Label: "Created", Value: kmsTime(c.GetCreateTime())}}}}
	clusterType := "Primary"
	repl := []console.Property{}
	if c.GetClusterType() == alloydbpb.Cluster_SECONDARY {
		clusterType = "Secondary"
		repl = append(repl, console.Property{Label: "Primary cluster", Value: c.GetSecondaryConfig().GetPrimaryClusterName()})
	} else if n := c.GetPrimaryConfig().GetSecondaryClusterNames(); len(n) > 0 {
		repl = append(repl, console.Property{Label: "Secondary clusters", Value: strings.Join(n, ", ")})
	}
	overview[0].Properties = append([]console.Property{overview[0].Properties[0], {Label: "Cluster type", Value: clusterType}}, overview[0].Properties[1:]...)
	if len(repl) > 0 {
		overview = append(overview, console.PropertyGroup{Heading: "Cross-region replication", Properties: repl})
	}
	if primary != nil {
		info, err := s.GetConnectionInfo(ctx, &alloydbpb.GetConnectionInfoRequest{Parent: primary.GetName()})
		props := []console.Property{{Label: "Instance URI", Value: primary.GetName()}}
		if err == nil {
			props = append(props, console.Property{Label: "Private IP", Value: info.GetIpAddress()},
				console.Property{Label: "Connector port", Value: strconv.Itoa(alloydb.ConnectorPort)})
		}
		if a := p.svc.engine.Addr(alloydb.ServerOf(primary.GetName())); a != "" {
			props = append(props, console.Property{Label: "Local PostgreSQL address", Value: a})
		}
		overview = append(overview, console.PropertyGroup{Heading: "Connectivity", Properties: props})
	}
	return console.Detail{
		Summary: []console.Property{{Label: "Region", Value: kmsLocation(c.GetName())}, {Label: "Status", Value: alloyDBState(c.GetState())},
			{Label: "Version", Value: alloyDBVersionLabel(c.GetDatabaseVersion())}},
		Sections: []console.Section{
			{ID: "overview", Label: "Overview", Kind: console.KindProperties, Groups: overview, Listing: instances},
			{ID: "instances", Label: "Instances", Listing: instances},
			{ID: "backups", Label: "Backups", Listing: alloyDBBackupListing(ctx, s, c.GetName(), kmsLocation(c.GetName()))},
			{ID: "users", Label: "Users", Listing: alloyDBUsers(ctx, s, c.GetName())},
			{ID: "databases", Label: "Databases", Listing: alloyDBDatabases(ctx, s, c.GetName())},
		},
	}, nil
}

func alloyDBUsers(ctx context.Context, s *alloydb.Server, cluster string) console.Listing {
	l := console.Listing{Columns: []string{"User name", "Authentication", "Database roles"}, NameColumn: "Name", Noun: "users"}
	resp, err := s.ListUsers(ctx, &alloydbpb.ListUsersRequest{Parent: cluster, PageSize: 1000})
	if err != nil {
		l.Unavailable = consoleErr(err)
		return l
	}
	for _, u := range resp.GetUsers() {
		auth := "Built-in"
		if u.GetUserType() == alloydbpb.User_ALLOYDB_IAM_USER {
			auth = "Cloud IAM"
		}
		l.Items = append(l.Items, console.Resource{Name: lastSegment(u.GetName()), Fields: map[string]string{
			"User name": lastSegment(u.GetName()), "Authentication": auth, "Database roles": kmsOrDash(strings.Join(u.GetDatabaseRoles(), ", "))}})
	}
	l.Total = len(l.Items)
	return l
}

func alloyDBDatabases(ctx context.Context, s *alloydb.Server, cluster string) console.Listing {
	l := console.Listing{Columns: []string{"Database", "Character set", "Collation"}, NameColumn: "Name", Noun: "databases"}
	resp, err := s.ListDatabases(ctx, &alloydbpb.ListDatabasesRequest{Parent: cluster, PageSize: 1000})
	if err != nil {
		l.Prompt = "Databases are listed once the cluster's primary instance is ready."
		return l
	}
	for _, d := range resp.GetDatabases() {
		if d.GetIsTemplateDatabase() {
			continue
		}
		l.Items = append(l.Items, console.Resource{Name: lastSegment(d.GetName()), Fields: map[string]string{
			"Database": lastSegment(d.GetName()), "Character set": d.GetCharset(), "Collation": d.GetCollation()}})
	}
	l.Total = len(l.Items)
	return l
}

func alloyDBBackupListing(ctx context.Context, s *alloydb.Server, cluster, region string) console.Listing {
	l := console.Listing{Columns: []string{"Backup", "Status", "Type", "Cluster", "Created", "Size", "Expires"}, NameColumn: "Name", Noun: "backups"}
	parent := strings.Join(strings.Split(cluster, "/")[:4], "/")
	if region == "" {
		parent = cluster
	}
	resp, err := s.ListBackups(ctx, &alloydbpb.ListBackupsRequest{Parent: parent, PageSize: 1000})
	if err != nil {
		l.Unavailable = consoleErr(err)
		return l
	}
	for _, b := range resp.GetBackups() {
		if region != "" && b.GetClusterName() != cluster {
			continue
		}
		typ := map[alloydbpb.Backup_Type]string{alloydbpb.Backup_ON_DEMAND: "On demand", alloydbpb.Backup_AUTOMATED: "Automated",
			alloydbpb.Backup_CONTINUOUS: "Continuous"}[b.GetType()]
		exp := "-"
		if b.GetExpiryTime() != nil {
			exp = kmsTime(b.GetExpiryTime())
		}
		l.Items = append(l.Items, console.Resource{Name: lastSegment(b.GetName()), Status: b.GetState().String(), Fields: map[string]string{
			"Backup": lastSegment(b.GetName()), "Status": alloyDBState(b.GetState()), "Type": typ, "Cluster": lastSegment(b.GetClusterName()),
			"Created": kmsTime(b.GetCreateTime()), "Size": consoleBytes(b.GetSizeBytes()), "Expires": exp}})
	}
	l.Total = len(l.Items)
	return l
}

func consoleBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func (p alloyDBProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 1 {
		return nil
	}
	s, c, err := p.cluster(ctx, project, path[0])
	if err != nil {
		return nil
	}
	ins, _ := alloyDBInstances(ctx, s, c.GetName())
	var names []string
	hasPrimary := false
	for _, in := range ins {
		names = append(names, lastSegment(in.GetName()))
		hasPrimary = hasPrimary || in.GetInstanceType() == alloydbpb.Instance_PRIMARY
	}
	idPattern := `^[a-z]([a-z0-9\-]{0,61}[a-z0-9])?$`
	readPool := console.Action{ID: "add-read-pool", Label: "Add read pool instance", Fields: []console.Field{
		{Name: "instanceId", Label: "Read pool instance ID", Type: "text", Required: true, Pattern: idPattern},
		{Name: "nodes", Label: "Node count", Type: "number", Default: "1", Help: "1 to 20 nodes."},
		{Name: "cpu", Label: "Machine", Type: "select", Options: alloyDBCPUs, OptionLabels: alloyDBCPULabels(), Default: "2"}}}
	primary := console.Action{ID: "add-primary", Label: "Create primary instance", Fields: []console.Field{
		{Name: "instanceId", Label: "Instance ID", Type: "text", Required: true, Pattern: idPattern},
		{Name: "cpu", Label: "Machine", Type: "select", Options: alloyDBCPUs, OptionLabels: alloyDBCPULabels(), Default: "2"}}}
	if hasPrimary {
		primary.Unavailable = "The cluster has a primary instance."
	} else {
		readPool.Unavailable = "Create the cluster's primary instance first."
	}
	backup := console.Action{ID: "backup", Label: "Create backup", Fields: []console.Field{
		{Name: "backupId", Label: "Backup ID", Type: "text", Required: true, Pattern: idPattern}}}
	if !hasPrimary {
		backup.Unavailable = "A backup is taken of the cluster's primary instance, which it does not have."
	}
	restore := console.Action{ID: "restore-pitr", Label: "Restore to a point in time", Fields: []console.Field{
		{Name: "clusterId", Label: "New cluster ID", Type: "text", Required: true, Pattern: idPattern},
		{Name: "time", Label: "Restore point (RFC 3339, UTC)", Type: "text", Required: true, Default: time.Now().UTC().Format(time.RFC3339)}}}
	if c.GetContinuousBackupInfo().GetEarliestRestorableTime() == nil {
		restore.Unavailable = "Continuous backup and recovery has no restore point yet."
	}
	secondary := c.GetClusterType() == alloydbpb.Cluster_SECONDARY
	if secondary {
		primary.Label, primary.ID = "Create secondary instance", "add-secondary"
		primary.Unavailable = ""
		if hasPrimary {
			primary.Unavailable = "The cluster has a secondary instance."
		}
		backup.Unavailable = ""
	}
	sqlOrCSV := map[string]string{"SQL": "SQL", "CSV": "CSV"}
	importAction := console.Action{ID: "import", Label: "Import", Fields: []console.Field{
		{Name: "uri", Label: "Source", Type: "text", Required: true, Help: "A Cloud Storage file: gs://bucket/file. A .gz file is decompressed."},
		{Name: "format", Label: "File format", Type: "radio", Options: []string{"SQL", "CSV"}, OptionLabels: sqlOrCSV, Default: "SQL"},
		{Name: "database", Label: "Database", Type: "text", Help: "For SQL, optional: the file can name its own with \\connect."},
		{Name: "table", Label: "Table", Type: "text", ShowWhen: "format=CSV"},
		{Name: "columns", Label: "Columns", Type: "text", ShowWhen: "format=CSV", Help: "Optional, separated by commas: every column, in the table's order, by default."},
		{Name: "user", Label: "User", Type: "text", Help: "Optional: the database user the import runs as."}}}
	exportAction := console.Action{ID: "export", Label: "Export", Fields: []console.Field{
		{Name: "uri", Label: "Destination", Type: "text", Required: true, Help: "A Cloud Storage file: gs://bucket/file. A name ending in .gz is compressed."},
		{Name: "format", Label: "File format", Type: "radio", Options: []string{"SQL", "CSV"}, OptionLabels: sqlOrCSV, Default: "SQL"},
		{Name: "database", Label: "Database", Type: "text", Required: true},
		{Name: "query", Label: "SQL query", Type: "text", ShowWhen: "format=CSV", Help: "The SELECT query whose rows are exported."},
		{Name: "tables", Label: "Tables", Type: "text", ShowWhen: "format=SQL", Help: "Optional, separated by commas: every table by default."},
		{Name: "schemaOnly", Label: "Export schema only", Type: "checkbox", ShowWhen: "format=SQL"},
		{Name: "clean", Label: "Clean target objects (DROP before CREATE, IF EXISTS)", Type: "checkbox", ShowWhen: "format=SQL"}}}
	if !hasPrimary {
		importAction.Unavailable = "An import runs on the cluster's primary instance, which it does not have."
		exportAction.Unavailable = "An export runs on the cluster's primary instance, which it does not have."
	}
	if secondary {
		importAction.Unavailable = "A secondary cluster takes no writes: import into its primary cluster."
	}
	createSecondary := console.Action{ID: "create-secondary", Label: "Create secondary cluster", Fields: []console.Field{
		{Name: "clusterId", Label: "Secondary cluster ID", Type: "text", Required: true, Pattern: idPattern},
		{Name: "region", Label: "Region", Type: "select", Options: alloydb.Locations(), Default: "us-east1",
			Help: "Another region is recommended."},
		{Name: "instanceId", Label: "Secondary instance ID", Type: "text", Required: true, Pattern: idPattern}}}
	switch {
	case secondary:
		createSecondary.Unavailable = "This is a secondary cluster: create secondaries of its primary cluster."
	case !hasPrimary:
		createSecondary.Unavailable = "A secondary cluster replicates the primary instance, which this cluster does not have."
	}
	promote := console.Action{ID: "promote", Label: "Promote", Confirm: "Replication from the primary cluster stops: this cluster becomes a standalone primary cluster, and its secondary instance takes writes."}
	switchover := console.Action{ID: "switchover", Label: "Switchover", Confirm: "This cluster becomes the primary cluster, and the primary cluster its secondary. Stop writing to the primary first."}
	if !secondary {
		promote.Unavailable = "Only a secondary cluster is promoted."
		switchover.Unavailable = "Only a secondary cluster switches over."
	}
	return []console.Action{primary, readPool,
		{ID: "edit-instance", Label: "Edit instance", Fields: []console.Field{
			{Name: "instance", Label: "Instance", Type: "select", Required: true, Options: names},
			{Name: "cpu", Label: "Machine", Type: "select", Options: alloyDBCPUs, OptionLabels: alloyDBCPULabels(), Default: "2"},
			{Name: "nodes", Label: "Node count (read pool)", Type: "number", Default: "1"},
			{Name: "flags", Label: "Database flags", Type: "text", Help: "name=value pairs, separated by commas."},
			{Name: "pooling", Label: "Enable Managed Connection Pooling", Type: "checkbox",
				Help: "A pooler beside the instance, on port 6432; the connectors and the Auth Proxy are pooled too."},
			{Name: "poolFlags", Label: "Connection pooling flags", Type: "text", ShowWhen: "pooling=true",
				Help: "name=value pairs, separated by commas: pool_mode, max_pool_size, min_pool_size, max_client_connections, ..."}}},
		importAction, exportAction, createSecondary, promote, switchover,
		{ID: "restart", Label: "Restart instance", Fields: []console.Field{{Name: "instance", Label: "Instance", Type: "select", Required: true, Options: names}}},
		{ID: "failover", Label: "Failover", Confirm: "The primary instance's standby becomes its active node.", Fields: []console.Field{
			{Name: "instance", Label: "Instance", Type: "select", Required: true, Options: names}}},
		{ID: "delete-instance", Label: "Delete instance", Destructive: true, Fields: []console.Field{
			{Name: "instance", Label: "Instance", Type: "select", Required: true, Options: names}}, Confirm: "The instance and its data are deleted."},
		backup, restore,
		{ID: "add-user", Label: "Add user account", Fields: []console.Field{
			{Name: "type", Label: "Authentication", Type: "radio", Options: []string{"ALLOYDB_BUILT_IN", "ALLOYDB_IAM_USER"}, Default: "ALLOYDB_BUILT_IN",
				OptionLabels: map[string]string{"ALLOYDB_BUILT_IN": "Built-in database authentication", "ALLOYDB_IAM_USER": "Cloud IAM"}},
			{Name: "user", Label: "User name", Type: "text", Required: true, Help: "For Cloud IAM, the principal's email address."},
			{Name: "password", Label: "Password", Type: "password", ShowWhen: "type=ALLOYDB_BUILT_IN"},
			{Name: "superuser", Label: "Grant alloydbsuperuser", Type: "checkbox"}}},
		{ID: "delete-user", Label: "Delete user", Destructive: true, Fields: []console.Field{{Name: "user", Label: "User name", Type: "text", Required: true}}},
		{ID: "delete", Label: "Delete cluster", Destructive: true, Leaves: true, Confirm: "The cluster and all its instances are deleted. Its backups are kept."},
	}
}

func (p alloyDBProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) != 1 {
		return fmt.Errorf("no cluster")
	}
	s, c, err := p.cluster(ctx, project, path[0])
	if err != nil {
		return err
	}
	inst := c.GetName() + "/instances/" + strings.TrimSpace(values["instance"])
	cpu, _ := strconv.Atoi(values["cpu"])
	nodes, _ := strconv.Atoi(values["nodes"])
	var op *longrunningpb.Operation
	switch action {
	case "add-primary":
		op, err = s.CreateInstance(ctx, &alloydbpb.CreateInstanceRequest{Parent: c.GetName(), InstanceId: strings.TrimSpace(values["instanceId"]),
			Instance: &alloydbpb.Instance{InstanceType: alloydbpb.Instance_PRIMARY, MachineConfig: &alloydbpb.Instance_MachineConfig{CpuCount: int32(cpu)}}})
		return err
	case "add-read-pool":
		_, err = s.CreateInstance(ctx, &alloydbpb.CreateInstanceRequest{Parent: c.GetName(), InstanceId: strings.TrimSpace(values["instanceId"]),
			Instance: &alloydbpb.Instance{InstanceType: alloydbpb.Instance_READ_POOL, MachineConfig: &alloydbpb.Instance_MachineConfig{CpuCount: int32(cpu)},
				ReadPoolConfig: &alloydbpb.Instance_ReadPoolConfig{NodeCount: int32(nodes)}}})
		return err
	case "edit-instance":
		in, err := s.GetInstance(ctx, &alloydbpb.GetInstanceRequest{Name: inst})
		if err != nil {
			return err
		}
		next := &alloydbpb.Instance{Name: inst, MachineConfig: &alloydbpb.Instance_MachineConfig{CpuCount: int32(cpu)}, DatabaseFlags: map[string]string{}}
		paths := []string{"machine_config.cpu_count", "database_flags"}
		for _, kv := range strings.Split(values["flags"], ",") {
			if k, v, ok := strings.Cut(strings.TrimSpace(kv), "="); ok {
				next.DatabaseFlags[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
		if in.GetInstanceType() == alloydbpb.Instance_READ_POOL {
			next.ReadPoolConfig = &alloydbpb.Instance_ReadPoolConfig{NodeCount: int32(nodes)}
			paths = append(paths, "read_pool_config.node_count")
		}
		if in.GetInstanceType() != alloydbpb.Instance_SECONDARY {
			next.ConnectionPoolConfig = &alloydbpb.Instance_ConnectionPoolConfig{Enabled: values["pooling"] == "true" || values["pooling"] == "on",
				Flags: map[string]string{}}
			for _, kv := range strings.Split(values["poolFlags"], ",") {
				if k, v, ok := strings.Cut(strings.TrimSpace(kv), "="); ok {
					next.ConnectionPoolConfig.Flags[strings.TrimSpace(k)] = strings.TrimSpace(v)
				}
			}
			paths = append(paths, "connection_pool_config")
		}
		op, err = s.UpdateInstance(ctx, &alloydbpb.UpdateInstanceRequest{Instance: next, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}})
		if err != nil {
			return err
		}
		return alloyDBWait(ctx, s, op, 5*time.Second)
	case "add-secondary":
		op, err = s.CreateSecondaryInstance(ctx, &alloydbpb.CreateSecondaryInstanceRequest{Parent: c.GetName(), InstanceId: strings.TrimSpace(values["instanceId"]),
			Instance: &alloydbpb.Instance{InstanceType: alloydbpb.Instance_SECONDARY, MachineConfig: &alloydbpb.Instance_MachineConfig{CpuCount: int32(cpu)}}})
		return err
	case "import":
		req := &alloydbpb.ImportClusterRequest{Name: c.GetName(), GcsUri: strings.TrimSpace(values["uri"]), Database: strings.TrimSpace(values["database"]),
			User: strings.TrimSpace(values["user"])}
		if values["format"] == "CSV" {
			req.ImportOptions = &alloydbpb.ImportClusterRequest_CsvImportOptions_{CsvImportOptions: &alloydbpb.ImportClusterRequest_CsvImportOptions{
				Table: strings.TrimSpace(values["table"]), Columns: consoleList(values["columns"])}}
		} else {
			req.ImportOptions = &alloydbpb.ImportClusterRequest_SqlImportOptions_{SqlImportOptions: &alloydbpb.ImportClusterRequest_SqlImportOptions{}}
		}
		op, err = s.ImportCluster(ctx, req)
		if err != nil {
			return err
		}
		return alloyDBWait(ctx, s, op, 2*time.Minute)
	case "export":
		req := &alloydbpb.ExportClusterRequest{Name: c.GetName(), Database: strings.TrimSpace(values["database"]),
			Destination: &alloydbpb.ExportClusterRequest_GcsDestination{GcsDestination: &alloydbpb.GcsDestination{Uri: strings.TrimSpace(values["uri"])}}}
		if values["format"] == "CSV" {
			req.ExportOptions = &alloydbpb.ExportClusterRequest_CsvExportOptions_{CsvExportOptions: &alloydbpb.ExportClusterRequest_CsvExportOptions{
				SelectQuery: values["query"]}}
		} else {
			on := func(k string) *bool { b := values[k] == "true" || values[k] == "on"; return &b }
			o := &alloydbpb.ExportClusterRequest_SqlExportOptions{Tables: consoleList(values["tables"]), SchemaOnly: on("schemaOnly"),
				CleanTargetObjects: on("clean"), IfExistTargetObjects: on("clean")}
			req.ExportOptions = &alloydbpb.ExportClusterRequest_SqlExportOptions_{SqlExportOptions: o}
		}
		op, err = s.ExportCluster(ctx, req)
		if err != nil {
			return err
		}
		return alloyDBWait(ctx, s, op, 2*time.Minute)
	case "create-secondary":
		loc := strings.Join(strings.Split(c.GetName(), "/")[:3], "/") + "/" + strings.TrimSpace(values["region"])
		sid := strings.TrimSpace(values["clusterId"])
		op, err = s.CreateSecondaryCluster(ctx, &alloydbpb.CreateSecondaryClusterRequest{Parent: loc, ClusterId: sid, Cluster: &alloydbpb.Cluster{
			NetworkConfig: c.GetNetworkConfig(), SecondaryConfig: &alloydbpb.Cluster_SecondaryConfig{PrimaryClusterName: c.GetName()}}})
		if err != nil {
			return err
		}
		if err := alloyDBWait(ctx, s, op, 30*time.Second); err != nil {
			return err
		}
		_, err = s.CreateSecondaryInstance(ctx, &alloydbpb.CreateSecondaryInstanceRequest{Parent: loc + "/clusters/" + sid,
			InstanceId: strings.TrimSpace(values["instanceId"]), Instance: &alloydbpb.Instance{}})
		return err
	case "promote":
		op, err = s.PromoteCluster(ctx, &alloydbpb.PromoteClusterRequest{Name: c.GetName()})
		if err != nil {
			return err
		}
		return alloyDBWait(ctx, s, op, 2*time.Minute)
	case "switchover":
		op, err = s.SwitchoverCluster(ctx, &alloydbpb.SwitchoverClusterRequest{Name: c.GetName()})
		if err != nil {
			return err
		}
		return alloyDBWait(ctx, s, op, 3*time.Minute)
	case "restart":
		op, err = s.RestartInstance(ctx, &alloydbpb.RestartInstanceRequest{Name: inst})
	case "failover":
		op, err = s.FailoverInstance(ctx, &alloydbpb.FailoverInstanceRequest{Name: inst})
	case "delete-instance":
		op, err = s.DeleteInstance(ctx, &alloydbpb.DeleteInstanceRequest{Name: inst})
	case "backup":
		loc := strings.Join(strings.Split(c.GetName(), "/")[:4], "/")
		op, err = s.CreateBackup(ctx, &alloydbpb.CreateBackupRequest{Parent: loc, BackupId: strings.TrimSpace(values["backupId"]),
			Backup: &alloydbpb.Backup{ClusterName: c.GetName()}})
	case "restore-pitr":
		at, perr := time.Parse(time.RFC3339, strings.TrimSpace(values["time"]))
		if perr != nil {
			return fmt.Errorf("restore point: %v", perr)
		}
		loc := strings.Join(strings.Split(c.GetName(), "/")[:4], "/")
		_, err = s.RestoreCluster(ctx, &alloydbpb.RestoreClusterRequest{Parent: loc, ClusterId: strings.TrimSpace(values["clusterId"]),
			Cluster: &alloydbpb.Cluster{NetworkConfig: c.GetNetworkConfig()},
			Source:  &alloydbpb.RestoreClusterRequest_ContinuousBackupSource{ContinuousBackupSource: &alloydbpb.ContinuousBackupSource{Cluster: c.GetName(), PointInTime: timestamppb.New(at)}}})
		return err
	case "add-user":
		u := &alloydbpb.User{UserType: alloydbpb.User_UserType(alloydbpb.User_UserType_value[values["type"]])}
		if u.UserType != alloydbpb.User_ALLOYDB_IAM_USER {
			u.Password = values["password"]
		}
		if values["superuser"] == "true" || values["superuser"] == "on" {
			u.DatabaseRoles = []string{alloydb.SuperuserRole}
		}
		_, err = s.CreateUser(ctx, &alloydbpb.CreateUserRequest{Parent: c.GetName(), UserId: strings.TrimSpace(values["user"]), User: u})
		return err
	case "delete-user":
		_, err = s.DeleteUser(ctx, &alloydbpb.DeleteUserRequest{Name: c.GetName() + "/users/" + strings.TrimSpace(values["user"])})
		return err
	case "delete":
		op, err = s.DeleteCluster(ctx, &alloydbpb.DeleteClusterRequest{Name: c.GetName(), Force: true})
		if err != nil {
			return err
		}
		return alloyDBWait(ctx, s, op, 2*time.Minute)
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	if err != nil {
		return err
	}
	return alloyDBWait(ctx, s, op, 3*time.Second)
}

// ---- Backups ----

type alloyDBBackupsProvider struct{ svc *alloyDBService }

func (alloyDBBackupsProvider) ID() string    { return "alloydb-backups" }
func (alloyDBBackupsProvider) Title() string { return "Backups" }

func (p alloyDBBackupsProvider) List(ctx context.Context, project string) (console.Listing, error) {
	s, err := alloyDBAPI(p.svc)
	if err != nil {
		return console.Listing{}, err
	}
	if project == "" {
		return console.Listing{Prompt: "AlloyDB lists a project's backups. Choose one in the toolbar."}, nil
	}
	l := alloyDBBackupListing(ctx, s, "projects/"+project+"/locations/-", "")
	for i := range l.Items {
		name := l.Items[i].Name
		var full string
		if resp, err := s.ListBackups(ctx, &alloydbpb.ListBackupsRequest{Parent: "projects/" + project + "/locations/-", PageSize: 1000}); err == nil {
			for _, b := range resp.GetBackups() {
				if lastSegment(b.GetName()) == name {
					full = b.GetName()
				}
			}
		}
		l.Items[i].Name = full
		l.Items[i].Fields["Region"] = kmsLocation(full)
		l.Items[i].Actions = []console.Action{
			{ID: "restore", Label: "Restore", Fields: []console.Field{
				{Name: "clusterId", Label: "New cluster ID", Type: "text", Required: true, Pattern: `^[a-z]([a-z0-9\-]{0,61}[a-z0-9])?$`},
				{Name: "network", Label: "Network", Type: "text", Default: "default"}}},
			{ID: "delete", Label: "Delete", Destructive: true, Confirm: "The backup is deleted."},
		}
	}
	l.Columns = append(l.Columns, "Region")
	return l, nil
}

func (p alloyDBBackupsProvider) Act(ctx context.Context, project, name, action string, values map[string]string) error {
	s, err := alloyDBAPI(p.svc)
	if err != nil {
		return err
	}
	if err := kmsOwned(project, name); err != nil {
		return err
	}
	switch action {
	case "restore":
		network := strings.TrimSpace(values["network"])
		if network != "" && !strings.HasPrefix(network, "projects/") {
			network = "projects/" + project + "/global/networks/" + network
		}
		loc := strings.Join(strings.Split(name, "/")[:4], "/")
		_, err = s.RestoreCluster(ctx, &alloydbpb.RestoreClusterRequest{Parent: loc, ClusterId: strings.TrimSpace(values["clusterId"]),
			Cluster: &alloydbpb.Cluster{NetworkConfig: &alloydbpb.Cluster_NetworkConfig{Network: network}},
			Source:  &alloydbpb.RestoreClusterRequest_BackupSource{BackupSource: &alloydbpb.BackupSource{BackupName: name}}})
		return err
	case "delete":
		op, err := s.DeleteBackup(ctx, &alloydbpb.DeleteBackupRequest{Name: name})
		if err != nil {
			return err
		}
		return alloyDBWait(ctx, s, op, 5*time.Second)
	}
	return fmt.Errorf("unknown action %q", action)
}

// ---- reset ----

type alloyDBResetter struct{ svc *alloyDBService }

func (r *alloyDBResetter) Name() string { return "alloydb" }

func (r *alloyDBResetter) Reset(ctx context.Context) error { return r.ResetProject(ctx, "") }

func (r *alloyDBResetter) ResetProject(ctx context.Context, project string) error {
	api := r.svc.API()
	if api == nil {
		return errors.New(alloyDBNotStarted)
	}
	return api.ResetProject(ctx, project)
}

// consoleList is a comma-separated field's items, trimmed, without empty
// ones.
func consoleList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
