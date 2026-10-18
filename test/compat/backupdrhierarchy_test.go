//go:build compat

package compat

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	backupdr "google.golang.org/api/backupdr/v1"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestBackupDRFetchUnderFolders (#1536): with a folder made by Resource
// Manager's official v3 client, a subfolder in it, a project in each and a
// Cloud SQL instance in each project (the official sqladmin client), the
// official backupdr/v1 client's folders and organizations
// resourceBackupConfigs.fetch answer the configs of every project under
// them, through the subfolder: the folder both instances, the subfolder
// only its own, the organization both. Each config is named under its own
// project; the filter, orderBy and paging are the project-level fetch's;
// a folder that does not exist is 403, as Google answers it.
//
// covers: backupdr.folders.locations.resourceBackupConfigs.fetch, backupdr.organizations.locations.resourceBackupConfigs.fetch
func TestBackupDRFetchUnderFolders(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvBackupDR) == "" {
		t.Skip("needs --services backupdr")
	}
	version := "POSTGRES_17"
	switch {
	case os.Getenv(EnvCloudSQL) != "":
	case os.Getenv(EnvMySQL) != "":
		version = "MYSQL_8_4"
	default:
		t.Skip("needs --services cloudsql or cloudsql-mysql")
	}
	bdr := backupDRService(t, h)
	svc := sqlAdminService(t, h)
	// Resource Manager as the sql shard's SQL_RESOURCEMANAGER (#1568), so
	// the Resource Manager suite stays in storage.
	shardEndpoint(h, envSQLResourceManager, EnvResourceManager)
	fc, _, pc := foldersClients(t, h)
	ctx := h.Context()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)

	folder := func(parent, label string) string {
		t.Helper()
		op, err := fc.CreateFolder(ctx, &rmpb.CreateFolderRequest{Folder: &rmpb.Folder{Parent: parent, DisplayName: folderLabel(h, label)}})
		if err != nil {
			t.Fatalf("CreateFolder in %s: %v", parent, err)
		}
		f, err := op.Wait(ctx)
		if err != nil {
			t.Fatalf("CreateFolder in %s: %v", parent, err)
		}
		t.Cleanup(func() { _, _ = fc.DeleteFolder(context.Background(), &rmpb.DeleteFolderRequest{Name: f.GetName()}) })
		return f.GetName()
	}
	project := func(parent, prefix string) string {
		t.Helper()
		id := v1ID(h, prefix)
		op, err := pc.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: id, Parent: parent}})
		if err != nil {
			t.Fatalf("CreateProject %s in %s: %v", id, parent, err)
		}
		if _, err := op.Wait(ctx); err != nil {
			t.Fatalf("CreateProject %s in %s: %v", id, parent, err)
		}
		t.Cleanup(func() {
			if op, err := pc.DeleteProject(context.Background(), &rmpb.DeleteProjectRequest{Name: "projects/" + id}); err == nil {
				_, _ = op.Wait(context.Background())
			}
		})
		return id
	}
	// Cleanups run last first: the instances go, then the projects, then
	// the subfolder, then the folder.
	top := folder(localOrg, "bdrh-")
	sub := folder(top, "bdrh-sub-")
	inTop, inSub := project(top, "bdrh-a-"), project(sub, "bdrh-b-")
	instTop, instSub := "bdrh-a-"+suffix, "bdrh-b-"+suffix
	for _, c := range []struct{ project, name string }{{inTop, instTop}, {inSub, instSub}} {
		createSQLInstance(t, svc, c.project, &sqladmin.DatabaseInstance{Name: c.name, DatabaseVersion: version,
			Region: "us-central1", RootPassword: "Root-pass1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}})
	}

	fetchAt := func(parent string) *backupdr.FoldersLocationsResourceBackupConfigsFetchCall {
		return bdr.Folders.Locations.ResourceBackupConfigs.Fetch(parent + "/locations/us-central1")
	}
	orgFetch := func() *backupdr.OrganizationsLocationsResourceBackupConfigsFetchCall {
		return bdr.Organizations.Locations.ResourceBackupConfigs.Fetch(localOrg + "/locations/us-central1")
	}
	ours := fmt.Sprintf("target_resource_display_name=%q OR target_resource_display_name=%q", instTop, instSub)
	// got is each config's project/instance, in the order answered.
	got := func(cs []*backupdr.ResourceBackupConfig) []string {
		var out []string
		for _, c := range cs {
			p := strings.TrimPrefix(c.Name, "projects/")
			p = p[:strings.Index(p, "/")]
			out = append(out, p+"/"+c.TargetResourceDisplayName)
			if c.TargetResourceType != "CLOUD_SQL_INSTANCE" || c.TargetResource != "//sqladmin.googleapis.com/projects/"+p+"/instances/"+c.TargetResourceDisplayName {
				t.Errorf("config %+v", c)
			}
		}
		return out
	}
	want := func(what string, cs []*backupdr.ResourceBackupConfig, err error, w ...string) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if g := got(cs); strings.Join(g, " ") != strings.Join(w, " ") {
			t.Errorf("%s: %v, want %v", what, g, w)
		}
	}
	a, b := inTop+"/"+instTop, inSub+"/"+instSub

	t.Run("folder", func(t *testing.T) {
		r, err := fetchAt(top).Filter(ours).Do()
		want("the folder", r.ResourceBackupConfigs, err, a, b)
		r, err = fetchAt(top).Filter(ours).OrderBy("target_resource_display_name desc").Do()
		want("the folder, descending", r.ResourceBackupConfigs, err, b, a)
		// Every config of the folder: its projects' own instances and the
		// instance up runs, which every project has, once per project.
		all, err := fetchAt(top).Do()
		if err != nil {
			t.Fatal(err)
		}
		projects := map[string]bool{}
		for _, s := range got(all.ResourceBackupConfigs) {
			projects[s[:strings.Index(s, "/")]] = true
		}
		if len(projects) != 2 || !projects[inTop] || !projects[inSub] {
			t.Errorf("the folder's configs are of %v, want %s and %s", projects, inTop, inSub)
		}
		// Paged one at a time, the same configs in the same order.
		var paged []*backupdr.ResourceBackupConfig
		tok := ""
		for i := 0; ; i++ {
			c := fetchAt(top).PageSize(1)
			if tok != "" {
				c = c.PageToken(tok)
			}
			page, err := c.Do()
			if err != nil || len(page.ResourceBackupConfigs) > 1 || i > len(all.ResourceBackupConfigs) {
				t.Fatalf("page %d: %v %+v", i, err, page)
			}
			paged = append(paged, page.ResourceBackupConfigs...)
			if tok = page.NextPageToken; tok == "" {
				break
			}
		}
		if g, w := got(paged), got(all.ResourceBackupConfigs); strings.Join(g, " ") != strings.Join(w, " ") {
			t.Errorf("paged %v, want %v", g, w)
		}
		_, err = fetchAt(top).Filter(`create_time="2026"`).Do()
		wantBackupDRCode(t, "a folder's fetch with an unknown key", err, http.StatusBadRequest, "unknown key")
	})

	t.Run("subfolder", func(t *testing.T) {
		r, err := fetchAt(sub).Filter(ours).Do()
		want("the subfolder", r.ResourceBackupConfigs, err, b)
	})

	t.Run("organization", func(t *testing.T) {
		r, err := orgFetch().Filter(ours).Do()
		want("the organization", r.ResourceBackupConfigs, err, a, b)
		r, err = orgFetch().Filter(fmt.Sprintf("target_resource_display_name=%q", instSub)).Do()
		want("the organization by name", r.ResourceBackupConfigs, err, b)
		all, err := orgFetch().PageSize(500).Do()
		if err != nil {
			t.Fatal(err)
		}
		names := []string{}
		for _, c := range all.ResourceBackupConfigs {
			names = append(names, c.TargetResourceDisplayName)
		}
		if !sort.StringsAreSorted(names) {
			t.Errorf("the organization's configs are not in display name order: %v", names)
		}
	})

	t.Run("refusals", func(t *testing.T) {
		_, err := fetchAt("folders/999999999999").Do()
		wantBackupDRCode(t, "a folder that does not exist", err, http.StatusForbidden, "folders/999999999999")
		_, err = bdr.Organizations.Locations.ResourceBackupConfigs.Fetch("organizations/42/locations/us-central1").Do()
		wantBackupDRCode(t, "another organization", err, http.StatusForbidden, "organizations/42")
	})
}
