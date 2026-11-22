package main

import (
	"context"
	"strings"
	"testing"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/genproto/googleapis/api/monitoredres"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/monitoring"
)

// The rest of #2146: the Alerting Summary pane; Create group's Name, Tag
// and Region criteria, matched against the products' resources; Log check
// failures on Create uptime check; Create snooze's Alert policies menu.
func TestConsoleMonitoringGoogleRound2(t *testing.T) {
	ctx := context.Background()
	admin, _, srv := monitoringAdminFixture(t)
	const project = "mon-round2"

	// Groups: the criteria write resource.metadata.*, and the members are
	// the products' resources with their name, region and tags.
	srv.AddResourceInventory(func(context.Context, string) ([]monitoring.InventoryResource, error) {
		return []monitoring.InventoryResource{
			{Resource: &monitoredres.MonitoredResource{Type: "gce_instance", Labels: map[string]string{"project_id": project, "instance_id": "1", "zone": "us-central1-a"}},
				Metadata: monitoring.ResourceMetadata{Name: "web-1", Region: "us-central1", UserLabels: map[string]string{"env": "prod"}, NetworkTags: []string{"http-server"}}},
			{Resource: &monitoredres.MonitoredResource{Type: "gce_instance", Labels: map[string]string{"project_id": project, "instance_id": "2", "zone": "europe-west2-b"}},
				Metadata: monitoring.ResourceMetadata{Name: "web-2", Region: "europe-west2"}},
		}, nil
	})
	groups := monitoringGroupsProvider{admin}
	_, fields := groups.CreateForm()
	var types []string
	for _, f := range fields {
		if f.Name == "criteria" {
			for _, o := range f.Group[0].Options {
				types = append(types, f.Group[0].OptionLabels[o])
			}
		}
	}
	if strings.Join(types, ",") != "Name,Tag,Region,Resource Type,Cloud Project" {
		t.Errorf("the criteria types = %v", types)
	}
	gid, err := groups.Create(ctx, project, map[string]string{"displayName": "Web", "combine": "AND",
		"criteria": `[{"type":"name","operator":"contains","value":"web"},{"type":"tag","operator":"contains","value":"http-server"},` +
			`{"type":"region","operator":"equals","value":"us-central1"}]`})
	if err != nil {
		t.Fatal(err)
	}
	g, _ := srv.GetGroup(ctx, &monitoringpb.GetGroupRequest{Name: "projects/" + project + "/groups/" + gid})
	if want := `resource.metadata.name = has_substring("web") AND resource.metadata.tag."http-server" = has_substring("") AND resource.metadata.region = "us-central1"`; g.GetFilter() != want {
		t.Errorf("the group's filter = %q, want %q", g.GetFilter(), want)
	}
	d, err := groups.Detail(ctx, project, []string{gid})
	if err != nil {
		t.Fatal(err)
	}
	m := d.Sections[0].Listing
	if len(m.Items) != 1 || m.Items[0].Name != "web-1" || m.Items[0].Fields["Region"] != "us-central1" ||
		m.Items[0].Fields["Tags"] != "env:prod, http-server" || m.Items[0].Fields["Resource type"] != "gce_instance" {
		t.Errorf("the group's members = %+v", m.Items)
	}
	tid, err := groups.Create(ctx, project, map[string]string{"displayName": "Prod",
		"criteria": `[{"type":"tag","operator":"contains","value":"env=prod"}]`})
	if err != nil {
		t.Fatal(err)
	}
	if g, _ := srv.GetGroup(ctx, &monitoringpb.GetGroupRequest{Name: "projects/" + project + "/groups/" + tid}); g.GetFilter() != `resource.metadata.tag."env" = has_substring("prod")` {
		t.Errorf("a key=value tag's filter = %q", g.GetFilter())
	}

	// Uptime: Log check failures, on by default, is the check's
	// logCheckFailures.
	uptime := monitoringUptimeProvider{admin}
	_, ufields := uptime.CreateForm()
	logField := false
	for _, f := range ufields {
		if f.Name == "logCheckFailures" && f.Label == "Log check failures" && f.Default == "true" && f.Section == upStepResponse {
			logField = true
		}
	}
	if !logField {
		t.Error("Create uptime check has no Log check failures, on by default, in Response validation")
	}
	uid, err := uptime.Create(ctx, project, map[string]string{"displayName": "Logged", "protocol": "TCP", "host": "127.0.0.1", "port": "1",
		"period": "60", "timeout": "5", "logCheckFailures": "true"})
	if err != nil {
		t.Fatal(err)
	}
	uname := "projects/" + project + "/uptimeCheckConfigs/" + uid
	uc, _ := srv.GetUptimeCheckConfig(ctx, &monitoringpb.GetUptimeCheckConfigRequest{Name: uname})
	if !monitoring.LogCheckFailures(uc) {
		t.Errorf("the check does not log its failures: %v", uc)
	}
	ud, err := uptime.Detail(ctx, project, []string{uid})
	if err != nil {
		t.Fatal(err)
	}
	if v := propValue(ud.Summary, "Log check failures"); v != "Enabled" {
		t.Errorf("the check's page says Log check failures %q", v)
	}
	if err := uptime.Edit(ctx, project, []string{uid}, map[string]string{"displayName": "Logged", "port": "1", "period": "60", "timeout": "5",
		"logCheckFailures": "false"}); err != nil {
		t.Fatal(err)
	}
	uc, _ = srv.GetUptimeCheckConfig(ctx, &monitoringpb.GetUptimeCheckConfigRequest{Name: uname})
	if monitoring.LogCheckFailures(uc) {
		t.Error("Edit did not clear Log check failures")
	}

	// Snoozes: the Alert policies menu lists the project's policies by name.
	policies := monitoringAlertingProvider{admin}
	pid, err := policies.Create(ctx, project, map[string]string{"displayName": "Queue depth", "conditionType": "threshold",
		"metric": "custom.googleapis.com/q", "resource": "global", "comparison": "COMPARISON_GT", "threshold": "3", "duration": "0"})
	if err != nil {
		t.Fatal(err)
	}
	snoozes := monitoringSnoozesProvider{admin}
	var picker *console.Field
	for _, f := range snoozes.CreateFormFor(ctx, project) {
		if f.Name == "policies" {
			f := f
			picker = &f
		}
	}
	if picker == nil || picker.Type != console.MultiSelectFieldType || len(picker.Options) != 1 || picker.Options[0] != pid ||
		picker.OptionLabels[pid] != "Queue depth" {
		t.Fatalf("Create snooze's Alert policies = %+v", picker)
	}
	sid, err := snoozes.Create(ctx, project, map[string]string{"displayName": "Quiet", "policies": pid + "\n", "minutes": "30"})
	if err != nil {
		t.Fatal(err)
	}
	sn, err := srv.GetSnooze(ctx, &monitoringpb.GetSnoozeRequest{Name: "projects/" + project + "/snoozes/" + sid})
	if err != nil || len(sn.GetCriteria().GetPolicies()) != 1 || sn.GetCriteria().GetPolicies()[0] != "projects/"+project+"/alertPolicies/"+pid {
		t.Fatalf("the snooze = %v, %v", sn, err)
	}

	// The Summary pane: the number of open alerts, and See all alerts.
	l, err := policies.List(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Summary) != 1 || l.Summary[0].Heading != "Summary" || summaryValue(l.Summary, "Open alerts") != "0" ||
		summaryValue(l.Summary, "Alerts") != "See all alerts" || !strings.HasPrefix(l.Summary[0].Properties[0].Link, "/monitoring/alerting/incidents?project=") {
		t.Errorf("the Summary pane = %+v", l.Summary)
	}
	il, err := (monitoringIncidentsProvider{admin}).List(ctx, project)
	if err != nil || len(il.Summary) != 1 {
		t.Errorf("the Alerts tab's Summary pane = %+v, %v", il.Summary, err)
	}
}

