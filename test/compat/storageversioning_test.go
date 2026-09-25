//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
)

// Object versioning (#498), to
// docs.cloud.google.com/storage/docs/object-versioning, through the official
// Go SDK. fake-gcs-server refuses versioning in persistent mode (#374), so
// these run against the builtin server.

func versionedBucket(t *testing.T, h *Harness, c *storage.Client, suffix string) *storage.BucketHandle {
	t.Helper()
	ctx := h.Context()
	bh := c.Bucket(h.Project() + "-" + suffix)
	if err := bh.Create(ctx, h.Project(), &storage.BucketAttrs{VersioningEnabled: true}); err != nil {
		t.Fatalf("create versioned bucket: %v", err)
	}
	t.Cleanup(func() { emptyAndDelete(context.Background(), bh) })
	return bh
}

func putObject(t *testing.T, ctx context.Context, o *storage.ObjectHandle, body string) *storage.ObjectAttrs {
	t.Helper()
	w := o.NewWriter(ctx)
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("write %s: %v", o.ObjectName(), err)
	}
	return w.Attrs()
}

func versionsOf(t *testing.T, ctx context.Context, bh *storage.BucketHandle, versions bool) []*storage.ObjectAttrs {
	t.Helper()
	var out []*storage.ObjectAttrs
	it := bh.Objects(ctx, &storage.Query{Versions: versions})
	for {
		a, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out
		}
		if err != nil {
			t.Fatalf("list (versions=%v): %v", versions, err)
		}
		out = append(out, a)
	}
}

// TestStorageVersioningKeepsNoncurrent: an overwrite keeps the old version,
// so a listing with versions shows 2 and a plain listing 1.
// covers: storage.objects.list, storage.objects.insert, storage.objects.get
func TestStorageVersioningKeepsNoncurrent(t *testing.T) {
	builtinOnly(t, "versioning in persistent mode, #374")
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()
	bh := versionedBucket(t, h, c, "versions")
	o := bh.Object("doc.txt")
	first := putObject(t, ctx, o, "one")
	second := putObject(t, ctx, o, "two")
	all := versionsOf(t, ctx, bh, true)
	if len(all) != 2 || all[0].Generation != first.Generation || all[1].Generation != second.Generation || all[0].Deleted.IsZero() {
		t.Fatalf("versions = %d, want the noncurrent first (with timeDeleted) and the live second", len(all))
	}
	if live := versionsOf(t, ctx, bh, false); len(live) != 1 || live[0].Generation != second.Generation {
		t.Errorf("plain list = %d objects, want the live one", len(live))
	}
	a, err := o.Generation(first.Generation).Attrs(ctx)
	if err != nil || a.Size != 3 {
		t.Errorf("the noncurrent version by generation = %v, %v", a, err)
	}
}

// TestStorageVersioningDeleteKeepsNoncurrent: a delete without a generation
// leaves the version noncurrent, and one with a generation removes it.
// covers: storage.objects.delete
func TestStorageVersioningDeleteKeepsNoncurrent(t *testing.T) {
	builtinOnly(t, "versioning in persistent mode, #374")
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()
	bh := versionedBucket(t, h, c, "vdelete")
	o := bh.Object("doc.txt")
	first := putObject(t, ctx, o, "one")
	if err := o.Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Attrs(ctx); !errors.Is(err, storage.ErrObjectNotExist) {
		t.Errorf("the live object after delete = %v", err)
	}
	if all := versionsOf(t, ctx, bh, true); len(all) != 1 || all[0].Generation != first.Generation {
		t.Fatalf("versions after delete = %d, want the one noncurrent", len(all))
	}
	if err := o.Generation(first.Generation).Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if all := versionsOf(t, ctx, bh, true); len(all) != 0 {
		t.Errorf("versions after deleting by generation = %d, want 0", len(all))
	}
}

