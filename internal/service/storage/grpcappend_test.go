package storage

import (
	"context"
	"io"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	gcs "cloud.google.com/go/storage"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	pb "github.com/cloudburrow/cloudburrow/internal/service/storage/storagev2pb"
)

func readAll(t *testing.T, ctx context.Context, o *gcs.ObjectHandle) string {
	t.Helper()
	r, err := o.NewReader(ctx)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b)
}

// TestGRPCAppendableObjects: the official Go client's appendable writer
// (Writer.Append, Flush, NewWriterFromAppendableObject) in a Rapid storage
// bucket, each step read back over JSON (#1443).
func TestGRPCAppendableObjects(t *testing.T) {
	g, j, _ := grpcClients(t)
	ctx := context.Background()

	if err := g.Bucket("plain").Create(ctx, "p", nil); err != nil {
		t.Fatal(err)
	}
	w := g.Bucket("plain").Object("a").NewWriter(ctx)
	w.Append = true
	_, _ = w.Write([]byte("x"))
	if err := w.Close(); grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Errorf("appendable write to a regional bucket = %v, want FAILED_PRECONDITION", err)
	}
	if err := g.Bucket("bad-rapid").Create(ctx, "p", &gcs.BucketAttrs{Location: "us-central1", StorageClass: "RAPID"}); grpcstatus.Code(err) != codes.InvalidArgument {
		t.Errorf("RAPID without a zone = %v, want INVALID_ARGUMENT", err)
	}

	zb := g.Bucket("zonal")
	if err := zb.Create(ctx, "p", &gcs.BucketAttrs{Location: "us-central1", StorageClass: "RAPID",
		CustomPlacementConfig: &gcs.CustomPlacementConfig{DataLocations: []string{"us-central1-a"}}}); err != nil {
		t.Fatalf("Create zonal: %v", err)
	}
	ja, err := j.Bucket("zonal").Attrs(ctx)
	if err != nil || ja.StorageClass != "RAPID" || ja.CustomPlacementConfig == nil || len(ja.CustomPlacementConfig.DataLocations) != 1 {
		t.Fatalf("JSON Attrs = %+v, %v", ja, err)
	}
	ga, err := zb.Attrs(ctx)
	if err != nil || ga.CustomPlacementConfig == nil || ga.CustomPlacementConfig.DataLocations[0] != "us-central1-a" {
		t.Fatalf("gRPC Attrs = %+v, %v", ga, err)
	}

	o := zb.Object("log")
	w = o.NewWriter(ctx)
	w.Append = true
	if _, err := w.Write([]byte("hello ")); err != nil {
		t.Fatal(err)
	}
	if n, err := w.Flush(); err != nil || n != 6 {
		t.Fatalf("Flush = %d, %v; want 6", n, err)
	}
	// Readable, and unfinalized, before the writer closes.
	if got := readAll(t, ctx, j.Bucket("zonal").Object("log")); got != "hello " {
		t.Errorf("JSON read while open = %q", got)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	attrs, err := j.Bucket("zonal").Object("log").Attrs(ctx)
	if err != nil || attrs.Size != 6 || !attrs.Finalized.IsZero() {
		t.Fatalf("JSON Attrs of the open object = %+v, %v; want size 6 and no timeFinalized", attrs, err)
	}
	if got := readAll(t, ctx, o); got != "hello " {
		t.Errorf("gRPC read of the open object = %q", got)
	}

	// Takeover: a new writer from the generation continues at its end.
	w2, off, err := o.Generation(attrs.Generation).NewWriterFromAppendableObject(ctx, &gcs.AppendableWriterOpts{FinalizeOnClose: true})
	if err != nil || off != 6 {
		t.Fatalf("NewWriterFromAppendableObject = %d, %v; want offset 6", off, err)
	}
	if _, err := w2.Write([]byte("world")); err != nil {
		t.Fatal(err)
	}
	if err := w2.Close(); err != nil {
		t.Fatalf("Close finalizing: %v", err)
	}
	fa, err := j.Bucket("zonal").Object("log").Attrs(ctx)
	if err != nil || fa.Size != 11 || fa.Finalized.IsZero() || fa.Generation != attrs.Generation {
		t.Fatalf("JSON Attrs after finalize = %+v, %v", fa, err)
	}
	if got := readAll(t, ctx, o); got != "hello world" {
		t.Errorf("read after finalize = %q", got)
	}
	w3, _, err := o.Generation(fa.Generation).NewWriterFromAppendableObject(ctx, nil)
	if err == nil {
		_, _ = w3.Write([]byte("!"))
		err = w3.Close()
	}
	if grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Errorf("append to a finalized object = %v, want FAILED_PRECONDITION", err)
	}

	// custom_placement_config is fixed at creation; ip_filter is refused.
	if _, err := zb.Update(ctx, gcs.BucketAttrsToUpdate{StorageClass: "RAPID"}); err != nil {
		t.Errorf("Update keeping the placement: %v", err)
	}
}

