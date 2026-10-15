//go:build compat

package compat

import (
	"testing"
	"time"

	lroauto "cloud.google.com/go/longrunning/autogen"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

// TestRunOperationsListAndWait (#1229): through Google's official
// longrunning OperationsClient on the Cloud Run port, ListOperations lists
// the location's operations (a CreateJob's and a RunJob's among them), in
// pages, and refuses a filter rather than ignoring it; WaitOperation returns
// a done operation at once, returns a pending one as it is when its timeout
// passes, and otherwise returns once the operation finishes; an unknown
// operation is NOT_FOUND.
func TestRunOperationsListAndWait(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	jc, _ := runJobClients(t, h)
	oc, err := lroauto.NewOperationsClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatalf("NewOperationsClient: %v", err)
	}
	t.Cleanup(func() { _ = oc.Close() })

	id := "compat-ops-job"
	name := runParent(h) + "/jobs/" + id
	cop, err := jc.CreateJob(ctx, &runpb.CreateJobRequest{Parent: runParent(h), JobId: id,
		Job: shellJob(`sleep 3; echo done`, 1, nil)})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	t.Cleanup(func() { _, _ = jc.DeleteJob(h.Context(), &runpb.DeleteJobRequest{Name: name}) })

	done, err := oc.WaitOperation(ctx, &longrunningpb.WaitOperationRequest{Name: cop.Name(), Timeout: durationpb.New(time.Second)})
	if err != nil || !done.GetDone() || done.GetResponse() == nil {
		t.Fatalf("WaitOperation of the CreateJob operation = %v, %v; want it done with the job", done, err)
	}

	rop, err := jc.RunJob(ctx, &runpb.RunJobRequest{Name: name})
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	pending, err := oc.WaitOperation(ctx, &longrunningpb.WaitOperationRequest{Name: rop.Name(), Timeout: durationpb.New(500 * time.Millisecond)})
	if err != nil || pending.GetDone() {
		t.Fatalf("WaitOperation of the RunJob operation with 500ms = %v, %v; want it returned pending", pending, err)
	}

	listed := map[string]bool{}
	pages := 0
	for it := oc.ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: runParent(h), PageSize: 1}); ; {
		op, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListOperations: %v", err)
		}
		listed[op.GetName()] = true
		pages++
	}
	if !listed[cop.Name()] || !listed[rop.Name()] || pages < 2 {
		t.Errorf("ListOperations (page size 1) listed %v; want the CreateJob and RunJob operations", listed)
	}
	if _, err := oc.ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: runParent(h), Filter: "done=true"}).Next(); status.Code(err) != codes.Unimplemented {
		t.Errorf("ListOperations with a filter = %v; want Unimplemented", err)
	}

	final, err := oc.WaitOperation(ctx, &longrunningpb.WaitOperationRequest{Name: rop.Name(), Timeout: durationpb.New(3 * time.Minute)})
	if err != nil || !final.GetDone() || final.GetError() != nil {
		t.Fatalf("WaitOperation of the RunJob operation = %v, %v; want it done and succeeded", final, err)
	}

	if _, err := oc.WaitOperation(ctx, &longrunningpb.WaitOperationRequest{Name: runParent(h) + "/operations/compat-nope"}); status.Code(err) != codes.NotFound {
		t.Errorf("WaitOperation of an unknown operation = %v; want NotFound", err)
	}
}
