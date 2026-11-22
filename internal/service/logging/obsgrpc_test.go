package logging

import (
	"context"
	"testing"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	obspb "cloud.google.com/go/observability/apiv1/observabilitypb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestObservabilityGRPCAdapter (#2117): the gRPC methods are their REST
// bindings, REST errors keep their canonical codes, and the Logging port's
// Operations service finds and lists the Observability API's operations
// beside Cloud Logging's.
func TestObservabilityGRPCAdapter(t *testing.T) {
	ctx := context.Background()
	a := NewAdmin(store.NewMemory())
	s := obsGRPC{a: a}
	ops := mergedOperations{a: a, logging: operationsServer{a: a}}
	const global = "projects/proj-g/locations/global"

	op, err := s.UpdateSettings(ctx, &obspb.UpdateSettingsRequest{Settings: &obspb.Settings{Name: global + "/settings", DefaultStorageLocation: "eu"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"default_storage_location"}}})
	if err != nil || !op.GetDone() {
		t.Fatalf("UpdateSettings = %v, %v", op, err)
	}
	if st, err := s.GetSettings(ctx, &obspb.GetSettingsRequest{Name: global + "/settings"}); err != nil || st.GetDefaultStorageLocation() != "eu" {
		t.Errorf("GetSettings = %v, %v", st, err)
	}
	if _, err := s.GetBucket(ctx, &obspb.GetBucketRequest{Name: "projects/proj-g/locations/us/buckets/_Trace"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetBucket of no bucket: %v; want NOT_FOUND", err)
	}
	if _, err := s.CreateBucket(ctx, &obspb.CreateBucketRequest{Parent: global, BucketId: "g"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a bucket in global: %v; want INVALID_ARGUMENT", err)
	}
	if _, err := s.GetSettings(ctx, &obspb.GetSettingsRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("no name: %v; want INVALID_ARGUMENT", err)
	}
	if got, err := ops.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: op.GetName()}); err != nil || got.GetName() != op.GetName() {
		t.Errorf("GetOperation = %v, %v", got, err)
	}
	for _, f := range []string{"", "done=true"} {
		l, err := ops.ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: global, Filter: f})
		if err != nil || len(l.GetOperations()) != 1 || l.GetOperations()[0].GetName() != op.GetName() {
			t.Errorf("ListOperations %q = %v, %v", f, l, err)
		}
	}
	if l, err := ops.ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: global, Filter: "request_type=CopyLogEntries"}); err != nil || len(l.GetOperations()) != 0 {
		t.Errorf("ListOperations of copies = %v, %v; want none", l, err)
	}
	if _, err := ops.ListOperations(ctx, &longrunningpb.ListOperationsRequest{Name: global, Filter: "name=x"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("another filter: %v; want INVALID_ARGUMENT", err)
	}
	if _, err := ops.DeleteOperation(ctx, &longrunningpb.DeleteOperationRequest{Name: op.GetName()}); err != nil {
		t.Errorf("DeleteOperation: %v", err)
	}
	if _, err := ops.GetOperation(ctx, &longrunningpb.GetOperationRequest{Name: op.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("GetOperation after delete: %v; want NOT_FOUND", err)
	}
}
