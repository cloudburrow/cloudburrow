//go:build compat

package compat

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	storagev1 "google.golang.org/api/storage/v1"
)

// TestStorageBulkRestore (#1279): objects.bulkRestore through Google's
// generated JSON API client, on objects the official storage client writes
// and deletes under the default soft delete policy.
//
//   - matchGlobs restores only the names it matches, as new live
//     generations the official client reads back; the rest stay
//     soft-deleted.
//   - Without allowOverwrite a name with a live version is skipped; with it
//     the soft-deleted version replaces the live one.
//   - softDeletedAfterTime later than every delete restores nothing.
//   - The answer is a done operation, read back with
//     buckets.operations.get and listed by buckets.operations.list.
//   - A malformed time is 400, and a missing bucket 404.
//
// covers: storage.objects.bulkRestore
func TestStorageBulkRestore(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	s := storageJSON(t, h)
	ctx := h.Context()
	bh := c.Bucket(h.Project() + "-bulkrestore")
	if err := bh.Create(ctx, h.Project(), nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { emptyAndDelete(ctx, bh) })
	b := bh.BucketName()

	for _, name := range []string{"logs/a.txt", "logs/b.txt", "img/c.png", "keep.txt"} {
		putText(t, ctx, bh, name)
		if err := bh.Object(name).Delete(ctx); err != nil {
			t.Fatalf("delete %s: %v", name, err)
		}
	}
	// keep.txt gets a live version again, so a bulk restore must skip it.
	w := bh.Object("keep.txt").NewWriter(ctx)
	_, _ = io.WriteString(w, "live")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	type meta struct {
		Type              string
		Restored, Skipped int64
	}
	// readMeta reads the operation's BulkRestoreObjectsMetadata.
	readMeta := func(op *storagev1.GoogleLongrunningOperation) meta {
		if op == nil {
			return meta{}
		}
		var m struct {
			Common struct {
				Type string `json:"type"`
			} `json:"commonMetadata"`
			Restored int64 `json:"successfulObjectCount,string"`
			Skipped  int64 `json:"skippedObjectCount,string"`
		}
		_ = json.Unmarshal(op.Metadata, &m)
		return meta{m.Common.Type, m.Restored, m.Skipped}
	}

	op, err := s.Objects.BulkRestore(b, &storagev1.BulkRestoreObjectsRequest{MatchGlobs: []string{"logs/**"}}).Context(ctx).Do()
	if err != nil || !op.Done || op.Name == "" {
		t.Fatalf("objects.bulkRestore logs/** = %+v, %v", op, err)
	}
	if m := readMeta(op); m.Restored != 2 || m.Type == "" {
		t.Errorf("bulkRestore metadata = %+v (%s); want 2 restored", m, op.Metadata)
	}
	for _, name := range []string{"logs/a.txt", "logs/b.txt"} {
		if a, err := bh.Object(name).Attrs(ctx); err != nil || a.Metageneration != 1 {
			t.Errorf("%s after the bulk restore = %+v, %v; want a live object", name, a, err)
		}
	}
	if _, err := bh.Object("img/c.png").Attrs(ctx); !errors.Is(err, storage.ErrObjectNotExist) {
		t.Errorf("img/c.png, outside the glob, reads %v; want it still deleted", err)
	}

	id := op.Name[strings.LastIndex(op.Name, "/")+1:]
	if got, err := s.Operations.Get(b, id).Context(ctx).Do(); err != nil || got.Name != op.Name || !got.Done {
		t.Errorf("buckets.operations.get = %+v, %v", got, err)
	}
	if ops, err := s.Operations.List(b).Context(ctx).Do(); err != nil || len(ops.Operations) != 1 {
		t.Errorf("buckets.operations.list = %+v, %v; want the bulk restore", ops, err)
	}

	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	op, err = s.Objects.BulkRestore(b, &storagev1.BulkRestoreObjectsRequest{SoftDeletedAfterTime: future}).Context(ctx).Do()
	if m := readMeta(op); err != nil || m.Restored != 0 {
		t.Errorf("bulkRestore softDeletedAfterTime=%s = %+v, %v; want nothing restored", future, m, err)
	}

	op, err = s.Objects.BulkRestore(b, &storagev1.BulkRestoreObjectsRequest{}).Context(ctx).Do()
	if m := readMeta(op); err != nil || m.Restored != 1 || m.Skipped != 1 {
		t.Errorf("bulkRestore of the rest = %+v, %v; want img/c.png restored and keep.txt skipped", m, err)
	}
	if r, err := bh.Object("keep.txt").NewReader(ctx); err != nil {
		t.Error(err)
	} else if got, _ := io.ReadAll(r); string(got) != "live" {
		t.Errorf("keep.txt reads %q; the skipped restore replaced the live version", got)
	}
	op, err = s.Objects.BulkRestore(b, &storagev1.BulkRestoreObjectsRequest{AllowOverwrite: true, MatchGlobs: []string{"keep.txt"}}).Context(ctx).Do()
	if m := readMeta(op); err != nil || m.Restored != 1 {
		t.Errorf("bulkRestore allowOverwrite = %+v, %v; want keep.txt restored", m, err)
	}
	if r, err := bh.Object("keep.txt").NewReader(ctx); err != nil {
		t.Error(err)
	} else if got, _ := io.ReadAll(r); string(got) != "x" {
		t.Errorf("keep.txt after allowOverwrite reads %q; want the restored x", got)
	}

	if _, err := s.Objects.BulkRestore(b, &storagev1.BulkRestoreObjectsRequest{CreatedBeforeTime: "yesterday"}).Context(ctx).Do(); httpCode(err) != http.StatusBadRequest {
		t.Errorf("bulkRestore with a malformed time = %v; want 400", err)
	}
	if _, err := s.Objects.BulkRestore(b+"-missing", &storagev1.BulkRestoreObjectsRequest{}).Context(ctx).Do(); httpCode(err) != http.StatusNotFound {
		t.Errorf("bulkRestore on a missing bucket = %v; want 404", err)
	}
}
