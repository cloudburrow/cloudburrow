//go:build compat

package compat

import (
	"context"
	"net/http"
	"testing"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	run "cloud.google.com/go/run/apiv2"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// covers: google.cloud.run.v2.WorkerPools/GetIamPolicy, google.cloud.run.v2.WorkerPools/SetIamPolicy, google.cloud.run.v2.WorkerPools/TestIamPermissions
//
// TestConsoleRunWorkerPoolDeployEditScaleDelete (#1300): a worker pool
// deployed from the console's Worker pools page reads back through the
// official WorkerPoolsClient with the form's settings; Edit deploys a new
// revision the pool's page lists; Scale changes the instance count and keeps
// the revision; the Permissions tab round-trips the pool's stored policy; the
// console's delete is NOT_FOUND through the client.
func TestConsoleRunWorkerPoolDeployEditScaleDelete(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	c, err := run.NewWorkerPoolsClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatalf("run.NewWorkerPoolsClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	project := h.Project()
	id := "compat-console-pool"
	name := runParent(h) + "/workerPools/" + id
	t.Cleanup(func() { _, _ = c.DeleteWorkerPool(context.Background(), &runpb.DeleteWorkerPoolRequest{Name: name}) })

	form := map[string]string{"name": id, "image": jobImage, "command": "sh -c", "args": "'exec sleep 3600'",
		"env": `{"GREETING":"one"}`, "instances": "2"}
	if code, resp := consoleCreate(t, addr, "run-worker-pools", project, form); code != http.StatusOK {
		t.Fatalf("console deploy worker pool = %d: %s", code, resp)
	}
	wp, err := c.GetWorkerPool(ctx, &runpb.GetWorkerPoolRequest{Name: name})
	if err != nil {
		t.Fatalf("GetWorkerPool of the console's pool: %v", err)
	}
	if wp.GetScaling().GetManualInstanceCount() != 2 || wp.GetTemplate().GetContainers()[0].GetImage() != jobImage {
		t.Errorf("the console's pool reads back as %v", wp)
	}
	first := wp.GetLatestCreatedRevision()

	form["env"] = `{"GREETING":"two"}`
	if code, resp := consoleEdit(t, addr, "run-worker-pools", project, []string{id}, form); code != http.StatusOK {
		t.Fatalf("console edit = %d: %s", code, resp)
	}
	wp, err = c.GetWorkerPool(ctx, &runpb.GetWorkerPoolRequest{Name: name})
	if err != nil || wp.GetLatestCreatedRevision() == first || wp.GetTemplate().GetContainers()[0].GetEnv()[0].GetValue() != "two" {
		t.Fatalf("after the console's edit: %v, %v; want a new revision with GREETING=two", wp, err)
	}
	page := consoleDetailOf(t, addr, "run-worker-pools", project, id)
	if !page.offers("scale") {
		t.Errorf("the pool's page does not offer Scale: %+v", page.Actions)
	}
	for _, s := range page.Sections {
		if s.ID == "revisions" && len(s.Listing.Items) != 2 {
			t.Errorf("the Revisions tab lists %d revisions, want 2", len(s.Listing.Items))
		}
	}

	second := wp.GetLatestCreatedRevision()
	if code, resp := consoleAct(t, addr, "run-worker-pools", project, []string{id}, "scale", map[string]string{"instances": "1"}); code != http.StatusOK {
		t.Fatalf("console scale = %d: %s", code, resp)
	}
	wp, err = c.GetWorkerPool(ctx, &runpb.GetWorkerPoolRequest{Name: name})
	if err != nil || wp.GetScaling().GetManualInstanceCount() != 1 || wp.GetLatestCreatedRevision() != second {
		t.Errorf("after the console's scale: %v, %v; want one instance on revision %s", wp, err, second)
	}

	consolePermissionsRoundTrip(t, h, addr, "run-worker-pools", []string{id}, policyHandle{
		get: func(ctx context.Context) (*iampb.Policy, error) {
			return c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
		},
		set: func(ctx context.Context, p *iampb.Policy) error {
			_, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name, Policy: p})
			return err
		},
		test: func(ctx context.Context, perms []string) ([]string, error) {
			r, err := c.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: name, Permissions: perms})
			return r.GetPermissions(), err
		},
	}, "roles/run.developer", "run.workerpools.get")

	if code, resp := consoleDo(t, addr, http.MethodDelete, "/api/resources/run-worker-pools?project="+project+"&name="+id, ""); code != http.StatusOK {
		t.Fatalf("console delete = %d: %s", code, resp)
	}
	if _, err := c.GetWorkerPool(ctx, &runpb.GetWorkerPoolRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetWorkerPool after the console's delete = %v, want NOT_FOUND", err)
	}
}

