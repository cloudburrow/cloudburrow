//go:build compat

package compat

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	run "cloud.google.com/go/run/apiv2"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// covers: google.cloud.run.v2.WorkerPools/UpdateWorkerPool, google.cloud.run.v2.WorkerPools/GetWorkerPool, google.cloud.run.v2.Revisions/ListRevisions, google.cloud.run.v2.Revisions/DeleteRevision
//
// TestRunWorkerPoolUpdateMaskAndInstanceSplits (#1387): through the official
// WorkerPoolsClient, an UpdateWorkerPool with an update_mask changes only
// the fields it names (AIP-134): the instance count alone keeps the
// description and the revision, and the template alone is a new revision
// at the same count. instanceSplits then run two revisions at once, each
// its percentage of the instances, as Google documents for worker pools:
// both revisions are listed as active, each has its share of pods, the
// split one cannot be deleted, and splitting back to 100% to the latest
// retires it. A mask naming an output-only field, and splits that do not
// add up to 100%, are INVALID_ARGUMENT.
func TestRunWorkerPoolUpdateMaskAndInstanceSplits(t *testing.T) {
	h := New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	c, err := run.NewWorkerPoolsClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatalf("run.NewWorkerPoolsClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	rc, err := run.NewRevisionsClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatalf("run.NewRevisionsClient: %v", err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	id := "compat-pool-split"
	name := runParent(h) + "/workerPools/" + id
	remove := func() {
		dctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if op, err := c.DeleteWorkerPool(dctx, &runpb.DeleteWorkerPoolRequest{Name: name}); err == nil {
			_, _ = op.Wait(dctx)
		}
	}
	t.Cleanup(remove)
	two, four := int32(2), int32(4)
	op, err := c.CreateWorkerPool(ctx, &runpb.CreateWorkerPoolRequest{Parent: runParent(h), WorkerPoolId: id,
		WorkerPool: &runpb.WorkerPool{Description: "kept",
			Template: &runpb.WorkerPoolRevisionTemplate{Containers: sleeper("one")},
			Scaling:  &runpb.WorkerPoolScaling{ManualInstanceCount: &two}}})
	if err != nil {
		t.Fatalf("CreateWorkerPool: %v", err)
	}
	created, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateWorkerPool: %v", err)
	}
	rev1 := created.GetLatestCreatedRevision()

	update := func(wp *runpb.WorkerPool, paths ...string) (*runpb.WorkerPool, error) {
		t.Helper()
		wp.Name = name
		op, err := c.UpdateWorkerPool(ctx, &runpb.UpdateWorkerPoolRequest{WorkerPool: wp,
			UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}})
		if err != nil {
			return nil, err
		}
		return op.Wait(ctx)
	}

	// The count alone: the description, sent empty, and the revision stay.
	got, err := update(&runpb.WorkerPool{Scaling: &runpb.WorkerPoolScaling{ManualInstanceCount: &four}}, "scaling.manual_instance_count")
	if err != nil {
		t.Fatalf("UpdateWorkerPool scaling.manual_instance_count: %v", err)
	}
	if got.GetScaling().GetManualInstanceCount() != 4 || got.GetDescription() != "kept" || got.GetLatestCreatedRevision() != rev1 ||
		got.GetTemplate().GetContainers()[0].GetImage() != jobImage {
		t.Errorf("after a masked rescale: count %d, description %q, revision %s (want 4, kept, %s)",
			got.GetScaling().GetManualInstanceCount(), got.GetDescription(), got.GetLatestCreatedRevision(), rev1)
	}
	// The template alone: a new revision, at the same count.
	got, err = update(&runpb.WorkerPool{Template: &runpb.WorkerPoolRevisionTemplate{Containers: sleeper("two")}}, "template")
	if err != nil {
		t.Fatalf("UpdateWorkerPool template: %v", err)
	}
	rev2 := got.GetLatestCreatedRevision()
	if rev2 == rev1 || got.GetScaling().GetManualInstanceCount() != 4 || got.GetDescription() != "kept" {
		t.Errorf("after a masked template change: revision %s (was %s), count %d, description %q",
			rev2, rev1, got.GetScaling().GetManualInstanceCount(), got.GetDescription())
	}
	if _, err := update(&runpb.WorkerPool{}, "etag"); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a mask naming etag = %v, want INVALID_ARGUMENT", err)
	}

	// Half the instances on each revision.
	revID := func(n string) string { return n[strings.LastIndex(n, "/")+1:] }
	half := []*runpb.InstanceSplit{
		{Type: runpb.InstanceSplitAllocationType_INSTANCE_SPLIT_ALLOCATION_TYPE_LATEST, Percent: 50},
		{Type: runpb.InstanceSplitAllocationType_INSTANCE_SPLIT_ALLOCATION_TYPE_REVISION, Revision: revID(rev1), Percent: 50},
	}
	if _, err := update(&runpb.WorkerPool{InstanceSplits: []*runpb.InstanceSplit{half[0]}}, "instance_splits"); status.Code(err) != codes.InvalidArgument {
		t.Errorf("splits adding up to 50%% = %v, want INVALID_ARGUMENT", err)
	}
	got, err = update(&runpb.WorkerPool{InstanceSplits: half}, "instance_splits")
	if err != nil {
		t.Fatalf("UpdateWorkerPool instance_splits: %v", err)
	}
	if got.GetLatestCreatedRevision() != rev2 || got.GetTerminalCondition().GetState() != runpb.Condition_CONDITION_SUCCEEDED {
		t.Errorf("after the split: revision %s (want %s), condition %v", got.GetLatestCreatedRevision(), rev2, got.GetTerminalCondition())
	}
	st := got.GetInstanceSplitStatuses()
	if len(st) != 2 || st[1].GetRevision() != revID(rev1) || st[1].GetPercent() != 50 || st[0].GetPercent() != 50 {
		t.Errorf("instanceSplitStatuses = %v", st)
	}
	active := func() map[string]bool {
		t.Helper()
		out := map[string]bool{}
		it := rc.ListRevisions(ctx, &runpb.ListRevisionsRequest{Parent: name})
		for {
			r, err := it.Next()
			if err == iterator.Done {
				return out
			}
			if err != nil {
				t.Fatalf("ListRevisions: %v", err)
			}
			for _, cond := range r.GetConditions() {
				if cond.GetType() == "Active" {
					out[r.GetName()] = cond.GetState() == runpb.Condition_CONDITION_SUCCEEDED
				}
			}
		}
	}
	if a := active(); len(a) != 2 || !a[rev1] || !a[rev2] {
		t.Errorf("revisions active = %v, want both", a)
	}
	pods := func(rev string) int {
		t.Helper()
		kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
		if kubeconfig == "" {
			t.Skipf("%s is not set", envKubeconfig)
		}
		out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig, "-n", "default", "get", "pods",
			"-l", "cloudburrow.dev/run-revision="+revID(rev), "--field-selector=status.phase=Running", "-o", "name").CombinedOutput()
		if err != nil {
			t.Fatalf("kubectl get pods: %v\n%s", err, out)
		}
		return len(strings.Fields(string(out)))
	}
	// Polled: the latest revision's surplus pods take their grace period
	// to stop.
	waitPods := func(rev string, want int) int {
		var n int
		for range 60 {
			if n = pods(rev); n == want {
				return n
			}
			time.Sleep(2 * time.Second)
		}
		return n
	}
	if a, b := waitPods(rev1, 2), waitPods(rev2, 2); a != 2 || b != 2 {
		t.Errorf("running pods: %d of %s and %d of %s, want 2 each", a, revID(rev1), b, revID(rev2))
	}
	if _, err := rc.DeleteRevision(ctx, &runpb.DeleteRevisionRequest{Name: rev1}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeleteRevision of the split revision = %v, want FAILED_PRECONDITION", err)
	}

	// All to the latest again: the old revision is retired.
	got, err = update(&runpb.WorkerPool{InstanceSplits: []*runpb.InstanceSplit{
		{Type: runpb.InstanceSplitAllocationType_INSTANCE_SPLIT_ALLOCATION_TYPE_LATEST, Percent: 100}}}, "instance_splits")
	if err != nil {
		t.Fatalf("UpdateWorkerPool back to the latest: %v", err)
	}
	if a := active(); a[rev1] || !a[rev2] {
		t.Errorf("revisions active after splitting back = %v, want only %s", a, rev2)
	}
	if n := waitPods(rev2, 4); n != 4 {
		t.Errorf("running pods of %s after splitting back = %d, want 4", revID(rev2), n)
	}
	if n := waitPods(rev1, 0); n != 0 {
		t.Errorf("running pods of %s after splitting back = %d, want 0", revID(rev1), n)
	}
}
