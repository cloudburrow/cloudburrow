//go:build compat

package compat

import (
	"errors"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	storagev1 "google.golang.org/api/storage/v1"
)

// apiStatus is the HTTP status of a JSON API error, 0 for anything else.
func apiStatus(err error) (int, string) {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		return ge.Code, ge.Message
	}
	return 0, ""
}

// TestStorageHardDeletePause (#1702, #1715): softDeletePolicy.hardDeletePause
// through Google's generated JSON API client (discovery revision 20261009),
// with the rules of https://docs.cloud.google.com/storage/docs/pause-hard-deletions.
//
//   - buckets.insert with the pause enabled is refused (400) and makes no
//     bucket.
//   - buckets.patch pausing a bucket without soft delete is refused (412).
//   - buckets.patch with hardDeletePause.enabled true takes it, keeps the
//     retention, and answers the pause's effectiveTime; buckets.get reads
//     it back unchanged.
//   - Under the pause an object the official client deletes is listed with
//     softDeleted=true and objects.restore makes it live again.
//   - Resuming within 24 hours is refused (412); so is disabling soft
//     delete (412), while changing the retention is taken; buckets.delete
//     is refused (409) and the bucket stays.
//
// The status of each refusal is UNVERIFIED (the page lists none, re-read
// 2026-11-17). The bucket stays paused for 24 hours, as on Google, so it
// outlives the test, empty, in this test's own project. What happens 24
// hours on (resuming, and the hard delete past hardDeleteTime) needs the
// server's clock moved, so it is unit-tested in internal/service/storage
// (TestStorageHardDeletePauseOncePerDay, TestStorageHardDeletePauseHoldsSweep).
//
// covers: storage.buckets.insert, storage.buckets.patch, storage.buckets.get, storage.buckets.delete
// unverified: storage.buckets.insert 400: hardDeletePause enabled at creation
// Docs re-read 2026-11-17, still unanswered: https://docs.cloud.google.com/storage/docs/pause-hard-deletions
// unverified: storage.buckets.patch 412: pausing hard deletions on a bucket without soft delete (softDeletePolicyNotSet)
// Docs re-read 2026-11-17, still unanswered: https://docs.cloud.google.com/storage/docs/pause-hard-deletions
// unverified: storage.buckets.patch 412: pausing or resuming hard deletions within 24 hours of the last change
// Docs re-read 2026-11-17, still unanswered: https://docs.cloud.google.com/storage/docs/pause-hard-deletions
// unverified: storage.buckets.patch 412: disabling soft delete while hard deletions are paused
// Docs re-read 2026-11-17, still unanswered: https://docs.cloud.google.com/storage/docs/pause-hard-deletions
// unverified: storage.buckets.delete 409: a bucket whose hard deletions are paused
// Docs re-read 2026-11-17, still unanswered: https://docs.cloud.google.com/storage/docs/pause-hard-deletions
func TestStorageHardDeletePause(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	s := storageJSON(t, h)
	ctx := h.Context()

	refused := h.Project() + "-hdpause-create"
	_, err := s.Buckets.Insert(h.Project(), &storagev1.Bucket{Name: refused, SoftDeletePolicy: &storagev1.BucketSoftDeletePolicy{
		RetentionDurationSeconds: 604800,
		HardDeletePause:          &storagev1.BucketSoftDeletePolicyHardDeletePause{Enabled: true},
	}}).Context(ctx).Do()
	if code, msg := apiStatus(err); code != 400 || !strings.Contains(msg, "during bucket creation") {
		t.Errorf("buckets.insert with the pause = %v; want 400", err)
	}
	if _, err := s.Buckets.Get(refused).Context(ctx).Do(); err == nil {
		t.Errorf("the refused buckets.insert made %s", refused)
	}

	none := c.Bucket(h.Project() + "-hdpause-none")
	if err := none.Create(ctx, h.Project(), &storage.BucketAttrs{SoftDeletePolicy: &storage.SoftDeletePolicy{RetentionDuration: 0}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { emptyAndDelete(ctx, none) })
	_, err = s.Buckets.Patch(none.BucketName(), &storagev1.Bucket{SoftDeletePolicy: &storagev1.BucketSoftDeletePolicy{
		HardDeletePause: &storagev1.BucketSoftDeletePolicyHardDeletePause{Enabled: true},
	}}).Context(ctx).Do()
	if code, _ := apiStatus(err); code != 412 {
		t.Errorf("pausing a bucket without soft delete = %v; want 412", err)
	}

	bh := c.Bucket(h.Project() + "-hdpause")
	if err := bh.Create(ctx, h.Project(), nil); err != nil {
		t.Fatal(err)
	}
	b := bh.BucketName()

	got, err := s.Buckets.Patch(b, &storagev1.Bucket{SoftDeletePolicy: &storagev1.BucketSoftDeletePolicy{
		HardDeletePause: &storagev1.BucketSoftDeletePolicyHardDeletePause{Enabled: true},
	}}).Context(ctx).Do()
	if err != nil {
		t.Fatalf("buckets.patch hardDeletePause enabled: %v", err)
	}
	p := got.SoftDeletePolicy
	if p == nil || p.HardDeletePause == nil || !p.HardDeletePause.Enabled || p.RetentionDurationSeconds != 604800 {
		t.Fatalf("softDeletePolicy after enabling the pause = %+v; want it enabled with the 7-day retention kept", p)
	}
	eff, err := time.Parse(time.RFC3339, p.HardDeletePause.EffectiveTime)
	if err != nil || time.Since(eff) > time.Hour || time.Until(eff) > time.Minute {
		t.Errorf("hardDeletePause.effectiveTime = %q, %v; want about now", p.HardDeletePause.EffectiveTime, err)
	}
	if again, err := s.Buckets.Get(b).Context(ctx).Do(); err != nil || again.SoftDeletePolicy == nil ||
		again.SoftDeletePolicy.HardDeletePause == nil || !again.SoftDeletePolicy.HardDeletePause.Enabled ||
		again.SoftDeletePolicy.HardDeletePause.EffectiveTime != p.HardDeletePause.EffectiveTime {
		t.Errorf("buckets.get after enabling the pause = %+v, %v", again, err)
	}

	putText(t, ctx, bh, "paused.txt")
	if err := bh.Object("paused.txt").Delete(ctx); err != nil {
		t.Fatal(err)
	}
	soft := softDeletedObjects(t, h, bh)
	if len(soft) != 1 || soft[0].HardDeleteTime.IsZero() {
		t.Fatalf("softDeleted listing under the pause = %+v; want paused.txt with its hardDeleteTime", soft)
	}
	if _, err := bh.Object("paused.txt").Generation(soft[0].Generation).Restore(ctx, &storage.RestoreOptions{}); err != nil {
		t.Errorf("objects.restore under the pause: %v", err)
	}
	if err := bh.Object("paused.txt").Delete(ctx); err != nil {
		t.Fatal(err)
	}

	_, err = s.Buckets.Patch(b, &storagev1.Bucket{SoftDeletePolicy: &storagev1.BucketSoftDeletePolicy{
		HardDeletePause: &storagev1.BucketSoftDeletePolicyHardDeletePause{Enabled: false, ForceSendFields: []string{"Enabled"}},
	}}).Context(ctx).Do()
	if code, msg := apiStatus(err); code != 412 || !strings.Contains(msg, "once every 24 hours") {
		t.Errorf("resuming right after the pause = %v; want 412", err)
	}
	_, err = s.Buckets.Patch(b, &storagev1.Bucket{SoftDeletePolicy: &storagev1.BucketSoftDeletePolicy{
		RetentionDurationSeconds: 0, ForceSendFields: []string{"RetentionDurationSeconds"},
	}}).Context(ctx).Do()
	if code, _ := apiStatus(err); code != 412 {
		t.Errorf("disabling soft delete while paused = %v; want 412", err)
	}
	got, err = s.Buckets.Patch(b, &storagev1.Bucket{SoftDeletePolicy: &storagev1.BucketSoftDeletePolicy{
		RetentionDurationSeconds: 1209600,
	}}).Context(ctx).Do()
	if err != nil || got.SoftDeletePolicy.RetentionDurationSeconds != 1209600 ||
		got.SoftDeletePolicy.HardDeletePause == nil || !got.SoftDeletePolicy.HardDeletePause.Enabled {
		t.Errorf("changing the retention while paused = %+v, %v; want 14 days, still paused", got, err)
	}

	err = s.Buckets.Delete(b).Context(ctx).Do()
	if code, msg := apiStatus(err); code != 409 || !strings.Contains(msg, "while hard deletions are paused") {
		t.Errorf("buckets.delete while paused = %v; want 409", err)
	}
	if _, err := bh.Attrs(ctx); err != nil {
		t.Errorf("the bucket after the refused delete: %v", err)
	}
}
