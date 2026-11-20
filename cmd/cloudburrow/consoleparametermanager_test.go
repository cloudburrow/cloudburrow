package main

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/service/parametermanager"
	"github.com/cloudburrow/cloudburrow/internal/service/secrets"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestConsoleParameterManager (#2054): Create parameter makes the
// parameter the form describes; New version adds a version whose page
// shows its payload and its payload rendered, a secret reference replaced
// by the secret version read through Secret Manager; Disable hides both;
// Delete refuses a parameter with versions, and deletes it once they are
// gone.
func TestConsoleParameterManager(t *testing.T) {
	ctx := context.Background()
	st := secrets.NewStore(store.NewMemory())
	if _, err := st.CreateSecret("cp", "db", nil, nil, "automatic"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddVersion("cp", "db", []byte("s3cr3t")); err != nil {
		t.Fatal(err)
	}
	svc := &parameterManagerService{secrets: &secretsService{store: st}}
	api, err := parametermanager.NewServer("", parametermanager.Options{AccessSecret: svc.accessSecret})
	if err != nil {
		t.Fatal(err)
	}
	svc.api = api
	p := parameterManagerProvider{svc: svc}

	title, fields := p.CreateForm()
	if title != "Create parameter" || len(fields) < 5 || fields[0].Name != "parameterId" || fields[2].Name != "format" {
		t.Fatalf("Create parameter form: %q %+v", title, fields)
	}
	name, err := p.Create(ctx, "cp", map[string]string{"parameterId": "app", "location": "global", "format": "JSON",
		"encryption": "google", "labels": `{"env":"dev"}`})
	if err != nil || name != "projects/cp/locations/global/parameters/app" {
		t.Fatalf("Create: %q, %v", name, err)
	}
	listing, err := p.List(ctx, "cp")
	if err != nil || len(listing.Items) != 1 || listing.Items[0].Fields["Format"] != "JSON" || listing.Items[0].Fields["Labels"] == "—" {
		t.Fatalf("List: %+v, %v", listing, err)
	}
	if err := p.ActAt(ctx, "cp", []string{name}, "newversion", map[string]string{"versionId": "v1", "payload": "{not json"}); err == nil {
		t.Fatal("a JSON parameter took a version that is not JSON")
	}
	payload := `{"password": "__REF__(\"//secretmanager.googleapis.com/projects/cp/secrets/db/versions/1\")"}`
	if err := p.ActAt(ctx, "cp", []string{name}, "newversion", map[string]string{"versionId": "v1", "payload": payload}); err != nil {
		t.Fatal(err)
	}
	version := "v1"
	d, err := p.Detail(ctx, "cp", []string{name, version})
	if err != nil || d.Unavailable != "" {
		t.Fatalf("version page: %+v, %v", d, err)
	}
	if got := workflowsSection(d, "payload").Text; got != payload {
		t.Errorf("Payload tab: %q", got)
	}
	if got := workflowsSection(d, "rendered").Text; got != `{"password": "s3cr3t"}` {
		t.Errorf("Rendered payload tab: %q", got)
	}
	if err := p.ActAt(ctx, "cp", []string{name, version}, "disable", nil); err != nil {
		t.Fatal(err)
	}
	d, _ = p.Detail(ctx, "cp", []string{name, version})
	if workflowsSection(d, "payload").Unavailable == "" || workflowsSection(d, "rendered").Unavailable == "" {
		t.Errorf("a disabled version's page shows its payload: %+v", d.Sections)
	}
	if acts := p.DetailActions(ctx, "cp", []string{name, version}); len(acts) == 0 || acts[0].ID != "enable" {
		t.Errorf("a disabled version's actions: %+v", acts)
	}
	if err := p.ActAt(ctx, "cp", []string{name}, "delete", nil); err == nil || !strings.Contains(err.Error(), "versions") {
		t.Fatalf("Delete of a parameter with a version: %v", err)
	}
	if err := p.ActAt(ctx, "cp", []string{name, version}, "deleteversion", nil); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "cp", []string{name}, "delete", nil); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "other", []string{name}, "delete", nil); err == nil {
		t.Error("another project's page acted on the parameter")
	}
}

