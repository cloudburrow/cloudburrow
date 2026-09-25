package storage

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gcs "cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"

	"github.com/cloudburrow/cloudburrow/internal/sched"
)

func apiCode(err error) (int, string) {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		reason := ""
		if len(ge.Errors) > 0 {
			reason = ge.Errors[0].Reason
		}
		return ge.Code, reason
	}
	return 0, ""
}

// Releasing an event-based hold restarts the retention clock; while held
// there is no retentionExpirationTime (#500).
func TestStorageEventHoldRestartsRetention(t *testing.T) {
	clock := sched.NewFakeClock(time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC))
	s, err := NewServer(Options{Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(s)
	t.Cleanup(h.Close)
	if code, body := raw(t, "POST", h.URL+"/storage/v1/b?project=p",
		`{"name":"held","retentionPolicy":{"retentionPeriod":"3600"},"defaultEventBasedHold":true}`); code != 200 {
		t.Fatal(body)
	}
	code, body := raw(t, "POST", h.URL+"/upload/storage/v1/b/held/o?uploadType=media&name=o&prettyPrint=false", "x")
	if code != 200 || !strings.Contains(body, `"eventBasedHold":true`) || strings.Contains(body, "retentionExpirationTime") {
		t.Fatalf("a new object under defaultEventBasedHold = %d %s", code, body)
	}
	clock.Advance(2 * time.Hour)
	if code, body := raw(t, "DELETE", h.URL+"/storage/v1/b/held/o/o", ""); code != 403 || !strings.Contains(body, "objectUnderActiveHold") {
		t.Errorf("delete under a hold = %d %s", code, body)
	}
	code, body = raw(t, "PATCH", h.URL+"/storage/v1/b/held/o/o", `{"eventBasedHold":false}`)
	var o struct {
		Expires string `json:"retentionExpirationTime"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &o) != nil || o.Expires != rfc3339(clock.Now().Add(time.Hour)) {
		t.Fatalf("release = %d %s; want retentionExpirationTime an hour from the release", code, body)
	}
	if code, body := raw(t, "DELETE", h.URL+"/storage/v1/b/held/o/o", ""); code != 403 || !strings.Contains(body, "retentionPolicyNotMet") {
		t.Errorf("delete an hour before the restarted period ends = %d %s", code, body)
	}
	clock.Advance(time.Hour)
	if code, body := raw(t, "DELETE", h.URL+"/storage/v1/b/held/o/o", ""); code != http.StatusNoContent {
		t.Errorf("delete after the period = %d %s", code, body)
	}
}

// A retention policy blocks delete and replace of a young object; a
// versioned bucket may still make it noncurrent.
func TestStorageRetentionBlocksDeleteInProcess(t *testing.T) {
	_, bh, _ := sdkBucket(t, "retained")
	ctx := context.Background()
	if _, err := bh.Update(ctx, gcs.BucketAttrsToUpdate{RetentionPolicy: &gcs.RetentionPolicy{RetentionPeriod: time.Hour}}); err != nil {
		t.Fatal(err)
	}
	o := bh.Object("r.txt")
	a := write(t, o, []byte("x"), nil)
	if a.RetentionExpirationTime.Sub(a.Created) != time.Hour {
		t.Errorf("retentionExpirationTime = %v, created %v", a.RetentionExpirationTime, a.Created)
	}
	if code, reason := apiCode(o.Delete(ctx)); code != 403 || reason != "retentionPolicyNotMet" {
		t.Errorf("delete = %d %s", code, reason)
	}
	w := o.NewWriter(ctx)
	_, _ = w.Write([]byte("y"))
	if code, reason := apiCode(w.Close()); code != 403 || reason != "retentionPolicyNotMet" {
		t.Errorf("replace = %d %s", code, reason)
	}
	if _, err := o.Update(ctx, gcs.ObjectAttrsToUpdate{Metadata: map[string]string{"k": "v"}}); err != nil {
		t.Errorf("a metadata patch under retention: %v", err)
	}
	if _, err := bh.Update(ctx, gcs.BucketAttrsToUpdate{VersioningEnabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := o.Delete(ctx); err != nil {
		t.Errorf("making a retained version noncurrent: %v", err)
	}
	if code, reason := apiCode(o.Generation(a.Generation).Delete(ctx)); code != 403 || reason != "retentionPolicyNotMet" {
		t.Errorf("deleting the retained noncurrent version = %d %s", code, reason)
	}
}

// Locking needs ifMetagenerationMatch and a policy; a locked period grows
// but does not shrink or go.
func TestStorageLockRetentionPolicyInProcess(t *testing.T) {
	_, bh, h := sdkBucket(t, "locked")
	ctx := context.Background()
	base := h.URL + "/storage/v1/b/locked"
	if code, body := raw(t, "POST", base+"/lockRetentionPolicy", ""); code != 400 {
		t.Errorf("lock without ifMetagenerationMatch = %d %s", code, body)
	}
	if code, body := raw(t, "POST", base+"/lockRetentionPolicy?ifMetagenerationMatch=1", ""); code != 400 || !strings.Contains(body, "badRequest") {
		t.Errorf("lock without a policy = %d %s", code, body)
	}
	a, err := bh.Update(ctx, gcs.BucketAttrsToUpdate{RetentionPolicy: &gcs.RetentionPolicy{RetentionPeriod: 2 * time.Hour}})
	if err != nil {
		t.Fatal(err)
	}
	if err := bh.If(gcs.BucketConditions{MetagenerationMatch: a.MetaGeneration - 1}).LockRetentionPolicy(ctx); err == nil {
		t.Error("lock with a stale metageneration succeeded")
	}
	if err := bh.If(gcs.BucketConditions{MetagenerationMatch: a.MetaGeneration}).LockRetentionPolicy(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := bh.Attrs(ctx)
	if err != nil || got.RetentionPolicy == nil || !got.RetentionPolicy.IsLocked {
		t.Fatalf("after lock = %+v, %v", got.RetentionPolicy, err)
	}
	_, err = bh.Update(ctx, gcs.BucketAttrsToUpdate{RetentionPolicy: &gcs.RetentionPolicy{RetentionPeriod: time.Hour}})
	if code, reason := apiCode(err); code != 400 || reason != "badRequestException" {
		t.Errorf("reduce after lock = %d %s", code, reason)
	}
	if code, body := raw(t, "PATCH", base, `{"retentionPolicy":null}`); code != 400 {
		t.Errorf("remove after lock = %d %s", code, body)
	}
	if _, err := bh.Update(ctx, gcs.BucketAttrsToUpdate{RetentionPolicy: &gcs.RetentionPolicy{RetentionPeriod: 3 * time.Hour}}); err != nil {
		t.Errorf("increase after lock: %v", err)
	}
	if code, _ := raw(t, "POST", h.URL+"/storage/v1/b?project=p", `{"name":"too-long","retentionPolicy":{"retentionPeriod":"3155760001"}}`); code != 400 {
		t.Errorf("a period over 100 years = %d", code)
	}
}

// Both holds block delete and replace; metadata stays editable.
func TestStorageHoldsBlockDeleteInProcess(t *testing.T) {
	_, bh, _ := sdkBucket(t, "holds")
	ctx := context.Background()
	for _, hold := range []gcs.ObjectAttrsToUpdate{{TemporaryHold: true}, {EventBasedHold: true}} {
		o := bh.Object("h.txt")
		write(t, o, []byte("x"), nil)
		if _, err := o.Update(ctx, hold); err != nil {
			t.Fatal(err)
		}
		if code, reason := apiCode(o.Delete(ctx)); code != 403 || reason != "objectUnderActiveHold" {
			t.Errorf("delete under %+v = %d %s", hold, code, reason)
		}
		if _, err := o.Update(ctx, gcs.ObjectAttrsToUpdate{ContentType: "text/csv"}); err != nil {
			t.Errorf("a metadata patch under a hold: %v", err)
		}
		if _, err := o.Update(ctx, gcs.ObjectAttrsToUpdate{TemporaryHold: false, EventBasedHold: false}); err != nil {
			t.Fatal(err)
		}
		if err := o.Delete(ctx); err != nil {
			t.Errorf("delete after release: %v", err)
		}
	}
}

// Object retention needs a bucket created with it enabled; an Unlocked one
// needs overrideUnlockedRetention to shorten, and a Locked one only grows.
func TestStorageObjectRetentionRequiresEnabledBucketInProcess(t *testing.T) {
	c, _ := sdk(t)
	ctx := context.Background()
	plain := c.Bucket("plain-bucket")
	if err := plain.Create(ctx, "p", nil); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	write(t, plain.Object("o"), []byte("x"), nil)
	_, err := plain.Object("o").Update(ctx, gcs.ObjectAttrsToUpdate{Retention: &gcs.ObjectRetention{Mode: "Unlocked", RetainUntil: until}})
	if code, reason := apiCode(err); code != 400 || reason != "invalid" {
		t.Errorf("retention on a bucket without it = %d %s", code, reason)
	}
	enabled := c.Bucket("retention-bucket").SetObjectRetention(true)
	if err := enabled.Create(ctx, "p", nil); err != nil {
		t.Fatal(err)
	}
	if a, err := enabled.Attrs(ctx); err != nil || a.ObjectRetentionMode != "Enabled" {
		t.Fatalf("objectRetention = %+v, %v", a, err)
	}
	o := enabled.Object("o")
	write(t, o, []byte("x"), nil)
	if _, err := o.Update(ctx, gcs.ObjectAttrsToUpdate{Retention: &gcs.ObjectRetention{Mode: "Unlocked", RetainUntil: until}}); err != nil {
		t.Fatal(err)
	}
	if code, reason := apiCode(o.Delete(ctx)); code != 403 || reason != "retentionPolicyNotMet" {
		t.Errorf("delete under object retention = %d %s", code, reason)
	}
	shorter := &gcs.ObjectRetention{Mode: "Unlocked", RetainUntil: until.Add(-time.Minute)}
	if code, reason := apiCode(func() error { _, err := o.Update(ctx, gcs.ObjectAttrsToUpdate{Retention: shorter}); return err }()); code != 403 || reason != "forbidden" {
		t.Errorf("shorten without override = %d %s", code, reason)
	}
	if _, err := o.OverrideUnlockedRetention(true).Update(ctx, gcs.ObjectAttrsToUpdate{Retention: shorter}); err != nil {
		t.Errorf("shorten with override: %v", err)
	}
	locked := &gcs.ObjectRetention{Mode: "Locked", RetainUntil: until}
	if _, err := o.OverrideUnlockedRetention(true).Update(ctx, gcs.ObjectAttrsToUpdate{Retention: locked}); err != nil {
		t.Fatal(err)
	}
	_, err = o.OverrideUnlockedRetention(true).Update(ctx, gcs.ObjectAttrsToUpdate{Retention: shorter})
	if code, reason := apiCode(err); code != 403 || reason != "forbidden" {
		t.Errorf("shorten a Locked retention = %d %s", code, reason)
	}
	_, err = o.Update(ctx, gcs.ObjectAttrsToUpdate{EventBasedHold: true})
	if code, reason := apiCode(err); code != 400 || reason != "invalid" {
		t.Errorf("an event-based hold with retention = %d %s", code, reason)
	}
}
