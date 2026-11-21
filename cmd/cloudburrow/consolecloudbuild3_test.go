package main

import (
	"context"
	"strings"
	"testing"
	"time"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// TestConsoleBuildRound3 (#1854): History's Reject rejects every checked
// build, and its column selector adds Trigger description and Artifacts;
// a build's page has Build dependencies; the Dashboard's Build history is
// a bar chart of each build, its cards pinnable and filtered by Filter
// triggers.
func TestConsoleBuildRound3(t *testing.T) {
	ctx := context.Background()
	proj := freshProject("console-r3")
	b := cloudBuildConsole(t, proj)
	p := buildTriggersProvider{b: b}
	if _, err := p.Create(ctx, "", map[string]string{"name": "gated", "region": "global", "image": "alpine:3.20",
		"command": "echo hi", "approval": "true", "description": "ships the web tier"}); err != nil {
		t.Fatalf("Create trigger: %v", err)
	}
	if _, err := p.Create(ctx, "", map[string]string{"name": "plain", "region": "global", "image": "alpine:3.20", "command": "echo hi"}); err != nil {
		t.Fatalf("Create trigger: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := p.Act(ctx, "", "gated", "run"); err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
	h, _ := b.List(ctx, "")
	var ids []string
	for _, it := range h.Items {
		ids = append(ids, it.Name)
		if it.Fields["Trigger description"] != "ships the web tier" || it.Fields["Artifacts"] != "—" {
			t.Errorf("History row %s = %v", it.Name, it.Fields)
		}
	}
	if strings.Join(h.OptionalColumns, ",") != "Trigger description,Artifacts" {
		t.Errorf("History's optional columns = %v", h.OptionalColumns)
	}
	for _, c := range h.OptionalColumns {
		if !strings.Contains(strings.Join(h.Columns, ","), c) {
			t.Errorf("optional column %s is not a column", c)
		}
	}
	if err := b.ActAt(ctx, "", nil, "reject-selected", map[string]string{"builds": strings.Join(ids, "\n"), "comment": "not now",
		"url": "https://example.test/review"}); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	for _, id := range ids {
		var st cbpb.Build_Status
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if st = b.build("", id).GetStatus(); st != cbpb.Build_PENDING {
				break
			}
		}
		a := b.build("", id).GetApproval()
		if a.GetState() != cbpb.BuildApproval_REJECTED || a.GetResult().GetComment() != "not now" || a.GetResult().GetUrl() != "https://example.test/review" {
			t.Errorf("build %s after Reject = %v, %v", id, st, a)
		}
	}
	if acts := b.ListSelectActions(ctx, ""); acts != nil {
		t.Errorf("Approve and Reject with nothing awaiting approval = %+v", acts)
	}

	// Build dependencies: the build's git dependencies, else a note.
	dep := buildDependencies(&cbpb.Build{Dependencies: []*cbpb.Dependency{{Dep: &cbpb.Dependency_Empty{Empty: true}},
		{Dep: &cbpb.Dependency_GitSource{GitSource: &cbpb.Dependency_GitSourceDependency{
			Repository: &cbpb.Dependency_GitSourceRepository{Repotype: &cbpb.Dependency_GitSourceRepository_Url{Url: "https://example.test/lib.git"}},
			Revision:   "v1.2.0", DestPath: "third_party/lib", Depth: 1}}}}})
	if len(dep.Items) != 1 || dep.Items[0].Name != "https://example.test/lib.git" || dep.Items[0].Fields["Revision"] != "v1.2.0" ||
		dep.Items[0].Fields["Destination path"] != "third_party/lib" || dep.NameColumn != "Repository URL" {
		t.Errorf("Build dependencies = %+v", dep)
	}

	// The Dashboard: a bar per build, pinnable cards, Filter triggers.
	d, _ := buildDashboardProvider{b: b}.List(ctx, "")
	if d.CardFilter != "Filter triggers" || len(d.Summary) != 2 {
		t.Fatalf("Dashboard = %+v", d)
	}
	var gated *console.PropertyGroup
	for i := range d.Summary {
		if d.Summary[i].Pin == "" || !strings.Contains(d.Summary[i].Filter, d.Summary[i].Properties[0].Value) {
			t.Errorf("card %q: pin %q, filter %q", d.Summary[i].Heading, d.Summary[i].Pin, d.Summary[i].Filter)
		}
		if strings.Contains(d.Summary[i].Filter, "web tier") {
			gated = &d.Summary[i]
		}
	}
	if gated == nil {
		t.Fatal("the gated trigger's card is not found by its description")
	}
	var chart *console.BarChart
	for _, pr := range gated.Properties {
		if pr.Label == "Build history" {
			chart = pr.Chart
		}
	}
	if chart == nil || len(chart.Bars) != 2 || !strings.HasPrefix(chart.Bars[0].Label, "Cancelled") ||
		!strings.HasPrefix(chart.Bars[0].Link, "/cloud-build/builds/") {
		t.Errorf("Build history chart = %+v", chart)
	}
}

// TestConsoleBuildSettingsServiceAccountDropDown (#1854): Settings'
// Service account drop-down lists the default build account first, and a
// choice not among them falls back to it.
func TestConsoleBuildSettingsServiceAccountDropDown(t *testing.T) {
	ctx := context.Background()
	b := cloudBuildConsole(t, freshProject("console-set"))
	p := buildSettingsProvider{b: b}
	def := p.defaultAccount(ctx, "")
	if def == "" {
		t.Fatal("no default build service account")
	}
	if got := p.account(console.WithScopeValue(ctx, settingsAccountKey, "nobody@example.test"), ""); got != def {
		t.Errorf("an unknown choice = %q; want the default %q", got, def)
	}
	if keys := p.FilterKeys(); len(keys) != 1 || keys[0] != settingsAccountKey {
		t.Errorf("FilterKeys = %v", keys)
	}
}

// TestRowDeleteIconCapability (#1854): a RowDeleteIconer's Delete is drawn
// as a trash icon, said in the capabilities the client reads.
func TestRowDeleteIconCapability(t *testing.T) {
	var _ console.RowDeleteIconer = buildWorkerPoolsProvider{}
	if _, ok := any(buildTriggersProvider{}).(console.RowDeleteIconer); ok {
		t.Error("Triggers' Delete is a menu item in Google's console, not a trash icon")
	}
}
