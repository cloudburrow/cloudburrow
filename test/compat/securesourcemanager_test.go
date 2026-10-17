//go:build compat

package compat

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	securesourcemanager "cloud.google.com/go/securesourcemanager/apiv1"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"github.com/google/uuid"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// covers: google.cloud.securesourcemanager.v1.SecureSourceManager/CreateInstance, google.cloud.securesourcemanager.v1.SecureSourceManager/GetInstance, google.cloud.securesourcemanager.v1.SecureSourceManager/ListInstances, google.cloud.securesourcemanager.v1.SecureSourceManager/DeleteInstance, google.cloud.securesourcemanager.v1.SecureSourceManager/CreateRepository, google.cloud.securesourcemanager.v1.SecureSourceManager/GetRepository, google.cloud.securesourcemanager.v1.SecureSourceManager/ListRepositories, google.cloud.securesourcemanager.v1.SecureSourceManager/UpdateRepository, google.cloud.securesourcemanager.v1.SecureSourceManager/DeleteRepository
//
// TestSecureSourceManagerPushTrigger (#1521), with the official Secure
// Source Manager and Cloud Build clients over gRPC on the Run port: an
// instance is created (ACTIVE), a repository on it is created, read,
// listed and updated; git clones it from its uris.gitHttps and pushes a
// .cloudbuild/triggers.yaml and a .cloudbuild/cloudbuild.yaml to main,
// which runs the trigger's build, as Google's "Connect to Cloud Build"
// documents: the build's docker build step builds the pushed commit's tree,
// with COMMIT_SHA, SHORT_SHA, REPO_NAME and REF_NAME substituted, and
// SUCCESS. A push to a branch the trigger ignores runs no build. An
// instance with a repository is not deleted without force.
func TestSecureSourceManagerPushTrigger(t *testing.T) {
	h := New(t)
	_ = runClient(t, h) // skips without CLOUDBURROW_TEST_RUN
	for _, tool := range []string{"docker", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is required", tool)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	t.Cleanup(cancel)
	c, err := securesourcemanager.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	cb := cloudBuildClient(ctx, t, h)
	parent := runParent(h)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]

	iop, err := c.CreateInstance(ctx, &ssmpb.CreateInstanceRequest{Parent: parent, InstanceId: "i-" + suffix})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	inst, err := iop.Wait(ctx)
	if err != nil || inst.GetState() != ssmpb.Instance_ACTIVE || inst.GetHostConfig().GetGitHttp() == "" {
		t.Fatalf("the instance = %v, %v", inst, err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(context.Background(), &ssmpb.DeleteInstanceRequest{Name: inst.GetName(), Force: true}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	if got, err := c.GetInstance(ctx, &ssmpb.GetInstanceRequest{Name: inst.GetName()}); err != nil || got.GetName() != inst.GetName() {
		t.Errorf("GetInstance = %v, %v", got, err)
	}
	found := false
	for it := c.ListInstances(ctx, &ssmpb.ListInstancesRequest{Parent: parent}); ; {
		x, err := it.Next()
		if err != nil {
			break
		}
		found = found || x.GetName() == inst.GetName()
	}
	if !found {
		t.Error("ListInstances does not list the instance")
	}

	rop, err := c.CreateRepository(ctx, &ssmpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "r-" + suffix,
		Repository: &ssmpb.Repository{Instance: inst.GetName(), Description: "first"}})
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	repo, err := rop.Wait(ctx)
	if err != nil || !strings.HasSuffix(repo.GetUris().GetGitHttps(), "/r-"+suffix+".git") {
		t.Fatalf("the repository = %v, %v", repo, err)
	}
	uop, err := c.UpdateRepository(ctx, &ssmpb.UpdateRepositoryRequest{Repository: &ssmpb.Repository{Name: repo.GetName(), Description: "second"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}})
	if err != nil {
		t.Fatalf("UpdateRepository: %v", err)
	}
	if _, err := uop.Wait(ctx); err != nil {
		t.Fatalf("UpdateRepository: %v", err)
	}
	if got, err := c.GetRepository(ctx, &ssmpb.GetRepositoryRequest{Name: repo.GetName()}); err != nil || got.GetDescription() != "second" {
		t.Errorf("GetRepository after the update = %v, %v", got, err)
	}
	listed := false
	for it := c.ListRepositories(ctx, &ssmpb.ListRepositoriesRequest{Parent: parent, Instance: inst.GetName()}); ; {
		x, err := it.Next()
		if err != nil {
			break
		}
		listed = listed || x.GetName() == repo.GetName()
	}
	if !listed {
		t.Error("ListRepositories does not list the repository")
	}
	if _, err := c.DeleteInstance(ctx, &ssmpb.DeleteInstanceRequest{Name: inst.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeleteInstance of an instance with a repository: %v; want FailedPrecondition", err)
	}

	work := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", work, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, body string) {
		p := filepath.Join(work, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("clone", "--quiet", repo.GetUris().GetGitHttps(), ".")
	write(".cloudbuild/triggers.yaml", `triggers:
- name: postsubmit
  eventType: push
  includedGitRefs: ^main$
  serviceAccount: projects/`+h.Project()+`/serviceAccounts/builder@`+h.Project()+`.iam.gserviceaccount.com
  substitutions:
    _KIND: release
`)
	write(".cloudbuild/cloudbuild.yaml", `steps:
- name: gcr.io/cloud-builders/docker
  args: [build, -t, 'us-central1-docker.pkg.dev/$PROJECT_ID/ssm/${REPO_NAME}:${SHORT_SHA}-${_KIND}-${REF_NAME}', .]
`)
	write("Dockerfile", "FROM scratch\nCOPY hello.txt /hello.txt\n")
	write("hello.txt", "hello\n")
	git("checkout", "--quiet", "-b", "main")
	git("add", ".")
	git("commit", "--quiet", "-m", "first")
	git("push", "--quiet", "origin", "main")
	sha := git("rev-parse", "HEAD")

	byCommit := func(rev string) *cbpb.Build {
		it := cb.ListBuilds(ctx, &cbpb.ListBuildsRequest{Parent: parent, ProjectId: h.Project()})
		for {
			b, err := it.Next()
			if err == iterator.Done {
				return nil
			}
			if err != nil {
				t.Fatalf("ListBuilds: %v", err)
			}
			if b.GetSource().GetGitSource().GetRevision() == rev {
				return b
			}
		}
	}
	var b *cbpb.Build
	for deadline := time.Now().Add(4 * time.Minute); time.Now().Before(deadline); time.Sleep(time.Second) {
		if b = byCommit(sha); b != nil && b.GetFinishTime() != nil {
			break
		}
	}
	want := "us-central1-docker.pkg.dev/" + h.Project() + "/ssm/r-" + suffix + ":" + sha[:7] + "-release-main"
	if b == nil || b.GetStatus() != cbpb.Build_SUCCESS || b.GetSteps()[0].GetArgs()[2] != want ||
		b.GetSource().GetGitSource().GetUrl() != repo.GetUris().GetGitHttps() || b.GetSubstitutions()["_KIND"] != "release" {
		t.Fatalf("the push's build = %v; want SUCCESS building %s", b, want)
	}

	git("checkout", "--quiet", "-b", "feature")
	write("hello.txt", "feature\n")
	git("commit", "--quiet", "-am", "feature")
	git("push", "--quiet", "origin", "feature")
	other := git("rev-parse", "HEAD")
	time.Sleep(3 * time.Second)
	if b := byCommit(other); b != nil {
		t.Errorf("a push to a branch includedGitRefs leaves out ran %v", b)
	}

	dop, err := c.DeleteRepository(ctx, &ssmpb.DeleteRepositoryRequest{Name: repo.GetName()})
	if err != nil {
		t.Fatalf("DeleteRepository: %v", err)
	}
	if err := dop.Wait(ctx); err != nil {
		t.Fatalf("DeleteRepository: %v", err)
	}
	if _, err := c.GetRepository(ctx, &ssmpb.GetRepositoryRequest{Name: repo.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("GetRepository after the delete: %v; want NotFound", err)
	}
	op, err := c.DeleteInstance(ctx, &ssmpb.DeleteInstanceRequest{Name: inst.GetName()})
	if err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	if err := op.Wait(ctx); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
}
