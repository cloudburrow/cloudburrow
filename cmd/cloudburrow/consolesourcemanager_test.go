package main

import (
	"context"
	"net"
	"net/http"
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
	if _, f := p.CreateForm(); len(f) != 7 || f[1].Type != "select" || f[3].Name != "private" || f[3].Unavailable != "" ||
		f[4].Name != "caPool" || f[4].ShowWhen != "private" || f[6].ShowWhen != "encryption=cmek" {
		t.Errorf("Create instance's fields = %+v", f)
	}
	// Private instance (#1856): a Private Service Connect instance with its
	// CA pool, whose page shows its service attachments.
	if _, err := p.Create(ctx, "", map[string]string{"name": "priv", "region": "us-central1", "private": "true", "caPool": "pool"}); err == nil {
		t.Error("Create instance took a CA pool that is not a resource name")
	}
	if _, err := p.Create(ctx, "", map[string]string{"name": "priv", "region": "us-central1", "private": "true",
		"caPool": "projects/console-ssm/locations/us-central1/caPools/web"}); err != nil {
		t.Fatal(err)
	}
	pd, err := p.Detail(ctx, "", []string{"priv"})
	if err != nil || pd.Unavailable != "" {
		t.Fatalf("the private instance's page = %+v, %v", pd, err)
	}
	pprops := map[string]string{}
	for _, pr := range pd.Summary {
		pprops[pr.Label] = pr.Value
	}
	if !strings.HasPrefix(pprops["Private instance"], "Yes") || pprops["CA pool"] != "projects/console-ssm/locations/us-central1/caPools/web" ||
		!strings.HasSuffix(pprops["HTTP service attachment"], "/serviceAttachments/priv-http") ||
		!strings.HasSuffix(pprops["SSH service attachment"], "/serviceAttachments/priv-ssh") {
		t.Errorf("the private instance's page = %v", pprops)
	}
	if err := p.Delete(ctx, "", "priv"); err != nil {
		t.Fatal(err)
	}
	acts := p.DetailActions(ctx, "", []string{"inst"})
	if len(acts) != 2 || acts[0].ID != "create-repo" || acts[1].ID != "create-connection" {
		t.Fatalf("the instance's actions = %+v", acts)
	}
	// Create repository is the web interface's page of its own, going on to
	// the new repository (#1856).
	if a := acts[0]; !a.Page || a.Opens != "id" || a.Title != "Create repository" || a.Submit != "Create repository" {
		t.Errorf("Create repository = %+v; want a page opening the repository it makes", a)
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
	if acts := p.DetailActions(ctx, "", []string{"inst", "app"}); len(acts) != 8 || acts[7].ID != "delete-link" {
		t.Errorf("the linked repository's actions = %+v", acts)
	}
	if err := p.ActAt(ctx, "", []string{"inst", "app"}, "delete-link", map[string]string{"link": link}); err != nil {
		t.Fatalf("delete-link: %v", err)
	}
	if err := p.ActAt(ctx, "", []string{"inst"}, "delete-connection", map[string]string{"connection": "conn"}); err != nil {
		t.Fatalf("delete-connection: %v", err)
	}

	acts = p.DetailActions(ctx, "", []string{"inst", "app"})
	if ids := ssmActionIDs(acts); ids != "new-issue new-pull add-branch-rule add-hook edit-description delete-repo" || acts[4].Fields[0].Default != "first" || !acts[5].Leaves {
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
	testSourceManagerReviews(t, ctx, p, prs[0].Opens, sections)
	testSourceManagerWebhooks(t, ctx, p, sections)
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
	testSourceManagerCodeOwners(t, ctx, p, work, git, sections)
}

// testSourceManagerCodeOwners (#1928): a CODEOWNERS file's page shows its
// line-level checks; Add branch rule's Require Code Owner Review on Pull
// Requests; a pull request's Code owners tab and summary show what the
// base branch's CODEOWNERS require and who approved, and Merge pull
// request is refused until they have.
func testSourceManagerCodeOwners(t *testing.T, ctx context.Context, p sourceManagerProvider, work string, git func(...string),
	sections func(path ...string) (console.Detail, map[string]console.Section)) {
	t.Helper()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(work, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("CODEOWNERS", "* lead@example.com\n[Docs][2]\n*.md doc1@example.com doc2@example.com\n* @team\nrelease-* /x/ rel@example.com\n")
	git("add", "CODEOWNERS")
	git("commit", "--quiet", "-m", "owners")
	git("push", "--quiet", "origin", "main")
	_, s := sections("inst", "app", "tree", "main", "CODEOWNERS")
	checks := s["codeowners"]
	if len(checks.Listing.Items) != 2 || checks.Listing.Items[0].Name != "4" || checks.Listing.Items[0].Status != "Error" ||
		checks.Listing.Items[1].Name != "5" || checks.Listing.Items[1].Status != "Information" || !strings.Contains(checks.Note, "1 lines have syntax errors") {
		t.Fatalf("the CODEOWNERS page's checks = %+v", checks)
	}
	if err := p.ActAt(ctx, "", []string{"inst", "app"}, "add-branch-rule", map[string]string{"id": "owners", "filter": "main",
		"requireCodeOwners": "true"}); err != nil {
		t.Fatalf("add-branch-rule: %v", err)
	}
	git("checkout", "--quiet", "-b", "docs")
	write("GUIDE.md", "# guide\n")
	git("add", "GUIDE.md")
	git("commit", "--quiet", "-m", "guide")
	git("push", "--quiet", "origin", "docs")
	if err := p.ActAt(ctx, "", []string{"inst", "app"}, "new-pull", map[string]string{"base": "main", "head": "docs", "title": "Guide"}); err != nil {
		t.Fatalf("new-pull: %v", err)
	}
	_, secs := sections("inst", "app")
	var pr []string
	for _, x := range secs["pulls"].Listing.Items {
		if x.Fields["Title"] == "Guide" {
			pr = x.Opens
		}
	}
	d, s := sections(pr...)
	co := s["codeowners"]
	if len(co.Listing.Items) != 2 || !strings.Contains(co.Note, "Branch rule owners requires code owner review") || !strings.Contains(co.Note, "1 lines") {
		t.Fatalf("the Code owners tab = %+v", co)
	}
	for _, x := range co.Listing.Items {
		if x.Status != "Pending" || x.Fields["Files"] != "GUIDE.md" || x.Fields["Approved by"] != "—" {
			t.Errorf("a requirement = %+v", x)
		}
	}
	if docs := co.Listing.Items[1]; docs.Fields["Section"] != "Docs" || docs.Fields["Approvals needed"] != "2" || docs.Fields["Owners"] != "doc1@example.com, doc2@example.com" {
		t.Errorf("the Docs requirement = %+v", docs)
	}
	summary := ""
	for _, x := range d.Summary {
		if x.Label == "Code owners" {
			summary = x.Value
		}
	}
	if summary != "0 of 2 satisfied; required by branch rule owners" {
		t.Errorf("the summary's Code owners = %q", summary)
	}
	if err := p.ActAt(ctx, "", pr, "merge-pull", nil); err == nil || !strings.Contains(err.Error(), "code owner") {
		t.Errorf("merge-pull without the code owners: %v", err)
	}
	if err := p.ActAt(ctx, "", pr, "close-pull", nil); err != nil {
		t.Logf("close-pull: %v", err)
	}
	git("checkout", "--quiet", "main")
}

// testSourceManagerReviews (#1927): a pull request's Comment, Add line
// comment, Reply, Resolve conversation, Review (Comment: the caller opened
// the pull request, so it cannot approve it), Edit and Delete, as the
// Conversation and Files changed tabs show them.
func testSourceManagerReviews(t *testing.T, ctx context.Context, p sourceManagerProvider, pp []string,
	sections func(path ...string) (console.Detail, map[string]console.Section)) {
	t.Helper()
	if ids := ssmActionIDs(p.DetailActions(ctx, "", pp)); !strings.HasPrefix(ids, "add-pr-comment add-line-comment review edit-pull merge-pull") {
		t.Errorf("an open pull request's actions = %s", ids)
	}
	for _, a := range []struct {
		id string
		v  map[string]string
	}{
		{"add-pr-comment", map[string]string{"body": "looks fine"}},
		{"add-line-comment", map[string]string{"path": "README.md", "line": "1", "lineBody": "a title?"}},
		{"review", map[string]string{"decision": "COMMENT", "body": "one note", "path": "README.md", "line": "1", "lineBody": "and here"}},
	} {
		if err := p.ActAt(ctx, "", pp, a.id, a.v); err != nil {
			t.Fatalf("%s: %v", a.id, err)
		}
	}
	if err := p.ActAt(ctx, "", pp, "review", map[string]string{"decision": "APPROVED"}); err == nil {
		t.Error("Approve on the caller's own pull request was accepted")
	}
	_, s := sections(pp...)
	conv := s["conversation"].Listing.Items
	byType := map[string]console.Resource{}
	for _, it := range conv {
		byType[it.Fields["Type"]] = it
	}
	if len(conv) != 5 || conv[0].Name != "description" || byType["Comment"].Fields["Comment"] != "looks fine" ||
		byType["Review"].Status != "Commented" || byType["Code comment"].Fields["File"] != "README.md:1" || byType["Code comment"].Status != "Unresolved" {
		t.Fatalf("the Conversation tab = %+v (%s)", conv, s["conversation"].Unavailable)
	}
	if !strings.Contains(s["files"].Text, "Comments on README.md:") || !strings.Contains(s["files"].Text, "a title?") {
		t.Errorf("Files changed does not show the line comments: %s", s["files"].Text)
	}
	code := append(append([]string{}, pp...), byType["Code comment"].Name)
	if ids := ssmActionIDs(p.DetailActions(ctx, "", code)); ids != "reply-comment resolve-comment edit-pr-comment delete-pr-comment" {
		t.Errorf("a code comment's actions = %s", ids)
	}
	for _, a := range []struct{ id, body string }{{"reply-comment", "done"}, {"resolve-comment", ""}, {"edit-pr-comment", "a heading?"}} {
		if err := p.ActAt(ctx, "", code, a.id, map[string]string{"body": a.body}); err != nil {
			t.Fatalf("%s: %v", a.id, err)
		}
	}
	_, s = sections(pp...)
	conv = s["conversation"].Listing.Items
	var reply console.Resource
	for i, it := range conv {
		if it.Name == byType["Code comment"].Name {
			if it.Status != "Resolved" || it.Fields["Comment"] != "a heading?" || i+1 >= len(conv) {
				t.Fatalf("the code comment after Reply, Resolve and Edit = %+v", it)
			}
			reply = conv[i+1]
		}
	}
	if reply.Fields["Type"] != "Reply to "+byType["Code comment"].Name || reply.Status != "Resolved" {
		t.Errorf("the reply, right after its conversation's first comment = %+v", reply)
	}
	if err := p.ActAt(ctx, "", code, "unresolve-comment", nil); err != nil {
		t.Fatalf("unresolve-comment: %v", err)
	}
	gen := append(append([]string{}, pp...), byType["Comment"].Name)
	if err := p.ActAt(ctx, "", gen, "delete-pr-comment", nil); err != nil {
		t.Fatalf("delete-pr-comment: %v", err)
	}
	if _, s := sections(pp...); len(s["conversation"].Listing.Items) != 5 {
		t.Errorf("the Conversation tab after Delete = %+v", s["conversation"].Listing.Items)
	}
}

// testSourceManagerWebhooks (#1927): Settings › Webhooks' Add webhook,
// the hook's row and page, Test delivery, which the target receives with
// Google's headers and the page lists under Recent deliveries, Edit and
// Delete.
func testSourceManagerWebhooks(t *testing.T, ctx context.Context, p sourceManagerProvider,
	sections func(path ...string) (console.Detail, map[string]console.Section)) {
	t.Helper()
	got := make(chan http.Header, 4)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			got <- r.Header.Clone()
		}
	}))
	t.Cleanup(target.Close)
	if err := p.ActAt(ctx, "", []string{"inst", "app"}, "add-hook", map[string]string{"id": "ci", "targetUri": target.URL + "/hook",
		"sensitiveQueryString": "key=k&secret=s", "onPush": "true", "branchFilter": "{main,dev}", "active": "true"}); err != nil {
		t.Fatalf("add-hook: %v", err)
	}
	webhooks := func() console.Section {
		_, secs := sections("inst", "app")
		for _, tab := range secs["settings"].Tabs {
			if tab.ID == "webhooks" {
				return tab
			}
		}
		t.Fatal("Settings has no Webhooks tab")
		return console.Section{}
	}
	wh := webhooks()
	if h := wh.Listing.Items; len(h) != 1 || h[0].Name != "ci" || h[0].Status != "Active" || h[0].Fields["Trigger on"] != "Push" ||
		h[0].Fields["Branch filter"] != "{main,dev}" || h[0].Fields["Target URL"] != target.URL+"/hook" {
		t.Fatalf("the Webhooks tab = %+v (%s)", h, wh.Unavailable)
	}
	hp := wh.Listing.Items[0].ActsOn
	if err := p.ActAt(ctx, "", hp, "test-hook", nil); err != nil {
		t.Fatalf("test-hook: %v", err)
	}
	select {
	case h := <-got:
		if h.Get("X-SecureSourceManager-Event") != "push" || h.Get("X-SecureSourceManager-Delivery") == "" || h.Get("Content-Type") != "application/json" {
			t.Errorf("Test delivery's headers = %v", h)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Test delivery reached no target")
	}
	d, s := sections(hp...)
	if d.Title != "ci" || len(s["deliveries"].Listing.Items) == 0 || s["deliveries"].Listing.Items[0].Status != "Succeeded" {
		t.Errorf("the webhook's page = %+v", d)
	}
	for _, p := range d.Summary {
		if strings.Contains(p.Value, "secret=s") {
			t.Errorf("the webhook's page shows its sensitive query string: %+v", p)
		}
	}
	if err := p.ActAt(ctx, "", hp, "edit-hook", map[string]string{"targetUri": target.URL + "/other", "onPullRequest": "true",
		"onPullRequestComment": "true", "active": "false"}); err != nil {
		t.Fatalf("edit-hook: %v", err)
	}
	if h := webhooks().Listing.Items; len(h) != 1 || h[0].Status != "Inactive" || h[0].Fields["Trigger on"] != "Pull request state changed, Pull request comment" {
		t.Errorf("the Webhooks tab after Edit = %+v", h)
	}
	if err := p.ActAt(ctx, "", hp, "delete-hook", nil); err != nil {
		t.Fatalf("delete-hook: %v", err)
	}
	if h := webhooks().Listing.Items; len(h) != 0 {
		t.Errorf("the Webhooks tab after Delete = %+v", h)
	}
}
