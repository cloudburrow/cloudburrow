package run

import (
	"context"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestParseSourceManagerTriggers(t *testing.T) {
	got, err := ParseSourceManagerTriggers([]byte(`triggers:
- name: postsubmit-run
  configFilePath: run.yaml
  eventType: push
  includedGitRefs: ^main$
  ignoredGitRefs: dev-branch
  serviceAccount: projects/p/serviceAccounts/sa@p.iam.gserviceaccount.com
  substitutions:
    _NODE_VERSION_1: v3.2.1
    REPO_NAME: dev
`))
	if err != nil || len(got) != 1 || got[0].ConfigFilePath != "run.yaml" || got[0].Substitutions["REPO_NAME"] != "dev" {
		t.Fatalf("ParseSourceManagerTriggers = %+v, %v", got, err)
	}
	if !got[0].accepts("refs/heads/main", nil) || got[0].accepts("refs/heads/feature", nil) {
		t.Error("includedGitRefs ^main$ does not select main alone")
	}
	for _, bad := range []string{
		"triggers:\n- name: -x\n",
		"triggers:\n- name: x\n  eventType: tag\n",
		"triggers:\n- name: x\n  substitutions: {lower: v}\n",
		"triggers:\n- name: x\n  unknown: v\n",
		"triggers:\n- name: x\n  includedFiles: '('\n",
	} {
		if _, err := ParseSourceManagerTriggers([]byte(bad)); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	files := ssmTrigger{Name: "f", IncludedFiles: `^src/`, IgnoredFiles: `\.md$`}
	if files.accepts("refs/heads/main", []string{"README.md", "docs/x"}) || !files.accepts("refs/heads/main", []string{"src/a.go"}) ||
		files.accepts("refs/heads/main", []string{"src/notes.md"}) {
		t.Error("file filters")
	}
}

// TestSourceManagerPushRunsTrigger (#1521): a repository on an instance is
// cloned and pushed over git's HTTP protocol; a push to main runs the
// triggers file's push trigger with the commit's tree and its built-in
// substitutions, and a push to an ignored branch runs nothing.
func TestSourceManagerPushRunsTrigger(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	if err := UseSourceManagerRoot(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	s := NewServer(&Knative{}, "t", time.Minute)
	b := &fakeBuilder{got: make(chan BuildSpec, 4)}
	cb := s.Builds(nil, b).CloudBuild()
	var srv *httptest.Server
	m := s.SecureSourceManager(func() string { return srv.URL })
	srv = httptest.NewServer(SourceManagerHandlerOr(m, cb, func(g grpc.ServiceRegistrar) { m.Register(g) }, nil))
	defer srv.Close()
	ctx := context.Background()

	parent := "projects/ssm-proj/locations/us-central1"
	if _, err := m.CreateRepository(ctx, &ssmpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "r1",
		Repository: &ssmpb.Repository{Instance: parent + "/instances/i1"}}); status.Code(err) != codes.NotFound {
		t.Fatalf("a repository on no instance: %v", err)
	}
	if _, err := m.CreateInstance(ctx, &ssmpb.CreateInstanceRequest{Parent: parent, InstanceId: "i1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateRepository(ctx, &ssmpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "r1",
		Repository: &ssmpb.Repository{Instance: parent + "/instances/i1"}}); err != nil {
		t.Fatal(err)
	}
	r, err := m.GetRepository(ctx, &ssmpb.GetRepositoryRequest{Name: parent + "/repositories/r1"})
	if err != nil {
		t.Fatal(err)
	}
	if want := srv.URL + "/sourcemanager/git/ssm-proj/us-central1/r1.git"; r.GetUris().GetGitHttps() != want {
		t.Fatalf("gitHttps = %q; want %q", r.GetUris().GetGitHttps(), want)
	}
	if _, err := m.DeleteInstance(ctx, &ssmpb.DeleteInstanceRequest{Name: parent + "/instances/i1"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("deleting an instance with a repository: %v", err)
	}

	work := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", work, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("clone", "--quiet", r.GetUris().GetGitHttps(), ".")
	write := func(name, body string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(work, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".cloudbuild/triggers.yaml", `triggers:
- name: on-push
  ignoredGitRefs: ^dev$
  serviceAccount: projects/ssm-proj/serviceAccounts/b@ssm-proj.iam.gserviceaccount.com
  substitutions:
    _TAG: t
`)
	write(".cloudbuild/cloudbuild.yaml", "steps:\n- name: gcr.io/cloud-builders/docker\n  args: [build, -t, 'x/${REPO_NAME}:${SHORT_SHA}-${_TAG}', .]\n")
	write("Dockerfile", "FROM scratch\n")
	git("checkout", "--quiet", "-b", "main")
	git("add", ".")
	git("commit", "--quiet", "-m", "first")
	git("push", "--quiet", "origin", "main")
	sha := git("rev-parse", "HEAD")

	select {
	case spec := <-b.got:
		if spec.LocalImage != localImageFor("x/r1:"+sha[:7]+"-t") {
			t.Errorf("image = %q; want the push's substitutions", spec.LocalImage)
		}
		tgz, _ := io.ReadAll(spec.Archive)
		if len(tgz) == 0 {
			t.Error("the build has no /workspace archive")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the push ran no build")
	}

	git("checkout", "--quiet", "-b", "dev")
	write("x.txt", "x")
	git("add", ".")
	git("commit", "--quiet", "-m", "dev")
	git("push", "--quiet", "origin", "dev")
	select {
	case spec := <-b.got:
		t.Errorf("a push to the ignored branch ran %q", spec.LocalImage)
	case <-time.After(2 * time.Second):
	}

	if _, err := m.DeleteInstance(ctx, &ssmpb.DeleteInstanceRequest{Name: parent + "/instances/i1", Force: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.GetRepository(ctx, &ssmpb.GetRepositoryRequest{Name: parent + "/repositories/r1"}); status.Code(err) != codes.NotFound {
		t.Errorf("a forced instance delete left its repository: %v", err)
	}
}
