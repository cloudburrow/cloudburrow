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
	acts := p.DetailActions(ctx, "", []string{"inst"})
	if len(acts) != 1 || acts[0].ID != "create-repo" {
		t.Fatalf("the instance's actions = %+v", acts)
	}
	if err := p.ActAt(ctx, "", []string{"inst"}, "create-repo", map[string]string{"id": "app", "description": "first", "branch": "main"}); err != nil {
		t.Fatal(err)
	}
	d, err := p.Detail(ctx, "", []string{"inst"})
	if err != nil || d.Unavailable != "" || len(d.Sections) != 1 || len(d.Sections[0].Listing.Items) != 1 ||
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
	if !strings.Contains(secs["push"].Text, "git clone "+clone) || !strings.Contains(secs["push"].Text, "git push google main") {
		t.Errorf("How to push = %q", secs["push"].Text)
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
	if b := secs["builds"].Listing.Items; len(b) != 1 || len(b[0].Fields["Commit"]) != 7 || !strings.HasPrefix(b[0].Link, "/run/builds/") {
		t.Errorf("the repository's builds = %+v (%s)", b, secs["builds"].Unavailable)
	}

	acts = p.DetailActions(ctx, "", []string{"inst", "app"})
	if len(acts) != 2 || acts[0].ID != "edit-description" || acts[0].Fields[0].Default != "first" || acts[1].ID != "delete-repo" || !acts[1].Leaves {
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
	if d, _ := p.Detail(ctx, "", []string{"inst"}); len(d.Sections) != 1 || len(d.Sections[0].Listing.Items) != 0 {
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
