//go:build compat

package compat

import (
	"errors"
	"net/http"
	"testing"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	storagev1 "google.golang.org/api/storage/v1"
)

// The JSON API methods the Go client's handles never send (#691):
// BucketHandle.Update and ObjectHandle.Update are patches, and the copier is
// objects.rewrite. Google's generated JSON API client,
// google.golang.org/api/storage/v1, sends each of them, so it drives them
// here.

// storageJSON returns the generated JSON API client pointed at the local
// instance.
func storageJSON(t *testing.T, h *Harness) *storagev1.Service {
	t.Helper()
	s, err := storagev1.NewService(h.Context(), option.WithEndpoint(h.Endpoint(EnvStorage)+"/storage/v1/"),
		option.WithoutAuthentication(), option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatalf("storagev1.NewService: %v", err)
	}
	return s
}

// httpCode is a googleapi error's HTTP status, or 0.
func httpCode(err error) int {
	var e *googleapi.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return 0
}

// TestStorageBucketsUpdateReplaces: buckets.update is a full replace, so a
// label or versioning the request omits is gone afterwards, the
// metageneration advances, and a stale ifMetagenerationMatch is 412.
// covers: storage.buckets.update
func TestStorageBucketsUpdateReplaces(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	bh := bucket(t, h, c)
	ctx := h.Context()
	if _, err := bh.Update(ctx, storage.BucketAttrsToUpdate{VersioningEnabled: true}); err != nil {
		t.Fatal(err)
	}
	ua := storage.BucketAttrsToUpdate{}
	ua.SetLabel("keep", "no")
	before, err := bh.Update(ctx, ua)
	if err != nil {
		t.Fatal(err)
	}
	if !before.VersioningEnabled || before.Labels["keep"] != "no" {
		t.Fatalf("setup: attrs = versioning %v, labels %v", before.VersioningEnabled, before.Labels)
	}

	s := storageJSON(t, h)
	got, err := s.Buckets.Update(bh.BucketName(), &storagev1.Bucket{Labels: map[string]string{"team": "a"}}).
		IfMetagenerationMatch(before.MetaGeneration).Context(ctx).Do()
	if err != nil {
		t.Fatalf("buckets.update: %v", err)
	}
	if got.Metageneration != before.MetaGeneration+1 {
		t.Errorf("metageneration after update = %d, want %d", got.Metageneration, before.MetaGeneration+1)
	}
	after, err := bh.Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Labels) != 1 || after.Labels["team"] != "a" {
		t.Errorf("labels after a replace = %v; want only team=a", after.Labels)
	}
	if after.VersioningEnabled {
		t.Error("versioning survived a replace that omitted it")
	}
	if after.Location != before.Location {
		t.Errorf("location after update = %q, want %q", after.Location, before.Location)
	}

	_, err = s.Buckets.Update(bh.BucketName(), &storagev1.Bucket{}).IfMetagenerationMatch(before.MetaGeneration).Context(ctx).Do()
	if httpCode(err) != http.StatusPreconditionFailed {
		t.Errorf("buckets.update with a stale ifMetagenerationMatch = %v; want 412", err)
	}
	if _, err := s.Buckets.Update(h.Project()+"-missing", &storagev1.Bucket{}).Context(ctx).Do(); httpCode(err) != http.StatusNotFound {
		t.Errorf("buckets.update on a missing bucket = %v; want 404", err)
	}
}

