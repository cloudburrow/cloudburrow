package main

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// TestConsoleLogScopesGoogleLayout (#1863): Google's Log Scopes tab. The
// _Default scope is listed, marked Default, and can be neither edited nor
// deleted; Create log scope takes Add projects and Add log views rows and
// the Name log scope section; Edit scope patches the scope; Set as default
// moves the Default marker (the Observability API's _Default scope, #2020) and a deleted
// default leaves _Default the default again.
func TestConsoleLogScopesGoogleLayout(t *testing.T) {
	_, svc := loggingConsole(t)
	ctx := context.Background()
	const project = "proj-1"
	ls := loggingLogScopesProvider{loggingAdminScreen: loggingAdminScreen{svc: svc}}

	label, fields := ls.CreateForm()
	var sections []string
	for _, f := range fields {
		if len(sections) == 0 || sections[len(sections)-1] != f.Section {
			sections = append(sections, f.Section)
		}
	}
	if label != "Create log scope" || ls.CreateSubmit() != "Create log scope" ||
		strings.Join(sections, "|") != "Projects|Log views|Name log scope" ||
		fields[0].AddLabel != "Add projects" || fields[1].AddLabel != "Add log views" {
		t.Fatalf("form %q %v %+v", label, sections, fields[:2])
	}
	if ls.RowEditLabel() != "Edit scope" || ls.DeleteLabel() != "Delete scope" ||
		!slices.Equal(ls.RowMenuOrder(), []string{scopeSetDefault, console.RowMenuEdit, console.RowMenuDelete}) {
		t.Error("the row menu is not More's Set as default, Edit scope, Delete scope")
	}

	name, err := ls.Create(ctx, project, map[string]string{"id": "mine",
		"projects": `[{"project":"proj-2"}]`,
		"views":    `[{"view":"projects/proj-1/locations/global/buckets/_Default/views/_AllLogs"}]`, "description": "d"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ls.Create(ctx, project, map[string]string{"id": "_Default", "projects": `[{"project":"proj-2"}]`}); err == nil {
		t.Error("a scope named _Default was created")
	}
	if _, err := ls.Create(ctx, project, map[string]string{"id": "empty"}); err == nil {
		t.Error("a scope with no project or view was created")
	}

	l, err := ls.List(ctx, project)
	if err != nil || len(l.Items) != 2 {
		t.Fatalf("List = %+v, %v", l.Items, err)
	}
	def, mine := l.Items[0], l.Items[1]
	if def.Name != defaultLogScopeName(project) || def.Fields["Default"] != "Default" || mine.Fields["Default"] != "—" ||
		mine.Fields["Projects"] != "projects/proj-2" || !strings.HasSuffix(mine.Fields["Log views"], "/views/_AllLogs") {
		t.Fatalf("rows %+v", l.Items)
	}
	if a := ls.Actions(def); a[0].Unavailable == "" {
		t.Error("Set as default is offered on the default scope")
	}
	if err := ls.Delete(ctx, project, def.Name); err == nil {
		t.Error("_Default was deleted")
	}
	if err := ls.Edit(ctx, project, []string{def.Name}, map[string]string{"projects": `[{"project":"proj-3"}]`}); err == nil {
		t.Error("_Default was edited")
	}
	if d, err := ls.Detail(ctx, project, []string{def.Name}); err != nil || d.Edit != nil {
		t.Errorf("_Default's page: %+v, %v", d.Edit, err)
	}

	d, err := ls.Detail(ctx, project, []string{name})
	if err != nil || d.Edit == nil || d.Edit.Label != "Edit scope" {
		t.Fatalf("Detail = %+v, %v", d, err)
	}
	if err := ls.Edit(ctx, project, []string{name}, map[string]string{"projects": `[{"project":"proj-3"}]`, "description": "e"}); err != nil {
		t.Fatal(err)
	}
	if err := ls.Act(ctx, project, name, scopeSetDefault); err != nil {
		t.Fatal(err)
	}
	l, _ = ls.List(ctx, project)
	if l.Items[1].Fields["Default"] != "Default" || l.Items[0].Fields["Default"] != "—" ||
		l.Items[1].Fields["Projects"] != "projects/proj-3" || l.Items[1].Fields["Log views"] != "—" {
		t.Fatalf("after Edit scope and Set as default: %+v", l.Items)
	}
	if err := ls.Delete(ctx, project, name); err != nil {
		t.Fatal(err)
	}
	if ls.defaultScope(project) != defaultLogScopeName(project) {
		t.Error("a deleted default scope stayed the default")
	}
}

// TestConsoleSavedQueryLibrary (#1863): the Logs Explorer's Save query
// keeps summary fields and their truncation, the Query library reads the
// raw filter and summary fields from each row, and Edit patches them.
func TestConsoleSavedQueryLibrary(t *testing.T) {
	_, svc := loggingConsole(t)
	ctx := context.Background()
	const project = "proj-1"
	sq := loggingSavedQueriesProvider{loggingAdminScreen{svc: svc}}
	name, err := sq.Create(ctx, project, map[string]string{"displayName": "Slow", "location": "global", "kind": savedQueryLogging,
		"query": "severity>=WARNING", "visibility": "SHARED", "summaryFields": "jsonPayload.method, httpRequest.status", "summaryFieldStart": "12"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sq.Create(ctx, project, map[string]string{"displayName": "x", "location": "global", "kind": savedQueryLogging,
		"query": "a", "visibility": "PRIVATE", "summaryFieldEnd": "many"}); err == nil {
		t.Error("a truncation that is not a number was accepted")
	}
	l, err := sq.List(ctx, project)
	if err != nil || len(l.Items) != 1 {
		t.Fatalf("List = %+v, %v", l.Items, err)
	}
	f := l.Items[0].Fields
	if f["filter"] != "severity>=WARNING" || f["kind"] != "logging" || f["summaryFields"] != "jsonPayload.method,httpRequest.status" ||
		f["summaryFieldStart"] != "12" || f["visibility"] != "SHARED" {
		t.Fatalf("row fields %+v", f)
	}
	if err := sq.Edit(ctx, project, []string{name}, map[string]string{"displayName": "Slower", "kind": savedQueryLogging,
		"query": "severity>=ERROR", "visibility": "PRIVATE", "summaryFieldEnd": "5"}); err != nil {
		t.Fatal(err)
	}
	if err := sq.Edit(ctx, "other", []string{name}, map[string]string{"displayName": "y"}); err == nil {
		t.Error("edited another project's saved query")
	}
	l, _ = sq.List(ctx, project)
	f = l.Items[0].Fields
	if f["Display name"] != "Slower" || f["filter"] != "severity>=ERROR" || f["summaryFields"] != "" || f["summaryFieldEnd"] != "5" ||
		f["summaryFieldStart"] != "" || f["visibility"] != "PRIVATE" {
		t.Fatalf("after Edit %+v", f)
	}
}

// TestConsoleLoggingRowMenus (#1863): the Log Router's row menu is
// Google's order with Troubleshoot sink last, and a log-based metric's is
// Edit metric, View in Metrics Explorer, Delete metric, the explorer link
// unavailable without Cloud Monitoring.
func TestConsoleLoggingRowMenus(t *testing.T) {
	if got := (loggingSinksProvider{}).RowMenuOrder(); !slices.Equal(got,
		[]string{"view", console.RowMenuEdit, "disable", "enable", console.RowMenuDelete, sinkCheckTroubleshoot}) {
		t.Errorf("sink menu order %v", got)
	}
	acts := (loggingSinksProvider{}).Actions(console.Resource{Name: "s", Fields: map[string]string{"Enabled": "Enabled"}})
	if len(acts) != 3 || acts[2].Label != "Troubleshoot sink" {
		t.Errorf("sink actions %+v", acts)
	}
	m := loggingMetricsProvider{}
	if m.RowEditLabel() != "Edit metric" || m.DeleteLabel() != "Delete metric" {
		t.Error("metric labels")
	}
	if a := m.Actions(console.Resource{Name: "projects/p/metrics/errs"}); a[0].Unavailable == "" || a[0].Href != "" {
		t.Errorf("without Cloud Monitoring: %+v", a)
	}
	m.monitoring = true
	if a := m.Actions(console.Resource{Name: "projects/p/metrics/errs"}); a[0].Href != "/monitoring/metrics-explorer?metric=logging.googleapis.com%2Fuser%2Ferrs&project=p" {
		t.Errorf("with Cloud Monitoring: %+v", a)
	}
}
