//go:build compat

package compat

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
)

// Soft delete and restore (#499), to
// docs.cloud.google.com/storage/docs/soft-delete. fake-gcs-server has no
// soft delete, so these run against the builtin server.

// rawStorage sends one JSON API request and returns the status and body.
func rawStorage(t *testing.T, h *Harness, method, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(h.Context(), method, h.Endpoint(EnvStorage)+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func softDeletedObjects(t *testing.T, h *Harness, bh *storage.BucketHandle) []*storage.ObjectAttrs {
	t.Helper()
	var out []*storage.ObjectAttrs
	it := bh.Objects(h.Context(), &storage.Query{SoftDeleted: true})
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

// TestStorageSoftDeleteAndRestore: under the default 7-day policy a delete
// soft-deletes the object, and a restore makes it live again as a new
// generation with metageneration 1.
// covers: storage.objects.restore, storage.objects.list, storage.objects.get
func TestStorageSoftDeleteAndRestore(t *testing.T) {
	builtinOnly(t, "soft delete")
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()
	bh := bucket(t, h, c)
	o := bh.Object("soft.txt")
	first := putObject(t, ctx, o, "kept")
	if err := o.Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Attrs(ctx); !errors.Is(err, storage.ErrObjectNotExist) {
		t.Errorf("a soft-deleted object is visible: %v", err)
	}
	soft := softDeletedObjects(t, h, bh)
	if len(soft) != 1 || soft[0].Generation != first.Generation || soft[0].HardDeleteTime.Sub(soft[0].SoftDeleteTime) != 7*24*time.Hour {
		t.Fatalf("soft-deleted = %d, want the one version with a 7-day hardDeleteTime", len(soft))
	}
	if a, err := o.Generation(first.Generation).SoftDeleted().Attrs(ctx); err != nil || a.SoftDeleteTime.IsZero() {
		t.Errorf("soft-deleted attrs = %v, %v", a, err)
	}
	restored, err := o.Generation(first.Generation).Restore(ctx, &storage.RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if restored.Generation <= first.Generation || restored.Metageneration != 1 {
		t.Errorf("restored generation %d, metageneration %d; want a new generation, metageneration 1", restored.Generation, restored.Metageneration)
	}
	r, err := o.NewReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if b, _ := io.ReadAll(r); string(b) != "kept" {
		t.Errorf("restored bytes = %q", b)
	}
}

// TestStorageRestoreNotSoftDeleted412: restoring a live version is 412
// objectNotSoftDeleted (status-codes page).
// covers: storage.objects.restore
func TestStorageRestoreNotSoftDeleted412(t *testing.T) {
	builtinOnly(t, "soft delete")
	h := New(t)
	c := storageClient(t, h)
	bh := bucket(t, h, c)
	a := putObject(t, h.Context(), bh.Object("live.txt"), "x")
	code, body := rawStorage(t, h, "POST", fmt.Sprintf("/storage/v1/b/%s/o/live.txt/restore?generation=%d", bh.BucketName(), a.Generation), "")
	if code != http.StatusPreconditionFailed || !strings.Contains(body, "objectNotSoftDeleted") {
		t.Errorf("restore of a live version = %d %s; want 412 objectNotSoftDeleted", code, body)
	}
}

// TestStorageRestoreWithoutPolicy412: on a bucket with soft delete off, a
// restore is 412 softDeletePolicyNotSet (status-codes page).
// covers: storage.objects.restore
func TestStorageRestoreWithoutPolicy412(t *testing.T) {
	builtinOnly(t, "soft delete")
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()
	name := h.Project() + "-nopolicy"
	if code, body := rawStorage(t, h, "POST", "/storage/v1/b?project="+h.Project(),
		`{"name":"`+name+`","softDeletePolicy":{"retentionDurationSeconds":"0"}}`); code != http.StatusOK {
		t.Fatalf("create = %d %s", code, body)
	}
	bh := c.Bucket(name)
	t.Cleanup(func() { emptyAndDelete(ctx, bh) })
	a := putObject(t, ctx, bh.Object("gone.txt"), "x")
	if err := bh.Object("gone.txt").Delete(ctx); err != nil {
		t.Fatal(err)
	}
	code, body := rawStorage(t, h, "POST", fmt.Sprintf("/storage/v1/b/%s/o/gone.txt/restore?generation=%d", name, a.Generation), "")
	if code != http.StatusPreconditionFailed || !strings.Contains(body, "softDeletePolicyNotSet") {
		t.Errorf("restore without a policy = %d %s; want 412 softDeletePolicyNotSet", code, body)
	}
}

// TestStorageBucketDeleteIgnoresSoftDeleted: a bucket whose only objects
// are soft-deleted can be deleted, and buckets.restore brings it back.
// covers: storage.buckets.delete
func TestStorageBucketDeleteIgnoresSoftDeleted(t *testing.T) {
	builtinOnly(t, "soft delete")
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()
	name := h.Project() + "-softbucket"
	bh := c.Bucket(name)
	if err := bh.Create(ctx, h.Project(), nil); err != nil {
		t.Fatal(err)
	}
	putObject(t, ctx, bh.Object("o.txt"), "x")
	if err := bh.Object("o.txt").Delete(ctx); err != nil {
		t.Fatal(err)
	}
	_, body := rawStorage(t, h, "GET", "/storage/v1/b/"+name, "")
	var b struct {
		Generation string `json:"generation"`
	}
	if err := json.Unmarshal([]byte(body), &b); err != nil || b.Generation == "" {
		t.Fatalf("bucket generation: %s", body)
	}
	if err := bh.Delete(ctx); err != nil {
		t.Fatalf("delete a bucket holding only a soft-deleted object: %v", err)
	}
	if code, body := rawStorage(t, h, "POST", "/storage/v1/b/"+name+"/restore?generation="+b.Generation, ""); code != http.StatusOK {
		t.Fatalf("buckets.restore = %d %s", code, body)
	}
	t.Cleanup(func() { emptyAndDelete(ctx, bh) })
	if soft := softDeletedObjects(t, h, bh); len(soft) != 1 {
		t.Errorf("the restored bucket's soft-deleted objects = %d, want 1", len(soft))
	}
}