// TestStorageObjectsUpdateReplaces: objects.update replaces an object's
// metadata, clearing what the request omits and keeping its bytes and
// generation; a stale ifMetagenerationMatch is 412.
// covers: storage.objects.update
func TestStorageObjectsUpdateReplaces(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	bh := bucket(t, h, c)
	ctx := h.Context()
	o := bh.Object("meta.txt")
	putObject(t, ctx, o, "payload")
	before, err := o.Update(ctx, storage.ObjectAttrsToUpdate{ContentType: "text/plain", CacheControl: "no-cache",
		Metadata: map[string]string{"old": "1"}})
	if err != nil {
		t.Fatal(err)
	}

	s := storageJSON(t, h)
	got, err := s.Objects.Update(bh.BucketName(), "meta.txt", &storagev1.Object{
		ContentType: "application/json", Metadata: map[string]string{"new": "2"}}).
		IfMetagenerationMatch(before.Metageneration).Context(ctx).Do()
	if err != nil {
		t.Fatalf("objects.update: %v", err)
	}
	if got.Metageneration != before.Metageneration+1 || got.Generation != before.Generation {
		t.Errorf("after update generation %d metageneration %d; want %d and %d",
			got.Generation, got.Metageneration, before.Generation, before.Metageneration+1)
	}
	after, err := o.Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.ContentType != "application/json" || after.CacheControl != "" {
		t.Errorf("after a replace contentType %q cacheControl %q; want application/json and none", after.ContentType, after.CacheControl)
	}
	if len(after.Metadata) != 1 || after.Metadata["new"] != "2" {
		t.Errorf("metadata after a replace = %v; want only new=2", after.Metadata)
	}
	if b := readObject(t, ctx, c, bh.BucketName(), "meta.txt"); b != "payload" {
		t.Errorf("bytes after update = %q", b)
	}

	_, err = s.Objects.Update(bh.BucketName(), "meta.txt", &storagev1.Object{}).IfMetagenerationMatch(before.Metageneration).Context(ctx).Do()
	if httpCode(err) != http.StatusPreconditionFailed {
		t.Errorf("objects.update with a stale ifMetagenerationMatch = %v; want 412", err)
	}
	if _, err := s.Objects.Update(bh.BucketName(), "missing.txt", &storagev1.Object{}).Context(ctx).Do(); httpCode(err) != http.StatusNotFound {
		t.Errorf("objects.update on a missing object = %v; want 404", err)
	}
}

// TestStorageObjectsCopy: objects.copy without a body keeps the source's
// metadata, with one takes the body's, the source survives, and
// ifGenerationMatch=0 on an existing destination is 412.
// covers: storage.objects.copy
func TestStorageObjectsCopy(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	bh := bucket(t, h, c)
	ctx := h.Context()
	src := bh.Object("src.txt")
	putObject(t, ctx, src, "copy me")
	if _, err := src.Update(ctx, storage.ObjectAttrsToUpdate{ContentType: "text/plain", Metadata: map[string]string{"from": "src"}}); err != nil {
		t.Fatal(err)
	}
	s := storageJSON(t, h)
	b := bh.BucketName()

	plain, err := s.Objects.Copy(b, "src.txt", b, "plain.txt", &storagev1.Object{}).Context(ctx).Do()
	if err != nil {
		t.Fatalf("objects.copy: %v", err)
	}
	if plain.ContentType != "text/plain" || plain.Metadata["from"] != "src" {
		t.Errorf("a copy with no body = contentType %q metadata %v; want the source's", plain.ContentType, plain.Metadata)
	}
	if got := readObject(t, ctx, c, bh.BucketName(), "plain.txt"); got != "copy me" {
		t.Errorf("copied bytes = %q", got)
	}

	body, err := s.Objects.Copy(b, "src.txt", b, "body.txt", &storagev1.Object{
		ContentType: "application/json", Metadata: map[string]string{"from": "body"}}).Context(ctx).Do()
	if err != nil {
		t.Fatalf("objects.copy with a body: %v", err)
	}
	attrs, err := bh.Object("body.txt").Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if attrs.ContentType != "application/json" || len(attrs.Metadata) != 1 || attrs.Metadata["from"] != "body" {
		t.Errorf("a copy with a body = contentType %q metadata %v; want the body's", attrs.ContentType, attrs.Metadata)
	}
	if attrs.Generation != body.Generation {
		t.Errorf("the copy reads back as generation %d, returned %d", attrs.Generation, body.Generation)
	}
	if _, err := src.Attrs(ctx); err != nil {
		t.Errorf("the copy removed the source: %v", err)
	}

	_, err = s.Objects.Copy(b, "src.txt", b, "plain.txt", &storagev1.Object{}).IfGenerationMatch(0).Context(ctx).Do()
	if httpCode(err) != http.StatusPreconditionFailed {
		t.Errorf("objects.copy onto an existing object with ifGenerationMatch=0 = %v; want 412", err)
	}
	if _, err := s.Objects.Copy(b, "missing.txt", b, "x.txt", &storagev1.Object{}).Context(ctx).Do(); httpCode(err) != http.StatusNotFound {
		t.Errorf("objects.copy of a missing source = %v; want 404", err)
	}
}

