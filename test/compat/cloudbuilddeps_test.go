//go:build compat

package compat

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// covers: google.devtools.cloudbuild.v1.CloudBuild/CreateBuild, google.devtools.cloudbuild.v1.CloudBuild/GetBuild
//
// TestCloudBuildGitDependencies (#2074), with the official cloudbuild,
// securesourcemanager and developerconnect clients over gRPC on the Run
// port, on an instance started with --cloudbuild-host-steps, as Google's
// "Manage build dependencies" page documents Build.dependencies
// (https://docs.cloud.google.com/build/docs/building/manage-dependencies,
// read 2026-11-20):
//   - a gitSource on a Secure Source Manager repository, by its git URL, at
//     a branch with the default depth 1, and the same repository through a
//     Developer Connect link at its first commit's tag with depth -1 and
//     fetchTags, are cloned before the step runs into
//     /workspace/<destPath>, where the step reads each file and the .git
//     directory: the first shallow, the second with the whole history;
//   - GetBuild records both dependencies as they were asked for, which
//     the console's Build dependencies tab lists;
//   - a dependency on a host this instance cannot reach is refused
//     FAILED_PRECONDITION with Google's requirement, before anything
//     dials it (the host is under .invalid, which never resolves);
//   - a destPath outside the working directory is INVALID_ARGUMENT.
func TestCloudBuildGitDependencies(t *testing.T) {
	h := New(t)
	_ = runClient(t, h) // skips without CLOUDBURROW_TEST_RUN
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	t.Cleanup(cancel)
	parent := runParent(h)
	cb := cloudBuildClient(ctx, t, h)

	dep := func(repo *cbpb.Dependency_GitSourceRepository, revision, dest string, depth int64, tags bool) *cbpb.Dependency {
		return &cbpb.Dependency{Dep: &cbpb.Dependency_GitSource{GitSource: &cbpb.Dependency_GitSourceDependency{
			Repository: repo, Revision: revision, DestPath: dest, Depth: depth, FetchTags: tags}}}
	}
	byURL := func(u string) *cbpb.Dependency_GitSourceRepository {
		return &cbpb.Dependency_GitSourceRepository{Repotype: &cbpb.Dependency_GitSourceRepository_Url{Url: u}}
	}
	// Refused before any repository exists: nothing is dialed.
	step := []*cbpb.BuildStep{{Name: stepImage(), Entrypoint: "sh", Args: []string{"-c", "true"}}}
	_, err := cb.CreateBuild(ctx, &cbpb.CreateBuildRequest{Parent: parent, ProjectId: h.Project(), Build: &cbpb.Build{Steps: step,
		Dependencies: []*cbpb.Dependency{dep(byURL("https://git.cloudburrow-test.invalid/team/lib.git"), "main", "lib", 0, false)}}})
	skipWithoutHostSteps(t, err)
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "cannot be reached from this instance") ||
		!strings.Contains(err.Error(), "either a public repository or linked to Cloud Build using Developer Connect") {
		t.Errorf("CreateBuild with a dependency on another host: %v; want FAILED_PRECONDITION with Google's requirement", err)
	}

	l := newSSMLink(ctx, t, h, parent, "cbd")
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	l.git("clone", "--quiet", l.repo.GetUris().GetGitHttps(), ".")
	l.git("checkout", "--quiet", "-b", "main")
	l.commit("first", map[string]string{"VERSION": "v1-" + suffix + "\n"})
	l.git("tag", "v1")
	l.commit("second", map[string]string{"VERSION": "v2-" + suffix + "\n", "lib/util.sh": "echo util\n"})
	l.git("push", "--quiet", "origin", "main", "v1")

	// A second repository on the instance, a submodule of the first at
	// vendor/sub on a branch of its own: recurseSubmodules clones it too.
	ssm := ssmClient(t, h)
	sop, err := ssm.CreateRepository(ctx, &ssmpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "cbd-sub-" + suffix,
		Repository: &ssmpb.Repository{Instance: l.repo.GetInstance()}})
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	sub, err := sop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	t.Cleanup(func() {
		if op, err := ssm.DeleteRepository(context.Background(), &ssmpb.DeleteRepositoryRequest{Name: sub.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	subWork := t.TempDir()
	subGit := func(args ...string) {
		t.Helper()
		if out, err := l.gitIn(subWork, nil, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	subGit("clone", "--quiet", sub.GetUris().GetGitHttps(), ".")
	subGit("checkout", "--quiet", "-b", "main")
	if err := os.WriteFile(filepath.Join(subWork, "SUB"), []byte("sub-"+suffix+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	subGit("add", "SUB")
	subGit("commit", "--quiet", "-m", "sub")
	subGit("push", "--quiet", "origin", "main")
	l.git("checkout", "--quiet", "-b", "with-sub")
	l.git("submodule", "--quiet", "add", sub.GetUris().GetGitHttps(), "vendor/sub")
	l.git("commit", "--quiet", "-m", "add the submodule")
	l.git("push", "--quiet", "origin", "with-sub")

	if _, err := cb.CreateBuild(ctx, &cbpb.CreateBuildRequest{Parent: parent, ProjectId: h.Project(), Build: &cbpb.Build{Steps: step,
		Dependencies: []*cbpb.Dependency{dep(byURL(l.repo.GetUris().GetGitHttps()), "main", "../up", 0, false)}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateBuild with destPath ../up: %v; want INVALID_ARGUMENT", err)
	}

	script := "set -e\n" +
		"test \"$(cat /workspace/deps/lib/VERSION)\" = v2-" + suffix + "\n" +
		"test -f /workspace/deps/lib/lib/util.sh\n" +
		"test -f /workspace/deps/lib/.git/shallow\n" +
		"test \"$(cat /workspace/third_party/old/VERSION)\" = v1-" + suffix + "\n" +
		"test ! -e /workspace/third_party/old/lib\n" +
		"test ! -f /workspace/third_party/old/.git/shallow\n" +
		"test \"$(cat /workspace/withsub/vendor/sub/SUB)\" = sub-" + suffix + "\n" +
		"test ! -e /workspace/nosub/vendor/sub/SUB\n" +
		"echo read $(cat /workspace/deps/lib/VERSION) and $(cat /workspace/third_party/old/VERSION)\n"
	deps := []*cbpb.Dependency{
		dep(byURL(l.repo.GetUris().GetGitHttps()), "main", "deps/lib", 0, false),
		dep(&cbpb.Dependency_GitSourceRepository{Repotype: &cbpb.Dependency_GitSourceRepository_DeveloperConnect{
			DeveloperConnect: l.link.GetName()}}, "v1", "third_party/old", -1, true),
		dep(byURL(l.repo.GetUris().GetGitHttps()), "with-sub", "withsub", 0, false),
		dep(byURL(l.repo.GetUris().GetGitHttps()), "with-sub", "nosub", 0, false),
	}
	deps[2].GetGitSource().RecurseSubmodules = true
	op, err := cb.CreateBuild(ctx, &cbpb.CreateBuildRequest{Parent: parent, ProjectId: h.Project(), Build: &cbpb.Build{
		Steps: []*cbpb.BuildStep{{Name: stepImage(), Script: script}}, Dependencies: deps}})
	skipWithoutHostSteps(t, err)
	if err != nil {
		t.Fatalf("CreateBuild with git dependencies: %v", err)
	}
	b, err := op.Wait(ctx)
	if err != nil || b.GetStatus() != cbpb.Build_SUCCESS {
		t.Fatalf("the build with git dependencies = %v, %v; want SUCCESS, its step reading each destPath", b, err)
	}
	got, err := cb.GetBuild(ctx, &cbpb.GetBuildRequest{ProjectId: h.Project(), Name: b.GetName(), Id: b.GetId()})
	if err != nil || len(got.GetDependencies()) != 4 {
		t.Fatalf("GetBuild = %v, %v; want its four dependencies", got, err)
	}
	first, second := got.GetDependencies()[0].GetGitSource(), got.GetDependencies()[1].GetGitSource()
	if first.GetRepository().GetUrl() != l.repo.GetUris().GetGitHttps() || first.GetRevision() != "main" || first.GetDestPath() != "deps/lib" ||
		second.GetRepository().GetDeveloperConnect() != l.link.GetName() || second.GetRevision() != "v1" || second.GetDepth() != -1 || !second.GetFetchTags() ||
		!got.GetDependencies()[2].GetGitSource().GetRecurseSubmodules() {
		t.Errorf("GetBuild's dependencies = %v", got.GetDependencies())
	}

	// A revision the repository lacks fails the build at FETCHSOURCE.
	op, err = cb.CreateBuild(ctx, &cbpb.CreateBuildRequest{Parent: parent, ProjectId: h.Project(), Build: &cbpb.Build{Steps: step,
		Dependencies: []*cbpb.Dependency{dep(byURL(l.repo.GetUris().GetGitHttps()), "no-such-branch", "x", 0, false)}}})
	if err != nil {
		t.Fatalf("CreateBuild at a missing revision: %v", err)
	}
	if _, err := op.Wait(ctx); err == nil {
		t.Error("the build at a missing revision succeeded; want FAILURE")
	}
	if b, err := cb.GetBuild(ctx, &cbpb.GetBuildRequest{ProjectId: h.Project(), Name: strings.Replace(op.Name(), "/operations/", "/builds/", 1)}); err != nil ||
		b.GetStatus() != cbpb.Build_FAILURE || !strings.Contains(b.GetStatusDetail(), `revision "no-such-branch" not found`) {
		t.Errorf("the build at a missing revision reads %v: %q, %v", b.GetStatus(), b.GetStatusDetail(), err)
	}
}

// covers: google.devtools.cloudbuild.v1.CloudBuild/CreateWorkerPool, google.devtools.cloudbuild.v1.CloudBuild/UpdateWorkerPool
//
// TestCloudBuildWorkerPoolMachineTypes (#2073), with the official
// cloudbuild client over gRPC on the Run port, as Google's private pool
// config file schema lists machineType
// (https://docs.cloud.google.com/build/docs/private-pools/private-pool-config-file-schema,
// read 2026-11-20): its e2, n2d and c3 machine types, by its table of the
// regions offering each: c3-highcpu-8 in us-central1 is created, and in
// europe-west2 (c3-standard and c3-highmem only) is INVALID_ARGUMENT, as
// is n2d-standard-2 in europe-north2 and n1-standard-1, which the schema
// does not list, anywhere; UpdateWorkerPool checks the same.
func TestCloudBuildWorkerPoolMachineTypes(t *testing.T) {
	h := New(t)
	_ = runClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	cb := cloudBuildClient(ctx, t, h)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	pool := func(mt string) *cbpb.WorkerPool {
		return &cbpb.WorkerPool{Config: &cbpb.WorkerPool_PrivatePoolV1Config{PrivatePoolV1Config: &cbpb.PrivatePoolV1Config{
			WorkerConfig: &cbpb.PrivatePoolV1Config_WorkerConfig{MachineType: mt}}}}
	}
	parent := func(region string) string { return "projects/" + h.Project() + "/locations/" + region }
	op, err := cb.CreateWorkerPool(ctx, &cbpb.CreateWorkerPoolRequest{Parent: parent("us-central1"), WorkerPoolId: "mt-" + suffix, WorkerPool: pool("c3-highcpu-8")})
	if err != nil {
		t.Fatalf("CreateWorkerPool c3-highcpu-8 in us-central1: %v", err)
	}
	w, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateWorkerPool c3-highcpu-8 in us-central1: %v", err)
	}
	t.Cleanup(func() {
		if op, err := cb.DeleteWorkerPool(context.Background(), &cbpb.DeleteWorkerPoolRequest{Name: w.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	for _, tc := range []struct{ region, mt string }{{"europe-west2", "c3-highcpu-8"}, {"europe-north2", "n2d-standard-2"}, {"us-central1", "n1-standard-1"}} {
		_, err := cb.CreateWorkerPool(ctx, &cbpb.CreateWorkerPoolRequest{Parent: parent(tc.region), WorkerPoolId: "bad-" + suffix, WorkerPool: pool(tc.mt)})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("CreateWorkerPool %s in %s: %v; want INVALID_ARGUMENT", tc.mt, tc.region, err)
		}
	}
	mask := &fieldmaskpb.FieldMask{Paths: []string{"private_pool_v1_config.worker_config.machine_type"}}
	next := pool("n2d-highmem-4")
	next.Name = w.GetName()
	if op, err := cb.UpdateWorkerPool(ctx, &cbpb.UpdateWorkerPoolRequest{WorkerPool: next, UpdateMask: mask}); err != nil {
		t.Errorf("UpdateWorkerPool to n2d-highmem-4: %v", err)
	} else if got, err := op.Wait(ctx); err != nil || got.GetPrivatePoolV1Config().GetWorkerConfig().GetMachineType() != "n2d-highmem-4" {
		t.Errorf("UpdateWorkerPool to n2d-highmem-4 = %v, %v", got, err)
	}
	next = pool("e2-standard-3")
	next.Name = w.GetName()
	if _, err := cb.UpdateWorkerPool(ctx, &cbpb.UpdateWorkerPoolRequest{WorkerPool: next, UpdateMask: mask}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("UpdateWorkerPool to e2-standard-3: %v; want INVALID_ARGUMENT", err)
	}
}