func summaryValue(groups []console.PropertyGroup, label string) string {
	for _, g := range groups {
		for _, p := range g.Properties {
			if p.Label == label {
				return p.Value
			}
		}
	}
	return ""
}

// The Compute Engine inventory (#2146): a VM is a gce_instance member with
// its name, region, labels and network tags.
func TestMonitoringComputeInventory(t *testing.T) {
	ctx := context.Background()
	s := computeVMConsoleFixture(t)
	const project = "p"
	vms := computeInstancesProvider{s}
	if _, err := vms.Create(ctx, project, map[string]string{"name": "web-1", "zone": "us-central1-a", "machineType": "e2-small",
		"image": "debian-cloud/debian-12", "bootDiskSize": "10", "network": "default", "labels": `{"env":"dev"}`,
		"externalIP": "Ephemeral", "networkTags": "http-server"}); err != nil {
		t.Fatal(err)
	}
	settle(t, s, project)
	rs, err := monitoringInventoryWiring{compute: s.svc}.computeInstances(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 {
		t.Fatalf("the inventory = %+v", rs)
	}
	r := rs[0]
	if r.Resource.GetType() != "gce_instance" || r.Resource.GetLabels()["zone"] != "us-central1-a" || r.Resource.GetLabels()["instance_id"] == "" ||
		r.Metadata.Name != "web-1" || r.Metadata.Region != "us-central1" || r.Metadata.UserLabels["env"] != "dev" ||
		r.Metadata.SystemLabels["machine_type"] != "e2-small" {
		t.Errorf("the VM's resource = %+v", r)
	}
	if strings.Join(r.Metadata.NetworkTags, ",") != "http-server" {
		t.Errorf("the VM's network tags = %v", r.Metadata.NetworkTags)
	}
}
