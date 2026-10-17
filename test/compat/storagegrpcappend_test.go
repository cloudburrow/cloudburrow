//go:build compat

package compat

import (
	"io"
	"strings"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/storage"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	pb "github.com/cloudburrow/cloudburrow/internal/service/storage/storagev2pb"
)

// TestStorageGRPCAppendableObjects: the official Go client's appendable
// writer over gRPC (#1443): a Rapid storage bucket created with a zonal
// custom_placement_config, an object written with Writer.Append and
// Flush, read back unfinalized over JSON and gRPC, taken over with
// NewWriterFromAppendableObject at its end, finalized on close, and then
// refused further appends.
// covers: google.storage.v2.Storage/BidiWriteObject, google.storage.v2.Storage/CreateBucket, google.storage.v2.Storage/GetBucket
func TestStorageGRPCAppendableObjects(t *testing.T) {
	h := New(t)
	g := storageGRPCClient(t, h, true)
	j := storageClient(t, h)
	ctx := h.Context()

	plain := h.Project() + "-grpc-plain"
	if err := g.Bucket(plain).Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { emptyAndDelete(ctx, j.Bucket(plain)) })
	w := g.Bucket(plain).Object("a").NewWriter(ctx)
	w.Append = true
	_, _ = w.Write([]byte("x"))
	if err := w.Close(); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("appendable write to a regional bucket = %v, want FAILED_PRECONDITION", err)
	}

	name := h.Project() + "-grpc-rapid"
	zb := g.Bucket(name)
	if err := zb.Create(ctx, h.Project(), &storage.BucketAttrs{Location: "us-central1", StorageClass: "RAPID",
		CustomPlacementConfig: &storage.CustomPlacementConfig{DataLocations: []string{"us-central1-a"}}}); err != nil {
		t.Fatalf("Create a Rapid bucket: %v", err)
	}
	t.Cleanup(func() { emptyAndDelete(ctx, j.Bucket(name)) })
	ja, err := j.Bucket(name).Attrs(ctx)
	if err != nil || ja.StorageClass != "RAPID" || ja.CustomPlacementConfig == nil || len(ja.CustomPlacementConfig.DataLocations) != 1 {
		t.Fatalf("JSON Attrs = %+v, %v", ja, err)
	}
	if ga, err := zb.Attrs(ctx); err != nil || ga.CustomPlacementConfig == nil || !strings.EqualFold(ga.CustomPlacementConfig.DataLocations[0], "us-central1-a") {
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
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	attrs, err := j.Bucket(name).Object("log").Attrs(ctx)
	if err != nil || attrs.Size != 6 || !attrs.Finalized.IsZero() {
		t.Fatalf("JSON Attrs of the open object = %+v, %v; want size 6 and no timeFinalized", attrs, err)
	}
	for label, c := range map[string]*storage.Client{"JSON": j, "gRPC": g} {
		if got := readString(t, h, c.Bucket(name).Object("log")); got != "hello " {
			t.Errorf("%s read of the unfinalized object = %q", label, got)
		}
	}

	w2, off, err := o.Generation(attrs.Generation).NewWriterFromAppendableObject(ctx, &storage.AppendableWriterOpts{FinalizeOnClose: true})
	if err != nil || off != 6 {
		t.Fatalf("NewWriterFromAppendableObject = %d, %v; want offset 6", off, err)
	}
	if _, err := w2.Write([]byte("world")); err != nil {
		t.Fatal(err)
	}
	if err := w2.Close(); err != nil {
		t.Fatalf("Close finalizing: %v", err)
	}
	fa, err := j.Bucket(name).Object("log").Attrs(ctx)
	if err != nil || fa.Size != 11 || fa.Finalized.IsZero() || fa.Generation != attrs.Generation {
		t.Fatalf("JSON Attrs after finalize = %+v, %v", fa, err)
	}
	if got := readString(t, h, o); got != "hello world" {
		t.Errorf("read after finalize = %q", got)
	}
	w3, _, err := o.Generation(fa.Generation).NewWriterFromAppendableObject(ctx, nil)
	if err == nil {
		_, _ = w3.Write([]byte("!"))
		err = w3.Close()
	}
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("append to a finalized object = %v, want FAILED_PRECONDITION", err)
	}
}