// TestStorageVersioningDisableKeepsExisting: disabling versioning stops new
// noncurrent versions and keeps those that exist.
// covers: storage.buckets.patch
func TestStorageVersioningDisableKeepsExisting(t *testing.T) {
	builtinOnly(t, "versioning in persistent mode, #374")
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()
	bh := versionedBucket(t, h, c, "voff")
	o := bh.Object("doc.txt")
	first := putObject(t, ctx, o, "one")
	putObject(t, ctx, o, "two")
	if _, err := bh.Update(ctx, storage.BucketAttrsToUpdate{VersioningEnabled: false}); err != nil {
		t.Fatal(err)
	}
	third := putObject(t, ctx, o, "three")
	all := versionsOf(t, ctx, bh, true)
	if len(all) != 2 || all[0].Generation != first.Generation || all[1].Generation != third.Generation {
		t.Errorf("versions after disabling = %d; want the kept noncurrent one and the live one", len(all))
	}
}

// TestStorageDoesNotExistWithOnlyNoncurrent: ifGenerationMatch=0 succeeds
// when only noncurrent versions exist (request-preconditions docs).
// covers: storage.objects.insert
func TestStorageDoesNotExistWithOnlyNoncurrent(t *testing.T) {
	builtinOnly(t, "versioning in persistent mode, #374")
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()
	bh := versionedBucket(t, h, c, "vabsent")
	o := bh.Object("doc.txt")
	putObject(t, ctx, o, "one")
	if err := o.Delete(ctx); err != nil {
		t.Fatal(err)
	}
	w := o.If(storage.Conditions{DoesNotExist: true}).NewWriter(ctx)
	_, _ = w.Write([]byte("again"))
	if err := w.Close(); err != nil {
		t.Fatalf("ifGenerationMatch=0 with only a noncurrent version: %v", err)
	}
	if all := versionsOf(t, ctx, bh, true); len(all) != 2 {
		t.Errorf("versions = %d, want the noncurrent one and the new live one", len(all))
	}
}

// Versioning across a restart of a persistent builtin server. CI runs
// TestStorageVersioningPersistentMode twice against one --data-dir: with
// the probe phase "setup" before stopping the server, and "check" after
// starting it again.
const envStorageVersioningProbe = "CLOUDBURROW_TEST_STORAGE_VERSIONING_PROBE"

type versioningProbe struct {
	Bucket     string `json:"bucket"`
	Noncurrent int64  `json:"noncurrent"`
	Live       int64  `json:"live"`
}

// TestStorageVersioningPersistentMode: the noncurrent and live versions
// written before a restart are both there after it, with their generations.
// covers: storage.objects.list
func TestStorageVersioningPersistentMode(t *testing.T) {
	phase, path, _ := strings.Cut(os.Getenv(envStorageVersioningProbe), ":")
	if phase == "" {
		t.Skipf("%s is not set: CI runs this around a restart of the builtin server", envStorageVersioningProbe)
	}
	builtinOnly(t, "versioning in persistent mode, #374")
	h := New(t)
	c := storageClient(t, h)
	ctx := h.Context()
	switch phase {
	case "setup":
		// Created without cleanup: the check after the restart removes it.
		bh := c.Bucket("versioning-restart-probe")
		if err := bh.Create(ctx, h.Project(), &storage.BucketAttrs{VersioningEnabled: true}); err != nil {
			t.Fatal(err)
		}
		o := bh.Object("doc.txt")
		first := putObject(t, ctx, o, "before")
		second := putObject(t, ctx, o, "after")
		b, _ := json.Marshal(versioningProbe{Bucket: "versioning-restart-probe", Noncurrent: first.Generation, Live: second.Generation})
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
	case "check":
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("the probe the setup left: %v", err)
		}
		var p versioningProbe
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		bh := c.Bucket(p.Bucket)
		t.Cleanup(func() { emptyAndDelete(context.Background(), bh) })
		all := versionsOf(t, ctx, bh, true)
		if len(all) != 2 || all[0].Generation != p.Noncurrent || all[1].Generation != p.Live || all[0].Deleted.IsZero() {
			t.Fatalf("after the restart: %d versions; want noncurrent %d and live %d", len(all), p.Noncurrent, p.Live)
		}
		r, err := bh.Object("doc.txt").Generation(p.Noncurrent).NewReader(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		buf := make([]byte, 16)
		n, _ := r.Read(buf)
		if string(buf[:n]) != "before" {
			t.Errorf("the noncurrent version reads %q after the restart", buf[:n])
		}
	default:
		t.Fatalf("%s must be setup:<file> or check:<file>, not %q", envStorageVersioningProbe, phase)
	}
}
