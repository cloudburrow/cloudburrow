//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	database "cloud.google.com/go/spanner/admin/database/apiv1"
	databasepb "cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	instance "cloud.google.com/go/spanner/admin/instance/apiv1"
	instancepb "cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// spannerConsolePage is the part of a Spanner console page this test reads.
type spannerConsolePage struct {
	Unavailable string
	Actions     []struct{ ID, Confirm string }
	Sections    []struct {
		ID, Unavailable string
		Groups          []struct {
			Heading    string
			Properties []struct{ Label, Value string }
		}
		Listing struct {
			Unavailable string
			Items       []struct {
				Name   string
				Fields map[string]string
				Opens  []string
			}
		}
	}
}

func (p spannerConsolePage) section(t *testing.T, id string) int {
	t.Helper()
	for i, s := range p.Sections {
		if s.ID == id {
			if s.Unavailable != "" || s.Listing.Unavailable != "" {
				t.Fatalf("the %s section is unavailable: %q %q", id, s.Unavailable, s.Listing.Unavailable)
			}
			return i
		}
	}
	t.Fatalf("the page has no %s section (unavailable: %q)", id, p.Unavailable)
	return -1
}

// TestConsoleSpannerSchemaConfigurationsAndPartitions.
//
// The Spanner console surfaces of #1207, through the console API, checked
// with the official admin clients. Update schema on a database's page sends
// UpdateDatabaseDdl: a CREATE TABLE and a CREATE INDEX typed together, with a
// semicolon inside a string default, are both in GetDatabaseDdl afterwards,
// and a DML statement typed there is refused with nothing applied. An
// instance's page shows its configuration as GetInstanceConfig reports it and
// lists the configurations ListInstanceConfigs does, the one in use marked.
// Create instance partition makes a partition the admin client gets, with
// the display name typed; the Instance partitions tab lists it and its row
// opens its page; the Partition operations tab lists its create operation, as
// ListInstancePartitionOperations does; Delete instance partition leaves
// GetInstancePartition NOT_FOUND.
//
// covers: google.spanner.admin.database.v1.DatabaseAdmin/GetDatabaseDdl, google.spanner.admin.instance.v1.InstanceAdmin/GetInstanceConfig, google.spanner.admin.instance.v1.InstanceAdmin/ListInstanceConfigs, google.spanner.admin.instance.v1.InstanceAdmin/GetInstancePartition, google.spanner.admin.instance.v1.InstanceAdmin/ListInstancePartitionOperations
func TestConsoleSpannerSchemaConfigurationsAndPartitions(t *testing.T) {
	h := New(t)
	spAddr := h.Endpoint(EnvSpanner)
	addr := consoleAddr(t, h)
	ctx := h.Context()
	project := h.Project()
	opts := spannerOpts(spAddr)

	const instanceID, dbID = "console-admin", "schema"
	instName := fmt.Sprintf("projects/%s/instances/%s", project, instanceID)
	dbName := spannerDatabase(t, ctx, spAddr, project, instanceID, dbID,
		"CREATE TABLE Widgets (Id INT64 NOT NULL, Name STRING(MAX)) PRIMARY KEY (Id)")
	instAdmin, err := instance.NewInstanceAdminClient(ctx, opts...)
	if err != nil {
		t.Fatalf("NewInstanceAdminClient: %v", err)
	}
	defer instAdmin.Close()
	t.Cleanup(func() {
		_ = instAdmin.DeleteInstance(context.Background(), &instancepb.DeleteInstanceRequest{Name: instName})
	})
	dbAdmin, err := database.NewDatabaseAdminClient(ctx, opts...)
	if err != nil {
		t.Fatalf("NewDatabaseAdminClient: %v", err)
	}
	defer dbAdmin.Close()

	act := func(path []string, action string, values map[string]string) (int, string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"Path": path, "Action": action, "Values": values})
		return consoleDo(t, addr, http.MethodPost, "/api/actions/spanner?project="+project, string(body))
	}
	page := func(path ...string) spannerConsolePage {
		t.Helper()
		var p spannerConsolePage
		consoleJSON(t, addr, http.MethodGet, "/api/detail/spanner?"+url.Values{"project": {project}, "name": path}.Encode(), "", &p)
		return p
	}
	ddl := func() string {
		t.Helper()
		got, err := dbAdmin.GetDatabaseDdl(ctx, &databasepb.GetDatabaseDdlRequest{Database: dbName})
		if err != nil {
			t.Fatalf("GetDatabaseDdl: %v", err)
		}
		return strings.Join(got.GetStatements(), "\n")
	}

	// Update schema, offered on the database's page and confirmed by name.
	dbPage := page(instanceID, dbID)
	offered := false
	for _, a := range dbPage.Actions {
		if a.ID == "updateddl" {
			offered = strings.Contains(a.Confirm, "database "+dbID)
		}
	}
	if !offered {
		t.Fatalf("the database's page does not offer Update schema confirmed with the database named: %+v", dbPage.Actions)
	}
	if code, out := act([]string{instanceID, dbID}, "updateddl", map[string]string{"statements": `
		CREATE TABLE Gadgets (Id INT64 NOT NULL, Note STRING(MAX) DEFAULT ("a;b")) PRIMARY KEY (Id);
		CREATE INDEX WidgetsByName ON Widgets (Name);`}); code != http.StatusOK {
		t.Fatalf("console Update schema = %d: %s", code, out)
	}
	if got := ddl(); !strings.Contains(got, "CREATE TABLE Gadgets") || !strings.Contains(got, "WidgetsByName") {
		t.Errorf("after the console's Update schema, GetDatabaseDdl is %q; want Gadgets and WidgetsByName", got)
	}
	before := ddl()
	if code, out := act([]string{instanceID, dbID}, "updateddl", map[string]string{"statements": "CREATE TABLE Never (Id INT64) PRIMARY KEY (Id); DELETE FROM Widgets WHERE TRUE"}); code != http.StatusBadRequest ||
		!strings.Contains(consoleError(t, out), "changes data") {
		t.Errorf("Update schema with a DELETE = %d %s, want 400 saying it changes data", code, out)
	}
	if got := ddl(); got != before {
		t.Errorf("a refused Update schema changed the DDL to %q", got)
	}

	// The instance's configuration, and the project's configurations.
	inst, err := instAdmin.GetInstance(ctx, &instancepb.GetInstanceRequest{Name: instName})
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	cfg, err := instAdmin.GetInstanceConfig(ctx, &instancepb.GetInstanceConfigRequest{Name: inst.GetConfig()})
	if err != nil {
		t.Fatalf("GetInstanceConfig: %v", err)
	}
	instPage := page(instanceID)
	var shownConfig string
	for _, g := range instPage.Sections[instPage.section(t, "configuration")].Groups {
		if g.Heading == "Instance configuration" {
			for _, p := range g.Properties {
				if p.Label == "Configuration name" {
					shownConfig = p.Value
				}
			}
		}
	}
	if shownConfig != cfg.GetName() {
		t.Errorf("the instance's page shows the configuration %q, GetInstanceConfig %q", shownConfig, cfg.GetName())
	}
	listed := map[string]string{}
	for _, it := range instPage.Sections[instPage.section(t, "instance-configs")].Listing.Items {
		listed[it.Name] = it.Fields["In use"]
	}
	cit := instAdmin.ListInstanceConfigs(ctx, &instancepb.ListInstanceConfigsRequest{Parent: "projects/" + project})
	n := 0
	for {
		c, err := cit.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("ListInstanceConfigs: %v", err)
		}
		n++
		id := c.GetName()[strings.LastIndex(c.GetName(), "/")+1:]
		want := "No"
		if c.GetName() == inst.GetConfig() {
			want = "Yes"
		}
		if listed[id] != want {
			t.Errorf("the Instance configurations tab shows %s in use %q, want %q", id, listed[id], want)
		}
	}
	if n == 0 || len(listed) != n {
		t.Errorf("the tab lists %d configurations, ListInstanceConfigs %d", len(listed), n)
	}

	// Create, list, open, see the operation of, and delete a partition.
	const partition = "console-part"
	partName := instName + "/instancePartitions/" + partition
	if code, out := act([]string{instanceID}, "createpartition", map[string]string{
		"partition": partition, "displayName": "From the console", "config": "emulator-config", "nodeCount": "2"}); code != http.StatusOK {
		t.Fatalf("console Create instance partition = %d: %s", code, out)
	}
	got, err := instAdmin.GetInstancePartition(ctx, &instancepb.GetInstancePartitionRequest{Name: partName})
	if err != nil {
		t.Fatalf("GetInstancePartition of the console-created partition: %v", err)
	}
	if got.GetDisplayName() != "From the console" || got.GetConfig() != fmt.Sprintf("projects/%s/instanceConfigs/emulator-config", project) {
		t.Errorf("the console-created partition is %v", got)
	}
	instPage = page(instanceID)
	parts := instPage.Sections[instPage.section(t, "partitions")].Listing.Items
	if len(parts) != 1 || parts[0].Name != partition || parts[0].Fields["Display name"] != "From the console" ||
		strings.Join(parts[0].Opens, "/") != instanceID+"/instancePartitions/"+partition {
		t.Errorf("the Instance partitions tab lists %+v", parts)
	}
	var consoleOps []string
	for _, it := range instPage.Sections[instPage.section(t, "partition-operations")].Listing.Items {
		if it.Fields["Partition"] == partition {
			consoleOps = append(consoleOps, it.Name)
		}
	}
	var apiOps []string
	oit := instAdmin.ListInstancePartitionOperations(ctx, &instancepb.ListInstancePartitionOperationsRequest{Parent: instName})
	for {
		o, err := oit.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("ListInstancePartitionOperations: %v", err)
		}
		if strings.HasPrefix(o.GetName(), partName+"/operations/") {
			apiOps = append(apiOps, o.GetName()[strings.LastIndex(o.GetName(), "/")+1:])
		}
	}
	if len(apiOps) == 0 || strings.Join(consoleOps, ",") != strings.Join(apiOps, ",") {
		t.Errorf("the Partition operations tab lists %v for the partition, ListInstancePartitionOperations %v", consoleOps, apiOps)
	}
	partPage := page(instanceID, "instancePartitions", partition)
	var resourceName string
	for _, p := range partPage.Sections[partPage.section(t, "properties")].Groups[0].Properties {
		if p.Label == "Resource name" {
			resourceName = p.Value
		}
	}
	if resourceName != partName {
		t.Errorf("the partition's page shows %q, want %q", resourceName, partName)
	}

	if code, out := act([]string{instanceID, "instancePartitions", partition}, "deletepartition", nil); code != http.StatusOK {
		t.Fatalf("console Delete instance partition = %d: %s", code, out)
	}
	if _, err := instAdmin.GetInstancePartition(ctx, &instancepb.GetInstancePartitionRequest{Name: partName}); status.Code(err) != codes.NotFound {
		t.Errorf("GetInstancePartition after the console's delete = %v, want NOT_FOUND", err)
	}
}