func readString(t *testing.T, h *Harness, o *storage.ObjectHandle) string {
	t.Helper()
	r, err := o.NewReader(h.Context())
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

// TestStorageGRPCSetIamPolicyUpdateMask: SetIamPolicy's update_mask, which
// the Go client never sends, through the google.storage.v2 stub generated
// from Google's protos (#1443): fields outside the mask keep the stored
// policy's values, an etag outside it is not checked, and the result reads
// back through the official JSON client. custom_placement_config cannot be
// changed, and an ip_filter mode other than Enabled or Disabled is refused
// (TestStorageIPFilterGRPC stores one).
// covers: google.storage.v2.Storage/SetIamPolicy, google.storage.v2.Storage/UpdateBucket
func TestStorageGRPCSetIamPolicyUpdateMask(t *testing.T) {
	h := New(t)
	j := storageClient(t, h)
	ctx := h.Context()
	name := h.Project() + "-grpc-mask"
	if err := j.Bucket(name).Create(ctx, h.Project(), nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { emptyAndDelete(ctx, j.Bucket(name)) })
	conn, err := grpc.NewClient(stripScheme(h.Endpoint(EnvStorage)), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	c := pb.NewStorageClient(conn)
	res := "projects/_/buckets/" + name
	set := func(p *iampb.Policy, paths ...string) (*iampb.Policy, error) {
		return c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: res, Policy: p, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}})
	}
	viewer := &iampb.Binding{Role: "roles/storage.objectViewer", Members: []string{"user:a@example.com"}}
	if _, err := set(&iampb.Policy{Bindings: []*iampb.Binding{viewer}}, "bindings"); err != nil {
		t.Fatalf("SetIamPolicy bindings: %v", err)
	}
	got, err := set(&iampb.Policy{Version: 3}, "version")
	if err != nil || got.GetVersion() != 3 || len(got.GetBindings()) != 1 {
		t.Fatalf("SetIamPolicy version only = %v, %v; want the binding kept", got, err)
	}
	if jp, err := j.Bucket(name).IAM().Policy(ctx); err != nil || !jp.HasRole("user:a@example.com", "roles/storage.objectViewer") {
		t.Fatalf("JSON policy = %v, %v", jp, err)
	}
	stale := []byte("BwXXXXXXXXX=")
	if _, err := set(&iampb.Policy{Etag: stale, Bindings: []*iampb.Binding{viewer}}, "bindings", "etag"); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("stale etag under the mask = %v, want FAILED_PRECONDITION", err)
	}
	if _, err := set(&iampb.Policy{Etag: stale, Bindings: []*iampb.Binding{viewer}}, "bindings"); err != nil {
		t.Errorf("stale etag outside the mask: %v", err)
	}
	if _, err := set(&iampb.Policy{}, "owners"); status.Code(err) != codes.InvalidArgument {
		t.Errorf("unknown mask path = %v, want INVALID_ARGUMENT", err)
	}

	_, err = c.UpdateBucket(ctx, &pb.UpdateBucketRequest{Bucket: &pb.Bucket{Name: res,
		CustomPlacementConfig: &pb.Bucket_CustomPlacementConfig{DataLocations: []string{"us-central1-b"}}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"custom_placement_config"}}})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("changing custom_placement_config = %v, want INVALID_ARGUMENT", err)
	}
	mode := "On"
	_, err = c.UpdateBucket(ctx, &pb.UpdateBucketRequest{Bucket: &pb.Bucket{Name: res, IpFilter: &pb.Bucket_IpFilter{Mode: &mode}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"ip_filter"}}})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("ip_filter mode On = %v, want INVALID_ARGUMENT", err)
	}
}

// TestGcloudStorageRapidBucket: gcloud storage creates a Rapid storage
// bucket in one zone (#1443), over the JSON API, and the official gRPC
// client reads its placement back.
func TestGcloudStorageRapidBucket(t *testing.T) {
	h := New(t)
	gs := newGcloudSession(t, h)
	g := storageGRPCClient(t, h, false)
	name := h.Project() + "-gcloud-rapid"
	gs.must("storage", "buckets", "create", "gs://"+name, "--location=us-central1", "--placement=us-central1-a", "--default-storage-class=RAPID")
	t.Cleanup(func() { _, _ = gs.run(nil, "storage", "rm", "-r", "gs://"+name) })
	a, err := g.Bucket(name).Attrs(h.Context())
	if err != nil || a.StorageClass != "RAPID" || a.CustomPlacementConfig == nil || !strings.EqualFold(strings.Join(a.CustomPlacementConfig.DataLocations, ","), "us-central1-a") {
		t.Fatalf("gRPC Attrs = %+v, %v", a, err)
	}
	if out := gs.must("storage", "buckets", "describe", "gs://"+name, "--format=json"); !strings.Contains(strings.ToLower(out), "us-central1-a") {
		t.Errorf("describe does not show the placement:\n%s", out)
	}
}
