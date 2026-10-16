//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cloudbuild "cloud.google.com/go/cloudbuild/apiv1/v2"
	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"github.com/google/uuid"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// argSource is a source archive whose Dockerfile fails unless the build
// arg TOKEN is want.
func argSource(t *testing.T, want string) []byte {
	t.Helper()
	return sourceArchive(t, map[string][]byte{"Dockerfile": []byte("FROM " + stepImage() + "\nARG TOKEN\n" +
		"RUN test \"$TOKEN\" = \"" + want + "\" && echo token-ok > /ok\n")})
}

// waitBuildStatus polls a build until its status is want.
func waitBuildStatus(ctx context.Context, t *testing.T, cb *cloudbuild.Client, name string, want cbpb.Build_Status) *cbpb.Build {
	t.Helper()
	var b *cbpb.Build
	for deadline := time.Now().Add(4 * time.Minute); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		var err error
		if b, err = cb.GetBuild(ctx, &cbpb.GetBuildRequest{Name: name}); err == nil && b.GetStatus() == want {
			return b
		}
	}
	t.Fatalf("build %s = %v; want %v", name, b.GetStatus(), want)
	return nil
}

// covers: google.devtools.cloudbuild.v1.CloudBuild/CreateBuild, google.devtools.cloudbuild.v1.CloudBuild/GetBuild
//
// TestCloudBuildDynamicSubstitutionsAndSecretBuildArg (#1449), with the
// official client: options.dynamicSubstitutions resolves a user
// substitution naming $PROJECT_ID and the bash expansion
// ${_VER#release-} in an image tag; a docker build step's secretEnv gives
// --build-arg TOKEN the Secret Manager value, which the Dockerfile checks,
// and the value is not in the step's args.
func TestCloudBuildDynamicSubstitutionsAndSecretBuildArg(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required: builds run with the host's Docker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	t.Cleanup(cancel)
	cb := cloudBuildClient(ctx, t, h)
	value := "tok-" + uuid.NewString()[:8]
	secret := buildSecret(ctx, t, h, h.Project(), value)
	bucket := uploadSource(ctx, t, h, "arg.tar.gz", argSource(t, value))
	op, err := cb.CreateBuild(ctx, &cbpb.CreateBuildRequest{Parent: runParent(h), ProjectId: h.Project(), Build: &cbpb.Build{
		Source:        &cbpb.Source{Source: &cbpb.Source_StorageSource{StorageSource: &cbpb.StorageSource{Bucket: bucket, Object: "arg.tar.gz"}}},
		Options:       &cbpb.BuildOptions{DynamicSubstitutions: true},
		Substitutions: map[string]string{"_REPO": "us-central1-docker.pkg.dev/${PROJECT_ID}/dyn", "_VER": "release-2.5"},
		AvailableSecrets: &cbpb.Secrets{SecretManager: []*cbpb.SecretManagerSecret{
			{VersionName: "projects/" + h.Project() + "/secrets/" + secret + "/versions/1", Env: "TOKEN"}}},
		Steps: []*cbpb.BuildStep{{Name: "gcr.io/cloud-builders/docker", SecretEnv: []string{"TOKEN"},
			Args: []string{"build", "-t", "${_REPO}/app:${_VER#release-}", "--build-arg", "TOKEN", "."}}},
	}})
	if err != nil {
		t.Fatalf("CreateBuild: %v", err)
	}
	b, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("the build failed: %v", err)
	}
	want := "us-central1-docker.pkg.dev/" + h.Project() + "/dyn/app:2.5"
	args := strings.Join(b.GetSteps()[0].GetArgs(), " ")
	if b.GetStatus() != cbpb.Build_SUCCESS || !strings.Contains(args, "-t "+want) || strings.Contains(args, value) {
		t.Errorf("build = %v, args %q", b.GetStatus(), args)
	}
}

// triggerREST sends a Cloud Build trigger JSON request to the Run port, as
// gcloud and Terraform do: this release of the Go client's protos has no
// approvalConfig, so only the JSON API carries it.
func triggerREST(t *testing.T, h *Harness, method, path string, body any) map[string]any {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, "http://"+h.Endpoint(EnvRun)+"/v1/"+path, rd)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s %s = %d %s", method, path, resp.StatusCode, raw)
	}
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return out
}

