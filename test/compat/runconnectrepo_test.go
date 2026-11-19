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
	developerconnect "cloud.google.com/go/developerconnect/apiv1"
	dcpb "cloud.google.com/go/developerconnect/apiv1/developerconnectpb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	securesourcemanager "cloud.google.com/go/securesourcemanager/apiv1"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"github.com/google/uuid"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// covers: google.devtools.cloudbuild.v1.CloudBuild/CreateBuildTrigger, google.devtools.cloudbuild.v1.CloudBuild/UpdateBuildTrigger, google.devtools.cloudbuild.v1.CloudBuild/RunBuildTrigger
//
// TestCloudRunContinuousDeployFromRepository (#1852): Cloud Run's Connect
// repo, as Google's "Continuous deployment from git using Cloud Build"
// sets it up, with the official clients over gRPC on the Run port. A
// Secure Source Manager repository linked through Developer Connect gets
// the Cloud Build trigger Google's console makes (a push to ^main$ runs
// Build, Push, and Deploy, gcloud run services update with the image and
// the commit's labels). A push to main builds the commit and deploys it as
// a new revision of the service, serving the commit's image and labelled
// with its SHA; a push to another branch builds nothing; RunBuildTrigger
// of the branch builds its head again.
func TestCloudRunContinuousDeployFromRepository(t *testing.T) {
	h := New(t)
	rc := runClient(t, h) // skips without CLOUDBURROW_TEST_RUN
	for _, tool := range []string{"docker", "git", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is required", tool)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	t.Cleanup(cancel)
	ssm, err := securesourcemanager.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ssm.Close() })
	dc, err := developerconnect.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dc.Close() })
	cb := cloudBuildClient(ctx, t, h)
	parent := runParent(h)
	project := h.Project()
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]

	iop, err := ssm.CreateInstance(ctx, &ssmpb.CreateInstanceRequest{Parent: parent, InstanceId: "cd-" + suffix})
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
	rop, err := ssm.CreateRepository(ctx, &ssmpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "app-" + suffix,
		Repository: &ssmpb.Repository{Instance: inst.GetName()}})
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	repo, err := rop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	cop, err := dc.CreateConnection(ctx, &dcpb.CreateConnectionRequest{Parent: parent, ConnectionId: "cd-" + suffix,
		Connection: &dcpb.Connection{ConnectionConfig: &dcpb.Connection_SecureSourceManagerInstanceConfig{
			SecureSourceManagerInstanceConfig: &dcpb.SecureSourceManagerInstanceConfig{Instance: inst.GetName()}}}})
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	conn, err := cop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	lop, err := dc.CreateGitRepositoryLink(ctx, &dcpb.CreateGitRepositoryLinkRequest{Parent: conn.GetName(), GitRepositoryLinkId: "app",
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

	// The service, before its first build: the console makes it with
	// Google's placeholder; any image does.
	service := "cd-" + suffix
	sop, err := rc.CreateService(ctx, &runpb.CreateServiceRequest{Parent: parent, ServiceId: service, Service: &runpb.Service{
		Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{Image: helloDigest}}}}})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	t.Cleanup(func() {
		if op, err := rc.DeleteService(context.Background(), &runpb.DeleteServiceRequest{Name: parent + "/services/" + service}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	first, err := sop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}

	image := "$_AR_HOSTNAME/$_AR_PROJECT_ID/$_AR_REPOSITORY/$REPO_NAME/$_SERVICE_NAME:$COMMIT_SHA"
	trig, err := cb.CreateBuildTrigger(ctx, &cbpb.CreateBuildTriggerRequest{Parent: parent, ProjectId: project, Trigger: &cbpb.BuildTrigger{
		Name: service + "-trigger",
		RepositoryEventConfig: &cbpb.RepositoryEventConfig{Repository: link.GetName(),
			Filter: &cbpb.RepositoryEventConfig_Push{Push: &cbpb.PushFilter{GitRef: &cbpb.PushFilter_Branch{Branch: "^main$"}}}},
		BuildTemplate: &cbpb.BuildTrigger_Build{Build: &cbpb.Build{
			Steps: []*cbpb.BuildStep{
				{Id: "Build", Name: "gcr.io/cloud-builders/docker", Args: []string{"build", "--no-cache", "-t", image, ".", "-f", "Dockerfile"}},
				{Id: "Push", Name: "gcr.io/cloud-builders/docker", Args: []string{"push", image}},
				{Id: "Deploy", Name: "gcr.io/google.com/cloudsdktool/cloud-sdk:slim", Entrypoint: "gcloud", Args: []string{
					"run", "services", "update", "$_SERVICE_NAME", "--platform=managed", "--image=" + image,
					"--labels=managed-by=gcp-cloud-build-deploy-cloud-run,commit-sha=$COMMIT_SHA,gcb-build-id=$BUILD_ID,gcb-trigger-id=$_TRIGGER_ID",
					"--region=$_DEPLOY_REGION", "--quiet"}},
			},
			Images:  []string{image},
			Options: &cbpb.BuildOptions{SubstitutionOption: cbpb.BuildOptions_ALLOW_LOOSE},
		}},
		Substitutions: map[string]string{"_AR_HOSTNAME": "us-central1-docker.pkg.dev", "_AR_PROJECT_ID": project,
			"_AR_REPOSITORY": "cloud-run-source-deploy", "_PLATFORM": "managed", "_SERVICE_NAME": service, "_DEPLOY_REGION": "us-central1"},
	}})
	if err != nil {
		t.Fatalf("CreateBuildTrigger: %v", err)
	}
	t.Cleanup(func() {
		_ = cb.DeleteBuildTrigger(context.Background(), &cbpb.DeleteBuildTriggerRequest{ProjectId: project, TriggerId: trig.GetId(),
			Name: trig.GetResourceName()})
	})
	trig.Substitutions["_TRIGGER_ID"] = trig.GetId()
	if trig, err = cb.UpdateBuildTrigger(ctx, &cbpb.UpdateBuildTriggerRequest{ProjectId: project, TriggerId: trig.GetId(), Trigger: trig,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"substitutions"}}}); err != nil {
		t.Fatalf("UpdateBuildTrigger: %v", err)
	}
	if trig.GetRepositoryEventConfig().GetPush().GetBranch() != "^main$" {
		t.Errorf("the trigger's push filter = %v", trig.GetRepositoryEventConfig())
	}

	// The commit: the sourcebuild fixture's server and a Dockerfile.
	work := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "git", append([]string{"-C", work, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("clone", "--quiet", repo.GetUris().GetGitHttps(), ".")
	src := sourceBuildDir(t)
	for _, f := range []string{"server", "Dockerfile"} {
		b, err := os.ReadFile(filepath.Join(src, f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, f), b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	git("checkout", "--quiet", "-b", "main")
	git("add", ".")
	git("commit", "--quiet", "-m", "first")
	git("push", "--quiet", "origin", "main")
	sha := git("rev-parse", "HEAD")

	buildsOf := func(rev string) []*cbpb.Build {
		var out []*cbpb.Build
		it := cb.ListBuilds(ctx, &cbpb.ListBuildsRequest{Parent: parent, ProjectId: project})
		for {
			b, err := it.Next()
			if err == iterator.Done {
				return out
			}
			if err != nil {
				t.Fatalf("ListBuilds: %v", err)
			}
			if b.GetSource().GetGitSource().GetRevision() == rev && b.GetBuildTriggerId() == trig.GetId() {
				out = append(out, b)
			}
		}
	}
	waitBuilt := func(rev string, n int) []*cbpb.Build {
		t.Helper()
		var builds []*cbpb.Build
		for deadline := time.Now().Add(8 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
			builds = buildsOf(rev)
			done := 0
			for _, b := range builds {
				if b.GetFinishTime() != nil {
					done++
				}
			}
			if done >= n {
				return builds
			}
		}
		t.Fatalf("the builds of %s = %v; want %d finished", rev, builds, n)
		return nil
	}
	builds := waitBuilt(sha, 1)
	if len(builds) != 1 || builds[0].GetStatus() != cbpb.Build_SUCCESS {
		t.Fatalf("the push's builds = %v; want one SUCCESS\n%s", builds, builds[0].GetStatusDetail())
	}
	want := "us-central1-docker.pkg.dev/" + project + "/cloud-run-source-deploy/app-" + suffix + "/" + service + ":" + sha
	got, err := rc.GetService(ctx, &runpb.GetServiceRequest{Name: parent + "/services/" + service})
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if img := got.GetTemplate().GetContainers()[0].GetImage(); img != want {
		t.Errorf("the service's image = %q; want %q", img, want)
	}
	if got.GetLatestReadyRevision() == first.GetLatestReadyRevision() || got.GetLatestReadyRevision() == "" {
		t.Errorf("the latest ready revision = %q; want a new one after %q", got.GetLatestReadyRevision(), first.GetLatestReadyRevision())
	}
	if l := got.GetLabels(); l["commit-sha"] != sha || l["gcb-trigger-id"] != trig.GetId() || l["managed-by"] != "gcp-cloud-build-deploy-cloud-run" {
		t.Errorf("the service's labels = %v; want the commit, the trigger and managed-by", l)
	}

	// Another branch builds nothing.
	git("checkout", "--quiet", "-b", "dev")
	git("commit", "--quiet", "--allow-empty", "-m", "dev")
	git("push", "--quiet", "origin", "dev")
	dev := git("rev-parse", "HEAD")
	time.Sleep(5 * time.Second)
	if b := buildsOf(dev); len(b) != 0 {
		t.Errorf("a push to dev ran %d builds; want none", len(b))
	}

	// RunBuildTrigger builds the branch's head again.
	if _, err := cb.RunBuildTrigger(ctx, &cbpb.RunBuildTriggerRequest{Name: trig.GetResourceName(), ProjectId: project, TriggerId: trig.GetId(),
		Source: &cbpb.RepoSource{Revision: &cbpb.RepoSource_BranchName{BranchName: "main"}}}); err != nil {
		t.Fatalf("RunBuildTrigger: %v", err)
	}
	if builds := waitBuilt(sha, 2); builds[0].GetStatus() != cbpb.Build_SUCCESS || builds[1].GetStatus() != cbpb.Build_SUCCESS {
		t.Errorf("after RunBuildTrigger, the builds of main = %v; want two SUCCESS", builds)
	}
}
