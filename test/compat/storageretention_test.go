//go:build compat

package compat

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
)

// Retention policies, holds and object retention (#500), to the bucket-lock,
// object-holds and object-lock docs and the status-codes page's reasons.
// A retained object cannot be deleted, so the buckets these
// leave behind are the server's to discard.

func apiError(err error) (int, string) {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		if len(ge.Errors) > 0 {
			return ge.Code, ge.Errors[0].Reason
		}
		return ge.Code, ""
	}
	return 0, ""
}

// TestStorageRetentionBlocksDelete: an object younger than the bucket's
// retention period cannot be deleted or replaced (403 retentionPolicyNotMet).
// covers: storage.objects.delete, storage.buckets.patch
func TestStorageRetentionBlocksDelete(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()
	bh := c.Bucket(h.Project() + "-retained")
	if err := bh.Create(ctx, h.Project(), &storage.BucketAttrs{RetentionPolicy: &storage.RetentionPolicy{RetentionPeriod: time.Hour}}); err != nil {
		t.Fatal(err)
	}
	o := bh.Object("r.txt")
	a := putObject(t, ctx, o, "x")
	if a.RetentionExpirationTime.Sub(a.Created) != time.Hour {
		t.Errorf("retentionExpirationTime %v for an object created %v; want an hour later", a.RetentionExpirationTime, a.Created)
	}
	if code, reason := apiError(o.Delete(ctx)); code != http.StatusForbidden || reason != "retentionPolicyNotMet" {
		t.Errorf("delete = %d %s; want 403 retentionPolicyNotMet", code, reason)
	}
	w := o.NewWriter(ctx)
	_, _ = w.Write([]byte("y"))
	if code, reason := apiError(w.Close()); code != http.StatusForbidden || reason != "retentionPolicyNotMet" {
		t.Errorf("replace = %d %s; want 403 retentionPolicyNotMet", code, reason)
	}
}

// TestStorageLockRetentionPolicy: locking needs ifMetagenerationMatch (400
// without), and a locked period cannot be reduced (400).
// covers: storage.buckets.patch, storage.buckets.get
func TestStorageLockRetentionPolicy(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()
	name := h.Project() + "-lock"
	bh := c.Bucket(name)
	if err := bh.Create(ctx, h.Project(), &storage.BucketAttrs{RetentionPolicy: &storage.RetentionPolicy{RetentionPeriod: 2 * time.Hour}}); err != nil {
		t.Fatal(err)
	}
	if code, body := rawStorage(t, h, "POST", "/storage/v1/b/"+name+"/lockRetentionPolicy", ""); code != http.StatusBadRequest {
		t.Errorf("lock without ifMetagenerationMatch = %d %s; want 400", code, body)
	}
	a, err := bh.Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := bh.If(storage.BucketConditions{MetagenerationMatch: a.MetaGeneration}).LockRetentionPolicy(ctx); err != nil {
		t.Fatal(err)
	}
	if a, err := bh.Attrs(ctx); err != nil || a.RetentionPolicy == nil || !a.RetentionPolicy.IsLocked {
		t.Fatalf("after lock: %v, %v", a, err)
	}
	_, err = bh.Update(ctx, storage.BucketAttrsToUpdate{RetentionPolicy: &storage.RetentionPolicy{RetentionPeriod: time.Hour}})
	if code, _ := apiError(err); code != http.StatusBadRequest {
		t.Errorf("reduce after lock = %v; want 400", err)
	}
	if _, err := bh.Update(ctx, storage.BucketAttrsToUpdate{RetentionPolicy: &storage.RetentionPolicy{RetentionPeriod: 3 * time.Hour}}); err != nil {
		t.Errorf("increase after lock: %v", err)
	}
	if err := bh.Delete(ctx); err != nil {
		t.Errorf("an empty locked bucket should delete: %v", err)
	}
}

// TestStorageHoldsBlockDelete: a temporary or event-based hold blocks delete
// (403 objectUnderActiveHold), and a metadata patch is still allowed.
// covers: storage.objects.patch, storage.objects.delete
func TestStorageHoldsBlockDelete(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()
	bh := bucket(t, h, c)
	for _, hold := range []storage.ObjectAttrsToUpdate{{TemporaryHold: true}, {EventBasedHold: true}} {
		o := bh.Object("held.txt")
		putObject(t, ctx, o, "x")
		if _, err := o.Update(ctx, hold); err != nil {
			t.Fatal(err)
		}
		if code, reason := apiError(o.Delete(ctx)); code != http.StatusForbidden || reason != "objectUnderActiveHold" {
			t.Errorf("delete under %+v = %d %s; want 403 objectUnderActiveHold", hold, code, reason)
		}
		if _, err := o.Update(ctx, storage.ObjectAttrsToUpdate{Metadata: map[string]string{"k": "v"}}); err != nil {
			t.Errorf("a metadata patch under a hold: %v", err)
		}
		if _, err := o.Update(ctx, storage.ObjectAttrsToUpdate{TemporaryHold: false, EventBasedHold: false}); err != nil {
			t.Fatal(err)
		}
		if err := o.Delete(ctx); err != nil {
			t.Errorf("delete after releasing the hold: %v", err)
		}
	}
}

// TestStorageObjectRetentionRequiresEnabledBucket: object retention needs a
// bucket created with enableObjectRetention=true (400 invalid otherwise),
// and then blocks delete (403 retentionPolicyNotMet).
// covers: storage.buckets.insert, storage.objects.patch
func TestStorageObjectRetentionRequiresEnabledBucket(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()
	until := time.Now().Add(time.Hour)
	plain := bucket(t, h, c)
	putObject(t, ctx, plain.Object("o"), "x")
	_, err := plain.Object("o").Update(ctx, storage.ObjectAttrsToUpdate{Retention: &storage.ObjectRetention{Mode: "Unlocked", RetainUntil: until}})
	if code, reason := apiError(err); code != http.StatusBadRequest || reason != "invalid" {
		t.Errorf("retention on a bucket without it = %d %s; want 400 invalid", code, reason)
	}
	enabled := c.Bucket(h.Project() + "-objret").SetObjectRetention(true)
	if err := enabled.Create(ctx, h.Project(), nil); err != nil {
		t.Fatal(err)
	}
	if a, err := enabled.Attrs(ctx); err != nil || a.ObjectRetentionMode != "Enabled" {
		t.Fatalf("objectRetention = %v, %v", a, err)
	}
	o := enabled.Object("o")
	putObject(t, ctx, o, "x")
	a, err := o.Update(ctx, storage.ObjectAttrsToUpdate{Retention: &storage.ObjectRetention{Mode: "Unlocked", RetainUntil: until}})
	if err != nil || a.Retention == nil || a.Retention.Mode != "Unlocked" {
		t.Fatalf("set retention = %v, %v", a, err)
	}
	if code, reason := apiError(o.Delete(ctx)); code != http.StatusForbidden || reason != "retentionPolicyNotMet" {
		t.Errorf("delete under object retention = %d %s; want 403 retentionPolicyNotMet", code, reason)
	}
}
