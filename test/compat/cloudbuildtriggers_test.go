//go:build compat

package compat

import (
	"context"
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
	"google.golang.org/genproto/googleapis/api/httpbody"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// triggeredBuild waits for the one build trigger id ran, by ListBuilds'
// trigger_id filter, to finish.
func triggeredBuild(ctx context.Context, t *testing.T, cb *cloudbuild.Client, parent, triggerID string, n int) *cbpb.Build {
	t.Helper()
	for deadline := time.Now().Add(4 * time.Minute); time.Now().Before(deadline); time.Sleep(time.Second) {
		var builds []*cbpb.Build
		it := cb.ListBuilds(ctx, &cbpb.ListBuildsRequest{Parent: parent, ProjectId: strings.Split(parent, "/")[1],
			Filter: fmt.Sprintf("trigger_id=%q", triggerID)})
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
		if len(builds) >= n && builds[0].GetFinishTime() != nil {
			return builds[0]
		}
	}
	t.Fatalf("trigger %s ran no finished build", triggerID)
	return nil
}

// covers: google.devtools.cloudbuild.v1.CloudBuild/CreateBuildTrigger, google.devtools.cloudbuild.v1.CloudBuild/GetBuildTrigger, google.devtools.cloudbuild.v1.CloudBuild/ListBuildTriggers, google.devtools.cloudbuild.v1.CloudBuild/UpdateBuildTrigger, google.devtools.cloudbuild.v1.CloudBuild/DeleteBuildTrigger, google.devtools.cloudbuild.v1.CloudBuild/RunBuildTrigger, google.devtools.cloudbuild.v1.CloudBuild/ReceiveTriggerWebhook, google.devtools.cloudbuild.v1.CloudBuild/CreateWorkerPool, google.devtools.cloudbuild.v1.CloudBuild/GetWorkerPool, google.devtools.cloudbuild.v1.CloudBuild/ListWorkerPools, google.devtools.cloudbuild.v1.CloudBuild/UpdateWorkerPool, google.devtools.cloudbuild.v1.CloudBuild/DeleteWorkerPool
//
// TestCloudBuildTriggersAndWorkerPools (#1422), with the official Cloud
// Build client over gRPC on the Run port, on an instance started with
// --services run,storage,secretmanager and --cloudbuild-host-steps:
//   - a worker pool is created (its operation waited on), read, listed,
//     its display name updated with a mask, and deleted;
//   - a manual trigger with an inline build on that pool is created, read
//     by ID and by name, listed, its description updated; RunBuildTrigger
//     runs it with a substitution override: the build is SUCCESS, carries
//     buildTriggerId and options.pool.name, and its step's args show
//     $TRIGGER_NAME and the overridden _GREETING; a disabled trigger is
//     FAILED_PRECONDITION;
//   - a webhook trigger checks its secret against Secret Manager: a wrong
//     secret is 403 over HTTP, the right one runs a build whose
//     $(body.message) binding is the payload's; ReceiveTriggerWebhook over
//     gRPC runs one too;
//   - GitHub, filename and repository triggers are UNIMPLEMENTED with the
//     reason (Pub/Sub triggers: TestCloudBuildPubsubTrigger); a build on a
//     missing pool is NOT_FOUND.
func TestCloudBuildTriggersAndWorkerPools(t *testing.T) {
	h := New(t)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is required: builds run with the host's Docker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	t.Cleanup(cancel)
	cb := cloudBuildClient(ctx, t, h)
	parent := runParent(h)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]

	// Worker pools.
	poolID := "pool-" + suffix
	pop, err := cb.CreateWorkerPool(ctx, &cbpb.CreateWorkerPoolRequest{Parent: parent, WorkerPoolId: poolID,
		WorkerPool: &cbpb.WorkerPool{DisplayName: "first", Config: &cbpb.WorkerPool_PrivatePoolV1Config{
			PrivatePoolV1Config: &cbpb.PrivatePoolV1Config{WorkerConfig: &cbpb.PrivatePoolV1Config_WorkerConfig{
				MachineType: "e2-standard-2", DiskSizeGb: 100}}}}})
	if err != nil {
		t.Fatalf("CreateWorkerPool: %v", err)
	}
	pool, err := pop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateWorkerPool.Wait: %v", err)
	}
	poolName := parent + "/workerPools/" + poolID
	if pool.GetName() != poolName || pool.GetState() != cbpb.WorkerPool_RUNNING || pool.GetUid() == "" ||
		pool.GetPrivatePoolV1Config().GetWorkerConfig().GetMachineType() != "e2-standard-2" {
		t.Errorf("the created pool = %v", pool)
	}
	t.Cleanup(func() {
		if op, err := cb.DeleteWorkerPool(context.Background(), &cbpb.DeleteWorkerPoolRequest{Name: poolName, AllowMissing: true}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	uop, err := cb.UpdateWorkerPool(ctx, &cbpb.UpdateWorkerPoolRequest{WorkerPool: &cbpb.WorkerPool{Name: poolName, DisplayName: "second"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}})
	if err != nil {
		t.Fatalf("UpdateWorkerPool: %v", err)
	}
	if p, err := uop.Wait(ctx); err != nil || p.GetDisplayName() != "second" || p.GetPrivatePoolV1Config().GetWorkerConfig().GetDiskSizeGb() != 100 {
		t.Errorf("UpdateWorkerPool = %v, %v", p, err)
	}
	if p, err := cb.GetWorkerPool(ctx, &cbpb.GetWorkerPoolRequest{Name: poolName}); err != nil || p.GetDisplayName() != "second" {
		t.Errorf("GetWorkerPool = %v, %v", p, err)
	}
	found := false
	for it := cb.ListWorkerPools(ctx, &cbpb.ListWorkerPoolsRequest{Parent: parent}); ; {
		p, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListWorkerPools: %v", err)
		}
		found = found || p.GetName() == poolName
	}
	if !found {
		t.Errorf("ListWorkerPools lacks %s", poolName)
	}

	// A manual trigger on the pool.
	img := stepImage()
	tname := "manual-" + suffix
	trig, err := cb.CreateBuildTrigger(ctx, &cbpb.CreateBuildTriggerRequest{Parent: parent, ProjectId: h.Project(), Trigger: &cbpb.BuildTrigger{
		Name: tname, Description: "first", Substitutions: map[string]string{"_GREETING": "hello"},
		BuildTemplate: &cbpb.BuildTrigger_Build{Build: &cbpb.Build{
			Steps:   []*cbpb.BuildStep{{Name: img, Entrypoint: "sh", Args: []string{"-c", "echo $TRIGGER_NAME $_GREETING $PROJECT_ID"}}},
			Options: &cbpb.BuildOptions{Pool: &cbpb.BuildOptions_PoolOption{Name: poolName}}}}}})
	if err != nil {
		t.Fatalf("CreateBuildTrigger: %v", err)
	}
	t.Cleanup(func() {
		_ = cb.DeleteBuildTrigger(context.Background(), &cbpb.DeleteBuildTriggerRequest{Name: trig.GetResourceName(), ProjectId: h.Project(), TriggerId: trig.GetId()})
	})
	if trig.GetId() == "" || trig.GetResourceName() != parent+"/triggers/"+trig.GetId() || trig.GetCreateTime() == nil {
		t.Errorf("the created trigger = %v", trig)
	}
	for _, key := range []string{trig.GetId(), tname} {
		got, err := cb.GetBuildTrigger(ctx, &cbpb.GetBuildTriggerRequest{Name: parent + "/triggers/" + key, ProjectId: h.Project(), TriggerId: key})
		if err != nil || got.GetId() != trig.GetId() {
			t.Errorf("GetBuildTrigger(%s) = %v, %v", key, got, err)
		}
	}
	found = false
	for it := cb.ListBuildTriggers(ctx, &cbpb.ListBuildTriggersRequest{Parent: parent, ProjectId: h.Project()}); ; {
		x, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListBuildTriggers: %v", err)
		}
		found = found || x.GetId() == trig.GetId()
	}
	if !found {
		t.Errorf("ListBuildTriggers lacks %s", trig.GetId())
	}
	upd, err := cb.UpdateBuildTrigger(ctx, &cbpb.UpdateBuildTriggerRequest{ProjectId: h.Project(), TriggerId: trig.GetId(),
		Trigger:    &cbpb.BuildTrigger{ResourceName: trig.GetResourceName(), Description: "second"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"description"}}})
	if err != nil || upd.GetDescription() != "second" || upd.GetBuild() == nil || upd.GetName() != tname {
		t.Fatalf("UpdateBuildTrigger = %v, %v", upd, err)
	}

	rop, err := cb.RunBuildTrigger(ctx, &cbpb.RunBuildTriggerRequest{Name: trig.GetResourceName(), ProjectId: h.Project(), TriggerId: trig.GetId(),
		Source: &cbpb.RepoSource{Substitutions: map[string]string{"_GREETING": "override"}}})
	skipWithoutHostSteps(t, err)
	if err != nil {
		t.Fatalf("RunBuildTrigger: %v", err)
	}
	b, err := rop.Wait(ctx)
	if err != nil {
		t.Fatalf("RunBuildTrigger.Wait: %v", err)
	}
	if b.GetStatus() != cbpb.Build_SUCCESS || b.GetBuildTriggerId() != trig.GetId() || b.GetOptions().GetPool().GetName() != poolName {
		t.Errorf("the triggered build = %v", b)
	}
	if got, want := strings.Join(b.GetSteps()[0].GetArgs(), " "), "-c echo "+tname+" override "+h.Project(); got != want {
		t.Errorf("the step's args = %q; want %q", got, want)
	}

	if _, err := cb.UpdateBuildTrigger(ctx, &cbpb.UpdateBuildTriggerRequest{ProjectId: h.Project(), TriggerId: trig.GetId(),
		Trigger:    &cbpb.BuildTrigger{ResourceName: trig.GetResourceName(), Disabled: true},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"disabled"}}}); err != nil {
		t.Fatalf("UpdateBuildTrigger(disabled): %v", err)
	}
	if _, err := cb.RunBuildTrigger(ctx, &cbpb.RunBuildTriggerRequest{Name: trig.GetResourceName(), ProjectId: h.Project(), TriggerId: trig.GetId()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("RunBuildTrigger of a disabled trigger: %v; want FailedPrecondition", err)
	}

	// A webhook trigger.
	secretValue := "wh-" + uuid.NewString()[:12]
	secret := buildSecret(ctx, t, h, h.Project(), secretValue)
	wh, err := cb.CreateBuildTrigger(ctx, &cbpb.CreateBuildTriggerRequest{Parent: parent, ProjectId: h.Project(), Trigger: &cbpb.BuildTrigger{
		Name: "hook-" + suffix, Substitutions: map[string]string{"_MSG": "$(body.message)"},
		WebhookConfig: &cbpb.WebhookConfig{AuthMethod: &cbpb.WebhookConfig_Secret{Secret: "projects/" + h.Project() + "/secrets/" + secret + "/versions/1"}},
		BuildTemplate: &cbpb.BuildTrigger_Build{Build: &cbpb.Build{
			Steps: []*cbpb.BuildStep{{Name: img, Entrypoint: "sh", Args: []string{"-c", "echo got $_MSG"}}}}}}})
	if err != nil {
		t.Fatalf("CreateBuildTrigger(webhook): %v", err)
	}
	t.Cleanup(func() {
		_ = cb.DeleteBuildTrigger(context.Background(), &cbpb.DeleteBuildTriggerRequest{Name: wh.GetResourceName(), ProjectId: h.Project(), TriggerId: wh.GetId()})
	})
	if wh.GetWebhookConfig().GetState() != cbpb.WebhookConfig_OK {
		t.Errorf("the webhook config = %v", wh.GetWebhookConfig())
	}
	post := func(secret string) (int, string) {
		u := "http://" + h.Endpoint(EnvRun) + "/v1/" + wh.GetResourceName() + ":webhook?key=k&secret=" + secret
		resp, err := http.Post(u, "application/json", strings.NewReader(`{"message":"from-the-hook"}`))
		if err != nil {
			t.Fatalf("POST :webhook: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}
	if code, body := post("wrong"); code != http.StatusForbidden {
		t.Errorf("a webhook with the wrong secret = %d %s; want 403", code, body)
	}
	if code, body := post(secretValue); code != http.StatusOK {
		t.Fatalf("the webhook = %d %s; want 200", code, body)
	}
	hb := triggeredBuild(ctx, t, cb, parent, wh.GetId(), 1)
	if hb.GetStatus() != cbpb.Build_SUCCESS || strings.Join(hb.GetSteps()[0].GetArgs(), " ") != "-c echo got from-the-hook" {
		t.Errorf("the webhook's build = %v", hb)
	}
	if _, err := cb.ReceiveTriggerWebhook(ctx, &cbpb.ReceiveTriggerWebhookRequest{Name: wh.GetResourceName(), Secret: secretValue,
		Body: &httpbody.HttpBody{ContentType: "application/json", Data: []byte(`{"message":"grpc"}`)}}); err != nil {
		t.Fatalf("ReceiveTriggerWebhook: %v", err)
	}
	if hb := triggeredBuild(ctx, t, cb, parent, wh.GetId(), 2); strings.Join(hb.GetSteps()[0].GetArgs(), " ") != "-c echo got grpc" {
		t.Errorf("the gRPC webhook's build = %v", hb)
	}

	// What needs a live service is refused with the reason.
	build := &cbpb.BuildTrigger_Build{Build: &cbpb.Build{Steps: []*cbpb.BuildStep{{Name: img, Args: []string{"true"}}}}}
	for name, tr := range map[string]*cbpb.BuildTrigger{
		"github":   {Name: "gh-" + suffix, Github: &cbpb.GitHubEventsConfig{Owner: "o", Name: "r"}, BuildTemplate: build},
		"filename": {Name: "fn-" + suffix, BuildTemplate: &cbpb.BuildTrigger_Filename{Filename: "cloudbuild.yaml"}},
		"repo":     {Name: "rp-" + suffix, SourceToBuild: &cbpb.GitRepoSource{Uri: "file:///tmp/repo", Ref: "refs/heads/main", RepoType: cbpb.GitFileSource_UNKNOWN}, BuildTemplate: build},
	} {
		_, err := cb.CreateBuildTrigger(ctx, &cbpb.CreateBuildTriggerRequest{Parent: parent, ProjectId: h.Project(), Trigger: tr})
		if status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), "not supported") {
			t.Errorf("a %s trigger: %v; want Unimplemented with the reason", name, err)
		}
	}
	_, err = cb.CreateBuild(ctx, &cbpb.CreateBuildRequest{Parent: parent, ProjectId: h.Project(), Build: &cbpb.Build{
		Steps: []*cbpb.BuildStep{{Name: img, Args: []string{"true"}}}, Options: &cbpb.BuildOptions{Pool: &cbpb.BuildOptions_PoolOption{Name: parent + "/workerPools/missing"}}}})
	if status.Code(err) != codes.NotFound {
		t.Errorf("a build on a missing pool: %v; want NotFound", err)
	}

	// Delete.
	if err := cb.DeleteBuildTrigger(ctx, &cbpb.DeleteBuildTriggerRequest{Name: trig.GetResourceName(), ProjectId: h.Project(), TriggerId: trig.GetId()}); err != nil {
		t.Fatalf("DeleteBuildTrigger: %v", err)
	}
	if _, err := cb.GetBuildTrigger(ctx, &cbpb.GetBuildTriggerRequest{Name: trig.GetResourceName(), ProjectId: h.Project(), TriggerId: trig.GetId()}); status.Code(err) != codes.NotFound {
		t.Errorf("GetBuildTrigger after delete: %v; want NotFound", err)
	}
	dop, err := cb.DeleteWorkerPool(ctx, &cbpb.DeleteWorkerPoolRequest{Name: poolName})
	if err != nil {
		t.Fatalf("DeleteWorkerPool: %v", err)
	}
	if err := dop.Wait(ctx); err != nil {
		t.Fatalf("DeleteWorkerPool.Wait: %v", err)
	}
	if _, err := cb.GetWorkerPool(ctx, &cbpb.GetWorkerPoolRequest{Name: poolName}); status.Code(err) != codes.NotFound {
		t.Errorf("GetWorkerPool after delete: %v; want NotFound", err)
	}
}

// TestGcloudBuildsTriggersAndWorkerPools (#1422): `gcloud builds
// worker-pools create`, `gcloud builds triggers create manual
// --inline-config` and `run`, and `gcloud builds triggers create webhook
// --secret`, through the configuration `cloudburrow gcloud-setup` writes,
// egress blocked. The manual trigger's build is SUCCESS with $TRIGGER_NAME
// substituted; the webhook trigger's URL runs a build with the secret.
func TestGcloudBuildsTriggersAndWorkerPools(t *testing.T) {
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
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	dir := t.TempDir()

	pool := "gpool-" + suffix
	g.must("builds", "worker-pools", "create", pool, "--region=us-central1", "--worker-machine-type=e2-standard-2")
	if out := g.must("builds", "worker-pools", "describe", pool, "--region=us-central1", "--format=value(state)"); lastLine(out) != "RUNNING" {
		t.Errorf("the pool's state = %q", out)
	}

	inline := filepath.Join(dir, "build.yaml")
	if err := os.WriteFile(inline, []byte("steps:\n- name: "+stepImage()+"\n  entrypoint: sh\n  args: ['-c', 'echo run by $TRIGGER_NAME $_WHO']\n"+
		"options:\n  pool:\n    name: "+parent+"/workerPools/"+pool+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	name := "gman-" + suffix
	g.must("builds", "triggers", "create", "manual", "--name="+name, "--region=us-central1", "--inline-config="+inline, "--substitutions=_WHO=gcloud")
	id := lastLine(g.must("builds", "triggers", "describe", name, "--region=us-central1", "--format=value(id)"))
	out, err := g.run(nil, "builds", "triggers", "run", name, "--region=us-central1")
	t.Logf("gcloud builds triggers run:\n%s", out)
	if err != nil && strings.Contains(out, hostStepsFlag) {
		t.Skip("the instance runs arbitrary build steps only with " + hostStepsFlag + " (#1382, ADR-0004); start it with the flag")
	}
	if err != nil {
		t.Fatalf("gcloud builds triggers run: %v", err)
	}
	b := triggeredBuild(ctx, t, cb, parent, id, 1)
	if b.GetStatus() != cbpb.Build_SUCCESS || strings.Join(b.GetSteps()[0].GetArgs(), " ") != "-c echo run by "+name+" gcloud" ||
		b.GetOptions().GetPool().GetName() != parent+"/workerPools/"+pool {
		t.Errorf("the triggered build = %v", b)
	}

	value := "gwh-" + uuid.NewString()[:12]
	secret := buildSecret(ctx, t, h, g.project, value)
	hook := "ghook-" + suffix
	g.must("builds", "triggers", "create", "webhook", "--name="+hook, "--region=us-central1", "--inline-config="+inline,
		"--secret=projects/"+g.project+"/secrets/"+secret+"/versions/1", "--substitutions=_WHO=hook")
	hid := lastLine(g.must("builds", "triggers", "describe", hook, "--region=us-central1", "--format=value(id)"))
	resp, err := http.Post("http://"+h.Endpoint(EnvRun)+"/v1/"+parent+"/triggers/"+hook+":webhook?key=k&secret="+value, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the webhook = %d", resp.StatusCode)
	}
	if b := triggeredBuild(ctx, t, cb, parent, hid, 1); b.GetStatus() != cbpb.Build_SUCCESS {
		t.Errorf("the webhook's build = %v", b)
	}

	g.must("builds", "triggers", "delete", name, "--region=us-central1", "--quiet")
	g.must("builds", "triggers", "delete", hook, "--region=us-central1", "--quiet")
	g.must("builds", "worker-pools", "delete", pool, "--region=us-central1", "--quiet")
	if out, err := g.run(nil, "builds", "triggers", "describe", name, "--region=us-central1"); err == nil {
		t.Errorf("the trigger is still there after delete:\n%s", out)
	}
}
