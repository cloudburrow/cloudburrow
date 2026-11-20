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
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

type stubIDTokens struct{}

func (stubIDTokens) ServiceAccountIDToken(account, audience string) (string, error) {
	return "id-token-for-" + account + "-at-" + audience, nil
}

// ssmConsole2071 is a Secure Source Manager on a gRPC listener, its
// provider, and the repositories' state directory.
func ssmConsole2071(t *testing.T) (sourceManagerProvider, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	if err := runadapter.UseSourceManagerRoot(root); err != nil {
		t.Fatal(err)
	}
	s := runadapter.NewServer(&runadapter.Knative{}, "t", time.Minute)
	cb := s.Builds(stubSource{}, blockingBuilder{}).CloudBuild()
	var srv *httptest.Server
	ssm := s.SecureSourceManager(func() string { return srv.URL }).WithTokens(stubIDTokens{})
	t.Cleanup(func() { ssm.WithTokens(nil) })
	srv = httptest.NewServer(runadapter.SourceManagerHandlerOr(ssm, cb, func(g grpc.ServiceRegistrar) { ssm.Register(g) }, nil))
	t.Cleanup(srv.Close)
	g := grpc.NewServer()
	cb.Register(g)
	ssm.Register(g)
	runadapter.NewOperationsServer(s).Register(g)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(g.Stop)
	return sourceManagerProvider{b: runBuildsProvider{defaultProject: "console-2071", runAddr: func() string { return ln.Addr().String() }}}, root
}

