//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/storage"
	control "cloud.google.com/go/storage/control/apiv2"
	"cloud.google.com/go/storage/control/apiv2/controlpb"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	storagev1 "google.golang.org/api/storage/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// Anywhere Caches, bucket relocation and operation cancel (#1444), through
// Google's generated JSON API client (google.golang.org/api/storage/v1) and
// the official Storage Control client's gRPC transport
// (cloud.google.com/go/storage/control/apiv2, NewStorageControlClient: its
// REST transport has no path for Anywhere Caches). A cache is a record: no
// cache tier exists locally, so reads are unaffected.

// locatedBucket is a bucket in loc, with optional custom placement.
func locatedBucket(t *testing.T, h *Harness, suffix, loc string, placement ...string) string {
	t.Helper()
	c := storageClient(t, h)
	bh := c.Bucket(h.Project() + suffix)
	attrs := &storage.BucketAttrs{Location: loc}
	if len(placement) > 0 {
		attrs.CustomPlacementConfig = &storage.CustomPlacementConfig{DataLocations: placement}
	}
	if err := bh.Create(h.Context(), h.Project(), attrs); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { emptyAndDelete(context.Background(), bh) })
	return bh.BucketName()
}

func lastSegment(name string) string { return name[strings.LastIndex(name, "/")+1:] }

// opMetadata decodes an operation's metadata.
func opMetadata(t *testing.T, op *storagev1.GoogleLongrunningOperation) map[string]any {
	t.Helper()
	m := map[string]any{}
	if err := json.Unmarshal(op.Metadata, &m); err != nil {
		t.Fatalf("operation metadata %s: %v", op.Metadata, err)
	}
	return m
}

