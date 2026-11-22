package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/dataform"
)

// TestConsoleDataform (#2105): Create repository makes the repository the
// form describes; its page has Google's tabs; Create development workspace,
// Create file, the file's Edit (Save), the uncommitted changes, Commit and
// Push reach the repository's git; Create release configuration and
// Create workflow configuration keep their schedules; Settings and Delete
// change and remove the repository. Compile needs Dataform core's
// container, which the browser test reaches.
func TestConsoleDataform(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	ctx := context.Background()
	api, err := dataform.NewServer(dataform.Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	p := dataformProvider{svc: &dataformService{api: api}}

	title, fields := p.CreateForm()
	if title != "Create repository" || fields[0].Name != "repositoryId" || fields[1].Label != "Region" || fields[2].Label != "Service account" {
		t.Fatalf("Create repository form: %q %+v", title, fields)
	}
	repo, err := p.Create(ctx, "cp", map[string]string{"repositoryId": "quick", "region": "us-central1"})
	if err != nil || repo != "projects/cp/locations/us-central1/repositories/quick" {
		t.Fatalf("Create: %q, %v", repo, err)
	}
	listing, err := p.List(ctx, "cp")
	if err != nil || len(listing.Items) != 1 || listing.Items[0].Fields["Region"] != "us-central1" {
		t.Fatalf("List: %+v, %v", listing, err)
	}
	d, err := p.Detail(ctx, "cp", []string{repo})
	if err != nil || d.Unavailable != "" {
		t.Fatalf("repository page: %+v, %v", d, err)
	}
	var tabs []string
	for _, s := range d.Sections {
		tabs = append(tabs, s.Group+"/"+s.Label)
	}
	if got := strings.Join(tabs, ","); got != "/Development workspaces,Releases & Scheduling/Release configurations,"+
		"Releases & Scheduling/Workflow configurations,/Workflow Execution Logs,/Settings,/Git connection,/Code,/" {
		t.Errorf("repository tabs: %s", got)
	}

	if err := p.ActAt(ctx, "cp", []string{repo}, "createworkspace", map[string]string{"workspaceId": "dev"}); err != nil {
		t.Fatal(err)
	}
	ws := []string{repo, "workspaces", "dev"}
	if err := p.ActAt(ctx, "cp", ws, "createfile", map[string]string{"path": "definitions/first.sqlx",
		"contents": "config { type: \"table\" }\nselect 1 as a\n"}); err != nil {
		t.Fatal(err)
	}
	d, _ = p.Detail(ctx, "cp", ws)
	files := workflowsSection(d, "files")
	if files == nil || len(files.Listing.Items) != 2 || files.Listing.Items[0].Name != "definitions/" ||
		files.Listing.Items[1].Name != "definitions/first.sqlx" {
		t.Fatalf("Files: %+v", files)
	}
	if ch := workflowsSection(d, "changes"); ch == nil || len(ch.Listing.Items) != 1 || ch.Listing.Items[0].Status != "Added" {
		t.Errorf("Uncommitted changes: %+v", ch)
	}
	if g := workflowsSection(d, "graph"); g == nil || g.Listing.Prompt == "" {
		t.Errorf("an uncompiled workspace's graph: %+v", g)
	}
	file := append(append([]string{}, ws...), "files", "definitions/first.sqlx")
	if err := p.ActAt(ctx, "cp", file, "save", map[string]string{"contents": "config { type: \"view\" }\nselect 2 as a\n"}); err != nil {
		t.Fatal(err)
	}
	d, _ = p.Detail(ctx, "cp", file)
	if code := workflowsSection(d, "code"); code == nil || !strings.Contains(code.Text, "select 2") || code.Language != "sql" {
		t.Errorf("file page: %+v", d)
	}
	if err := p.ActAt(ctx, "cp", ws, "commit", map[string]string{"message": "first", "authorName": "A", "authorEmail": "a@example.com"}); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "cp", ws, "push", nil); err != nil {
		t.Fatal(err)
	}
	d, _ = p.Detail(ctx, "cp", ws)
	if ch := workflowsSection(d, "changes"); len(ch.Listing.Items) != 0 || !strings.Contains(ch.Listing.Note, "0 ahead") {
		t.Errorf("after commit and push: %+v", ch.Listing)
	}
	if err := p.ActAt(ctx, "cp", ws, "pull", nil); err != nil {
		t.Fatal(err)
	}

	if err := p.ActAt(ctx, "cp", []string{repo}, "createrelease", map[string]string{"releaseId": "prod", "gitCommitish": "main",
		"cronSchedule": "0 * * * *", "timeZone": "Europe/Paris", "vars": `{"env":"prod"}`}); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "cp", []string{repo}, "createworkflow", map[string]string{"workflowId": "hourly", "releaseConfig": "prod",
		"cronSchedule": "30 * * * *", "scope": "tags", "tags": "daily", "dependencies": "true"}); err != nil {
		t.Fatal(err)
	}
	d, _ = p.Detail(ctx, "cp", []string{repo})
	if r := workflowsSection(d, "releases"); len(r.Listing.Items) != 1 || r.Listing.Items[0].Fields["Time zone"] != "Europe/Paris" {
		t.Errorf("Release configurations: %+v", r.Listing)
	}
	if w := workflowsSection(d, "workflows"); len(w.Listing.Items) != 1 || w.Listing.Items[0].Fields["Frequency"] != "30 * * * *" {
		t.Errorf("Workflow configurations: %+v", w.Listing)
	}
	d, _ = p.Detail(ctx, "cp", []string{repo, "workflows", "hourly"})
	if d.Unavailable != "" || !strings.Contains(dataformProperty(d, "Actions"), "daily") {
		t.Errorf("workflow configuration page: %+v", d)
	}
	if err := p.ActAt(ctx, "cp", []string{repo, "releases", "prod"}, "editrelease", map[string]string{"gitCommitish": "main", "cronSchedule": "",
		"disabled": "true"}); err != nil {
		t.Fatal(err)
	}
	d, _ = p.Detail(ctx, "cp", []string{repo, "releases", "prod"})
	if d.Unavailable != "" || d.Summary[3].Value != "Disabled" {
		t.Errorf("edited release page: %+v", d.Summary)
	}

	if err := p.ActAt(ctx, "cp", []string{repo}, "settings", map[string]string{"defaultDatabase": "other", "schemaSuffix": "dev"}); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "cp", []string{repo}, "connectgit", map[string]string{"url": "https://example.com/x.git", "branch": "main",
		"secret": "projects/cp/secrets/t/versions/1"}); err == nil {
		t.Error("Connect with Git took a remote outside the instance")
	}
	if err := p.ActAt(ctx, "cp", file, "deletefile", nil); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "cp", ws, "deleteworkspace", nil); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "cp", []string{repo}, "delete", nil); err != nil {
		t.Fatal(err)
	}
	if listing, _ := p.List(ctx, "cp"); len(listing.Items) != 0 {
		t.Errorf("after Delete: %+v", listing.Items)
	}
}

// dataformProperty is the value of the first property labelled label on
// any of the page's property tabs.
func dataformProperty(d console.Detail, label string) string {
	for _, s := range d.Sections {
		for _, g := range s.Groups {
			for _, p := range g.Properties {
				if p.Label == label {
					return p.Value
				}
			}
		}
	}
	return ""
}