// TestConsoleSourceManagerRound3 (#2071): Create repository's Initialize
// repository writes .gitignore, LICENSE and README.md as the first commit;
// the code view has its branch and tag selector, History, a file's
// language and Blame; the Issues tab sorts and filters by label,
// milestone and assignee; an issue's Labels, Milestones and Assignees are
// kept across a restart; the plus icon's Start review keeps a comment
// Pending until Review sends it; a webhook's service account
// authentication sends the repository service account's ID token.
func TestConsoleSourceManagerRound3(t *testing.T) {
	p, root := ssmConsole2071(t)
	ctx := context.Background()
	if _, err := p.Create(ctx, "", map[string]string{"name": "inst", "region": "us-central1"}); err != nil {
		t.Fatal(err)
	}
	form := createRepositoryAction().Fields
	byName := map[string]console.Field{}
	for _, f := range form {
		byName[f.Name] = f
	}
	if byName["initialize"].Unavailable != "" || byName["gitignore"].ShowWhen != "initialize" || len(byName["gitignore"].Options) != 231 ||
		len(byName["license"].Options) != 460 {
		t.Fatalf("Create repository's Initialize repository fields = %+v %+v", byName["initialize"], byName["license"].ShowWhen)
	}
	if err := p.ActAt(ctx, "", []string{"inst"}, "create-repo", map[string]string{"id": "app", "description": "An app.", "branch": "main",
		"initialize": "true", "gitignore": "go", "license": "mit"}); err != nil {
		t.Fatal(err)
	}
	sections := func(path ...string) (console.Detail, map[string]console.Section) {
		t.Helper()
		d, err := p.Detail(ctx, "", path)
		if err != nil || d.Unavailable != "" {
			t.Fatalf("%v = %+v, %v", path, d, err)
		}
		out := map[string]console.Section{}
		for _, s := range d.Sections {
			out[s.ID] = s
		}
		return d, out
	}
	_, secs := sections("inst", "app")
	var names []string
	for _, it := range secs["code"].Listing.Items {
		names = append(names, it.Name)
	}
	if strings.Join(names, " ") != ".gitignore LICENSE README.md" {
		t.Fatalf("the initialized repository's Code tab = %v (%s)", names, secs["code"].Unavailable)
	}
	if h := secs["history"].Listing.Items; len(h) != 1 || h[0].Fields["Message"] != "Initial commit" {
		t.Fatalf("History = %+v", h)
	}
	if _, s := sections("inst", "app", "tree", "main", "README.md"); s["file"].Text != "# app\n\nAn app.\n" || s["file"].Language != "markdown" {
		t.Errorf("README.md = %+v", s["file"])
	}

	// A clone has the files; a second commit and a tag.
	clone := secs["code"].Note[strings.Index(secs["code"].Note, "git clone ")+len("git clone "):]
	work := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", work, "-c", "user.name=Ada", "-c", "user.email=ada@example.com"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("clone", "--quiet", clone, ".")
	if b, err := os.ReadFile(filepath.Join(work, "LICENSE")); err != nil || !strings.Contains(string(b), "MIT License") {
		t.Fatalf("the clone's LICENSE = %.60q, %v", b, err)
	}
	if err := os.WriteFile(filepath.Join(work, "main.go"), []byte("package main\n\n// main runs.\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "main.go")
	git("commit", "--quiet", "-m", "Add main")
	git("tag", "v1")
	git("push", "--quiet", "origin", "main", "v1")
	d, s := sections("inst", "app", "tree", "main", "main.go")
	f := s["file"]
	if f.Language != "go" || len(f.Blame) != 1 || f.Blame[0].Count != 4 || f.Blame[0].Author != "Ada" || f.Blame[0].Summary != "Add main" {
		t.Errorf("main.go's code view = language %q, blame %+v", f.Language, f.Blame)
	}
	if f.Refs == nil || f.Refs.Current != "main" || len(f.Refs.Branches) != 1 || len(f.Refs.Tags) != 1 || f.Refs.Tags[0].Name != "v1" ||
		strings.Join(f.Refs.Tags[0].Path, "/") != "inst/app/tree/v1/main.go" {
		t.Errorf("main.go's ref selector = %+v", f.Refs)
	}
	if h := s["history"].Listing.Items; len(h) != 1 || h[0].Fields["Message"] != "Add main" {
		t.Errorf("main.go's History = %+v (%s)", h, d.Title)
	}
	if _, s := sections("inst", "app"); len(s["history"].Listing.Items) != 2 {
		t.Errorf("the repository's History = %+v", s["history"].Listing.Items)
	}

	// Labels, milestones, assignees; the Issues tab's menus and sort.
	for _, title := range []string{"First", "Second"} {
		if err := p.ActAt(ctx, "", []string{"inst", "app"}, "new-issue", map[string]string{"title": title}); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range []struct {
		id string
		v  map[string]string
	}{
		{"new-label", map[string]string{"name": "bug", "color": "#d93025", "description": "Something is wrong"}},
		{"new-label", map[string]string{"name": "docs", "color": "#1a73e8"}},
		{"new-milestone", map[string]string{"title": "v1", "due": "2026-12-31"}},
	} {
		if err := p.ActAt(ctx, "", []string{"inst", "app"}, a.id, a.v); err != nil {
			t.Fatalf("%s: %v", a.id, err)
		}
	}
	if err := p.ActAt(ctx, "", []string{"inst", "app"}, "new-label", map[string]string{"name": "bug", "color": "#000000"}); err == nil {
		t.Error("a second label named bug was made")
	}
	one := []string{"inst", "app", "issues", "1"}
	if ids := ssmActionIDs(p.DetailActions(ctx, "", one)); !strings.Contains(ids, "set-labels set-milestone set-assignees") {
		t.Fatalf("the issue's actions = %s", ids)
	}
	me := runadapter.SourceManagerConsoleAccount()
	for _, a := range []struct {
		id string
		v  map[string]string
	}{
		{"set-labels", map[string]string{"labelNames": "bug\ndocs", "label-0": "true", "label-1": "false"}},
		{"set-milestone", map[string]string{"milestone": "v1"}},
		{"set-assignees", map[string]string{"assignees": me + "\nbob@example.com"}},
	} {
		if err := p.ActAt(ctx, "", one, a.id, a.v); err != nil {
			t.Fatalf("%s: %v", a.id, err)
		}
	}
	if err := p.ActAt(ctx, "", one, "set-milestone", map[string]string{"milestone": "v9"}); err == nil {
		t.Error("an issue took a milestone the repository does not have")
	}
	id, _ := sections(one...)
	props := map[string]console.Property{}
	for _, pr := range id.Summary {
		props[pr.Label] = pr
	}
	if props["Labels"].Value != "bug" || props["Labels"].Action != "set-labels" || props["Milestone"].Value != "v1" ||
		props["Assignees"].Value != "bob@example.com, "+me && props["Assignees"].Value != me+", bob@example.com" {
		t.Errorf("the issue's details = %+v", props)
	}
	_, secs = sections("inst", "app")
	is := secs["issues"].Listing
	if len(is.Sorts) != 4 || is.Sorts[0].Label != "Newest" || len(is.Menus) != 5 || is.Menus[0].Label != "Labels" ||
		strings.Join(is.Menus[0].Options, ",") != "bug,docs" || is.Menus[3].Label != "Type" || is.Menus[4].Label != "Assigned to me" || is.Menus[4].Toggle != me {
		t.Errorf("the Issues tab's menus = %+v %+v", is.Sorts, is.Menus)
	}
	rows := map[string]console.Resource{}
	for _, it := range is.Items {
		rows[it.Name] = it
	}
	if rows["1"].Fields["Labels"] != "bug" || rows["1"].Fields["Milestone"] != "v1" || rows["2"].Fields["Labels"] != "—" ||
		rows["1"].Fields["_number"] >= rows["2"].Fields["_number"] || !strings.Contains(rows["1"].Fields["_type"], "Assigned to you") ||
		!strings.Contains(rows["1"].Fields["_type"], "Created by you") {
		t.Errorf("the Issues tab's rows = %+v", is.Items)
	}
	if ls := secs["labels"].Listing.Items; len(ls) != 2 || ls[0].Name != "bug" || ls[0].Fields["Issues"] != "1" {
		t.Errorf("the Labels tab = %+v", ls)
	}
	if ms := secs["milestones"].Listing.Items; len(ms) != 1 || ms[0].Status != "Open" || ms[0].Fields["Due date"] != "2026-12-31" {
		t.Errorf("the Milestones tab = %+v", ms)
	}
	if err := p.ActAt(ctx, "", []string{"inst", "app", "labels", "docs"}, "edit-label", map[string]string{"name": "documentation", "color": "#188038"}); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "", []string{"inst", "app", "milestones", "v1"}, "close-milestone", nil); err != nil {
		t.Fatal(err)
	}
	// A restart keeps them.
	if err := runadapter.UseSourceManagerRoot(root); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(root, "state.json"))
	if !strings.Contains(string(b), `"documentation"`) || !strings.Contains(string(b), `"bob@example.com"`) {
		t.Errorf("the state file holds no labels or assignees: %.300s", b)
	}

	// The plus icon: Start review keeps a comment Pending; Review sends it.
	git("checkout", "--quiet", "-b", "feature")
	if err := os.WriteFile(filepath.Join(work, "main.go"), []byte("package main\n\n// main runs, twice.\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("commit", "--quiet", "-am", "Twice")
	git("push", "--quiet", "origin", "feature")
	if err := p.ActAt(ctx, "", []string{"inst", "app"}, "new-pull", map[string]string{"base": "main", "head": "feature", "title": "Twice"}); err != nil {
		t.Fatal(err)
	}
	pr := []string{"inst", "app", "pulls", "1"}
	if err := p.ActAt(ctx, "", pr, "start-review", map[string]string{"path": "main.go", "line": "3", "lineBody": "why twice?"}); err != nil {
		t.Fatalf("start-review: %v", err)
	}
	if err := p.ActAt(ctx, "", pr, "start-review", map[string]string{"path": "main.go", "line": "-3", "lineBody": "this was fine"}); err != nil {
		t.Fatalf("start-review: %v", err)
	}
	// A pull request's Reviewers, Labels, Milestone, Assignees and Due date.
	if err := p.ActAt(ctx, "", pr, "set-reviewers", map[string]string{"reviewers": "Bob@example.com"}); err != nil {
		t.Fatalf("set-reviewers: %v", err)
	}
	if err := p.ActAt(ctx, "", pr, "set-due", map[string]string{"due": "2026-12-01"}); err != nil {
		t.Fatalf("set-due: %v", err)
	}
	if err := p.ActAt(ctx, "", pr, "set-due", map[string]string{"due": "soon"}); err == nil {
		t.Error("set-due took a date that is not one")
	}
	if err := p.ActAt(ctx, "", pr, "set-labels", map[string]string{"labelNames": "bug\ndocumentation", "label-0": "true"}); err != nil {
		t.Fatalf("set-labels on a pull request: %v", err)
	}
	if err := p.ActAt(ctx, "", []string{"inst", "app", "issues", "2"}, "set-reviewers", map[string]string{"reviewers": "bob@example.com"}); err == nil {
		t.Error("an issue took reviewers")
	}
	d, s = sections(pr...)
	pprops := map[string]console.Property{}
	for _, pp := range d.Summary {
		pprops[pp.Label] = pp
	}
	if pprops["Reviewers"].Value != "bob@example.com" || pprops["Due date"].Value != "2026-12-01" || pprops["Labels"].Value != "bug" ||
		pprops["Due date"].Action != "set-due" {
		t.Errorf("the pull request's details = %+v", pprops)
	}
	_, secs = sections("inst", "app")
	if pl := secs["pulls"].Listing; len(pl.Menus) != 5 || len(pl.Items) != 1 || pl.Items[0].Fields["Labels"] != "bug" {
		t.Errorf("the Pull requests tab = %+v", pl)
	}
	if ls := secs["labels"].Listing.Items; ls[0].Fields["Pull requests"] != "1" {
		t.Errorf("the Labels tab's pull requests = %+v", ls[0].Fields)
	}
	files := s["files"]
	if files.Kind != console.KindDiff || files.Diff == nil || files.Diff.Pending != 2 || len(files.Diff.Files) != 1 {
		t.Fatalf("Files changed = %+v", files.Diff)
	}
	txt := ssmDiffText(files)
	if !strings.Contains(txt, "-3 // main runs.") || !strings.Contains(txt, "+3 // main runs, twice.") ||
		!strings.Contains(txt, "line 3 comment pending-1 Pending: why twice?") || !strings.Contains(txt, "line -3 comment pending-2 Pending: this was fine") {
		t.Errorf("Files changed = %s", txt)
	}
	if err := p.ActAt(ctx, "", pr, "review", map[string]string{"decision": "COMMENT", "body": "two notes"}); err != nil {
		t.Fatalf("review: %v", err)
	}
	_, s = sections(pr...)
	if s["files"].Diff.Pending != 0 || !strings.Contains(ssmDiffText(s["files"]), "Unresolved: why twice?") {
		t.Errorf("after Review, Files changed = %s", ssmDiffText(s["files"]))
	}
	kinds := 0
	for _, it := range s["conversation"].Listing.Items {
		if it.Fields["Type"] == "Code comment" {
			kinds++
		}
	}
	if kinds != 2 {
		t.Errorf("the review sent %d code comments, want 2: %+v", kinds, s["conversation"].Listing.Items)
	}

	// Enable service account authentication.
	var mu sync.Mutex
	var got []string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Header.Get("Authorization"))
		mu.Unlock()
	}))
	t.Cleanup(target.Close)
	if err := p.ActAt(ctx, "", []string{"inst", "app"}, "add-hook", map[string]string{"id": "h", "targetUri": target.URL, "onPush": "true",
		"active": "true", "serviceAccountAuth": "true"}); err == nil || !strings.Contains(err.Error(), "service account") {
		t.Errorf("service account authentication without the repository's service account: %v", err)
	}
	if err := p.ActAt(ctx, "", []string{"inst"}, "create-repo", map[string]string{"id": "svc", "serviceAccount": "builder@console-2071.iam.gserviceaccount.com"}); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "", []string{"inst", "svc"}, "add-hook", map[string]string{"id": "h", "targetUri": target.URL + "/hook", "onPush": "true",
		"active": "true", "serviceAccountAuth": "true"}); err != nil {
		t.Fatal(err)
	}
	hp := []string{"inst", "svc", "hooks", "h"}
	hd, _ := sections(hp...)
	for _, pr := range hd.Summary {
		if pr.Label == "Service account authentication" && pr.Value != "Enabled" {
			t.Errorf("the hook's page says %q", pr.Value)
		}
	}
	if err := p.ActAt(ctx, "", hp, "test-hook", nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != "Bearer id-token-for-builder@console-2071.iam.gserviceaccount.com-at-"+target.URL+"/hook" {
		t.Errorf("the delivery's Authorization = %q", got)
	}
}