// waitOp polls an operation until it is done.
func waitOp(t *testing.T, s *storagev1.Service, bucket, id string) *storagev1.GoogleLongrunningOperation {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		op, err := s.Operations.Get(bucket, id).Do()
		if err != nil {
			t.Fatalf("operations.get %s: %v", id, err)
		}
		if op.Done || time.Now().After(deadline) {
			return op
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// TestStorageAnywhereCachesJSON: insert returns a create operation and the
// cache is creating until it is done, then running; a second cache in the
// zone, and one outside the bucket's location, are refused; update is an
// operation during which pendingUpdate is true; pause, resume and disable
// move the state; a create can be cancelled, which removes the cache, and an
// update cannot. Reading an object is unaffected by its bucket's caches.
//
// unverified: storage.anywhereCaches.insert 409: a second cache in the zone (ANYWHERE_CACHE_ALREADY_EXISTS)
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/storage/docs/anywhere-cache
// unverified: storage.anywhereCaches.insert 400: a zone outside the bucket's location
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/storage/docs/anywhere-cache
// unverified: storage.anywhereCaches.update 400: another update while one is pending
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/storage/docs/anywhere-cache
// unverified: storage.anywhereCaches.pause 400: a cache that is not running
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/storage/docs/anywhere-cache
// unverified: storage.buckets.operations.cancel 400: an Anywhere Cache update's operation, and a done one
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/storage/docs/anywhere-cache
// covers: storage.anywhereCaches.insert, storage.anywhereCaches.get, storage.anywhereCaches.list, storage.anywhereCaches.update
// covers: storage.anywhereCaches.pause, storage.anywhereCaches.resume, storage.anywhereCaches.disable, storage.buckets.operations.cancel
func TestStorageAnywhereCachesJSON(t *testing.T) {
	h := New(t)
	s := storageJSON(t, h)
	b := locatedBucket(t, h, "-acj", "US-CENTRAL1")
	putText(t, h.Context(), storageClient(t, h).Bucket(b), "o.txt")

	op, err := s.AnywhereCaches.Insert(b, &storagev1.AnywhereCache{Zone: "us-central1-a", Ttl: "172800s"}).Do()
	if err != nil || op.Done || op.Kind != "storage#operation" {
		t.Fatalf("insert = %+v, %v; want a running operation", op, err)
	}
	if md := opMetadata(t, op); md["anywhereCacheId"] != "us-central1-a" || md["ttl"] != "172800s" ||
		md["@type"] != "type.googleapis.com/google.storage.control.v2.CreateAnywhereCacheMetadata" {
		t.Errorf("create metadata = %v", md)
	}
	if c, err := s.AnywhereCaches.Get(b, "us-central1-a").Do(); err != nil || c.State != "creating" || c.AdmissionPolicy != "admit-on-first-miss" {
		t.Errorf("get while creating = %+v, %v", c, err)
	}
	if _, err := s.AnywhereCaches.Insert(b, &storagev1.AnywhereCache{Zone: "us-central1-a"}).Do(); httpCode(err) != http.StatusConflict {
		t.Errorf("second cache in the zone = %v; want 409", err)
	}
	if _, err := s.AnywhereCaches.Insert(b, &storagev1.AnywhereCache{Zone: "europe-west1-b"}).Do(); httpCode(err) != http.StatusBadRequest {
		t.Errorf("cache outside the location = %v; want 400", err)
	}
	if done := waitOp(t, s, b, lastSegment(op.Name)); !done.Done || done.Error != nil {
		t.Fatalf("create operation = %+v", done)
	}
	if c, err := s.AnywhereCaches.Get(b, "us-central1-a").Do(); err != nil || c.State != "running" || c.Ttl != "172800s" || c.Bucket != b {
		t.Errorf("get after create = %+v, %v", c, err)
	}

	up, err := s.AnywhereCaches.Update(b, "us-central1-a", &storagev1.AnywhereCache{Ttl: "86400s", AdmissionPolicy: "admit-on-second-miss"}).Do()
	if err != nil || up.Done {
		t.Fatalf("update = %+v, %v", up, err)
	}
	if c, err := s.AnywhereCaches.Get(b, "us-central1-a").Do(); err != nil || !c.PendingUpdate {
		t.Errorf("get while updating = %+v, %v; want pendingUpdate", c, err)
	}
	if _, err := s.AnywhereCaches.Update(b, "us-central1-a", &storagev1.AnywhereCache{Ttl: "3600s"}).Do(); httpCode(err) != http.StatusBadRequest {
		t.Errorf("second update while pending = %v; want 400", err)
	}
	if err := s.Operations.Cancel(b, lastSegment(up.Name)).Do(); httpCode(err) != http.StatusBadRequest {
		t.Errorf("cancel an update = %v; want 400", err)
	}
	waitOp(t, s, b, lastSegment(up.Name))
	if c, err := s.AnywhereCaches.Get(b, "us-central1-a").Do(); err != nil || c.PendingUpdate || c.Ttl != "86400s" || c.AdmissionPolicy != "admit-on-second-miss" {
		t.Errorf("get after update = %+v, %v", c, err)
	}

	for _, step := range []struct {
		do   func(...googleapi.CallOption) (*storagev1.AnywhereCache, error)
		want string
	}{
		{s.AnywhereCaches.Pause(b, "us-central1-a").Do, "paused"},
		{s.AnywhereCaches.Resume(b, "us-central1-a").Do, "running"},
		{s.AnywhereCaches.Disable(b, "us-central1-a").Do, "disabled"},
	} {
		if c, err := step.do(); err != nil || c.State != step.want {
			t.Errorf("state change = %+v, %v; want %s", c, err, step.want)
		}
	}
	if _, err := s.AnywhereCaches.Pause(b, "us-central1-a").Do(); httpCode(err) != http.StatusBadRequest {
		t.Errorf("pause a disabled cache = %v; want 400", err)
	}
	// The object is read from the bucket, cache or none.
	if r, err := storageClient(t, h).Bucket(b).Object("o.txt").NewReader(h.Context()); err != nil {
		t.Errorf("read with a disabled cache: %v", err)
	} else {
		_ = r.Close()
	}

	op2, err := s.AnywhereCaches.Insert(b, &storagev1.AnywhereCache{Zone: "us-central1-b"}).Do()
	if err != nil {
		t.Fatalf("insert b: %v", err)
	}
	if err := s.Operations.Cancel(b, lastSegment(op2.Name)).Do(); err != nil {
		t.Fatalf("cancel a create: %v", err)
	}
	got, err := s.Operations.Get(b, lastSegment(op2.Name)).Do()
	if err != nil || !got.Done || got.Error == nil || got.Error.Code != int64(codes.Canceled) || opMetadata(t, got)["commonMetadata"].(map[string]any)["requestedCancellation"] != true {
		t.Errorf("cancelled create = %+v, %v", got, err)
	}
	if err := s.Operations.Cancel(b, lastSegment(op2.Name)).Do(); httpCode(err) != http.StatusBadRequest {
		t.Errorf("cancel a done operation = %v; want 400", err)
	}
	l, err := s.AnywhereCaches.List(b).Do()
	if err != nil || len(l.Items) != 1 || l.Items[0].AnywhereCacheId != "us-central1-a" {
		t.Errorf("list = %+v, %v; want the one disabled cache", l, err)
	}
}

func storageControlGRPC(t *testing.T, h *Harness) *control.StorageControlClient {
	t.Helper()
	c, err := control.NewStorageControlClient(h.Context(), option.WithEndpoint(stripScheme(h.Endpoint(EnvStorage))),
		option.WithoutAuthentication(), option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatalf("NewStorageControlClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestStorageAnywhereCachesControl: the Storage Control client's gRPC
// transport creates a cache and waits on its operation, updates it with a
// field mask and waits again, lists, pauses, resumes and disables it, and
// cancels a second create through the long-running operation's Cancel.
//
// covers: storage.anywhereCaches.insert, storage.anywhereCaches.update, storage.anywhereCaches.get, storage.anywhereCaches.list
// covers: storage.anywhereCaches.pause, storage.anywhereCaches.resume, storage.anywhereCaches.disable, storage.buckets.operations.cancel
func TestStorageAnywhereCachesControl(t *testing.T) {
	h := New(t)
	sc := storageControlGRPC(t, h)
	ctx := h.Context()
	b := locatedBucket(t, h, "-acg", "US", "US-CENTRAL1", "US-EAST1")
	parent := "projects/_/buckets/" + b

	if _, err := sc.CreateAnywhereCache(ctx, &controlpb.CreateAnywhereCacheRequest{Parent: parent,
		AnywhereCache: &controlpb.AnywhereCache{Zone: "us-west1-a"}}); grpcCode(err) != codes.InvalidArgument {
		t.Errorf("cache outside the placement = %v; want INVALID_ARGUMENT", err)
	}
	op, err := sc.CreateAnywhereCache(ctx, &controlpb.CreateAnywhereCacheRequest{Parent: parent,
		AnywhereCache: &controlpb.AnywhereCache{Zone: "us-east1-b", Ttl: durationpb.New(48 * time.Hour), AdmissionPolicy: "admit-on-second-miss"}})
	if err != nil {
		t.Fatalf("CreateAnywhereCache: %v", err)
	}
	if md, err := op.Metadata(); err != nil || md.GetZone() != "us-east1-b" || md.GetCommonMetadata().GetType() != "create-anywhere-cache" {
		t.Errorf("create metadata = %v, %v", md, err)
	}
	c, err := op.Wait(ctx)
	name := parent + "/anywhereCaches/us-east1-b"
	if err != nil || c.GetName() != name || c.GetState() != "running" || c.GetTtl().AsDuration() != 48*time.Hour || c.GetAdmissionPolicy() != "admit-on-second-miss" {
		t.Fatalf("created cache = %v, %v", c, err)
	}
	if _, err := sc.CreateAnywhereCache(ctx, &controlpb.CreateAnywhereCacheRequest{Parent: parent,
		AnywhereCache: &controlpb.AnywhereCache{Zone: "us-east1-b"}}); grpcCode(err) != codes.AlreadyExists {
		t.Errorf("second create = %v; want ALREADY_EXISTS", err)
	}
	uop, err := sc.UpdateAnywhereCache(ctx, &controlpb.UpdateAnywhereCacheRequest{
		AnywhereCache: &controlpb.AnywhereCache{Name: name, Ttl: durationpb.New(24 * time.Hour)},
		UpdateMask:    &fieldmaskpb.FieldMask{Paths: []string{"ttl"}}})
	if err != nil {
		t.Fatalf("UpdateAnywhereCache: %v", err)
	}
	if c, err := uop.Wait(ctx); err != nil || c.GetTtl().AsDuration() != 24*time.Hour || c.GetAdmissionPolicy() != "admit-on-second-miss" || c.GetPendingUpdate() {
		t.Errorf("updated cache = %v, %v", c, err)
	}
	if c, err := sc.GetAnywhereCache(ctx, &controlpb.GetAnywhereCacheRequest{Name: name}); err != nil || c.GetZone() != "us-east1-b" {
		t.Errorf("GetAnywhereCache = %v, %v", c, err)
	}
	if c, err := sc.PauseAnywhereCache(ctx, &controlpb.PauseAnywhereCacheRequest{Name: name}); err != nil || c.GetState() != "paused" {
		t.Errorf("PauseAnywhereCache = %v, %v", c, err)
	}
	if _, err := sc.PauseAnywhereCache(ctx, &controlpb.PauseAnywhereCacheRequest{Name: name}); grpcCode(err) != codes.FailedPrecondition {
		t.Errorf("pause a paused cache = %v; want FAILED_PRECONDITION", err)
	}
	if c, err := sc.ResumeAnywhereCache(ctx, &controlpb.ResumeAnywhereCacheRequest{Name: name}); err != nil || c.GetState() != "running" {
		t.Errorf("ResumeAnywhereCache = %v, %v", c, err)
	}
	if c, err := sc.DisableAnywhereCache(ctx, &controlpb.DisableAnywhereCacheRequest{Name: name}); err != nil || c.GetState() != "disabled" {
		t.Errorf("DisableAnywhereCache = %v, %v", c, err)
	}

	op2, err := sc.CreateAnywhereCache(ctx, &controlpb.CreateAnywhereCacheRequest{Parent: parent,
		AnywhereCache: &controlpb.AnywhereCache{Zone: "us-central1-c"}})
	if err != nil {
		t.Fatalf("CreateAnywhereCache c: %v", err)
	}
	if err := sc.LROClient.CancelOperation(ctx, &longrunningpb.CancelOperationRequest{Name: op2.Name()}); err != nil {
		t.Fatalf("CancelOperation: %v", err)
	}
	if _, err := op2.Wait(ctx); grpcCode(err) != codes.Canceled {
		t.Errorf("cancelled create's Wait = %v; want CANCELED", err)
	}
	it := sc.ListAnywhereCaches(ctx, &controlpb.ListAnywhereCachesRequest{Parent: parent})
	n := 0
	for {
		c, err := it.Next()
		if err != nil {
			break
		}
		n++
		if c.GetName() != name {
			t.Errorf("listed %s; want only %s", c.GetName(), name)
		}
	}
	if n != 1 {
		t.Errorf("ListAnywhereCaches = %d caches; want 1", n)
	}
}

// TestStorageRelocate: a dry run is done at once and changes nothing; a
// relocation from a region to another syncs, waits READY for
// advanceRelocateBucket, then moves the bucket; a relocation can be
// cancelled before it is finalized; a relocation between configurable
// dual-regions of one multi-region finishes by itself.
//
// unverified: storage.buckets.relocate 400: the bucket's own location
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/storage/docs/json_api/v1/buckets/relocate
// unverified: storage.buckets.relocate 409: a second relocation while one runs
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/storage/docs/json_api/v1/buckets/relocate
// unverified: storage.buckets.operations.advanceRelocateBucket 400: a relocation still syncing
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/storage/docs/bucket-relocation/overview
// covers: storage.buckets.relocate, storage.buckets.operations.advanceRelocateBucket, storage.buckets.operations.cancel
func TestStorageRelocate(t *testing.T) {
	h := New(t)
	s := storageJSON(t, h)
	b := locatedBucket(t, h, "-rel", "US-CENTRAL1")

	if _, err := s.Buckets.Relocate(b, &storagev1.RelocateBucketRequest{DestinationLocation: "US-CENTRAL1"}).Do(); httpCode(err) != http.StatusBadRequest {
		t.Errorf("relocate to its own location = %v; want 400", err)
	}
	dry, err := s.Buckets.Relocate(b, &storagev1.RelocateBucketRequest{DestinationLocation: "US-EAST1", ValidateOnly: true}).Do()
	if err != nil || !dry.Done || opMetadata(t, dry)["validateOnly"] != true {
		t.Errorf("dry run = %+v, %v", dry, err)
	}
	op, err := s.Buckets.Relocate(b, &storagev1.RelocateBucketRequest{DestinationLocation: "US-EAST1"}).Do()
	if err != nil || op.Done {
		t.Fatalf("relocate = %+v, %v", op, err)
	}
	md := opMetadata(t, op)
	if md["@type"] != "type.googleapis.com/google.storage.control.v2.RelocateBucketMetadata" || md["relocationState"] != "SYNCING" ||
		md["sourceLocation"] != "US-CENTRAL1" || md["destinationLocation"] != "US-EAST1" {
		t.Errorf("relocation metadata = %v", md)
	}
	if _, err := s.Buckets.Relocate(b, &storagev1.RelocateBucketRequest{DestinationLocation: "US-WEST1"}).Do(); httpCode(err) != http.StatusConflict {
		t.Errorf("second relocation = %v; want 409", err)
	}
	id := lastSegment(op.Name)
	if err := s.Operations.AdvanceRelocateBucket(b, id, &storagev1.AdvanceRelocateBucketOperationRequest{}).Do(); httpCode(err) != http.StatusBadRequest {
		t.Errorf("advance while syncing = %v; want 400", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for opMetadata(t, op)["finalizationState"] != "READY" && time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		if op, err = s.Operations.Get(b, id).Do(); err != nil {
			t.Fatal(err)
		}
	}
	if attrs, _ := s.Buckets.Get(b).Do(); attrs == nil || attrs.Location != "US-CENTRAL1" {
		t.Errorf("bucket before finalizing = %+v", attrs)
	}
	if err := s.Operations.AdvanceRelocateBucket(b, id, &storagev1.AdvanceRelocateBucketOperationRequest{Ttl: "7200s"}).Do(); err != nil {
		t.Fatalf("advanceRelocateBucket: %v", err)
	}
	op = waitOp(t, s, b, id)
	if md := opMetadata(t, op); !op.Done || op.Error != nil || md["relocationState"] != "SUCCEEDED" || md["finalizationState"] != "FINALIZED" || md["writeDowntimeExpireTime"] == nil {
		t.Errorf("finalized relocation = %+v %v", op, md)
	}
	if attrs, err := s.Buckets.Get(b).Do(); err != nil || attrs.Location != "US-EAST1" || attrs.LocationType != "region" {
		t.Errorf("bucket after relocation = %+v, %v", attrs, err)
	}

	op, err = s.Buckets.Relocate(b, &storagev1.RelocateBucketRequest{DestinationLocation: "EU"}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Operations.Cancel(b, lastSegment(op.Name)).Do(); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if op, err := s.Operations.Get(b, lastSegment(op.Name)).Do(); err != nil || !op.Done || opMetadata(t, op)["relocationState"] != "CANCELLED" {
		t.Errorf("cancelled relocation = %+v, %v", op, err)
	}
	if attrs, _ := s.Buckets.Get(b).Do(); attrs == nil || attrs.Location != "US-EAST1" {
		t.Errorf("bucket after a cancelled relocation = %+v", attrs)
	}

	d := locatedBucket(t, h, "-reld", "US", "US-CENTRAL1", "US-EAST1")
	op, err = s.Buckets.Relocate(d, &storagev1.RelocateBucketRequest{DestinationLocation: "US",
		DestinationCustomPlacementConfig: &storagev1.RelocateBucketRequestDestinationCustomPlacementConfig{DataLocations: []string{"US-EAST1", "US-WEST1"}}}).Do()
	if err != nil || opMetadata(t, op)["finalizationState"] != "NOT_REQUIRED" {
		t.Fatalf("dual-region relocation = %+v, %v", op, err)
	}
	if op := waitOp(t, s, d, lastSegment(op.Name)); !op.Done || op.Error != nil {
		t.Errorf("dual-region relocation = %+v", op)
	}
	if attrs, err := s.Buckets.Get(d).Do(); err != nil || attrs.CustomPlacementConfig == nil || strings.Join(attrs.CustomPlacementConfig.DataLocations, ",") != "US-EAST1,US-WEST1" {
		t.Errorf("dual-region after relocation = %+v, %v", attrs, err)
	}
}

// TestGcloudStorageAnywhereCachesRelocate: gcloud storage buckets
// anywhere-caches create, describe, list, update, disable and resume, and
// gcloud storage buckets relocate (a dry run, a relocation, --finalize) with
// gcloud storage operations cancel, all through the egress-guarded session.
//
// covers: storage.anywhereCaches.insert, storage.anywhereCaches.get, storage.anywhereCaches.list, storage.anywhereCaches.update
// covers: storage.anywhereCaches.disable, storage.anywhereCaches.resume
// covers: storage.buckets.relocate, storage.buckets.operations.advanceRelocateBucket, storage.buckets.operations.cancel
func TestGcloudStorageAnywhereCachesRelocate(t *testing.T) {
	h := New(t)
	g := newGcloudSession(t, h)
	s := storageJSON(t, h)
	b := locatedBucket(t, h, "-gac", "US-CENTRAL1")
	url := "gs://" + b

	g.must("storage", "buckets", "anywhere-caches", "create", url, "us-central1-a", "--ttl=2d")
	waitCache := func(want string) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for {
			c, err := s.AnywhereCaches.Get(b, "us-central1-a").Do()
			if err == nil && c.State == want && !c.PendingUpdate {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("cache = %+v, %v; want %s", c, err, want)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	waitCache("running")
	if out := g.must("storage", "buckets", "anywhere-caches", "describe", b+"/us-central1-a", "--format=json"); !strings.Contains(out, `"172800s"`) || !strings.Contains(out, "running") {
		t.Errorf("describe = %s", out)
	}
	if out := g.must("storage", "buckets", "anywhere-caches", "list", url, "--format=value(zone)"); !strings.Contains(out, "us-central1-a") {
		t.Errorf("list = %s", out)
	}
	g.must("storage", "buckets", "anywhere-caches", "update", b+"/us-central1-a", "--ttl=1d")
	waitCache("running")
	if c, _ := s.AnywhereCaches.Get(b, "us-central1-a").Do(); c == nil || c.Ttl != "86400s" {
		t.Errorf("after update = %+v", c)
	}
	g.must("storage", "buckets", "anywhere-caches", "disable", b+"/us-central1-a")
	waitCache("disabled")
	g.must("storage", "buckets", "anywhere-caches", "resume", b+"/us-central1-a")
	waitCache("running")

	g.must("storage", "buckets", "relocate", url, "--location=us-east1", "--dry-run")
	if attrs, _ := s.Buckets.Get(b).Do(); attrs == nil || attrs.Location != "US-CENTRAL1" {
		t.Errorf("bucket after a dry run = %+v", attrs)
	}
	g.must("storage", "buckets", "relocate", url, "--location=us-east1")
	ops, err := s.Operations.List(b).Do()
	if err != nil {
		t.Fatal(err)
	}
	var name string
	for _, op := range ops.Operations {
		if !op.Done && strings.Contains(string(op.Metadata), "RelocateBucketMetadata") {
			name = op.Name
		}
	}
	if name == "" {
		t.Fatalf("no running relocation in %+v", ops)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		op, err := s.Operations.Get(b, lastSegment(name)).Do()
		if err == nil && opMetadata(t, op)["finalizationState"] == "READY" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("relocation never READY: %+v, %v", op, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	g.must("storage", "buckets", "relocate", "--finalize", "--operation="+name, "--ttl=2h")
	if op := waitOp(t, s, b, lastSegment(name)); !op.Done || op.Error != nil {
		t.Errorf("finalized relocation = %+v", op)
	}
	if attrs, _ := s.Buckets.Get(b).Do(); attrs == nil || attrs.Location != "US-EAST1" {
		t.Errorf("bucket after relocation = %+v", attrs)
	}

	op, err := s.Buckets.Relocate(b, &storagev1.RelocateBucketRequest{DestinationLocation: "US-WEST1"}).Do()
	if err != nil {
		t.Fatal(err)
	}
	g.must("storage", "operations", "cancel", op.Name)
	if op, err := s.Operations.Get(b, lastSegment(op.Name)).Do(); err != nil || opMetadata(t, op)["relocationState"] != "CANCELLED" {
		t.Errorf("cancelled by gcloud = %+v, %v", op, err)
	}
}

// TestStorageNotificationsWithoutPubSub: on an instance serving no Pub/Sub,
// a notification configuration has no topic to name or deliver to, so
// notifications.insert is refused 501 naming the reason, never accepted and
// dropped; list still answers (empty), and get and delete of a missing
// configuration are 404. Skipped when the instance serves Pub/Sub.
//
// covers: storage.notifications.list
func TestStorageNotificationsWithoutPubSub(t *testing.T) {
	if strings.TrimSpace(os.Getenv(EnvPubSub)) != "" {
		t.Skip("the instance serves Pub/Sub; TestNotificationConfigsCRUDThroughTheSDK covers notifications with it")
	}
	h := New(t)
	s := storageJSON(t, h)
	b := locatedBucket(t, h, "-nps", "US")
	_, err := s.Notifications.Insert(b, &storagev1.Notification{Topic: "//pubsub.googleapis.com/projects/" + h.Project() + "/topics/t", PayloadFormat: "JSON_API_V1"}).Do()
	if httpCode(err) != http.StatusNotImplemented || !strings.Contains(err.Error(), "Pub/Sub") {
		t.Errorf("insert without Pub/Sub = %v; want 501 naming Pub/Sub", err)
	}
	if l, err := s.Notifications.List(b).Do(); err != nil || len(l.Items) != 0 {
		t.Errorf("list = %+v, %v; want none", l, err)
	}
	if _, err := s.Notifications.Get(b, "1").Do(); httpCode(err) != http.StatusNotFound {
		t.Errorf("get = %v; want 404", err)
	}
}