// TestGRPCAppendTakeoverSupersedes: a takeover stops the earlier writer.
func TestGRPCAppendTakeoverSupersedes(t *testing.T) {
	g, _, addr := grpcClients(t)
	ctx := context.Background()
	zb := g.Bucket("zonal")
	if err := zb.Create(ctx, "p", &gcs.BucketAttrs{Location: "us-central1", StorageClass: "RAPID",
		CustomPlacementConfig: &gcs.CustomPlacementConfig{DataLocations: []string{"us-central1-a"}}}); err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := pb.NewStorageClient(conn)
	open := func(first *pb.BidiWriteObjectRequest) (pb.Storage_BidiWriteObjectClient, *pb.BidiWriteObjectResponse) {
		t.Helper()
		s, err := c.BidiWriteObject(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Send(first); err != nil {
			t.Fatal(err)
		}
		resp, err := s.Recv()
		if err != nil {
			t.Fatalf("first answer: %v", err)
		}
		return s, resp
	}
	a, ra := open(&pb.BidiWriteObjectRequest{
		FirstMessage: &pb.BidiWriteObjectRequest_WriteObjectSpec{WriteObjectSpec: &pb.WriteObjectSpec{
			Resource: &pb.Object{Bucket: "projects/_/buckets/zonal", Name: "o"}, Appendable: proto.Bool(true)}},
		Data:  &pb.BidiWriteObjectRequest_ChecksummedData{ChecksummedData: &pb.ChecksummedData{Content: []byte("abc")}},
		Flush: true, StateLookup: true})
	gen := ra.GetResource().GetGeneration()
	if ra.GetResource().GetSize() != 3 || ra.GetResource().GetFinalizeTime() != nil || len(ra.GetWriteHandle().GetHandle()) == 0 {
		t.Fatalf("first writer's answer = %v", ra)
	}
	_, rb := open(&pb.BidiWriteObjectRequest{FirstMessage: &pb.BidiWriteObjectRequest_AppendObjectSpec{AppendObjectSpec: &pb.AppendObjectSpec{
		Bucket: "projects/_/buckets/zonal", Object: "o", Generation: gen}}})
	if rb.GetResource().GetSize() != 3 || string(rb.GetWriteHandle().GetHandle()) == string(ra.GetWriteHandle().GetHandle()) {
		t.Fatalf("takeover answer = %v", rb)
	}
	if err := a.Send(&pb.BidiWriteObjectRequest{WriteOffset: 3, Flush: true, StateLookup: true,
		Data: &pb.BidiWriteObjectRequest_ChecksummedData{ChecksummedData: &pb.ChecksummedData{Content: []byte("d")}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Recv(); grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Errorf("superseded writer's append = %v, want FAILED_PRECONDITION", err)
	}
	// The earlier handle resumes nothing either.
	_, err = c.UpdateBucket(ctx, &pb.UpdateBucketRequest{Bucket: &pb.Bucket{Name: "projects/_/buckets/zonal",
		CustomPlacementConfig: &pb.Bucket_CustomPlacementConfig{DataLocations: []string{"us-central1-b"}}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"custom_placement_config"}}})
	if grpcstatus.Code(err) != codes.InvalidArgument {
		t.Errorf("changing custom_placement_config = %v, want INVALID_ARGUMENT", err)
	}
	_, err = c.UpdateBucket(ctx, &pb.UpdateBucketRequest{Bucket: &pb.Bucket{Name: "projects/_/buckets/zonal",
		IpFilter: &pb.Bucket_IpFilter{Mode: proto.String("On")}}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"ip_filter"}}})
	if grpcstatus.Code(err) != codes.InvalidArgument {
		t.Errorf("ip_filter mode On = %v, want INVALID_ARGUMENT", err)
	}
}

// TestGRPCSetIamPolicyUpdateMask: update_mask keeps the fields it leaves
// out (#1443).
func TestGRPCSetIamPolicyUpdateMask(t *testing.T) {
	g, j, addr := grpcClients(t)
	ctx := context.Background()
	if err := g.Bucket("iam").Create(ctx, "p", nil); err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := pb.NewStorageClient(conn)
	res := "projects/_/buckets/iam"
	set := func(p *iampb.Policy, paths ...string) (*iampb.Policy, error) {
		req := &iampb.SetIamPolicyRequest{Resource: res, Policy: p}
		if paths != nil {
			req.UpdateMask = &fieldmaskpb.FieldMask{Paths: paths}
		}
		return c.SetIamPolicy(ctx, req)
	}
	viewer := &iampb.Binding{Role: "roles/storage.objectViewer", Members: []string{"user:a@example.com"}}
	if _, err := set(&iampb.Policy{Bindings: []*iampb.Binding{viewer}}, "bindings"); err != nil {
		t.Fatalf("SetIamPolicy bindings: %v", err)
	}
	// A mask of version only leaves the bindings as they are.
	got, err := set(&iampb.Policy{Version: 3}, "version")
	if err != nil || got.GetVersion() != 3 || len(got.GetBindings()) != 1 || got.GetBindings()[0].GetRole() != viewer.Role {
		t.Fatalf("SetIamPolicy version = %v, %v", got, err)
	}
	jp, err := j.Bucket("iam").IAM().Policy(ctx)
	if err != nil || !jp.HasRole("user:a@example.com", "roles/storage.objectViewer") {
		t.Fatalf("JSON policy = %v, %v", jp, err)
	}
	// A stale etag under the mask is refused; outside it, it is not checked.
	stale := []byte("BwXXXXXXXXX=")
	if _, err := set(&iampb.Policy{Etag: stale, Bindings: []*iampb.Binding{viewer}}, "bindings", "etag"); grpcstatus.Code(err) != codes.FailedPrecondition {
		t.Errorf("stale etag under the mask = %v, want FAILED_PRECONDITION", err)
	}
	if _, err := set(&iampb.Policy{Etag: stale, Bindings: []*iampb.Binding{viewer}}, "bindings"); err != nil {
		t.Errorf("stale etag outside the mask: %v", err)
	}
	if _, err := set(&iampb.Policy{}, "owners"); grpcstatus.Code(err) != codes.InvalidArgument {
		t.Errorf("unknown path = %v, want INVALID_ARGUMENT", err)
	}
	if _, err := set(&iampb.Policy{}, "audit_configs"); grpcstatus.Code(err) != codes.Unimplemented {
		t.Errorf("audit_configs = %v, want UNIMPLEMENTED", err)
	}
}
