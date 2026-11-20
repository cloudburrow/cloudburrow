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
