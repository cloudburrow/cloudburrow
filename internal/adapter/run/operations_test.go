package run

import (
	"context"
	"testing"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

// ListOperations lists one location's operations, in pages, and refuses a
// filter rather than ignoring it.
func TestListOperations(t *testing.T) {
	t.Parallel()
	s := NewServer(&Knative{}, "inst", time.Second)
	ops := NewOperationsServer(s)
	for i := 0; i < 3; i++ {
		s.ops.Create(jobParent, "t")
	}
	s.ops.Create("projects/demo-project/locations/us-central1-b", "other")

	ctx := context.Background()
	var got []string
	token := ""
	for {
		resp, err := ops.ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: jobParent, PageSize: 2, PageToken: token})
		if err != nil {
			t.Fatal(err)
		}
		for _, op := range resp.GetOperations() {
			got = append(got, op.GetName())
		}
		if token = resp.GetNextPageToken(); token == "" {
			break
		}
	}
	if len(got) != 3 {
		t.Fatalf("listed %v; want the 3 operations of %s alone", got, jobParent)
	}
	for _, c := range []struct {
		req  *longrunningpb.ListOperationsRequest
		code codes.Code
	}{
		{&longrunningpb.ListOperationsRequest{Name: jobParent, Filter: "done=true"}, codes.Unimplemented},
		{&longrunningpb.ListOperationsRequest{Name: "projects/p"}, codes.InvalidArgument},
		{&longrunningpb.ListOperationsRequest{Name: jobParent, PageToken: "bogus"}, codes.InvalidArgument},
	} {
		if _, err := ops.ListOperations(ctx, c.req); status.Code(err) != c.code {
			t.Errorf("ListOperations(%v) = %v; want %v", c.req, err, c.code)
		}
	}
}

// WaitOperation returns when the operation finishes, or as it is when the
// timeout passes, and NOT_FOUND for an operation it does not know.
func TestWaitOperation(t *testing.T) {
	t.Parallel()
	s := NewServer(&Knative{}, "inst", time.Second)
	ops := NewOperationsServer(s)
	ctx := context.Background()

	pending := s.ops.Create(jobParent, "t")
	op, err := ops.WaitOperation(ctx, &longrunningpb.WaitOperationRequest{Name: pending.Name, Timeout: durationpb.New(150 * time.Millisecond)})
	if err != nil || op.GetDone() {
		t.Fatalf("wait with a timeout = %v, %v; want the pending operation", op, err)
	}

	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = s.ops.Succeed(pending.Name, nil)
	}()
	start := time.Now()
	op, err = ops.WaitOperation(ctx, &longrunningpb.WaitOperationRequest{Name: pending.Name, Timeout: durationpb.New(10 * time.Second)})
	if err != nil || !op.GetDone() {
		t.Fatalf("wait = %v, %v; want it done", op, err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("wait took %v; it should return when the operation finishes", d)
	}

	if _, err := ops.WaitOperation(ctx, &longrunningpb.WaitOperationRequest{Name: jobParent + "/operations/nope"}); status.Code(err) != codes.NotFound {
		t.Errorf("wait of an unknown operation = %v; want NOT_FOUND", err)
	}
	if _, err := ops.WaitOperation(ctx, &longrunningpb.WaitOperationRequest{Name: pending.Name, Timeout: durationpb.New(-time.Second)}); err != nil {
		t.Errorf("wait of a done operation with any timeout = %v; want it at once", err)
	}
}
