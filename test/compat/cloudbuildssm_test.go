//go:build compat

package compat

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cloudbuild "cloud.google.com/go/cloudbuild/apiv1/v2"
	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	developerconnect "cloud.google.com/go/developerconnect/apiv1"
	dcpb "cloud.google.com/go/developerconnect/apiv1/developerconnectpb"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"github.com/google/uuid"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ssmLink is a Secure Source Manager repository linked through Developer
// Connect, with a work tree to commit and push from.
type ssmLink struct {
	t       *testing.T
	ctx     context.Context
	dc      *developerconnect.Client
	repo    *ssmpb.Repository
	conn    *dcpb.Connection
	link    *dcpb.GitRepositoryLink
	work    string
	headers []string // git -c options sent with every command
}

// newSSMLink makes an instance, a repository on it, a connection to the
// instance and a link to the repository, in parent; all are deleted when
// the test ends. It skips where git is missing.
func newSSMLink(ctx context.Context, t *testing.T, h *Harness, parent, prefix string) *ssmLink {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	ssm := ssmClient(t, h)
	dc, err := developerconnect.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dc.Close() })
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	iop, err := ssm.CreateInstance(ctx, &ssmpb.CreateInstanceRequest{Parent: parent, InstanceId: prefix + "-" + suffix})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	inst, err := iop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	t.Cleanup(func() {
		if op, err := ssm.DeleteInstance(context.Background(), &ssmpb.DeleteInstanceRequest{Name: inst.GetName(), Force: true}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	rop, err := ssm.CreateRepository(ctx, &ssmpb.CreateRepositoryRequest{Parent: parent, RepositoryId: prefix + "-" + suffix,
		Repository: &ssmpb.Repository{Instance: inst.GetName()}})
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	repo, err := rop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	t.Cleanup(func() {
		if op, err := ssm.DeleteRepository(context.Background(), &ssmpb.DeleteRepositoryRequest{Name: repo.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	cop, err := dc.CreateConnection(ctx, &dcpb.CreateConnectionRequest{Parent: parent, ConnectionId: prefix + "-" + suffix,
		Connection: &dcpb.Connection{ConnectionConfig: &dcpb.Connection_SecureSourceManagerInstanceConfig{
			SecureSourceManagerInstanceConfig: &dcpb.SecureSourceManagerInstanceConfig{Instance: inst.GetName()}}}})
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	conn, err := cop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	lop, err := dc.CreateGitRepositoryLink(ctx, &dcpb.CreateGitRepositoryLinkRequest{Parent: conn.GetName(), GitRepositoryLinkId: "link",
		GitRepositoryLink: &dcpb.GitRepositoryLink{CloneUri: repo.GetUris().GetGitHttps()}})
	if err != nil {
		t.Fatalf("CreateGitRepositoryLink: %v", err)
	}
	link, err := lop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateGitRepositoryLink: %v", err)
	}
	t.Cleanup(func() {
		if op, err := dc.DeleteGitRepositoryLink(context.Background(), &dcpb.DeleteGitRepositoryLinkRequest{Name: link.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
		if op, err := dc.DeleteConnection(context.Background(), &dcpb.DeleteConnectionRequest{Name: conn.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	return &ssmLink{t: t, ctx: ctx, dc: dc, repo: repo, conn: conn, link: link, work: t.TempDir()}
}

// gitIn runs git in dir with the link's options, returning its output and
// error.
func (l *ssmLink) gitIn(dir string, opts []string, args ...string) (string, error) {
	full := append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "credential.helper="}, opts...)
	cmd := exec.CommandContext(l.ctx, "git", append(full, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// git runs git in the work tree, failing the test on an error.
func (l *ssmLink) git(args ...string) string {
	l.t.Helper()
	out, err := l.gitIn(l.work, l.headers, args...)
	if err != nil {
		l.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return out
}

// commit writes files and commits them, returning the commit's SHA.
func (l *ssmLink) commit(msg string, files map[string]string) string {
	l.t.Helper()
	for name, body := range files {
		p := filepath.Join(l.work, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			l.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			l.t.Fatal(err)
		}
	}
	l.git("add", "-A")
	l.git("commit", "--quiet", "-m", msg)
	return l.git("rev-parse", "HEAD")
}

// triggerBuilds are the builds of a trigger, by their source's commit.
func triggerBuilds(ctx context.Context, t *testing.T, cb *cloudbuild.Client, parent, project, triggerID string) map[string][]*cbpb.Build {
	t.Helper()
	out := map[string][]*cbpb.Build{}
	it := cb.ListBuilds(ctx, &cbpb.ListBuildsRequest{Parent: parent, ProjectId: project, Filter: `trigger_id="` + triggerID + `"`})
	for {
		b, err := it.Next()
		if err == iterator.Done {
			return out
		}
		if err != nil {
			t.Fatalf("ListBuilds: %v", err)
		}
		rev := b.GetSource().GetGitSource().GetRevision()
		out[rev] = append(out[rev], b)
	}
}

// covers: google.devtools.cloudbuild.v1.CloudBuild/CreateBuildTrigger, google.devtools.cloudbuild.v1.CloudBuild/RunBuildTrigger, google.devtools.cloudbuild.v1.CloudBuild/ListBuilds
//
// TestCloudBuildSourceManagerTriggerFilters (#1955), with the official
// cloudbuild and developerconnect clients over gRPC on the Run port: a
// BuildTrigger on a Developer Connect link to a Secure Source Manager
// repository, with includedFiles ["src/**"] and ignoredFiles
// ["**/*.md"], as Google's BuildTrigger reference documents the globs
// (filepath.Match with **, ignored first). A push touching only ignored and
// unincluded files (docs/guide.md, src/notes.md, README) builds nothing; a
// push touching src/app.go builds that commit. A trigger whose CEL filter
// is BRANCH_NAME == "release" builds a push to release but not to main; a
// filter using ! (which Google's trigger filters do not support), naming
// an unknown variable, or not boolean, is INVALID_ARGUMENT. RunBuildTrigger
// with source.tagName builds the tag's commit with TAG_NAME, and with
// commitSha that commit, unfiltered; a tag the repository lacks is
// FAILED_PRECONDITION.
func TestCloudBuildSourceManagerTriggerFilters(t *testing.T) {
	h := New(t)
	_ = runClient(t, h) // skips without CLOUDBURROW_TEST_RUN
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required: builds run with the host's Docker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	t.Cleanup(cancel)
	parent := runParent(h)
	l := newSSMLink(ctx, t, h, parent, "cbf")
	cb := cloudBuildClient(ctx, t, h)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]

	// The repository's first commit, and a tag on it, before any trigger.
	l.git("clone", "--quiet", l.repo.GetUris().GetGitHttps(), ".")
	l.git("checkout", "--quiet", "-b", "main")
	first := l.commit("first", map[string]string{"src/app.go": "package app\n", "README": "app\n"})
	l.git("tag", "v1")
	l.git("push", "--quiet", "origin", "main", "v1")

	build := &cbpb.Build{Steps: []*cbpb.BuildStep{{Name: stepImage(), Entrypoint: "sh", Args: []string{"-c", "echo $COMMIT_SHA $TAG_NAME"}}}}
	repoEvents := func(branch string) *cbpb.RepositoryEventConfig {
		return &cbpb.RepositoryEventConfig{Repository: l.link.GetName(),
			Filter: &cbpb.RepositoryEventConfig_Push{Push: &cbpb.PushFilter{GitRef: &cbpb.PushFilter_Branch{Branch: branch}}}}
	}
	files, err := cb.CreateBuildTrigger(ctx, &cbpb.CreateBuildTriggerRequest{Parent: parent, ProjectId: h.Project(),
		Trigger: &cbpb.BuildTrigger{Name: "files-" + suffix, RepositoryEventConfig: repoEvents("^main$"),
			IncludedFiles: []string{"src/**"}, IgnoredFiles: []string{"**/*.md"},
			BuildTemplate: &cbpb.BuildTrigger_Build{Build: build}}})
	if err != nil {
		t.Fatalf("CreateBuildTrigger with includedFiles and ignoredFiles: %v", err)
	}
	t.Cleanup(func() {
		_ = cb.DeleteBuildTrigger(context.Background(), &cbpb.DeleteBuildTriggerRequest{Name: files.GetResourceName()})
	})
	if got, err := cb.GetBuildTrigger(ctx, &cbpb.GetBuildTriggerRequest{Name: files.GetResourceName()}); err != nil ||
		strings.Join(got.GetIncludedFiles(), ",") != "src/**" || strings.Join(got.GetIgnoredFiles(), ",") != "**/*.md" {
		t.Errorf("GetBuildTrigger = %v, %v; want its includedFiles and ignoredFiles", got, err)
	}

	for _, bad := range []string{`!(BRANCH_NAME == "main")`, `_NOPE == "x"`, `BRANCH_NAME`} {
		_, err := cb.CreateBuildTrigger(ctx, &cbpb.CreateBuildTriggerRequest{Parent: parent, ProjectId: h.Project(),
			Trigger: &cbpb.BuildTrigger{Name: "bad-" + suffix, RepositoryEventConfig: repoEvents(".*"), Filter: bad,
				BuildTemplate: &cbpb.BuildTrigger_Build{Build: build}}})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("CreateBuildTrigger with filter %q: %v; want InvalidArgument", bad, err)
		}
	}
	if _, err := cb.CreateBuildTrigger(ctx, &cbpb.CreateBuildTriggerRequest{Parent: parent, ProjectId: h.Project(),
		Trigger: &cbpb.BuildTrigger{Name: "glob-" + suffix, RepositoryEventConfig: repoEvents(".*"), IncludedFiles: []string{"src/[x"},
			BuildTemplate: &cbpb.BuildTrigger_Build{Build: build}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateBuildTrigger with a malformed glob: %v; want InvalidArgument", err)
	}
	filtered, err := cb.CreateBuildTrigger(ctx, &cbpb.CreateBuildTriggerRequest{Parent: parent, ProjectId: h.Project(),
		Trigger: &cbpb.BuildTrigger{Name: "filter-" + suffix, RepositoryEventConfig: repoEvents(".*"),
			Filter: `BRANCH_NAME == "release" && REPO_NAME.matches("^cbf-")`, BuildTemplate: &cbpb.BuildTrigger_Build{Build: build}}})
	if err != nil {
		t.Fatalf("CreateBuildTrigger with a filter: %v", err)
	}
	t.Cleanup(func() {
		_ = cb.DeleteBuildTrigger(context.Background(), &cbpb.DeleteBuildTriggerRequest{Name: filtered.GetResourceName()})
	})

	// A manual run at the tag, and at a commit: not filtered by files.
	op, err := cb.RunBuildTrigger(ctx, &cbpb.RunBuildTriggerRequest{Name: files.GetResourceName(), ProjectId: h.Project(),
		TriggerId: files.GetId(), Source: &cbpb.RepoSource{Revision: &cbpb.RepoSource_TagName{TagName: "v1"}}})
	skipWithoutHostSteps(t, err)
	if err != nil {
		t.Fatalf("RunBuildTrigger at tag v1: %v", err)
	}
	b, err := op.Wait(ctx)
	if err != nil || b.GetStatus() != cbpb.Build_SUCCESS || b.GetSource().GetGitSource().GetRevision() != first ||
		len(b.GetSteps()) != 1 || b.GetSteps()[0].GetArgs()[1] != "echo "+first+" v1" {
		t.Errorf("the build at tag v1 = %v, %v; want SUCCESS at %s with TAG_NAME v1", b, err, first)
	}
	if _, err := cb.RunBuildTrigger(ctx, &cbpb.RunBuildTriggerRequest{Name: files.GetResourceName(), ProjectId: h.Project(),
		TriggerId: files.GetId(), Source: &cbpb.RepoSource{Revision: &cbpb.RepoSource_TagName{TagName: "v9"}}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("RunBuildTrigger at a missing tag: %v; want FailedPrecondition", err)
	}

	// A push of ignored and unincluded files only, then one of src/app.go.
	ignored := l.commit("docs", map[string]string{"docs/guide.md": "# guide\n", "src/notes.md": "notes\n", "README": "app, again\n"})
	l.git("push", "--quiet", "origin", "main")
	included := l.commit("code", map[string]string{"src/app.go": "package app\n\nconst V = 2\n"})
	l.git("push", "--quiet", "origin", "main")
	l.git("checkout", "--quiet", "-b", "release")
	release := l.commit("release", map[string]string{"VERSION": "2\n"})
	l.git("push", "--quiet", "origin", "release")

	op, err = cb.RunBuildTrigger(ctx, &cbpb.RunBuildTriggerRequest{Name: files.GetResourceName(), ProjectId: h.Project(),
		TriggerId: files.GetId(), Source: &cbpb.RepoSource{Revision: &cbpb.RepoSource_CommitSha{CommitSha: ignored}}})
	if err != nil {
		t.Fatalf("RunBuildTrigger at a commit: %v", err)
	}
	if b, err := op.Wait(ctx); err != nil || b.GetSource().GetGitSource().GetRevision() != ignored {
		t.Errorf("the build at commit %s = %v, %v", ignored, b, err)
	}

	var byFiles, byFilter map[string][]*cbpb.Build
	for deadline := time.Now().Add(3 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		byFiles = triggerBuilds(ctx, t, cb, parent, h.Project(), files.GetId())
		byFilter = triggerBuilds(ctx, t, cb, parent, h.Project(), filtered.GetId())
		if len(byFiles[included]) > 0 && len(byFilter[release]) > 0 {
			break
		}
	}
	if n := len(byFiles[included]); n != 1 {
		t.Errorf("the push of src/app.go ran %d builds of the files trigger; want 1", n)
	}
	if n := len(byFiles[ignored]); n != 1 { // the manual run at the commit only
		t.Errorf("the commit touching only ignored and unincluded files has %d builds of the files trigger; want only the manual run's", n)
	}
	if n := len(byFiles[release]); n != 0 {
		t.Errorf("the push to release ran %d builds of the ^main$ trigger; want none", n)
	}
	if n := len(byFilter[release]); n != 1 {
		t.Errorf("the push to release ran %d builds of the filtered trigger; want 1", n)
	}
	for _, sha := range []string{ignored, included} {
		if n := len(byFilter[sha]); n != 0 {
			t.Errorf("a push to main ran %d builds of the trigger filtering BRANCH_NAME == \"release\"; want none", n)
		}
	}
}

// covers: google.cloud.developerconnect.v1.DeveloperConnect/FetchReadToken, google.cloud.developerconnect.v1.DeveloperConnect/FetchReadWriteToken
//
// TestDeveloperConnectReadTokens (#1955), with the official
// developerconnect client over gRPC on the Run port: FetchReadToken on a
// link to a Secure Source Manager repository gives a token, its
// gitUsername and an expirationTime in the next hour. git clones the
// repository with it (sent as HTTP basic credentials); a push with it is
// refused 403, as is a clone of another repository. FetchReadWriteToken's
// token pushes. A token the instance never gave is refused 401; a link that
// does not exist is NOT_FOUND.
func TestDeveloperConnectReadTokens(t *testing.T) {
	h := New(t)
	_ = runClient(t, h) // skips without CLOUDBURROW_TEST_RUN
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	parent := runParent(h)
	l := newSSMLink(ctx, t, h, parent, "dct")
	other := newSSMLink(ctx, t, h, parent, "dco")

	l.git("clone", "--quiet", l.repo.GetUris().GetGitHttps(), ".")
	l.git("checkout", "--quiet", "-b", "main")
	sha := l.commit("first", map[string]string{"hello.txt": "hello\n"})
	l.git("push", "--quiet", "origin", "main")

	read, err := l.dc.FetchReadToken(ctx, &dcpb.FetchReadTokenRequest{GitRepositoryLink: l.link.GetName()})
	if err != nil {
		t.Fatalf("FetchReadToken: %v", err)
	}
	if read.GetToken() == "" || read.GetGitUsername() == "" {
		t.Fatalf("FetchReadToken gave no token or no gitUsername")
	}
	if exp := read.GetExpirationTime().AsTime(); !exp.After(time.Now()) || exp.After(time.Now().Add(61*time.Minute)) {
		t.Errorf("the read token expires at %v; want within the next hour", exp)
	}
	auth := func(user, tok string) []string {
		return []string{"-c", "http.extraHeader=Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+tok))}
	}
	readAuth := auth(read.GetGitUsername(), read.GetToken())

	clone := t.TempDir()
	if out, err := l.gitIn(clone, readAuth, "clone", "--quiet", l.repo.GetUris().GetGitHttps(), "."); err != nil {
		t.Fatalf("git clone with the read token: %v\n%s", err, out)
	}
	if got, err := l.gitIn(clone, nil, "rev-parse", "HEAD"); err != nil || got != sha {
		t.Errorf("the clone's HEAD = %q, %v; want %s", got, err, sha)
	}
	if b, err := os.ReadFile(filepath.Join(clone, "hello.txt")); err != nil || string(b) != "hello\n" {
		t.Errorf("the clone's hello.txt = %q, %v", b, err)
	}
	if err := os.WriteFile(filepath.Join(clone, "more.txt"), []byte("more\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := l.gitIn(clone, nil, "add", "more.txt"); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	if out, err := l.gitIn(clone, []string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, "commit", "--quiet", "-m", "more"); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	if out, err := l.gitIn(clone, readAuth, "push", "origin", "main"); err == nil || !strings.Contains(out, "403") {
		t.Errorf("git push with the read token: %v\n%s\nwant refused 403", err, out)
	}
	if out, err := l.gitIn(t.TempDir(), readAuth, "clone", "--quiet", other.repo.GetUris().GetGitHttps(), "."); err == nil || !strings.Contains(out, "403") {
		t.Errorf("git clone of another repository with the read token: %v\n%s\nwant refused 403", err, out)
	}
	if out, err := l.gitIn(t.TempDir(), auth("oauth2", "cbdc.r.forged"), "clone", "--quiet", l.repo.GetUris().GetGitHttps(), "."); err == nil ||
		// git answers a 401 by asking for credentials, which it cannot here.
		!(strings.Contains(out, "401") || strings.Contains(out, "could not read Username") || strings.Contains(out, "Authentication failed")) {
		t.Errorf("git clone with a token the instance never gave: %v\n%s\nwant refused 401", err, out)
	}

	rw, err := l.dc.FetchReadWriteToken(ctx, &dcpb.FetchReadWriteTokenRequest{GitRepositoryLink: l.link.GetName()})
	if err != nil {
		t.Fatalf("FetchReadWriteToken: %v", err)
	}
	if !rw.GetExpirationTime().AsTime().After(time.Now()) {
		t.Errorf("the read-write token expires at %v", rw.GetExpirationTime().AsTime())
	}
	if out, err := l.gitIn(clone, auth(rw.GetGitUsername(), rw.GetToken()), "push", "--quiet", "origin", "main"); err != nil {
		t.Errorf("git push with the read-write token: %v\n%s", err, out)
	}
	pushed, _ := l.gitIn(clone, nil, "rev-parse", "HEAD")
	if out, err := l.gitIn(l.work, nil, "ls-remote", "origin", "refs/heads/main"); err != nil || !strings.HasPrefix(out, pushed) {
		t.Errorf("main after the read-write push = %q, %v; want %s", out, err, pushed)
	}

	if _, err := l.dc.FetchReadToken(ctx, &dcpb.FetchReadTokenRequest{GitRepositoryLink: l.conn.GetName() + "/gitRepositoryLinks/missing"}); status.Code(err) != codes.NotFound {
		t.Errorf("FetchReadToken on a missing link: %v; want NotFound", err)
	}
}

// TestGcloudBuildsTriggersSourceManagerFiles (#1955): `gcloud beta builds
// triggers create developer-connect --git-repository-link
// --branch-pattern --included-files --ignored-files --inline-config`,
// through the configuration `cloudburrow gcloud-setup` writes, egress
// blocked: the official client reads the trigger's link, push filter and
// globs back, and `describe` shows them; `delete` removes it.
func TestGcloudBuildsTriggersSourceManagerFiles(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	g := newGcloudSession(t, h)
	ctx := h.Context()
	parent := "projects/" + g.project + "/locations/us-central1"
	l := newSSMLink(ctx, t, h, parent, "gcf")
	cb := cloudBuildClient(ctx, t, h)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	inline := filepath.Join(t.TempDir(), "build.yaml")
	if err := os.WriteFile(inline, []byte("steps:\n- name: "+stepImage()+"\n  args: ['true']\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	name := "gfiles-" + suffix
	g.must("beta", "builds", "triggers", "create", "developer-connect", "--name="+name, "--region=us-central1",
		"--git-repository-link="+l.link.GetName(), "--branch-pattern=^main$", "--inline-config="+inline,
		"--included-files=src/**,lib/*.go", "--ignored-files=**/*.md")
	out := g.must("builds", "triggers", "describe", name, "--region=us-central1", "--format=value(includedFiles,ignoredFiles)")
	if !strings.Contains(out, "src/**") || !strings.Contains(out, "lib/*.go") || !strings.Contains(out, "**/*.md") {
		t.Errorf("gcloud builds triggers describe = %q; want the globs", out)
	}
	var tr *cbpb.BuildTrigger
	for it := cb.ListBuildTriggers(ctx, &cbpb.ListBuildTriggersRequest{Parent: parent, ProjectId: g.project}); ; {
		x, err := it.Next()
		if err != nil {
			break
		}
		if x.GetName() == name {
			tr = x
		}
	}
	if tr == nil || tr.GetRepositoryEventConfig().GetRepository() != l.link.GetName() ||
		tr.GetRepositoryEventConfig().GetPush().GetBranch() != "^main$" ||
		strings.Join(tr.GetIncludedFiles(), ",") != "src/**,lib/*.go" || strings.Join(tr.GetIgnoredFiles(), ",") != "**/*.md" {
		t.Fatalf("the trigger gcloud made = %v", tr)
	}
	g.must("builds", "triggers", "delete", name, "--region=us-central1", "--quiet")
	if _, err := cb.GetBuildTrigger(ctx, &cbpb.GetBuildTriggerRequest{Name: tr.GetResourceName()}); status.Code(err) != codes.NotFound {
		t.Errorf("GetBuildTrigger after delete: %v; want NotFound", err)
	}
}

// covers: google.devtools.cloudbuild.v1.CloudBuild/CreateBuildTrigger, google.devtools.cloudbuild.v1.CloudBuild/GetBuildTrigger, google.devtools.cloudbuild.v1.CloudBuild/UpdateBuildTrigger, google.devtools.cloudbuild.v1.CloudBuild/DeleteBuildTrigger
//
// TestTerraformCloudBuildSourceManagerTrigger (#1955): a
// google_cloudbuild_trigger with developer_connect_event_config on a link
// to a Secure Source Manager repository, included_files, ignored_files and
// a filter, applies through `cloudburrow terraform` with egress blocked;
// the official client reads back the link, the globs and the filter; the
// plan is clean; new globs and filter apply in place, and the plan is
// clean again; destroy deletes the trigger.
func TestTerraformCloudBuildSourceManagerTrigger(t *testing.T) {
	h := New(t)
	_ = runClient(t, h) // skips without CLOUDBURROW_TEST_RUN
	m := newTFModule(t)
	ctx := h.Context()
	parent := runParent(h)
	l := newSSMLink(ctx, t, h, parent, "tfcb")
	cb := cloudBuildClient(ctx, t, h)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	name := "tf-ssm-" + suffix
	module := func(included, ignored, filter string) string {
		return fmt.Sprintf(`
resource "google_cloudbuild_trigger" "t" {
  project  = %[1]q
  location = "us-central1"
  name     = %[2]q
  developer_connect_event_config {
    git_repository_link = %[3]q
    push {
      branch = "^main$"
    }
  }
  included_files = [%[4]q]
  ignored_files  = [%[5]q]
  filter         = %[6]q
  build {
    step {
      name = %[7]q
      args = ["true"]
    }
  }
}
`, h.Project(), name, l.link.GetName(), included, ignored, filter, stepImage())
	}
	check := func(when, included, ignored, filter string) {
		t.Helper()
		var tr *cbpb.BuildTrigger
		for it := cb.ListBuildTriggers(ctx, &cbpb.ListBuildTriggersRequest{Parent: parent, ProjectId: h.Project()}); ; {
			x, err := it.Next()
			if err != nil {
				break
			}
			if x.GetName() == name {
				tr = x
			}
		}
		if tr == nil || tr.GetRepositoryEventConfig().GetRepository() != l.link.GetName() ||
			strings.Join(tr.GetIncludedFiles(), ",") != included || strings.Join(tr.GetIgnoredFiles(), ",") != ignored ||
			tr.GetFilter() != filter {
			t.Errorf("%s, the trigger = %v; want link %s, includedFiles %s, ignoredFiles %s, filter %s",
				when, tr, l.link.GetName(), included, ignored, filter)
		}
	}
	m.write(module("src/**", "**/*.md", `BRANCH_NAME == "main"`))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", "src/**", "**/*.md", `BRANCH_NAME == "main"`)
	m.planClean()
	m.write(module("lib/**", "docs/**", `REPO_NAME.matches("^tfcb-")`))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("the change was not applied in place:\n%s", lastLines(out, 30))
	}
	check("after the change", "lib/**", "docs/**", `REPO_NAME.matches("^tfcb-")`)
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	for it := cb.ListBuildTriggers(ctx, &cbpb.ListBuildTriggersRequest{Parent: parent, ProjectId: h.Project()}); ; {
		x, err := it.Next()
		if err != nil {
			break
		}
		if x.GetName() == name {
			t.Errorf("the trigger is still there after destroy: %v", x)
		}
	}
}
