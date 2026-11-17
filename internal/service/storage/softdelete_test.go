package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gcs "cloud.google.com/go/storage"
	"google.golang.org/api/iterator"

	"github.com/cloudburrow/cloudburrow/internal/sched"
)

func softDeletedOf(t *testing.T, bh *gcs.BucketHandle) []*gcs.ObjectAttrs {
	t.Helper()
	var out []*gcs.ObjectAttrs
	it := bh.Objects(context.Background(), &gcs.Query{SoftDeleted: true})
	for {
		a, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out
		}
		if err != nil {
			t.Fatalf("list softDeleted: %v", err)
		}
		out = append(out, a)
	}
}

// A delete under the default 7-day policy soft-deletes the object: hidden,
// listable with softDeleted=true, readable as metadata by generation, and
// restorable as a new generation with metageneration 1 (#499).
func TestStorageSoftDeleteAndRestoreInProcess(t *testing.T) {
	_, bh, _ := sdkBucket(t, "soft")
	ctx := context.Background()
	o := bh.Object("s.txt")
	first := write(t, o, []byte("kept"), nil)
	if err := o.Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Attrs(ctx); !errors.Is(err, gcs.ErrObjectNotExist) {
		t.Errorf("a soft-deleted object is visible: %v", err)
	}
	soft := softDeletedOf(t, bh)
	if len(soft) != 1 || soft[0].Generation != first.Generation || soft[0].SoftDeleteTime.IsZero() ||
		soft[0].HardDeleteTime.Sub(soft[0].SoftDeleteTime) != 7*24*time.Hour {
		t.Fatalf("soft-deleted = %+v", soft)
	}
	a, err := o.Generation(first.Generation).SoftDeleted().Attrs(ctx)
	if err != nil || a.SoftDeleteTime.IsZero() {
		t.Errorf("soft-deleted attrs = %+v, %v", a, err)
	}
	restored, err := o.Generation(first.Generation).Restore(ctx, &gcs.RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if restored.Generation <= first.Generation || restored.Metageneration != 1 || !restored.SoftDeleteTime.IsZero() {
		t.Errorf("restored = gen %d meta %d soft %v; want a new generation, metageneration 1", restored.Generation, restored.Metageneration, restored.SoftDeleteTime)
	}
	if b := read(t, o, 0, -1); string(b) != "kept" {
		t.Errorf("restored bytes = %q", b)
	}
	if got := softDeletedOf(t, bh); len(got) != 0 {
		t.Errorf("still soft-deleted after restore: %d", len(got))
	}
}

// An overwrite on an unversioned bucket soft-deletes the replaced version,
// and a restore over a live object soft-deletes that one in turn.
func TestStorageSoftDeleteOverwriteAndRestoreOverLive(t *testing.T) {
	_, bh, _ := sdkBucket(t, "soft-over")
	ctx := context.Background()
	o := bh.Object("o.txt")
	first := write(t, o, []byte("one"), nil)
	second := write(t, o, []byte("two"), nil)
	if soft := softDeletedOf(t, bh); len(soft) != 1 || soft[0].Generation != first.Generation {
		t.Fatalf("after an overwrite, soft-deleted = %v", soft)
	}
	if _, err := o.Generation(first.Generation).Restore(ctx, &gcs.RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	if b := read(t, o, 0, -1); string(b) != "one" {
		t.Errorf("live after restoring over it = %q", b)
	}
	if soft := softDeletedOf(t, bh); len(soft) != 1 || soft[0].Generation != second.Generation {
		t.Errorf("the replaced live version is not soft-deleted: %v", soft)
	}
}

func restoreRaw(t *testing.T, base, bucket, name string, gen int64) (int, string) {
	t.Helper()
	return raw(t, "POST", fmt.Sprintf("%s/storage/v1/b/%s/o/%s/restore?generation=%d", base, bucket, name, gen), "")
}

// Restoring a live version is 412 objectNotSoftDeleted.
func TestStorageRestoreNotSoftDeleted412InProcess(t *testing.T) {
	_, bh, h := sdkBucket(t, "not-soft")
	a := write(t, bh.Object("live.txt"), []byte("x"), nil)
	code, body := restoreRaw(t, h.URL, "not-soft", "live.txt", a.Generation)
	if code != http.StatusPreconditionFailed || !strings.Contains(body, "objectNotSoftDeleted") {
		t.Errorf("restore of a live version = %d %s", code, body)
	}
	if code, _ := restoreRaw(t, h.URL, "not-soft", "never.txt", 1); code != http.StatusNotFound {
		t.Errorf("restore of an object that never existed = %d", code)
	}
	if code, _ := raw(t, "POST", h.URL+"/storage/v1/b/not-soft/o/live.txt/restore", ""); code != http.StatusBadRequest {
		t.Errorf("restore without a generation = %d", code)
	}
}

// Without a soft delete policy a delete is permanent, and a restore is 412
// softDeletePolicyNotSet.
func TestStorageRestoreWithoutPolicy412InProcess(t *testing.T) {
	_, h := sdk(t)
	if code, body := raw(t, "POST", h.URL+"/storage/v1/b?project=p",
		`{"name":"no-policy","softDeletePolicy":{"retentionDurationSeconds":"0"}}`); code != 200 {
		t.Fatal(body)
	}
	var obj struct {
		Generation string `json:"generation"`
	}
	code, body := raw(t, "POST", h.URL+"/upload/storage/v1/b/no-policy/o?uploadType=media&name=o", "x")
	if code != 200 || json.Unmarshal([]byte(body), &obj) != nil {
		t.Fatalf("upload = %d %s", code, body)
	}
	if code, _ := raw(t, "DELETE", h.URL+"/storage/v1/b/no-policy/o/o", ""); code != http.StatusNoContent {
		t.Fatalf("delete = %d", code)
	}
	code, body = raw(t, "POST", h.URL+"/storage/v1/b/no-policy/o/o/restore?generation="+obj.Generation, "")
	if code != http.StatusPreconditionFailed || !strings.Contains(body, "softDeletePolicyNotSet") {
		t.Errorf("restore without a policy = %d %s", code, body)
	}
	if code, body := raw(t, "GET", h.URL+"/storage/v1/b/no-policy/o?softDeleted=true", ""); code != 200 || strings.Contains(body, `"items"`) {
		t.Errorf("a delete without a policy left something soft-deleted: %d %s", code, body)
	}
}

// A soft-deleted object is gone at its hardDeleteTime: reads stop seeing it
// then, and Sweep removes its record.
func TestStorageHardDeleteAfterRetention(t *testing.T) {
	clock := sched.NewFakeClock(time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC))
	s, err := NewServer(Options{Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(s)
	t.Cleanup(h.Close)
	if code, body := raw(t, "POST", h.URL+"/storage/v1/b?project=p", `{"name":"hard"}`); code != 200 {
		t.Fatal(body)
	}
	code, body := raw(t, "POST", h.URL+"/upload/storage/v1/b/hard/o?uploadType=media&name=o", "x")
	var obj struct {
		Generation string `json:"generation"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &obj) != nil {
		t.Fatalf("upload = %d %s", code, body)
	}
	if code, _ := raw(t, "DELETE", h.URL+"/storage/v1/b/hard/o/o", ""); code != http.StatusNoContent {
		t.Fatalf("delete = %d", code)
	}
	next, err := s.Sweep()
	if err != nil || !next.Equal(clock.Now().Add(7*24*time.Hour)) {
		t.Fatalf("Sweep before the retention = next %v, %v; want the hardDeleteTime", next, err)
	}
	clock.Advance(7*24*time.Hour - time.Second)
	if code, _ := raw(t, "GET", h.URL+"/storage/v1/b/hard/o/o?softDeleted=true&generation="+obj.Generation, ""); code != 200 {
		t.Errorf("a second before its hardDeleteTime = %d; want it still soft-deleted", code)
	}
	clock.Advance(time.Second)
	if code, _ := raw(t, "GET", h.URL+"/storage/v1/b/hard/o/o?softDeleted=true&generation="+obj.Generation, ""); code != http.StatusNotFound {
		t.Errorf("at its hardDeleteTime = %d; want 404", code)
	}
	if code, _ := raw(t, "POST", h.URL+"/storage/v1/b/hard/o/o/restore?generation="+obj.Generation, ""); code != http.StatusNotFound {
		t.Errorf("a restore after the hardDeleteTime = %d; want 404", code)
	}
	if next, err := s.Sweep(); err != nil || !next.IsZero() {
		t.Fatalf("Sweep after the retention = %v, %v", next, err)
	}
	_ = s.meta.View(func(tx Tx) error {
		if keys := tx.List(softObjectPrefix); len(keys) != 0 {
			t.Errorf("Sweep left %v", keys)
		}
		return nil
	})

	// Run sweeps on its own at the next hardDeleteTime.
	if code, _ := raw(t, "POST", h.URL+"/upload/storage/v1/b/hard/o?uploadType=media&name=p", "y"); code != 200 {
		t.Fatal("upload p")
	}
	if code, _ := raw(t, "DELETE", h.URL+"/storage/v1/b/hard/o/p", ""); code != http.StatusNoContent {
		t.Fatal("delete p")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx, clock); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		clock.Advance(time.Hour)
		n := 0
		_ = s.meta.View(func(tx Tx) error { n = len(tx.List(softObjectPrefix)); return nil })
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run did not sweep the second object")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	clock.Advance(time.Hour)
	<-done
}

// A bucket whose only objects are soft-deleted can be deleted; the bucket is
// then soft-deleted itself, restorable by generation, and comes back empty
// with its soft-deleted objects restorable again.
func TestStorageBucketDeleteIgnoresSoftDeletedInProcess(t *testing.T) {
	_, bh, h := sdkBucket(t, "soft-bucket")
	ctx := context.Background()
	a := write(t, bh.Object("o.txt"), []byte("x"), nil)
	if err := bh.Object("o.txt").Delete(ctx); err != nil {
		t.Fatal(err)
	}
	var battrs struct {
		Generation string `json:"generation"`
	}
	if _, body := raw(t, "GET", h.URL+"/storage/v1/b/soft-bucket", ""); json.Unmarshal([]byte(body), &battrs) != nil {
		t.Fatal(body)
	}
	if err := bh.Delete(ctx); err != nil {
		t.Fatalf("delete a bucket holding only a soft-deleted object: %v", err)
	}
	if _, err := bh.Attrs(ctx); !errors.Is(err, gcs.ErrBucketNotExist) {
		t.Errorf("the deleted bucket is visible: %v", err)
	}
	gen := battrs.Generation
	if code, body := raw(t, "GET", h.URL+"/storage/v1/b/soft-bucket?softDeleted=true&generation="+gen, ""); code != 200 || !strings.Contains(body, "softDeleteTime") {
		t.Errorf("get the soft-deleted bucket = %d %s", code, body)
	}
	if code, body := raw(t, "GET", h.URL+"/storage/v1/b?project=demo-project&softDeleted=true", ""); code != 200 || !strings.Contains(body, `"soft-bucket"`) {
		t.Errorf("list soft-deleted buckets = %d %s", code, body)
	}
	if code, body := raw(t, "POST", h.URL+"/storage/v1/b/soft-bucket/restore?generation="+gen, ""); code != 200 {
		t.Fatalf("restore the bucket = %d %s", code, body)
	}
	if got := listVersionsOf(t, bh, false); len(got) != 0 {
		t.Errorf("a restored bucket has live objects: %v", got)
	}
	if _, err := bh.Object("o.txt").Generation(a.Generation).Restore(ctx, &gcs.RestoreOptions{}); err != nil {
		t.Errorf("restore the object after the bucket: %v", err)
	}
}

// The policy is 0, or 7 to 90 days; effectiveTime moves when it changes.
func TestStorageSoftDeletePolicyBounds(t *testing.T) {
	_, h := sdk(t)
	for body, want := range map[string]int{
		`{"name":"p-3d","softDeletePolicy":{"retentionDurationSeconds":"259200"}}`:   400,
		`{"name":"p-91d","softDeletePolicy":{"retentionDurationSeconds":"7862400"}}`: 400,
		`{"name":"p-0","softDeletePolicy":{"retentionDurationSeconds":"0"}}`:         200,
		`{"name":"p-90d","softDeletePolicy":{"retentionDurationSeconds":7776000}}`:   200,
	} {
		if code, resp := raw(t, "POST", h.URL+"/storage/v1/b?project=p", body); code != want {
			t.Errorf("%s = %d %s; want %d", body, code, resp, want)
		}
	}
	_, before := raw(t, "GET", h.URL+"/storage/v1/b/p-90d", "")
	time.Sleep(2 * time.Millisecond)
	_, after := raw(t, "PATCH", h.URL+"/storage/v1/b/p-90d", `{"softDeletePolicy":{"retentionDurationSeconds":"864000"}}`)
	eff := func(b string) string {
		var v struct {
			P struct {
				D string `json:"retentionDurationSeconds"`
				E string `json:"effectiveTime"`
			} `json:"softDeletePolicy"`
		}
		_ = json.Unmarshal([]byte(b), &v)
		return v.P.D + "@" + v.P.E
	}
	if eff(before) == eff(after) || !strings.HasPrefix(eff(after), "864000@") {
		t.Errorf("effectiveTime did not move with the policy: %s -> %s", eff(before), eff(after))
	}
	_, same := raw(t, "PATCH", h.URL+"/storage/v1/b/p-90d", `{"labels":{"a":"b"}}`)
	if eff(same) != eff(after) {
		t.Errorf("an unrelated patch moved effectiveTime: %s -> %s", eff(after), eff(same))
	}
}

// With versioning, a deleted noncurrent version becomes soft-deleted
// (soft-delete docs).
func TestStorageSoftDeleteOfNoncurrent(t *testing.T) {
	bh, _ := versionedBucket(t, "soft-versioned")
	ctx := context.Background()
	o := bh.Object("v.txt")
	first := write(t, o, []byte("one"), nil)
	write(t, o, []byte("two"), nil)
	if err := o.Generation(first.Generation).Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if soft := softDeletedOf(t, bh); len(soft) != 1 || soft[0].Generation != first.Generation {
		t.Errorf("soft-deleted after deleting the noncurrent version = %v", soft)
	}
	if got := listVersionsOf(t, bh, true); len(got) != 1 {
		t.Errorf("versions = %d, want only the live one", len(got))
	}
}

// A bucket's softDeletePolicy.hardDeletePause (#1702) keeps its soft-deleted
// objects past their hardDeleteTime: reads, softDeleted listings,
// objects.restore and bulkRestore still see them, and Sweep leaves them.
// Disabling the pause resumes the hard delete.
func TestStorageHardDeletePauseHoldsSweep(t *testing.T) {
	clock := sched.NewFakeClock(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))
	s, err := NewServer(Options{Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(s)
	t.Cleanup(h.Close)
	if code, body := raw(t, "POST", h.URL+"/storage/v1/b?project=p",
		`{"name":"paused","softDeletePolicy":{"retentionDurationSeconds":"604800"}}`); code != 200 {
		t.Fatal(body)
	}
	if code, body := raw(t, "PATCH", h.URL+"/storage/v1/b/paused", `{"softDeletePolicy":{"hardDeletePause":{"enabled":true}}}`); code != 200 {
		t.Fatal(body)
	}
	gens := map[string]string{}
	for _, name := range []string{"a", "b", "c", "d"} {
		code, body := raw(t, "POST", h.URL+"/upload/storage/v1/b/paused/o?uploadType=media&name="+name, name)
		var obj struct {
			Generation string `json:"generation"`
		}
		if code != 200 || json.Unmarshal([]byte(body), &obj) != nil {
			t.Fatalf("upload %s = %d %s", name, code, body)
		}
		gens[name] = obj.Generation
		if code, _ := raw(t, "DELETE", h.URL+"/storage/v1/b/paused/o/"+name, ""); code != http.StatusNoContent {
			t.Fatalf("delete %s = %d", name, code)
		}
	}
	clock.Advance(30 * 24 * time.Hour)
	if next, err := s.Sweep(); err != nil || !next.IsZero() {
		t.Fatalf("Sweep past the hardDeleteTime under the pause = %v, %v; want nothing scheduled", next, err)
	}
	if code, body := raw(t, "GET", h.URL+"/storage/v1/b/paused/o/a?softDeleted=true&generation="+gens["a"], ""); code != 200 {
		t.Errorf("a paused object past its hardDeleteTime = %d %s; want it still soft-deleted", code, body)
	}
	if code, body := raw(t, "GET", h.URL+"/storage/v1/b/paused/o?softDeleted=true", ""); code != 200 ||
		strings.Count(body, `"kind": "storage#object"`) != 4 {
		t.Errorf("softDeleted listing under the pause = %d %s; want all four", code, body)
	}
	if code, body := raw(t, "POST", h.URL+"/storage/v1/b/paused/o/a/restore?generation="+gens["a"], ""); code != 200 {
		t.Errorf("objects.restore under the pause = %d %s", code, body)
	}
	if code, body := raw(t, "POST", h.URL+"/storage/v1/b/paused/o/bulkRestore", `{"matchGlobs":["b"]}`); code != 200 ||
		!strings.Contains(body, `"succeededCount": "1"`) {
		t.Errorf("bulkRestore under the pause = %d %s; want b restored", code, body)
	}
	for _, name := range []string{"a", "b"} {
		if code, _ := raw(t, "GET", h.URL+"/storage/v1/b/paused/o/"+name, ""); code != 200 {
			t.Errorf("%s after its restore = %d; want it live", name, code)
		}
	}

	// Disabling the pause: c and d are past their hardDeleteTime, so gone.
	if code, body := raw(t, "PATCH", h.URL+"/storage/v1/b/paused", `{"softDeletePolicy":{"hardDeletePause":{"enabled":false}}}`); code != 200 {
		t.Fatal(body)
	}
	if code, _ := raw(t, "GET", h.URL+"/storage/v1/b/paused/o/c?softDeleted=true&generation="+gens["c"], ""); code != http.StatusNotFound {
		t.Errorf("c once the pause is disabled = %d; want 404", code)
	}
	if code, _ := raw(t, "POST", h.URL+"/storage/v1/b/paused/o/d/restore?generation="+gens["d"], ""); code != http.StatusNotFound {
		t.Errorf("restoring d once the pause is disabled = %d; want 404", code)
	}
	if _, err := s.Sweep(); err != nil {
		t.Fatal(err)
	}
	_ = s.meta.View(func(tx Tx) error {
		if keys := tx.List(softObjectPrefix); len(keys) != 0 {
			t.Errorf("Sweep after the pause left %v", keys)
		}
		return nil
	})
}

// The pause page's rules (#1715) on the server's clock: the pause changes,
// either way, at most once every 24 hours (412); it needs soft delete
// (412 softDeletePolicyNotSet); while it holds, soft delete can't be
// disabled, by a 0 retention or by removing the policy (412), though the
// retention can change, and buckets.delete is refused (409).
func TestStorageHardDeletePauseOncePerDay(t *testing.T) {
	clock := sched.NewFakeClock(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))
	s, err := NewServer(Options{Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(s)
	t.Cleanup(h.Close)
	patch := func(body string) (int, string) {
		t.Helper()
		return raw(t, "PATCH", h.URL+"/storage/v1/b/day", body)
	}
	const on, off = `{"softDeletePolicy":{"hardDeletePause":{"enabled":true}}}`, `{"softDeletePolicy":{"hardDeletePause":{"enabled":false}}}`
	if code, body := raw(t, "POST", h.URL+"/storage/v1/b?project=p", `{"name":"day","softDeletePolicy":{"retentionDurationSeconds":"0"}}`); code != 200 {
		t.Fatal(body)
	}
	if code, body := patch(on); code != http.StatusPreconditionFailed || !strings.Contains(body, "softDeletePolicyNotSet") {
		t.Errorf("pausing a bucket without soft delete = %d %s; want 412 softDeletePolicyNotSet", code, body)
	}
	if code, body := patch(`{"softDeletePolicy":{"retentionDurationSeconds":"604800","hardDeletePause":{"enabled":true}}}`); code != http.StatusPreconditionFailed {
		t.Errorf("pausing while enabling soft delete = %d %s; want 412, the bucket has no soft delete yet", code, body)
	}
	if code, body := patch(`{"softDeletePolicy":{"retentionDurationSeconds":"604800"}}`); code != 200 {
		t.Fatal(body)
	}
	if code, body := patch(on); code != 200 {
		t.Fatalf("the first pause = %d %s", code, body)
	}
	if code, body := patch(on); code != 200 {
		t.Errorf("pausing again, which changes nothing = %d %s; want 200", code, body)
	}

	clock.Advance(23*time.Hour + 59*time.Minute)
	if code, body := patch(off); code != http.StatusPreconditionFailed || !strings.Contains(body, "at most once every 24 hours") {
		t.Errorf("resuming 23h59m after the pause = %d %s; want 412", code, body)
	}
	for _, body := range []string{
		`{"softDeletePolicy":{"retentionDurationSeconds":"0"}}`,
		`{"softDeletePolicy":null}`,
		`{"softDeletePolicy":{"retentionDurationSeconds":"0","hardDeletePause":{"enabled":false}}}`,
	} {
		if code, got := patch(body); code != http.StatusPreconditionFailed || !strings.Contains(got, "Soft delete can't be disabled") {
			t.Errorf("patch %s while paused = %d %s; want 412", body, code, got)
		}
	}
	if code, body := raw(t, "PUT", h.URL+"/storage/v1/b/day", `{"softDeletePolicy":{"retentionDurationSeconds":"0","hardDeletePause":{"enabled":true}}}`); code != http.StatusPreconditionFailed {
		t.Errorf("buckets.update disabling soft delete while paused = %d %s; want 412", code, body)
	}
	if code, body := patch(`{"softDeletePolicy":{"retentionDurationSeconds":"1209600"}}`); code != 200 ||
		!strings.Contains(body, `"retentionDurationSeconds": "1209600"`) || !strings.Contains(body, `"enabled": true`) {
		t.Errorf("changing the retention while paused = %d %s; want it taken, still paused", code, body)
	}
	if code, body := raw(t, "DELETE", h.URL+"/storage/v1/b/day", ""); code != http.StatusConflict || !strings.Contains(body, "while hard deletions are paused") {
		t.Errorf("buckets.delete while paused = %d %s; want 409", code, body)
	}
	if code, _ := raw(t, "GET", h.URL+"/storage/v1/b/day", ""); code != 200 {
		t.Errorf("the bucket after the refused delete = %d; want it live", code)
	}

	clock.Advance(time.Minute)
	if code, body := patch(off); code != 200 {
		t.Fatalf("resuming 24h after the pause = %d %s", code, body)
	}
	if code, body := patch(on); code != http.StatusPreconditionFailed || !strings.Contains(body, "can't be paused until 2026-11-03T12:00:00.000Z") {
		t.Errorf("pausing right after the resume = %d %s; want 412 naming when it can", code, body)
	}
	if code, body := patch(`{"softDeletePolicy":{"retentionDurationSeconds":"0"}}`); code != 200 {
		t.Errorf("disabling soft delete once resumed = %d %s", code, body)
	}
	if code, body := raw(t, "DELETE", h.URL+"/storage/v1/b/day", ""); code != http.StatusNoContent {
		t.Errorf("buckets.delete once resumed = %d %s", code, body)
	}
}
