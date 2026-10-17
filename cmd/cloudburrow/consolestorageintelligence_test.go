package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	gcsbuiltin "github.com/cloudburrow/cloudburrow/internal/service/storage"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestConsoleStorageIntelligence (#1519): the Storage Intelligence screen
// lists the project's, the organization's and a folder's configuration
// (GetProject/GetOrganization/GetFolderIntelligenceConfig over REST, as an
// application calls them), all INHERIT at first; Enable on the project makes
// it STANDARD and effective for itself; Edit filter on its page sets an
// excluded bucket regex; Start trial on the organization makes the folder's
// effective configuration the organization's; Inherit from parent on the
// project clears its filter; the configurations read back through the
// storage server's own API.
func TestConsoleStorageIntelligence(t *testing.T) {
	ctx := context.Background()
	gcs, err := gcsbuiltin.NewServer(gcsbuiltin.Options{})
	if err != nil {
		t.Fatal(err)
	}
	backend := httptest.NewServer(gcs)
	t.Cleanup(backend.Close)
	reg := resourcemanager.New(store.NewMemory())
	folder, err := reg.CreateFolder(resourcemanager.OrganizationName, "Data")
	if err != nil {
		t.Fatal(err)
	}
	p := storageIntelligenceProvider{endpoint: strings.TrimPrefix(backend.URL, "http://"), registry: reg}
	const project = "intel-proj"
	rows := func() map[string]console.Resource {
		t.Helper()
		l, err := p.List(ctx, project)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]console.Resource{}
		for _, r := range l.Items {
			out[r.Name] = r
		}
		return out
	}
	r := rows()
	if len(r) != 3 || r["projects/"+project].Status != "INHERIT" || r[resourcemanager.OrganizationName].Status != "INHERIT" || r[folder.Name].Fields["Effective edition"] != "NONE" {
		t.Fatalf("first listing = %+v", r)
	}
	if got := settingsActionIDs(p.Actions(r["projects/"+project])); got != "standard,trial,disable" {
		t.Errorf("an inheriting row offers %s", got)
	}
	if err := p.Act(ctx, project, "projects/"+project, "standard"); err != nil {
		t.Fatal(err)
	}
	path := []string{"projects/" + project}
	if got := settingsActionIDs(p.DetailActions(ctx, project, path)); !strings.Contains(got, "filter") || strings.Contains(got, "standard") {
		t.Errorf("a STANDARD page offers %s", got)
	}
	if err := p.ActAt(ctx, project, path, "filter", map[string]string{"excludeBuckets": "^tmp-.*", "includeLocations": "us-central1"}); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, project, path, "filter", map[string]string{"includeBuckets": "a", "excludeBuckets": "b"}); err == nil {
		t.Error("a filter both including and excluding buckets was sent")
	}
	r = rows()
	pr := r["projects/"+project]
	if pr.Status != "STANDARD" || pr.Fields["Effective edition"] != "STANDARD" || !strings.Contains(pr.Fields["Filter"], "^tmp-.*") || !strings.Contains(pr.Fields["Filter"], "us-central1") {
		t.Errorf("project after Enable and Edit filter = %+v", pr)
	}
	if err := p.Act(ctx, project, resourcemanager.OrganizationName, "trial"); err != nil {
		t.Fatal(err)
	}
	r = rows()
	if f := r[folder.Name]; f.Fields["Effective edition"] != "STANDARD" || f.Fields["Effective from"] != resourcemanager.OrganizationName {
		t.Errorf("folder under a trial organization = %+v", f)
	}
	if o := r[resourcemanager.OrganizationName]; o.Status != "TRIAL" || o.Fields["Trial ends"] == "" {
		t.Errorf("organization in trial = %+v", o)
	}
	if err := p.Act(ctx, project, "projects/"+project, "inherit"); err != nil {
		t.Fatal(err)
	}
	cfg, err := gcs.GetIntelligenceConfig(context.Background(), "projects/"+project+"/locations/global/intelligenceConfig", "projects")
	if err != nil || cfg.GetEditionConfig().String() != "INHERIT" || cfg.GetFilter() != nil {
		t.Errorf("the project's configuration after Inherit = %v, %v", cfg, err)
	}
	if err := p.Act(ctx, project, "folders/1", "standard"); err == nil {
		t.Error("an action on a row the screen does not list was sent")
	}
	d, _ := p.Detail(ctx, project, []string{folder.Name})
	if d.Unavailable != "" || len(d.Sections) != 1 {
		t.Errorf("the folder's page = %+v", d)
	}
	// Reset the organization for any other test of this package.
	_ = p.Act(ctx, project, resourcemanager.OrganizationName, "inherit")
}

// TestConsoleFolderAppManagement (#1519): a folder's page shows App
// management Disabled and offers Enable app management, which sets the
// app-management capability (folders.capabilities.patch); the page then
// shows Enabled and its Management project (#1532), and offers Disable,
// which clears both.
func TestConsoleFolderAppManagement(t *testing.T) {
	ctx := context.Background()
	reg := resourcemanager.New(store.NewMemory())
	h := hierarchyProvider{registry: reg}
	id, err := h.Create(ctx, "", map[string]string{"displayName": "Apps", "parent": resourcemanager.OrganizationName})
	if err != nil {
		t.Fatal(err)
	}
	prop := func(label string) string {
		t.Helper()
		d, _ := h.Detail(ctx, "", []string{id})
		for _, p := range d.Sections[0].Groups[0].Properties {
			if p.Label == label {
				return p.Value
			}
		}
		return ""
	}
	appProp := func() string { return prop("App management") }
	if got := appProp(); got != "Disabled" || !strings.Contains(settingsActionIDs(h.DetailActions(ctx, "", []string{id})), folderAppOn) {
		t.Fatalf("a new folder's App management = %q", got)
	}
	if err := h.ActAt(ctx, "", []string{id}, folderAppOn, nil); err != nil {
		t.Fatal(err)
	}
	if c, err := reg.FolderCapability(id, appManagement); err != nil || !c.Value || appProp() != "Enabled" {
		t.Errorf("after Enable = %+v, %v, %q", c, err, appProp())
	}
	// Its management project (#1532) is on the page.
	if got := prop("Management project"); got != resourcemanager.ManagementProjectID(id) {
		t.Errorf("Management project = %q; want %s", got, resourcemanager.ManagementProjectID(id))
	}
	if !strings.Contains(settingsActionIDs(h.DetailActions(ctx, "", []string{id})), folderAppOff) {
		t.Error("an enabled folder does not offer Disable app management")
	}
	if err := h.ActAt(ctx, "", []string{id}, folderAppOff, nil); err != nil {
		t.Fatal(err)
	}
	if c, _ := reg.FolderCapability(id, appManagement); c.Value || prop("Management project") != "" {
		t.Error("Disable app management left the capability set or its management project")
	}
}
