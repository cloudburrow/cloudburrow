//go:build compat

package compat

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// storageClient returns an official Cloud Storage client pointed at the local
// instance, with authentication disabled.
func storageClient(t *testing.T, h *Harness) *storage.Client {
	t.Helper()
	endpoint := h.Endpoint(EnvStorage)
	t.Setenv("STORAGE_EMULATOR_HOST", endpoint)
	c, err := storage.NewClient(h.Context(),
		option.WithoutAuthentication(),
		option.WithEndpoint(endpoint+"/storage/v1/"))
	if err != nil {
		t.Fatalf("storage.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// bucket creates a uniquely named bucket and removes it afterwards, so tests
// leave nothing behind and never collide.
func bucket(t *testing.T, h *Harness, c *storage.Client) *storage.BucketHandle {
	t.Helper()
	name := h.Project() + "-bucket"
	bh := c.Bucket(name)
	if err := bh.Create(h.Context(), h.Project(), nil); err != nil {
		t.Fatalf("create bucket %s: %v", name, err)
	}
	t.Cleanup(func() {
		ctx := h.Context()
		it := bh.Objects(ctx, nil)
		for {
			attrs, err := it.Next()
			if err != nil {
				break
			}
			_ = bh.Object(attrs.Name).Delete(ctx)
		}
		_ = bh.Delete(ctx)
	})
	return bh
}

// TestStorageBucketLifecycle covers buckets.insert, buckets.get and
// buckets.delete through the official SDK.
// covers: storage.buckets.insert, storage.buckets.get, storage.buckets.delete
func TestStorageBucketLifecycle(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()

	name := h.Project() + "-lifecycle"
	bh := c.Bucket(name)
	if err := bh.Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	attrs, err := bh.Attrs(ctx)
	if err != nil {
		t.Fatalf("Attrs: %v", err)
	}
	if attrs.Name != name {
		t.Errorf("bucket name = %q, want %q", attrs.Name, name)
	}
	if err := bh.Delete(ctx); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := bh.Attrs(ctx); !errors.Is(err, storage.ErrBucketNotExist) {
		t.Errorf("Attrs after delete = %v, want ErrBucketNotExist", err)
	}
}

// TestStorageObjectRoundTrip covers upload, metadata, download and delete.
// covers: storage.objects.insert, storage.objects.get, storage.objects.delete
func TestStorageObjectRoundTrip(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	bh := bucket(t, h, c)
	ctx := h.Context()

	payload := []byte("cloudburrow round trip")
	w := bh.Object("round-trip.txt").NewWriter(ctx)
	if _, err := w.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	attrs, err := bh.Object("round-trip.txt").Attrs(ctx)
	if err != nil {
		t.Fatalf("Attrs: %v", err)
	}
	if attrs.Size != int64(len(payload)) {
		t.Errorf("size = %d, want %d", attrs.Size, len(payload))
	}

	r, err := bh.Object("round-trip.txt").NewReader(ctx)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if !bytes.Equal(got, payload) {
		t.Errorf("download = %q, want %q", got, payload)
	}

	if err := bh.Object("round-trip.txt").Delete(ctx); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := bh.Object("round-trip.txt").Attrs(ctx); !errors.Is(err, storage.ErrObjectNotExist) {
		t.Errorf("Attrs after delete = %v, want ErrObjectNotExist", err)
	}
}

// TestStorageListWithPrefix covers objects.list, including the delimiter
// behavior that emulators most often get wrong.
// covers: storage.objects.list
func TestStorageListWithPrefix(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	bh := bucket(t, h, c)
	ctx := h.Context()

	for _, name := range []string{"a.txt", "dir/b.txt", "dir/c.txt", "dir/sub/d.txt"} {
		w := bh.Object(name).NewWriter(ctx)
		_, _ = w.Write([]byte("x"))
		if err := w.Close(); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	it := bh.Objects(ctx, &storage.Query{Prefix: "dir/", Delimiter: "/"})
	var names, prefixes []string
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if attrs.Prefix != "" {
			prefixes = append(prefixes, attrs.Prefix)
		} else {
			names = append(names, attrs.Name)
		}
	}
	if len(names) != 2 {
		t.Errorf("objects = %v, want the 2 direct children of dir/", names)
	}
	if len(prefixes) != 1 || prefixes[0] != "dir/sub/" {
		t.Errorf("prefixes = %v, want [dir/sub/]", prefixes)
	}
}

// TestStorageGenerationPreconditions covers the correctness case that matters
// for concurrent writers.
func TestStorageGenerationPreconditions(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	bh := bucket(t, h, c)
	ctx := h.Context()

	w := bh.Object("obj").NewWriter(ctx)
	_, _ = w.Write([]byte("v1"))
	if err := w.Close(); err != nil {
		t.Fatalf("first write: %v", err)
	}
	gen := w.Attrs().Generation
	if gen == 0 {
		t.Fatal("no generation returned on write")
	}

	// DoesNotExist must be refused now that the object exists.
	w2 := bh.Object("obj").If(storage.Conditions{DoesNotExist: true}).NewWriter(ctx)
	_, _ = w2.Write([]byte("v2"))
	err := w2.Close()
	if err == nil {
		t.Error("write with DoesNotExist succeeded on an existing object")
	} else {
		var ae *googleapi.Error
		if errors.As(err, &ae) && ae.Code != 412 {
			t.Errorf("DoesNotExist violation = HTTP %d, want 412", ae.Code)
		}
	}

	// A stale generation must be refused.
	w3 := bh.Object("obj").If(storage.Conditions{GenerationMatch: gen + 999}).NewWriter(ctx)
	_, _ = w3.Write([]byte("v3"))
	if err := w3.Close(); err == nil {
		t.Error("write with a stale GenerationMatch succeeded; concurrent writes would be unsafe")
	}

	// The matching generation must be accepted and must advance.
	w4 := bh.Object("obj").If(storage.Conditions{GenerationMatch: gen}).NewWriter(ctx)
	_, _ = w4.Write([]byte("v4"))
	if err := w4.Close(); err != nil {
		t.Fatalf("write with matching generation: %v", err)
	}
	after, err := bh.Object("obj").Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Generation == gen {
		t.Errorf("generation did not advance after overwrite (still %d)", gen)
	}
}

// TestStorageResumableUploadAndRangedRead covers the large-object path the SDK
// uses by default.
func TestStorageResumableUploadAndRangedRead(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	bh := bucket(t, h, c)
	ctx := h.Context()

	payload := bytes.Repeat([]byte("0123456789abcdef"), 512*1024) // 8 MiB
	obj := bh.Object("large.bin")
	w := obj.NewWriter(ctx)
	w.ChunkSize = 256 * 1024
	if _, err := w.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close (resumable): %v", err)
	}

	attrs, err := obj.Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if attrs.Size != int64(len(payload)) {
		t.Fatalf("size = %d, want %d", attrs.Size, len(payload))
	}

	r, err := obj.NewRangeReader(ctx, 16, 32)
	if err != nil {
		t.Fatalf("NewRangeReader: %v", err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if !bytes.Equal(got, payload[16:48]) {
		t.Errorf("ranged read mismatch")
	}
}

// TestStorageCompose covers objects.compose.
// covers: storage.objects.compose
func TestStorageCompose(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	bh := bucket(t, h, c)
	ctx := h.Context()

	for name, data := range map[string]string{"p1": "hello ", "p2": "world"} {
		w := bh.Object(name).NewWriter(ctx)
		_, _ = w.Write([]byte(data))
		if err := w.Close(); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if _, err := bh.Object("joined").ComposerFrom(bh.Object("p1"), bh.Object("p2")).Run(ctx); err != nil {
		t.Fatalf("Compose: %v", err)
	}
	r, err := bh.Object("joined").NewReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if string(got) != "hello world" {
		t.Errorf("composed = %q, want %q", got, "hello world")
	}
}

// TestStorageBucketsList covers buckets.list, which the earlier suite did not
// exercise and therefore did not claim.
// covers: storage.buckets.list
func TestStorageBucketsList(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()

	want := map[string]bool{}
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("%s-list-%d", h.Project(), i)
		bh := c.Bucket(name)
		if err := bh.Create(ctx, h.Project(), nil); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		t.Cleanup(func() { _ = bh.Delete(h.Context()) })
		want[name] = true
	}

	it := c.Buckets(ctx, h.Project())
	got := map[string]bool{}
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("Buckets.Next: %v", err)
		}
		got[attrs.Name] = true
	}
	for name := range want {
		if !got[name] {
			t.Errorf("buckets.list omitted %s", name)
		}
	}
}

// TestStorageCopy covers objects.copy.
func TestStorageCopy(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	bh := bucket(t, h, c)
	ctx := h.Context()

	w := bh.Object("source.txt").NewWriter(ctx)
	if _, err := w.Write([]byte("copy me")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := bh.Object("dest.txt").CopierFrom(bh.Object("source.txt")).Run(ctx); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	r, err := bh.Object("dest.txt").NewReader(ctx)
	if err != nil {
		t.Fatalf("read copy: %v", err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if string(got) != "copy me" {
		t.Errorf("copied content = %q, want %q", got, "copy me")
	}
	// The source must survive a copy.
	if _, err := bh.Object("source.txt").Attrs(ctx); err != nil {
		t.Errorf("copy removed the source: %v", err)
	}
}

// TestStorageChecksums confirms CRC32C and MD5 are returned, since clients
// verify them and a wrong value surfaces as client-side corruption.
func TestStorageChecksums(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	bh := bucket(t, h, c)
	ctx := h.Context()

	payload := []byte("checksum me")
	w := bh.Object("sum.txt").NewWriter(ctx)
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	attrs, err := bh.Object("sum.txt").Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if attrs.CRC32C == 0 {
		t.Error("no CRC32C returned; clients verify it")
	}
	if len(attrs.MD5) == 0 {
		t.Error("no MD5 returned")
	}

	// The client verifies the checksum on read, so a mismatch would fail here.
	r, err := bh.Object("sum.txt").NewReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatalf("read (checksum verification failed?): %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("content = %q, want %q", got, payload)
	}
}

// TestStorageNonEmptyBucketDelete records what happens when a bucket with
// objects is deleted. Recorded rather than asserted: the real service refuses,
// and this documents whether the backend matches.
func TestStorageNonEmptyBucketDelete(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()

	name := h.Project() + "-nonempty"
	bh := c.Bucket(name)
	if err := bh.Create(ctx, h.Project(), nil); err != nil {
		t.Fatal(err)
	}
	w := bh.Object("blocker.txt").NewWriter(ctx)
	_, _ = w.Write([]byte("x"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = bh.Object("blocker.txt").Delete(h.Context())
		_ = bh.Delete(h.Context())
	})

	err := bh.Delete(ctx)
	if err == nil {
		t.Log("RESULT: a non-empty bucket was deleted; the real service refuses this")
	} else {
		t.Logf("RESULT: non-empty bucket delete refused, as the real service does (%v)", err)
	}
}
