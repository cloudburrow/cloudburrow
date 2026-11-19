package main

import (
	"context"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// TestConsoleSourceManagerInstancesAndRepositories (#1527): Create
// instance lists it; its page lists its repositories and Create repository
// makes one; the repository's page shows its clone URL and how to push;
// a push of .cloudbuild/triggers.yaml shows its triggers and the build the
// push ran; Edit description changes it; the Permissions tabs store a
// policy on the instance and the repository; Delete repository and Delete
// instance remove them.
func TestConsoleSourceManagerInstancesAndRepositories(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	if err := runadapter.UseSourceManagerRoot(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s := runadapter.NewServer(&runadapter.Knative{}, "t", time.Minute)
	cb := s.Builds(stubSource{}, blockingBuilder{}).CloudBuild()
	var srv *httptest.Server
	ssm := s.SecureSourceManager(func() string { return srv.URL })
	srv = httptest.NewServer(runadapter.SourceManagerHandlerOr(ssm, cb, func(g grpc.ServiceRegistrar) { ssm.Register(g) }, nil))
	t.Cleanup(srv.Close)
	g := grpc.NewServer()
	cb.Register(g)
	ssm.Register(g)
	s.DeveloperConnect().Register(g)
	runadapter.NewOperationsServer(s).Register(g)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(g.Stop)
	p := sourceManagerProvider{b: runBuildsProvider{defaultProject: "console-ssm", runAddr: func() string { return ln.Addr().String() }}}

	if _, err := p.Create(ctx, "", map[string]string{"name": "inst", "region": "us-central1"}); err != nil {
		t.Fatal(err)
	}
	l, err := p.List(ctx, "")
	if err != nil || l.Unavailable != "" || len(l.Items) != 1 || l.Items[0].Name != "inst" || l.Items[0].Status != "Active" || !l.RowsOpenable {
		t.Fatalf("the Instances list = %+v, %v", l, err)
	}
	if _, f := p.CreateForm(); len(f) != 6 || f[1].Type != "select" || f[3].Unavailable == "" || f[5].ShowWhen != "encryption=cmek" {
		t.Errorf("Create instance's fields = %+v", f)
	}
	if _, err := p.Create(ctx, "", map[string]string{"name": "x", "region": "us-central1", "private": "true"}); err == nil {
		t.Error("Create instance made a private instance")
	}
	acts := p.DetailActions(ctx, "", []string{"inst"})
	if len(acts) != 2 || acts[0].ID != "create-repo" || acts[1].ID != "create-connection" {
		t.Fatalf("the instance's actions = %+v", acts)
	}
	if err := p.ActAt(ctx, "", []string{"inst"}, "create-repo", map[string]string{"id": "app", "description": "first", "branch": "main"}); err != nil {
		t.Fatal(err)
	}
	d, err := p.Detail(ctx, "", []string{"inst"})
	if err != nil || d.Unavailable != "" || len(d.Sections) != 3 || len(d.Sections[0].Listing.Items) != 1 ||
		d.Sections[0].Listing.Items[0].Name != "app" || !d.Sections[0].Listing.RowsOpenable {
		t.Fatalf("the instance's page = %+v, %v", d, err)
	}

	repoPage := func() (map[string]string, map[string]console.Section) {
		t.Helper()
		d, err := p.Detail(ctx, "", []string{"inst", "app"})
		if err != nil || d.Unavailable != "" {
			t.Fatalf("the repository's page = %+v, %v", d, err)
		}
		props, secs := map[string]string{}, map[string]console.Section{}
		for _, pr := range d.Summary {
			props[pr.Label] = pr.Value
		}
		for _, s := range d.Sections {
			secs[s.ID] = s
		}
		return props, secs
	}
	props, secs := repoPage()
	clone := props["Clone URL"]
	if clone != srv.URL+"/sourcemanager/git/console-ssm/us-central1/app.git" || props["Description"] != "first" || props["Default branch"] != "main" {
		t.Fatalf("the repository's page = %v", props)
	}
	if !strings.Contains(secs["code"].Text, "git clone "+clone) || !strings.Contains(secs["code"].Text, "git push google main") {
		t.Errorf("an empty repository's Code tab = %q", secs["code"].Text)
	}
	if secs["pulls"].Note == "" || len(secs["pulls"].Actions) != 0 {
		t.Errorf("an empty repository's Pull requests tab = %+v", secs["pulls"])
	}
	if len(secs["triggers"].Listing.Items) != 0 || len(secs["builds"].Listing.Items) != 0 {
		t.Errorf("a new repository's triggers and builds = %+v, %+v", secs["triggers"].Listing.Items, secs["builds"].Listing.Items)
	}

	work := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", work, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("clone", "--quiet", clone, ".")
	for name, body := range map[string]string{
		".cloudbuild/triggers.yaml": "triggers:\n- name: on-push\n  includedGitRefs: ^main$\n" +
			"  serviceAccount: projects/console-ssm/serviceAccounts/b@console-ssm.iam.gserviceaccount.com\n- name: off\n  disabled: true\n" +
			"  serviceAccount: projects/console-ssm/serviceAccounts/b@console-ssm.iam.gserviceaccount.com\n",
		".cloudbuild/cloudbuild.yaml": "steps:\n- name: gcr.io/cloud-builders/docker\n  args: [build, -t, 'x/${REPO_NAME}:${SHORT_SHA}', .]\n",
		"Dockerfile":                  "FROM scratch\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(work, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("checkout", "--quiet", "-b", "main")
	git("add", ".")
	git("commit", "--quiet", "-m", "first")
	git("push", "--quiet", "origin", "main")

	_, secs = repoPage()
	trig := map[string]console.Resource{}
	for _, r := range secs["triggers"].Listing.Items {
		trig[r.Name] = r
	}
	if trig["on-push"].Status != "Enabled" || trig["on-push"].Fields["Branches"] != "^main$" || trig["on-push"].Fields["Event"] != "push" ||
		trig["off"].Status != "Disabled" {
		t.Errorf("the repository's triggers = %+v (%s)", secs["triggers"].Listing.Items, secs["triggers"].Unavailable)
	}
	deadline := time.Now().Add(20 * time.Second)
	for len(secs["builds"].Listing.Items) == 0 && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		_, secs = repoPage()
	}
	if b := secs["builds"].Listing.Items; len(b) != 1 || len(b[0].Fields["Commit"]) != 7 || !strings.HasPrefix(b[0].Link, "/cloud-build/builds/") {
		t.Errorf("the repository's builds = %+v (%s)", b, secs["builds"].Unavailable)
	}

	testSourceManagerRepositoryPages(t, ctx, p, work, git)

	// Developer Connect (#1546): Connect to Developer Connect on the
	// instance's page, then Link repository and Delete link on the
	// repository's, and Delete connection.
	if err := p.ActAt(ctx, "", []string{"inst"}, "create-connection", map[string]string{"id": "conn"}); err != nil {
		t.Fatalf("create-connection: %v", err)
	}
	if d, _ := p.Detail(ctx, "", []string{"inst"}); len(d.Sections) != 3 || len(d.Sections[1].Listing.Items) != 1 || d.Sections[1].Listing.Items[0].Name != "conn" {
		t.Errorf("the instance's connections = %+v", d.Sections)
	}
	if err := p.ActAt(ctx, "", []string{"inst", "app"}, "create-link", map[string]string{"connection": "conn", "id": "link"}); err != nil {
		t.Fatalf("create-link: %v", err)
	}
	link := "projects/console-ssm/locations/us-central1/connections/conn/gitRepositoryLinks/link"
	linked := false
	if d, _ := p.Detail(ctx, "", []string{"inst", "app"}); true {
		for _, sec := range d.Sections {
			linked = linked || (sec.ID == "links" && len(sec.Listing.Items) == 1 && sec.Listing.Items[0].Name == link)
		}
	}
	if !linked {
		t.Error("the repository's page does not list the link")
	}
	if acts := p.DetailActions(ctx, "", []string{"inst", "app"}); len(acts) != 7 || acts[6].ID != "delete-link" {
		t.Errorf("the linked repository's actions = %+v", acts)
	}
	if err := p.ActAt(ctx, "", []string{"inst", "app"}, "delete-link", map[string]string{"link": link}); err != nil {
		t.Fatalf("delete-link: %v", err)
	}
	if err := p.ActAt(ctx, "", []string{"inst"}, "delete-connection", map[string]string{"connection": "conn"}); err != nil {
		t.Fatalf("delete-connection: %v", err)
	}

	acts = p.DetailActions(ctx, "", []string{"inst", "app"})
	if ids := ssmActionIDs(acts); ids != "new-issue new-pull add-branch-rule edit-description delete-repo" || acts[3].Fields[0].Default != "first" || !acts[4].Leaves {
		t.Fatalf("the repository's actions = %+v", acts)
	}
	if err := p.ActAt(ctx, "", []string{"inst", "app"}, "edit-description", map[string]string{"description": "second"}); err != nil {
		t.Fatal(err)
	}
	if props, _ := repoPage(); props["Description"] != "second" {
		t.Errorf("the description after Edit description = %q", props["Description"])
	}

	for _, path := range [][]string{{"inst"}, {"inst", "app"}} {
		if p.PolicyOn(ctx, path) == nil {
			t.Fatalf("no Permissions tab on %v", path)
		}
		pol, err := p.GetPolicy(ctx, "", path)
		if err != nil {
			t.Fatal(err)
		}
		pol.Bindings = append(pol.Bindings, console.Binding{Role: "roles/securesourcemanager.repoReader", Members: []string{"user:a@example.com"}})
		if err := p.SetPolicy(ctx, "", path, pol); err != nil {
			t.Fatalf("SetPolicy on %v: %v", path, err)
		}
		if got, err := p.GetPolicy(ctx, "", path); err != nil || len(got.Bindings) != 1 || got.Bindings[0].Members[0] != "user:a@example.com" {
			t.Errorf("the policy on %v = %+v, %v", path, got, err)
		}
		if held, err := p.TestPermissions(ctx, "", path, []string{"securesourcemanager.repos.get"}); err != nil || len(held) != 1 {
			t.Errorf("TestPermissions on %v = %v, %v", path, held, err)
		}
	}

	if err := p.ActAt(ctx, "", []string{"inst", "app"}, "delete-repo", nil); err != nil {
		t.Fatal(err)
	}
	if d, _ := p.Detail(ctx, "", []string{"inst"}); len(d.Sections) != 3 || len(d.Sections[0].Listing.Items) != 0 {
		t.Errorf("the instance's repositories after Delete repository = %+v", d.Sections)
	}
	if err := p.ActAt(ctx, "", []string{"inst"}, "create-repo", map[string]string{"id": "again"}); err != nil {
		t.Fatal(err)
	}
	if err := p.Delete(ctx, "", "inst"); err != nil {
		t.Fatalf("Delete of an instance with a repository: %v", err)
	}
	if l, _ := p.List(ctx, ""); len(l.Items) != 0 {
		t.Errorf("the Instances list after delete = %+v", l.Items)
	}
}

func ssmActionIDs(acts []console.Action) string {
	ids := make([]string, len(acts))
	for i, a := range acts {
		ids[i] = a.ID
	}
	return strings.Join(ids, " ")
}

// testSourceManagerRepositoryPages (#1856): the Code tab lists the pushed
// files, a folder's page its files and a file's page its content; Branches
// lists main; New issue, Comment, Close issue and Reopen issue; a branch's
// New pull request, its Files changed and Merge pull request, which moves
// main; Add branch rule, Edit and Delete.
func testSourceManagerRepositoryPages(t *testing.T, ctx context.Context, p sourceManagerProvider, work string, git func(...string)) {
	t.Helper()
	sections := func(path ...string) (console.Detail, map[string]console.Section) {
		t.Helper()
		d, err := p.Detail(ctx, "", path)
		if err != nil || d.Unavailable != "" {
			t.Fatalf("the page %v = %+v, %v", path, d, err)
		}
		secs := map[string]console.Section{}
		for _, s := range d.Sections {
			secs[s.ID] = s
		}
		return d, secs
	}
	_, secs := sections("inst", "app")
	code := secs["code"].Listing.Items
	if len(code) != 2 || code[0].Name != ".cloudbuild" || code[0].Fields["Type"] != "Folder" || code[1].Name != "Dockerfile" ||
		strings.Join(code[1].Opens, "/") != "inst/app/tree/main/Dockerfile" {
		t.Fatalf("the Code tab = %+v (%s)", code, secs["code"].Unavailable)
	}
	if _, s := sections("inst", "app", "tree", "main", ".cloudbuild"); len(s["code"].Listing.Items) != 2 {
		t.Errorf("the .cloudbuild folder = %+v", s["code"])
	}
	if _, s := sections("inst", "app", "tree", "main", "Dockerfile"); s["file"].Text != "FROM scratch\n" {
		t.Errorf("the Dockerfile's page = %+v", s["file"])
	}
	if b := secs["branches"].Listing.Items; len(b) != 1 || b[0].Name != "main" || b[0].Status != "Default" || b[0].Fields["Message"] != "first" {
		t.Errorf("the Branches tab = %+v (%s)", b, secs["branches"].Unavailable)
	}

	if err := p.ActAt(ctx, "", []string{"inst", "app"}, "new-issue", map[string]string{"title": "Broken", "body": "it is"}); err != nil {
		t.Fatalf("new-issue: %v", err)
	}
	_, secs = sections("inst", "app")
	is := secs["issues"].Listing.Items
	if len(is) != 1 || is[0].Fields["Title"] != "Broken" || is[0].Status != "Open" {
		t.Fatalf("the Issues tab = %+v (%s)", is, secs["issues"].Unavailable)
	}
	ip := is[0].Opens
	for _, a := range []struct{ id, body string }{{"add-comment", "me too"}, {"close-issue", ""}} {
		if err := p.ActAt(ctx, "", ip, a.id, map[string]string{"body": a.body}); err != nil {
			t.Fatalf("%s: %v", a.id, err)
		}
	}
	d, s := sections(ip...)
	if !strings.HasPrefix(d.Title, "Broken #") || s["conversation"].Text != "it is" || len(s["comments"].Listing.Items) != 1 ||
		s["comments"].Listing.Items[0].Fields["Comment"] != "me too" {
		t.Errorf("the issue's page = %+v", d)
	}
	if ids := ssmActionIDs(p.DetailActions(ctx, "", ip)); ids != "add-comment edit-issue reopen-issue delete-issue" {
		t.Errorf("a closed issue's actions = %s", ids)
	}
	if err := p.ActAt(ctx, "", ip, "reopen-issue", nil); err != nil {
		t.Fatal(err)
	}

	git("checkout", "--quiet", "-b", "feature")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("# app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "README.md")
	git("commit", "--quiet", "-m", "readme")
	git("push", "--quiet", "origin", "feature")
	acts := p.DetailActions(ctx, "", []string{"inst", "app"})
	if acts[1].ID != "new-pull" || acts[1].Fields[0].Default != "main" || acts[1].Fields[1].Default != "feature" {
		t.Fatalf("New pull request = %+v", acts[1])
	}
	if err := p.ActAt(ctx, "", []string{"inst", "app"}, "new-pull", map[string]string{"base": "main", "head": "feature", "title": "Add a readme"}); err != nil {
		t.Fatalf("new-pull: %v", err)
	}
	_, secs = sections("inst", "app")
	prs := secs["pulls"].Listing.Items
	if len(prs) != 1 || prs[0].Fields["Branches"] != "feature → main" || prs[0].Status != "Open" {
		t.Fatalf("the Pull requests tab = %+v (%s)", prs, secs["pulls"].Unavailable)
	}
	if _, s := sections(prs[0].Opens...); !strings.Contains(s["files"].Text, "README.md") || !strings.Contains(s["files"].Text, "+# app") {
		t.Errorf("Files changed = %+v", s["files"])
	}
	if err := p.ActAt(ctx, "", prs[0].Opens, "merge-pull", nil); err != nil {
		t.Fatalf("merge-pull: %v", err)
	}
	if _, s := sections("inst", "app", "tree", "main"); len(s["code"].Listing.Items) != 3 {
		t.Errorf("main after Merge pull request = %+v (%s)", s["code"].Listing.Items, s["code"].Unavailable)
	}

	if err := p.ActAt(ctx, "", []string{"inst", "app"}, "add-branch-rule", map[string]string{"id": "protect", "filter": "main",
		"requirePullRequest": "true", "minApprovals": "0"}); err != nil {
		t.Fatalf("add-branch-rule: %v", err)
	}
	_, secs = sections("inst", "app")
	var rules console.Section
	for _, tab := range secs["settings"].Tabs {
		if tab.ID == "branchrules" {
			rules = tab
		}
	}
	if r := rules.Listing.Items; len(r) != 1 || r[0].Name != "protect" || r[0].Fields["Pull request"] != "Required" || r[0].Status != "Enabled" {
		t.Fatalf("the Branch rules tab = %+v (%s)", r, rules.Unavailable)
	}
	rp := rules.Listing.Items[0].ActsOn
	if err := p.ActAt(ctx, "", rp, "edit-branch-rule", map[string]string{"filter": "main", "requireLinearHistory": "true"}); err != nil {
		t.Fatalf("edit-branch-rule: %v", err)
	}
	if err := p.ActAt(ctx, "", rp, "delete-branch-rule", nil); err != nil {
		t.Fatalf("delete-branch-rule: %v", err)
	}
	git("checkout", "--quiet", "main")
	git("pull", "--quiet", "origin", "main")
}