// covers: google.devtools.cloudbuild.v1.CloudBuild/ApproveBuild, google.devtools.cloudbuild.v1.CloudBuild/RunBuildTrigger, google.devtools.cloudbuild.v1.CloudBuild/CancelBuild, google.devtools.cloudbuild.v1.CloudBuild/GetBuild, google.devtools.cloudbuild.v1.CloudBuild/GetBuildTrigger
//
// TestCloudBuildApproveBuild (#1449): a trigger made through the JSON API
// with approvalConfig.approvalRequired reads back with it; each build the
// official client's RunBuildTrigger starts is PENDING with approval
// PENDING and does not run. ApproveBuild APPROVED runs one to SUCCESS
// with approval APPROVED and the comment; approving it again is
// FAILED_PRECONDITION; REJECTED ends another CANCELLED with approval
// REJECTED; CancelBuild ends a third CANCELLED with approval CANCELLED.
func TestCloudBuildApproveBuild(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required: builds run with the host's Docker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	t.Cleanup(cancel)
	cb := cloudBuildClient(ctx, t, h)
	bucket := uploadSource(ctx, t, h, "approve.tar.gz", argSource(t, ""))
	parent := runParent(h)
	tr := triggerREST(t, h, http.MethodPost, parent+"/triggers", map[string]any{
		"name": "gated-" + uuid.NewString()[:8], "approvalConfig": map[string]any{"approvalRequired": true},
		"build": map[string]any{
			"source": map[string]any{"storageSource": map[string]any{"bucket": bucket, "object": "approve.tar.gz"}},
			"steps":  []any{map[string]any{"name": "gcr.io/cloud-builders/docker", "args": []string{"build", "-t", "gcr.io/" + h.Project() + "/gated", "."}}},
		}})
	name, _ := tr["resourceName"].(string)
	t.Cleanup(func() { _ = cb.DeleteBuildTrigger(context.Background(), &cbpb.DeleteBuildTriggerRequest{Name: name}) })
	if ac, _ := tr["approvalConfig"].(map[string]any); ac["approvalRequired"] != true {
		t.Errorf("the created trigger = %v", tr)
	}
	if got := triggerREST(t, h, http.MethodGet, name, nil); fmt.Sprint(got["approvalConfig"]) != "map[approvalRequired:true]" {
		t.Errorf("GET trigger approvalConfig = %v", got["approvalConfig"])
	}
	if _, err := cb.GetBuildTrigger(ctx, &cbpb.GetBuildTriggerRequest{Name: name}); err != nil {
		t.Errorf("GetBuildTrigger: %v", err)
	}
	run := func() (*cloudbuild.RunBuildTriggerOperation, *cbpb.Build) {
		t.Helper()
		op, err := cb.RunBuildTrigger(ctx, &cbpb.RunBuildTriggerRequest{Name: name})
		if err != nil {
			t.Fatalf("RunBuildTrigger: %v", err)
		}
		md, err := op.Metadata()
		if err != nil {
			t.Fatal(err)
		}
		b := md.GetBuild()
		if b.GetStatus() != cbpb.Build_PENDING || b.GetApproval().GetState() != cbpb.BuildApproval_PENDING ||
			!b.GetApproval().GetConfig().GetApprovalRequired() {
			t.Fatalf("the triggered build = %v %v", b.GetStatus(), b.GetApproval())
		}
		return op, b
	}

	_, pending := run()
	time.Sleep(time.Second)
	if g, _ := cb.GetBuild(ctx, &cbpb.GetBuildRequest{Name: pending.GetName()}); g.GetStatus() != cbpb.Build_PENDING || g.GetStartTime() != nil {
		t.Errorf("a pending build ran: %v", g.GetStatus())
	}
	aop, err := cb.ApproveBuild(ctx, &cbpb.ApproveBuildRequest{Name: pending.GetName(),
		ApprovalResult: &cbpb.ApprovalResult{Decision: cbpb.ApprovalResult_APPROVED, Comment: "ship it"}})
	if err != nil {
		t.Fatalf("ApproveBuild: %v", err)
	}
	if ab, err := aop.Wait(ctx); err != nil || ab.GetApproval().GetState() != cbpb.BuildApproval_APPROVED {
		t.Errorf("ApproveBuild.Wait = %v, %v", ab.GetApproval(), err)
	}
	done := waitBuildStatus(ctx, t, cb, pending.GetName(), cbpb.Build_SUCCESS)
	if done.GetApproval().GetResult().GetComment() != "ship it" || done.GetApproval().GetResult().GetApprovalTime() == nil {
		t.Errorf("the approved build's approval = %v", done.GetApproval())
	}
	if _, err := cb.ApproveBuild(ctx, &cbpb.ApproveBuildRequest{Name: pending.GetName(),
		ApprovalResult: &cbpb.ApprovalResult{Decision: cbpb.ApprovalResult_APPROVED}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("approving a finished build = %v; want FAILED_PRECONDITION", err)
	}

	rop, rejected := run()
	if _, err := cb.ApproveBuild(ctx, &cbpb.ApproveBuildRequest{Name: rejected.GetName(),
		ApprovalResult: &cbpb.ApprovalResult{Decision: cbpb.ApprovalResult_REJECTED, Comment: "no"}}); err != nil {
		t.Fatalf("ApproveBuild(REJECTED): %v", err)
	}
	if g := waitBuildStatus(ctx, t, cb, rejected.GetName(), cbpb.Build_CANCELLED); g.GetApproval().GetState() != cbpb.BuildApproval_REJECTED {
		t.Errorf("the rejected build's approval = %v", g.GetApproval())
	}
	if _, err := rop.Wait(ctx); status.Code(err) != codes.Canceled {
		t.Errorf("the rejected build's operation = %v; want CANCELLED", err)
	}

	_, third := run()
	if g, err := cb.CancelBuild(ctx, &cbpb.CancelBuildRequest{Name: third.GetName()}); err != nil ||
		g.GetStatus() != cbpb.Build_CANCELLED || g.GetApproval().GetState() != cbpb.BuildApproval_CANCELLED {
		t.Errorf("CancelBuild of a pending build = %v, %v", g, err)
	}
}

// pendingBuildOf waits for trigger id's newest build to be PENDING.
func pendingBuildOf(ctx context.Context, t *testing.T, cb *cloudbuild.Client, parent, id string, n int) *cbpb.Build {
	t.Helper()
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		var builds []*cbpb.Build
		it := cb.ListBuilds(ctx, &cbpb.ListBuildsRequest{Parent: parent, ProjectId: strings.Split(parent, "/")[1],
			Filter: fmt.Sprintf("trigger_id=%q", id)})
		for {
			b, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatalf("ListBuilds: %v", err)
			}
			builds = append(builds, b)
		}
		if len(builds) >= n && builds[0].GetStatus() == cbpb.Build_PENDING {
			return builds[0]
		}
	}
	t.Fatalf("trigger %s has no pending build", id)
	return nil
}

