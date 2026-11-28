package main

import (
	"context"
	"strings"
	"testing"

	alloydbpb "cloud.google.com/go/alloydb/apiv1/alloydbpb"
	"google.golang.org/genproto/googleapis/type/dayofweek"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/alloydb"
)

// TestConsoleAlloyDBParity2251Round2 holds round 2's pages to Google's:
// the Overview's Maintenance and its Edit, read back from the API; the
// Clusters list's Maintenance column; System insights' views and Query
// insights' tab with Cloud Monitoring not served (each says why);
// and the API's limit of one maintenance window.
func TestConsoleAlloyDBParity2251Round2(t *testing.T) {
	ctx := context.Background()
	api, err := alloydb.NewServer("", "", alloydb.Options{ConnectorHost: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	svc := &alloyDBService{api: api, engine: newAlloyDBEngine(config.Config{}, nil)}
	p := alloyDBProvider{svc: svc}
	_, _ = p.Create(ctx, "p1", map[string]string{"clusterId": "c1", "password": "pw", "version": "POSTGRES_16", "region": "us-central1",
		"network": "default", "instanceId": "primary", "cpu": "2"})
	name := "projects/p1/locations/us-central1/clusters/c1"
	detail := func() console.Detail {
		t.Helper()
		d, err := p.Detail(ctx, "p1", []string{name})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	section := func(d console.Detail, id string) console.Section {
		t.Helper()
		for _, s := range d.Sections {
			if s.ID == id {
				return s
			}
		}
		t.Fatalf("no %s tab in %v", id, d.Sections)
		return console.Section{}
	}
	maint := func() map[string]console.Property {
		out := map[string]console.Property{}
		for _, g := range section(detail(), "overview").Groups {
			if g.Heading == "Maintenance" {
				for _, pr := range g.Properties {
					out[pr.Label] = pr
				}
			}
		}
		return out
	}

	// Maintenance: Any window by default, Edit beside it.
	m := maint()
	if w := m["Maintenance window"]; w.Value != "Any window" || w.Action != "edit-maintenance" || w.ActionText != "Edit" {
		t.Errorf("Maintenance window = %+v", w)
	}
	if m["Upcoming maintenance"].Value != "None scheduled" {
		t.Errorf("Upcoming maintenance = %+v", m["Upcoming maintenance"])
	}
	var edit console.Action
	for _, a := range p.DetailActions(ctx, "p1", []string{name}) {
		if a.ID == "edit-maintenance" {
			edit = a
		}
	}
	if edit.Submit != "Update" || !edit.Linked || len(edit.Fields) != 2 || edit.Fields[0].Label != "Maintenance window" ||
		edit.Fields[0].Default != "ANY" || edit.Fields[0].OptionLabels["ANY"] != "Any window" || len(edit.Fields[1].Options) != 24 {
		t.Fatalf("Edit maintenance = %+v", edit)
	}
	if err := p.ActAt(ctx, "p1", []string{name}, "edit-maintenance", map[string]string{"day": "TUESDAY", "hour": "23"}); err != nil {
		t.Fatal(err)
	}
	c, _ := api.GetCluster(ctx, &alloydbpb.GetClusterRequest{Name: name})
	ws := c.GetMaintenanceUpdatePolicy().GetMaintenanceWindows()
	if len(ws) != 1 || ws[0].GetDay() != dayofweek.DayOfWeek_TUESDAY || ws[0].GetStartTime().GetHours() != 23 {
		t.Fatalf("the window reads back as %v", c.GetMaintenanceUpdatePolicy())
	}
	if v := maint()["Maintenance window"].Value; v != "Tuesday, 23:00 UTC" {
		t.Errorf("after Edit, Maintenance window = %q", v)
	}
	if err := p.ActAt(ctx, "p1", []string{name}, "edit-maintenance", map[string]string{"day": "ANY"}); err != nil {
		t.Fatal(err)
	}
	if v := maint()["Maintenance window"].Value; v != "Any window" {
		t.Errorf("cleared, Maintenance window = %q", v)
	}
	if l, _ := p.List(ctx, "p1"); l.Items[0].Fields["Maintenance"] != "None scheduled" {
		t.Errorf("the list's Maintenance = %q", l.Items[0].Fields["Maintenance"])
	}

	// System insights and Query insights, with no Cloud Monitoring here.
	d := detail()
	si := section(d, "system-insights")
	if si.Label != "System insights" || si.Unavailable == "" || !strings.Contains(si.Unavailable, "Cloud Monitoring is not served") {
		t.Errorf("System insights without Monitoring = %+v", si)
	}
	qi := section(d, "query-insights")
	if qi.Label != "Query insights" {
		t.Errorf("Query insights = %+v", qi)
	}
	ov := section(d, "overview")
	if len(ov.Series) != len(alloyDBOverviewCharts) || ov.Series[0].Label != "Max CPU utilization" || ov.Series[0].Unavailable == "" {
		t.Errorf("the Overview's charts = %+v", ov.Series)
	}

	// The API's bounds for the maintenance window.
	if _, err := api.UpdateCluster(ctx, &alloydbpb.UpdateClusterRequest{Cluster: &alloydbpb.Cluster{Name: name,
		MaintenanceUpdatePolicy: &alloydbpb.MaintenanceUpdatePolicy{MaintenanceWindows: []*alloydbpb.MaintenanceUpdatePolicy_MaintenanceWindow{
			{Day: dayofweek.DayOfWeek_MONDAY}, {Day: dayofweek.DayOfWeek_FRIDAY}}}}}); err == nil {
		t.Error("two maintenance windows were taken")
	}
}

// Query insights' Edit is Google's: Query lengths 256 to 4500, Query plan
// sampling with its Maximum sampling rate, Store client IP addresses and
// Store application tags, ending in Update instance.
func TestConsoleAlloyDBQueryInsightsEdit2251(t *testing.T) {
	in := &alloydbpb.Instance{Name: "projects/p/locations/l/clusters/c/instances/primary", InstanceType: alloydbpb.Instance_PRIMARY}
	a := alloyDBEditInsightsAction(in)
	if a.ID != "edit-query-insights-primary" || a.Label != "Edit" || a.Submit != "Update instance" {
		t.Fatalf("Edit = %+v", a)
	}
	labels := []string{}
	for _, f := range a.Fields[1:] {
		labels = append(labels, f.Label)
	}
	if got := strings.Join(labels, "|"); got != "Query lengths|Query plan sampling|Maximum sampling rate|Store client IP addresses|Store application tags" {
		t.Errorf("fields = %s", got)
	}
	if a.Fields[1].Default != "1024" || a.Fields[2].Default != "true" || a.Fields[3].Default != "5" {
		t.Errorf("defaults = %+v", a.Fields)
	}
}

// An instance row's Edit has Google's database flags under Advanced
// configuration options: Add flag, each flag chosen from the New database
// flag list, its value, and Delete.
func TestConsoleAlloyDBFlagsPicker2251(t *testing.T) {
	ctx := context.Background()
	api, err := alloydb.NewServer("", "", alloydb.Options{ConnectorHost: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	c := &alloydbpb.Cluster{Name: "projects/p1/locations/us-central1/clusters/c1", DatabaseVersion: alloydbpb.DatabaseVersion_POSTGRES_16}
	in := &alloydbpb.Instance{Name: c.GetName() + "/instances/primary", InstanceType: alloydbpb.Instance_PRIMARY, State: alloydbpb.Instance_READY,
		MachineConfig: &alloydbpb.Instance_MachineConfig{CpuCount: 4}, DatabaseFlags: map[string]string{"work_mem": "8192", "log_lock_waits": "on"}}
	a := alloyDBInstanceEdit(ctx, api, c, in)
	var flags console.Field
	for _, f := range a.Fields {
		if f.Name == "flags" {
			flags = f
		}
	}
	if a.Submit != "Update instance" || flags.Type != console.GroupFieldType || flags.AddLabel != "Add flag" || flags.RemoveLabel != "Delete" ||
		flags.Section != "Advanced configuration options" || len(flags.Group) != 2 || flags.Group[0].Label != "New database flag" {
		t.Fatalf("Flags = %+v", flags)
	}
	if flags.Default != `[{"flag":"log_lock_waits","value":"on"},{"flag":"work_mem","value":"8192"}]` {
		t.Errorf("the instance's flags as rows = %s", flags.Default)
	}
	opts := strings.Join(flags.Group[0].Options, ",")
	for _, want := range []string{"work_mem", "max_connections", "log_lock_waits"} {
		if !strings.Contains(","+opts+",", ","+want+",") {
			t.Errorf("the New database flag list has no %s: %s", want, opts)
		}
	}
	if a.Fields[0].Default != "4" {
		t.Errorf("Machine's default = %q", a.Fields[0].Default)
	}
}

// The events timeline names each operation as Google's does.
func TestAlloyDBEventNames2251(t *testing.T) {
	for _, tc := range []struct{ verb, target, name, typ string }{
		{"create", "projects/p/locations/l/clusters/c", "Cluster create", "CLUSTER_CREATE"},
		{"update", "projects/p/locations/l/clusters/c/instances/i", "Instance update", "INSTANCE_UPDATE"},
		{"restart", "projects/p/locations/l/clusters/c/instances/i", "Instance restart", "INSTANCE_RESTART"},
		{"failover", "projects/p/locations/l/clusters/c/instances/i", "Instance failover", "INSTANCE_FAILOVER"},
		{"promote", "projects/p/locations/l/clusters/c", "Cluster promote", "CLUSTER_PROMOTE"},
		{"create", "projects/p/locations/l/backups/b", "Backup create", "BACKUP_CREATE"},
		{"delete", "projects/p/locations/l/clusters/c", "", ""},
	} {
		if n, typ := alloyDBEventName(tc.verb, tc.target); n != tc.name || typ != tc.typ {
			t.Errorf("%s %s = %q %q, want %q %q", tc.verb, tc.target, n, typ, tc.name, tc.typ)
		}
	}
}

// The AlloyDB Monitoring source reads nothing without the API.
func TestMonitoringAlloyDBWithoutAPI2251(t *testing.T) {
	cs, ins := monitoringAlloyDB{srv: func() *alloydb.Server { return nil }}.AlloyDBSample()
	if cs != nil || ins != nil {
		t.Errorf("sample = %v %v", cs, ins)
	}
}

// The Backups page's rows take Restore and Edit labels addressed by path,
// as the console sends an action with fields.
var _ console.PathActor = alloyDBBackupsProvider{}
