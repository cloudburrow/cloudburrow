//go:build compat

package compat

import (
	"net/http"
	"testing"

	storagev1 "google.golang.org/api/storage/v1"
)

// TestStorageOperationsListFilter (#1280): buckets.operations.list with an
// AIP-160 filter, through Google's generated JSON API client, on a bucket
// holding a folder rename, a recursive folder delete and a bulk restore.
//
//   - metadata.commonMetadata.type picks one kind, = and !=, and a quoted
//     value; OR and NOT combine; done=true keeps all, done=false none;
//     metadata.commonMetadata.createTime compares as a time.
//   - A field that is no Operation field, or a malformed expression, is
//     400.
//
// covers: storage.buckets.operations.list
func TestStorageOperationsListFilter(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	s := storageJSON(t, h)
	ctx := h.Context()
	bh := hnsBucket(t, h, c, s, "-opsfilter")
	b := bh.BucketName()

	for _, f := range []string{"a/", "b/"} {
		if _, err := s.Folders.Insert(b, &storagev1.Folder{Name: f}).Context(ctx).Do(); err != nil {
			t.Fatalf("folders.insert %s: %v", f, err)
		}
	}
	if _, err := s.Folders.Rename(b, "a/", "c/").Context(ctx).Do(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Folders.DeleteRecursive(b, "b/").Context(ctx).Do(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Objects.BulkRestore(b, &storagev1.BulkRestoreObjectsRequest{}).Context(ctx).Do(); err != nil {
		t.Fatal(err)
	}

	count := func(filter string) int {
		t.Helper()
		ops, err := s.Operations.List(b).Filter(filter).Context(ctx).Do()
		if err != nil {
			t.Errorf("buckets.operations.list filter=%q: %v", filter, err)
			return -1
		}
		return len(ops.Operations)
	}
	for filter, want := range map[string]int{
		"": 3,
		`metadata.commonMetadata.type = "RENAME_FOLDER"`:                                           1,
		`metadata.commonMetadata.type != RENAME_FOLDER`:                                            2,
		`metadata.commonMetadata.type=RENAME_FOLDER OR metadata.commonMetadata.type=DELETE_FOLDER`: 2,
		`NOT metadata.commonMetadata.type:FOLDER`:                                                  1,
		`done = true`:  3,
		`done = false`: 0,
		`metadata.commonMetadata.createTime > "2000-01-01T00:00:00Z" AND done = true`: 3,
		`metadata.commonMetadata.createTime < "2000-01-01T00:00:00Z"`:                 0,
		`metadata.@type:BulkRestoreObjectsMetadata`:                                   1,
		`metadata.destinationFolderId = "c/"`:                                         1,
	} {
		if got := count(filter); got != want {
			t.Errorf("buckets.operations.list filter=%q lists %d; want %d", filter, got, want)
		}
	}
	for _, bad := range []string{`kind = "x"`, `done = maybe`, `(done = true`, `done =~ t`} {
		if _, err := s.Operations.List(b).Filter(bad).Context(ctx).Do(); httpCode(err) != http.StatusBadRequest {
			t.Errorf("buckets.operations.list filter=%q = %v; want 400", bad, err)
		}
	}
}