// covers: google.devtools.cloudbuild.v1.CloudBuild/ApproveBuild, google.devtools.cloudbuild.v1.CloudBuild/CreateBuildTrigger, google.devtools.cloudbuild.v1.CloudBuild/RunBuildTrigger
//
// TestGcloudBuildsApproveAndReject (#1449): `gcloud builds triggers create
// manual --require-approval`, `gcloud builds triggers run`, `gcloud alpha builds
// approve` and `gcloud beta builds reject`, through the configuration
// `cloudburrow gcloud-setup` writes, egress blocked: the trigger describes
// with approvalConfig, an approved build runs to SUCCESS, and a rejected
// one is CANCELLED with approval REJECTED.
func TestGcloudBuildsApproveAndReject(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvRun)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required: builds run with the host's Docker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	t.Cleanup(cancel)
	g := newGcloudSession(t, h)
	cb := cloudBuildClient(ctx, t, h)
	parent := "projects/" + g.project + "/locations/us-central1"
	inline := filepath.Join(t.TempDir(), "build.yaml")
	if err := os.WriteFile(inline, []byte("steps:\n- name: "+stepImage()+"\n  entrypoint: sh\n  args: ['-c', 'echo approved']\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	name := "gappr-" + uuid.NewString()[:8]
	g.must("builds", "triggers", "create", "manual", "--name="+name, "--region=us-central1", "--inline-config="+inline, "--require-approval")
	t.Cleanup(func() { _, _ = g.run(nil, "builds", "triggers", "delete", name, "--region=us-central1", "--quiet") })
	if out := g.must("builds", "triggers", "describe", name, "--region=us-central1", "--format=value(approvalConfig.approvalRequired)"); lastLine(out) != "True" {
		t.Errorf("the trigger's approvalConfig.approvalRequired = %q", out)
	}
	id := lastLine(g.must("builds", "triggers", "describe", name, "--region=us-central1", "--format=value(id)"))

	if out, err := g.run(nil, "builds", "triggers", "run", name, "--region=us-central1"); err != nil {
		t.Fatalf("gcloud builds triggers run: %v\n%s", err, out)
	}
	b := pendingBuildOf(ctx, t, cb, parent, id, 1)
	// approve takes --location only in gcloud's alpha track.
	out, err := g.run(nil, "alpha", "builds", "approve", b.GetId(), "--location=us-central1", "--comment=from gcloud")
	if err != nil {
		t.Fatalf("gcloud builds approve: %v\n%s", err, out)
	}
	done := waitBuildStatus(ctx, t, cb, b.GetName(), cbpb.Build_SUCCESS)
	if done.GetApproval().GetState() != cbpb.BuildApproval_APPROVED || done.GetApproval().GetResult().GetComment() != "from gcloud" {
		t.Errorf("the approved build's approval = %v", done.GetApproval())
	}

	// reject takes no --location in any track: a global trigger's build.
	global := name + "-g"
	g.must("builds", "triggers", "create", "manual", "--name="+global, "--inline-config="+inline, "--require-approval")
	t.Cleanup(func() { _, _ = g.run(nil, "builds", "triggers", "delete", global, "--quiet") })
	gid := lastLine(g.must("builds", "triggers", "describe", global, "--format=value(id)"))
	if out, err := g.run(nil, "builds", "triggers", "run", global); err != nil {
		t.Fatalf("gcloud builds triggers run: %v\n%s", err, out)
	}
	b = pendingBuildOf(ctx, t, cb, "projects/"+g.project+"/locations/global", gid, 1)
	if out, err := g.run(nil, "beta", "builds", "reject", b.GetId(), "--comment=not now"); err != nil {
		t.Fatalf("gcloud builds reject: %v\n%s", err, out)
	}
	if r := waitBuildStatus(ctx, t, cb, b.GetName(), cbpb.Build_CANCELLED); r.GetApproval().GetState() != cbpb.BuildApproval_REJECTED {
		t.Errorf("the rejected build's approval = %v", r.GetApproval())
	}
}

// covers: google.devtools.cloudbuild.v1.CloudBuild/CreateBuildTrigger, google.devtools.cloudbuild.v1.CloudBuild/GetBuildTrigger, google.devtools.cloudbuild.v1.CloudBuild/UpdateBuildTrigger, google.devtools.cloudbuild.v1.CloudBuild/DeleteBuildTrigger, google.devtools.cloudbuild.v1.CloudBuild/RunBuildTrigger
//
// TestTerraformCloudBuildTriggerApproval (#1449): a webhook
// google_cloudbuild_trigger with approval_config { approval_required =
// true } applies through `cloudburrow terraform` with egress blocked; the
// JSON API reads it back with approvalConfig; the plan is clean; a run is
// PENDING; turning approval off changes it in place; the plan is clean
// again.
func TestTerraformCloudBuildTriggerApproval(t *testing.T) {
	h := New(t)
	m := newTFModule(t)
	_ = runClient(t, h) // skips without CLOUDBURROW_TEST_RUN
	ctx := h.Context()
	cb := cloudBuildClient(ctx, t, h)
	secret := buildSecret(ctx, t, h, h.Project(), "tf-approval-secret")
	suffix := strings.TrimPrefix(h.Project(), "cb-test-")
	tname := "tf-appr-" + suffix
	module := func(required bool) string {
		return fmt.Sprintf(`
resource "google_cloudbuild_trigger" "t" {
  project  = %[1]q
  location = "us-central1"
  name     = %[2]q
  webhook_config {
    secret = "projects/%[1]s/secrets/%[3]s/versions/1"
  }
  approval_config {
    approval_required = %[4]t
  }
  build {
    step {
      name       = "alpine:3.20"
      entrypoint = "sh"
      args       = ["-c", "echo gated"]
    }
  }
}
`, h.Project(), tname, secret, required)
	}
	find := func() map[string]any {
		t.Helper()
		list := triggerREST(t, h, http.MethodGet, runParent(h)+"/triggers", nil)
		ts, _ := list["triggers"].([]any)
		for _, x := range ts {
			if tr, _ := x.(map[string]any); tr["name"] == tname {
				return tr
			}
		}
		t.Fatalf("no trigger %s in %v", tname, list)
		return nil
	}
	m.write(module(true))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	t.Cleanup(func() { _, _ = m.tf("destroy", "-auto-approve", "-input=false", "-no-color") })
	tr := find()
	if fmt.Sprint(tr["approvalConfig"]) != "map[approvalRequired:true]" {
		t.Errorf("the trigger's approvalConfig = %v", tr["approvalConfig"])
	}
	m.planClean()
	op, err := cb.RunBuildTrigger(ctx, &cbpb.RunBuildTriggerRequest{Name: tr["resourceName"].(string)})
	if err != nil {
		t.Fatalf("RunBuildTrigger: %v", err)
	}
	if md, err := op.Metadata(); err != nil || md.GetBuild().GetStatus() != cbpb.Build_PENDING {
		t.Errorf("the run = %v, %v; want PENDING", md.GetBuild().GetStatus(), err)
	} else {
		_, _ = cb.CancelBuild(ctx, &cbpb.CancelBuildRequest{Name: md.GetBuild().GetName()})
	}

	m.write(module(false))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("the change was not applied in place:\n%s", lastLines(out, 20))
	}
	if ac := fmt.Sprint(find()["approvalConfig"]); strings.Contains(ac, "true") {
		t.Errorf("after the change, approvalConfig = %v", ac)
	}
	m.planClean()
}