// TestStorageObjectMove: ObjectHandle.Move sends objects.move, which renames
// the object within its bucket: the destination has the bytes and
// metadata, the source is gone, and nothing is left soft-deleted.
// covers: storage.objects.move
func TestStorageObjectMove(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	bh := bucket(t, h, c)
	ctx := h.Context()
	src := bh.Object("from.txt")
	putObject(t, ctx, src, "moving")
	if _, err := src.Update(ctx, storage.ObjectAttrsToUpdate{Metadata: map[string]string{"k": "v"}}); err != nil {
		t.Fatal(err)
	}

	moved, err := src.Move(ctx, storage.MoveObjectDestination{Object: "to.txt"})
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	if moved.Name != "to.txt" {
		t.Errorf("Move returned name %q", moved.Name)
	}
	if got := readObject(t, ctx, c, bh.BucketName(), "to.txt"); got != "moving" {
		t.Errorf("moved bytes = %q", got)
	}
	if a, err := bh.Object("to.txt").Attrs(ctx); err != nil || a.Metadata["k"] != "v" {
		t.Errorf("the moved object's metadata = %v, %v", a, err)
	}
	if _, err := src.Attrs(ctx); !errors.Is(err, storage.ErrObjectNotExist) {
		t.Errorf("the source after a move = %v; want not found", err)
	}
	if soft := softDeletedObjects(t, h, bh); len(soft) != 0 {
		t.Errorf("a move left %d soft-deleted objects", len(soft))
	}
	if _, err := src.Move(ctx, storage.MoveObjectDestination{Object: "again.txt"}); !errors.Is(err, storage.ErrObjectNotExist) && httpCode(err) != http.StatusNotFound {
		t.Errorf("moving a missing object = %v; want not found", err)
	}
}

// TestStorageBucketsRestore: a deleted bucket is soft-deleted under its
// policy, buckets.restore brings it back by generation with its
// soft-deleted objects restorable, a restore while a live bucket has the
// name is 409, and one of a generation that is not soft-deleted is 404.
// covers: storage.buckets.restore
func TestStorageBucketsRestore(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()
	name := h.Project() + "-restore"
	bh := c.Bucket(name)
	if err := bh.Create(ctx, h.Project(), nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { emptyAndDelete(ctx, bh) })
	putObject(t, ctx, bh.Object("o.txt"), "x")
	s := storageJSON(t, h)
	live, err := s.Buckets.Get(name).Context(ctx).Do()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.Buckets.Restore(name, live.Generation).Context(ctx).Do(); httpCode(err) != http.StatusNotFound {
		t.Errorf("buckets.restore of a generation that is live, not soft-deleted = %v; want 404", err)
	}
	if err := bh.Object("o.txt").Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if err := bh.Delete(ctx); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := bh.Attrs(ctx); !errors.Is(err, storage.ErrBucketNotExist) {
		t.Fatalf("the deleted bucket reads as %v; want not found", err)
	}

	// A new live bucket of the same name blocks the restore until it goes.
	if err := bh.Create(ctx, h.Project(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Buckets.Restore(name, live.Generation).Context(ctx).Do(); httpCode(err) != http.StatusConflict {
		t.Errorf("buckets.restore while a live bucket has the name = %v; want 409", err)
	}
	if err := bh.Delete(ctx); err != nil {
		t.Fatal(err)
	}

	got, err := s.Buckets.Restore(name, live.Generation).Context(ctx).Do()
	if err != nil {
		t.Fatalf("buckets.restore: %v", err)
	}
	if got.Name != name || got.Generation != live.Generation {
		t.Errorf("buckets.restore returned %s generation %d; want %s generation %d", got.Name, got.Generation, name, live.Generation)
	}
	if _, err := bh.Attrs(ctx); err != nil {
		t.Fatalf("the restored bucket is not readable: %v", err)
	}
	soft := softDeletedObjects(t, h, bh)
	if len(soft) != 1 {
		t.Fatalf("the restored bucket's soft-deleted objects = %d, want 1", len(soft))
	}
	if _, err := bh.Object("o.txt").Generation(soft[0].Generation).Restore(ctx, &storage.RestoreOptions{}); err != nil {
		t.Fatalf("restore the object in the restored bucket: %v", err)
	}
	if got := readObject(t, ctx, c, bh.BucketName(), "o.txt"); got != "x" {
		t.Errorf("the restored object reads %q", got)
	}
	if _, err := s.Buckets.Restore(h.Project()+"-never", 1).Context(ctx).Do(); httpCode(err) != http.StatusNotFound {
		t.Errorf("buckets.restore of a bucket that never existed = %v; want 404", err)
	}
}
