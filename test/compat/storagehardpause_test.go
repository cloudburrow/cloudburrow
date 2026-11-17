//go:build compat

package compat

import (
	"testing"
	"time"

	"cloud.google.com/go/storage"
	storagev1 "google.golang.org/api/storage/v1"
)

// TestStorageHardDeletePause (#1702): softDeletePolicy.hardDeletePause
// through Google's generated JSON API client (discovery revision 20261009).
//
//   - buckets.patch with hardDeletePause.enabled true takes it, keeps the
//     retention, and answers the pause's effectiveTime; buckets.get reads
//     it back unchanged.
//   - Under the pause an object the official client deletes is listed with
//     softDeleted=true and objects.restore makes it live again.
//   - Disabling the pause drops its effectiveTime.
//
// What the pause does past an object's hardDeleteTime (the sweep skips it,
// reads keep it) needs a clock seven days on, so it is unit-tested in
// internal/service/storage (TestStorageHardDeletePauseHoldsSweep).
//
// covers: storage.buckets.patch, storage.buckets.get
func TestStorageHardDeletePause(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	s := storageJSON(t, h)
	ctx := h.Context()
	bh := c.Bucket(h.Project() + "-hdpause")
	if err := bh.Create(ctx, h.Project(), nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { emptyAndDelete(ctx, bh) })
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

	got, err = s.Buckets.Patch(b, &storagev1.Bucket{SoftDeletePolicy: &storagev1.BucketSoftDeletePolicy{
		HardDeletePause: &storagev1.BucketSoftDeletePolicyHardDeletePause{Enabled: false, ForceSendFields: []string{"Enabled"}},
	}}).Context(ctx).Do()
	if err != nil {
		t.Fatalf("buckets.patch hardDeletePause disabled: %v", err)
	}
	if hp := got.SoftDeletePolicy.HardDeletePause; hp != nil && (hp.Enabled || hp.EffectiveTime != "") {
		t.Errorf("hardDeletePause after disabling it = %+v; want it off without effectiveTime", hp)
	}
}
