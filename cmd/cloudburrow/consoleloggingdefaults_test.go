package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"cloud.google.com/go/logging/apiv2/loggingpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

// TestConsoleDefaultSinkAndDefaultSettings (#2108): the Log Router's
// _Default sink is edited and disabled from its page and row (its Delete is
// greyed out), and Logging Settings edits an organization's default
// resource settings, defaultSinkConfig included, through the parent picker.
func TestConsoleDefaultSinkAndDefaultSettings(t *testing.T) {
	_, svc, _, _ := sinksConsole(t)
	ctx := context.Background()
	const project = "proj-1"
	name := "projects/" + project + "/sinks/_Default"
	sinks := loggingSinksProvider{loggingAdminScreen{svc: svc}}
	d, err := sinks.Detail(ctx, project, []string{name})
	if err != nil || d.Edit == nil {
		t.Fatalf("the _Default sink's page = %+v, %v; want an Edit sink form", d, err)
	}
	values := map[string]string{}
	for _, f := range d.Edit.Fields {
		values[f.Name] = f.Default
	}
	values["filter"] = `severity >= ERROR`
	if err := sinks.Edit(ctx, project, []string{name}, values); err != nil {
		t.Fatalf("Edit sink of _Default: %v", err)
	}
	if err := sinks.Act(ctx, project, name, "disable"); err != nil {
		t.Fatalf("Disable sink of _Default: %v", err)
	}
	sk, err := svc.admin.ConfigServer().GetSink(ctx, &loggingpb.GetSinkRequest{SinkName: name})
	if err != nil || sk.GetFilter() != `severity >= ERROR` || !sk.GetDisabled() ||
		sk.GetDestination() != "logging.googleapis.com/projects/proj-1/locations/global/buckets/_Default" {
		t.Errorf("the API reads %v, %v", sk, err)
	}

	settings := loggingSettingsProvider{loggingAdminScreen{svc: svc}}
	org := resourcemanager.OrganizationName
	octx := console.WithScopeValue(ctx, bucketScopeKey, org)
	l, err := settings.List(octx, project)
	if err != nil || len(l.Items) != 1 || l.Items[0].Name != org+"/settings" || l.Items[0].Fields["Default sink filter"] != "Built-in" {
		t.Fatalf("the organization's settings listing = %+v, %v", l.Items, err)
	}
	if err := settings.Edit(octx, project, []string{org + "/settings"}, map[string]string{"storageLocation": "europe-west1",
		"disableDefaultSink": "false", "sinkMode": "OVERWRITE", "sinkFilter": ""}); err == nil || !strings.Contains(err.Error(), "OVERWRITE") {
		t.Errorf("OVERWRITE without a filter = %v; want the API's refusal", err)
	}
	if err := settings.Edit(octx, project, []string{org + "/settings"}, map[string]string{"storageLocation": "europe-west1",
		"disableDefaultSink": "true", "sinkMode": "APPEND", "sinkFilter": "severity >= WARNING", "sinkExclusions": "no-debug: textPayload = \"debug\""}); err != nil {
		t.Fatalf("Edit settings of the organization: %v", err)
	}
	sc, err := svc.admin.SinkConfigOf(org + "/settings")
	if err != nil || sc == nil || sc.Mode != "APPEND" || len(sc.Exclusions) != 1 || sc.Exclusions[0].Name != "no-debug" {
		t.Fatalf("SinkConfigOf = %+v, %v", sc, err)
	}
	od, err := settings.Detail(octx, project, []string{org + "/settings"})
	page := fmt.Sprint(od.Summary, od.Sections)
	for _, want := range []string{"europe-west1", "Disabled", "APPEND", "severity >= WARNING", "no-debug"} {
		if err != nil || !strings.Contains(page, want) {
			t.Errorf("the organization's settings page lacks %q: %s (%v)", want, page, err)
		}
	}
	if err := settings.Edit(octx, project, []string{"projects/proj-1/settings"}, map[string]string{}); err == nil {
		t.Error("the project's settings were edited from the organization's scope")
	}

	// A project made now: Logs Storage lists its buckets in europe-west1,
	// and the Log Router its _Default sink disabled with the filter.
	svc.hierarchy.OnCreated(svc.admin.Born)
	if _, err := svc.hierarchy.Create(resourcemanager.Project{ProjectID: "born-here", Parent: org}); err != nil {
		t.Fatal(err)
	}
	buckets := loggingBucketsProvider{loggingAdminScreen{svc: svc}}
	bl, err := buckets.List(ctx, "born-here")
	var names []string
	for _, it := range bl.Items {
		names = append(names, it.Name)
	}
	if err != nil || !strings.Contains(strings.Join(names, " "), "projects/born-here/locations/europe-west1/buckets/_Default") ||
		!strings.Contains(strings.Join(names, " "), "projects/born-here/locations/europe-west1/buckets/_Required") {
		t.Errorf("born-here's buckets = %v, %v", names, err)
	}
	sl, err := sinks.List(ctx, "born-here")
	if err != nil || len(sl.Items) != 2 || sl.Items[0].Fields["Enabled"] != "Disabled" || !strings.Contains(sl.Items[0].Fields["Filter"], "severity >= WARNING") ||
		sl.Items[0].Fields["Destination"] != "logging.googleapis.com/projects/born-here/locations/europe-west1/buckets/_Default" {
		t.Errorf("born-here's sinks = %+v, %v", sl.Items, err)
	}
}