// TestConsoleRunInstanceCreateStopStartDelete (#1299): an instance created
// on the console's Instances page reads back through the official
// InstancesClient with the form's settings; Stop and Start from its page
// stop and start it as the client reads it; the console's delete is
// NOT_FOUND through the client.
func TestConsoleRunInstanceCreateStopStartDelete(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	c, err := run.NewInstancesClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatalf("run.NewInstancesClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	project := h.Project()
	id := "compat-console-instance"
	name := runParent(h) + "/instances/" + id
	t.Cleanup(func() { _, _ = c.DeleteInstance(context.Background(), &runpb.DeleteInstanceRequest{Name: name}) })

	form := map[string]string{"name": id, "description": "from the console", "image": jobImage,
		"command": "sh -c", "args": "'exec sleep 3600'", "env": `{"GREETING":"hi"}`}
	if code, resp := consoleCreate(t, addr, "run-instances", project, form); code != http.StatusOK {
		t.Fatalf("console create instance = %d: %s", code, resp)
	}
	in, err := c.GetInstance(ctx, &runpb.GetInstanceRequest{Name: name})
	if err != nil || in.GetDescription() != "from the console" || in.GetContainers()[0].GetImage() != jobImage {
		t.Fatalf("the console's instance reads back as %v, %v", in, err)
	}
	waitCondition(t, "instance", func() (*runpb.Condition, error) {
		in, err := c.GetInstance(ctx, &runpb.GetInstanceRequest{Name: name})
		return in.GetTerminalCondition(), err
	})
	if page := consoleDetailOf(t, addr, "run-instances", project, id); !page.offers("stop") || page.offers("start") {
		t.Errorf("a running instance's page offers %+v; want Stop", page.Actions)
	}
	if code, resp := consoleAct(t, addr, "run-instances", project, []string{id}, "stop", nil); code != http.StatusOK {
		t.Fatalf("console stop = %d: %s", code, resp)
	}
	if in, err := c.GetInstance(ctx, &runpb.GetInstanceRequest{Name: name}); err != nil ||
		in.GetTerminalCondition().GetMessage() == "" && in.GetTerminalCondition().GetState() == runpb.Condition_CONDITION_SUCCEEDED {
		t.Errorf("after the console's stop: %v, %v", in, err)
	}
	waitCondition(t, "instance", func() (*runpb.Condition, error) {
		in, err := c.GetInstance(ctx, &runpb.GetInstanceRequest{Name: name})
		return in.GetTerminalCondition(), err
	})
	page := consoleDetailOf(t, addr, "run-instances", project, id)
	if page.summary("Status") != "Stopped" || !page.offers("start") {
		t.Errorf("a stopped instance's page: status %q, actions %+v; want Stopped and Start", page.summary("Status"), page.Actions)
	}
	if code, resp := consoleAct(t, addr, "run-instances", project, []string{id}, "start", nil); code != http.StatusOK {
		t.Fatalf("console start = %d: %s", code, resp)
	}
	if in, err := c.GetInstance(ctx, &runpb.GetInstanceRequest{Name: name}); err != nil ||
		in.GetTerminalCondition().GetMessage() == "the instance is stopped" {
		t.Errorf("after the console's start: %v, %v", in, err)
	}
	if code, resp := consoleDo(t, addr, http.MethodDelete, "/api/resources/run-instances?project="+project+"&name="+id, ""); code != http.StatusOK {
		t.Fatalf("console delete = %d: %s", code, resp)
	}
	if _, err := c.GetInstance(ctx, &runpb.GetInstanceRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetInstance after the console's delete = %v, want NOT_FOUND", err)
	}
}