// TestConsoleSpannerEditInstance (#1461): Edit instance on an instance's
// page, prefilled from it, changes its display name and processing units
// through the official admin client's UpdateInstance, which the call front
// in the Spanner pod answers; GetInstance reads the change back, and a
// capacity Google refuses is refused with the API's message.
//
// covers: google.spanner.admin.instance.v1.InstanceAdmin/UpdateInstance
func TestConsoleSpannerEditInstance(t *testing.T) {
	h := New(t)
	spAddr := h.Endpoint(EnvSpanner)
	addr := consoleAddr(t, h)
	ctx := h.Context()
	project := h.Project()
	const instanceID = "console-edit"
	instName := fmt.Sprintf("projects/%s/instances/%s", project, instanceID)
	spannerDatabase(t, ctx, spAddr, project, instanceID, "editdb", "CREATE TABLE T (Id INT64 NOT NULL) PRIMARY KEY (Id)")
	instAdmin, err := instance.NewInstanceAdminClient(ctx, spannerOpts(spAddr)...)
	if err != nil {
		t.Fatalf("NewInstanceAdminClient: %v", err)
	}
	defer instAdmin.Close()
	t.Cleanup(func() {
		_ = instAdmin.DeleteInstance(context.Background(), &instancepb.DeleteInstanceRequest{Name: instName})
	})
	var p spannerConsolePage
	consoleJSON(t, addr, http.MethodGet, "/api/detail/spanner?"+url.Values{"project": {project}, "name": {instanceID}}.Encode(), "", &p)
	offered := false
	for _, a := range p.Actions {
		offered = offered || a.ID == "editinstance"
	}
	if !offered {
		t.Fatalf("the instance's page offers no Edit instance: %+v", p.Actions)
	}
	act := func(values map[string]string) (int, string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"Path": []string{instanceID}, "Action": "editinstance", "Values": values})
		return consoleDo(t, addr, http.MethodPost, "/api/actions/spanner?project="+project, string(body))
	}
	if code, out := act(map[string]string{"displayName": "edited in console", "processingUnits": "300"}); code != http.StatusOK {
		t.Fatalf("Edit instance = %d %s", code, out)
	}
	in, err := instAdmin.GetInstance(ctx, &instancepb.GetInstanceRequest{Name: instName})
	if err != nil || in.GetDisplayName() != "edited in console" || in.GetProcessingUnits() != 300 {
		t.Errorf("after Edit instance = %v, %v", in, err)
	}
	if code, out := act(map[string]string{"displayName": "edited in console", "processingUnits": "1500"}); code == http.StatusOK || !strings.Contains(out, "processing_units") {
		t.Errorf("1500 processing units = %d %s, want the API's refusal", code, out)
	}
}