// TestConsoleParameterTemplates (#2063): Create template makes the
// template the form describes; New version adds a version whose page shows
// its payload; Render, with a YAML parameter's version chosen from those
// the form offers, answers the payload and the rendered value; Disable
// makes Render unavailable; Delete refuses a template with versions, and
// deletes it once they are gone.
func TestConsoleParameterTemplates(t *testing.T) {
	ctx := context.Background()
	svc := &parameterManagerService{}
	api, err := parametermanager.NewServer("", parametermanager.Options{})
	if err != nil {
		t.Fatal(err)
	}
	svc.api = api
	params := parameterManagerProvider{svc: svc}
	p := parameterTemplatesProvider{svc: svc}
	param, err := params.Create(ctx, "cp", map[string]string{"parameterId": "vals", "location": "global", "format": "YAML", "encryption": "google"})
	if err != nil {
		t.Fatal(err)
	}
	if err := params.ActAt(ctx, "cp", []string{param}, "newversion", map[string]string{"versionId": "v1", "payload": "level: debug\n"}); err != nil {
		t.Fatal(err)
	}

	title, fields := p.CreateForm()
	if title != "Create template" || len(fields) < 4 || fields[0].Name != "templateId" || fields[2].Name != "format" {
		t.Fatalf("Create template form: %q %+v", title, fields)
	}
	name, err := p.Create(ctx, "cp", map[string]string{"templateId": "tpl", "location": "global", "format": "TEMPLATE_FORMAT_YAML", "labels": `{"team":"burrow"}`})
	if err != nil || name != "projects/cp/locations/global/templates/tpl" {
		t.Fatalf("Create: %q, %v", name, err)
	}
	listing, err := p.List(ctx, "cp")
	if err != nil || len(listing.Items) != 1 || listing.Items[0].Fields["Format"] != "YAML" {
		t.Fatalf("List: %+v, %v", listing, err)
	}
	if err := p.ActAt(ctx, "cp", []string{name}, "newversion", map[string]string{"versionId": "bad", "payload": "a: {{if .x}}1{{end}}\n"}); err == nil {
		t.Fatal("a template version with control flow was accepted")
	}
	payload := "log: {{.level}}\n"
	if err := p.ActAt(ctx, "cp", []string{name}, "newversion", map[string]string{"versionId": "tv1", "payload": payload}); err != nil {
		t.Fatal(err)
	}
	d, err := p.Detail(ctx, "cp", []string{name, "tv1"})
	if err != nil || d.Unavailable != "" || workflowsSection(d, "payload").Text != payload {
		t.Fatalf("version page: %+v, %v", d, err)
	}
	acts := p.DetailActions(ctx, "cp", []string{name, "tv1"})
	if len(acts) == 0 || acts[0].ID != "render" || len(acts[0].Fields) != 1 || len(acts[0].Fields[0].Options) != 1 || acts[0].Fields[0].Options[0] != param+"/versions/v1" {
		t.Fatalf("version actions: %+v", acts)
	}
	rows, err := p.ActAtResult(ctx, "cp", []string{name, "tv1"}, "render", map[string]string{"parameterVersion": param + "/versions/v1"})
	if err != nil || len(rows.Items) != 1 || rows.Items[0].Fields["Rendered value"] != "log: debug\n" || rows.Items[0].Fields["Payload"] != payload {
		t.Fatalf("Render: %+v, %v", rows, err)
	}
	if err := p.ActAt(ctx, "cp", []string{name, "tv1"}, "disable", nil); err != nil {
		t.Fatal(err)
	}
	if acts := p.DetailActions(ctx, "cp", []string{name, "tv1"}); acts[0].Unavailable == "" || acts[1].ID != "enable" {
		t.Errorf("a disabled version's actions: %+v", acts)
	}
	if err := p.ActAt(ctx, "cp", []string{name}, "delete", nil); err == nil || !strings.Contains(err.Error(), "versions") {
		t.Fatalf("Delete of a template with a version: %v", err)
	}
	if err := p.ActAt(ctx, "cp", []string{name, "tv1"}, "deleteversion", nil); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "cp", []string{name}, "delete", nil); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "other", []string{name}, "delete", nil); err == nil {
		t.Error("another project's page acted on the template")
	}
}
