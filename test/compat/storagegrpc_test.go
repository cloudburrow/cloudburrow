//go:build compat

package compat

import (
	"bytes"
	"errors"
	"io"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/iam"
	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// storageGRPCClient returns the official Go client on its gRPC transport
// (storage.NewGRPCClient), pointed at the instance's Cloud Storage address
// as the emulator docs describe: the same host:port as the JSON API, which
// serves google.storage.v2 as h2c (#1321), with insecure credentials. bidi
// selects BidiReadObject for reads instead of ReadObject.
func storageGRPCClient(t *testing.T, h *Harness, bidi bool) *storage.Client {
	t.Helper()
	addr := stripScheme(h.Endpoint(EnvStorage))
	opts := []option.ClientOption{option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
	if bidi {
		opts = append(opts, storage.WithGRPCBidiReads())
	}
	c, err := storage.NewGRPCClient(h.Context(), opts...)
	if err != nil {
		t.Fatalf("storage.NewGRPCClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestStorageGRPCBuckets: bucket CRUD, IAM and the retention lock over
// gRPC, each read back over the JSON API.
// covers: google.storage.v2.Storage/CreateBucket, google.storage.v2.Storage/GetBucket, google.storage.v2.Storage/ListBuckets, google.storage.v2.Storage/UpdateBucket, google.storage.v2.Storage/DeleteBucket
// covers: google.storage.v2.Storage/LockBucketRetentionPolicy, google.storage.v2.Storage/GetIamPolicy, google.storage.v2.Storage/SetIamPolicy, google.storage.v2.Storage/TestIamPermissions
func TestStorageGRPCBuckets(t *testing.T) {
	h := New(t)
	g := storageGRPCClient(t, h, false)
	j := storageClient(t, h)
	ctx := h.Context()

	name := h.Project() + "-grpc-b"
	bh := g.Bucket(name)
	if err := bh.Create(ctx, h.Project(), &storage.BucketAttrs{Labels: map[string]string{"team": "a"},
		RetentionPolicy: &storage.RetentionPolicy{RetentionPeriod: time.Hour}, UniformBucketLevelAccess: storage.UniformBucketLevelAccess{Enabled: true}}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { emptyAndDelete(ctx, j.Bucket(name)) })
	if err := bh.Create(ctx, h.Project(), nil); status.Code(err) != codes.AlreadyExists {
		t.Errorf("second Create = %v, want ALREADY_EXISTS", err)
	}
	attrs, err := bh.Attrs(ctx)
	if err != nil || attrs.Labels["team"] != "a" || attrs.RetentionPolicy == nil || attrs.RetentionPolicy.RetentionPeriod != time.Hour {
		t.Fatalf("Attrs = %+v, %v", attrs, err)
	}
	ja, err := j.Bucket(name).Attrs(ctx)
	if err != nil || ja.MetaGeneration != attrs.MetaGeneration || ja.Labels["team"] != "a" {
		t.Fatalf("JSON Attrs = %+v, %v; want gRPC's metageneration %d", ja, err, attrs.MetaGeneration)
	}

	up, err := bh.Update(ctx, storage.BucketAttrsToUpdate{VersioningEnabled: true})
	if err != nil || !up.VersioningEnabled || up.MetaGeneration != attrs.MetaGeneration+1 {
		t.Fatalf("Update = %+v, %v", up, err)
	}
	if _, err := bh.If(storage.BucketConditions{MetagenerationMatch: attrs.MetaGeneration}).Update(ctx, storage.BucketAttrsToUpdate{VersioningEnabled: false}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Update with a stale metageneration = %v, want FAILED_PRECONDITION", err)
	}
	if ja, _ := j.Bucket(name).Attrs(ctx); ja == nil || !ja.VersioningEnabled {
		t.Errorf("JSON sees versioning %+v", ja)
	}

	found := false
	it := g.Buckets(ctx, h.Project())
	for {
		b, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("Buckets: %v", err)
		}
		found = found || b.Name == name
	}
	if !found {
		t.Errorf("ListBuckets does not list %s", name)
	}

	p, err := bh.IAM().Policy(ctx)
	if err != nil {
		t.Fatalf("Policy: %v", err)
	}
	p.Add("user:dev@example.com", iam.RoleName("roles/storage.objectViewer"))
	if err := bh.IAM().SetPolicy(ctx, p); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}
	jp, err := j.Bucket(name).IAM().Policy(ctx)
	if err != nil || !jp.HasRole("user:dev@example.com", "roles/storage.objectViewer") {
		t.Fatalf("JSON policy = %v, %v", jp, err)
	}
	perms, err := bh.IAM().TestPermissions(ctx, []string{"storage.objects.get"})
	if err != nil || len(perms) != 1 {
		t.Errorf("TestPermissions = %v, %v", perms, err)
	}

	cur, _ := bh.Attrs(ctx)
	if err := bh.If(storage.BucketConditions{MetagenerationMatch: cur.MetaGeneration}).LockRetentionPolicy(ctx); err != nil {
		t.Fatalf("LockRetentionPolicy: %v", err)
	}
	if ja, _ := j.Bucket(name).Attrs(ctx); ja == nil || !ja.RetentionPolicy.IsLocked {
		t.Errorf("JSON sees retention %+v", ja.RetentionPolicy)
	}

	empty := g.Bucket(h.Project() + "-grpc-e")
	if err := empty.Create(ctx, h.Project(), nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := empty.Delete(ctx); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := j.Bucket(h.Project() + "-grpc-e").Attrs(ctx); !errors.Is(err, storage.ErrBucketNotExist) {
		t.Errorf("JSON Attrs after a gRPC delete = %v", err)
	}
	if _, err := empty.Attrs(ctx); !errors.Is(err, storage.ErrBucketNotExist) {
		t.Errorf("gRPC Attrs after delete = %v, want ErrBucketNotExist", err)
	}
}

// TestStorageGRPCObjects: writes, reads (whole, ranged, ReadObject and
// BidiReadObject), listing, update, compose, rewrite, move, delete and
// restore over gRPC, crossed with the JSON API both ways, under the same
// preconditions and checksums.
// covers: google.storage.v2.Storage/BidiWriteObject, google.storage.v2.Storage/StartResumableWrite, google.storage.v2.Storage/ReadObject, google.storage.v2.Storage/BidiReadObject, google.storage.v2.Storage/GetObject, google.storage.v2.Storage/ListObjects
// covers: google.storage.v2.Storage/UpdateObject, google.storage.v2.Storage/DeleteObject, google.storage.v2.Storage/ComposeObject, google.storage.v2.Storage/RewriteObject, google.storage.v2.Storage/RestoreObject, google.storage.v2.Storage/MoveObject
func TestStorageGRPCObjects(t *testing.T) {
	h := New(t)
	g := storageGRPCClient(t, h, false)
	gb := storageGRPCClient(t, h, true)
	j := storageClient(t, h)
	ctx := h.Context()
	jb := bucket(t, h, j)
	bh, bb := g.Bucket(jb.BucketName()), gb.Bucket(jb.BucketName())

	// A resumable write over several chunks, then a one-shot write.
	payload := bytes.Repeat([]byte("cloudburrow-grpc"), 300_000) // 4.8 MB
	w := bh.Object("dir/big.bin").NewWriter(ctx)
	w.ChunkSize = 1 << 20
	w.ContentType = "application/octet-stream"
	w.Metadata = map[string]string{"via": "grpc"}
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("resumable Close: %v", err)
	}
	big := w.Attrs()
	jo, err := jb.Object("dir/big.bin").Attrs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if jo.Generation != big.Generation || jo.CRC32C != big.CRC32C || !bytes.Equal(jo.MD5, big.MD5) || jo.Size != int64(len(payload)) || jo.Metadata["via"] != "grpc" {
		t.Fatalf("JSON attrs %+v differ from the gRPC write's %+v", jo, big)
	}
	w = bh.Object("dir/one.txt").NewWriter(ctx)
	w.ChunkSize = 0
	_, _ = w.Write([]byte("one shot"))
	if err := w.Close(); err != nil {
		t.Fatalf("one-shot Close: %v", err)
	}

	// The same preconditions as JSON.
	w = bh.Object("dir/one.txt").If(storage.Conditions{DoesNotExist: true}).NewWriter(ctx)
	_, _ = w.Write([]byte("x"))
	if err := w.Close(); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DoesNotExist write over an object = %v, want FAILED_PRECONDITION", err)
	}
	// The same checksums: a wrong CRC32C is refused and stores nothing.
	w = bh.Object("dir/bad.txt").NewWriter(ctx)
	w.ChunkSize = 0
	w.SendCRC32C, w.CRC32C = true, 42
	_, _ = w.Write([]byte("checksummed"))
	if err := w.Close(); err == nil {
		t.Error("a write with a wrong crc32c was accepted")
	}
	if _, err := jb.Object("dir/bad.txt").Attrs(ctx); !errors.Is(err, storage.ErrObjectNotExist) {
		t.Errorf("JSON Attrs of the refused write = %v", err)
	}

	// JSON writes, gRPC reads, both read paths.
	jw := jb.Object("hello.txt").NewWriter(ctx)
	_, _ = jw.Write([]byte("hello over grpc"))
	if err := jw.Close(); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]*storage.BucketHandle{"ReadObject": bh, "BidiReadObject": bb} {
		r, err := c.Object("hello.txt").NewReader(ctx)
		if err != nil {
			t.Fatalf("%s NewReader: %v", name, err)
		}
		got, _ := io.ReadAll(r)
		_ = r.Close()
		if string(got) != "hello over grpc" || r.Attrs.Generation != jw.Attrs().Generation {
			t.Errorf("%s read %q gen %d", name, got, r.Attrs.Generation)
		}
		r, err = c.Object("hello.txt").NewRangeReader(ctx, 11, 4)
		if err != nil {
			t.Fatalf("%s NewRangeReader: %v", name, err)
		}
		got, _ = io.ReadAll(r)
		_ = r.Close()
		if string(got) != "grpc" {
			t.Errorf("%s ranged read %q, want grpc", name, got)
		}
		r, err = c.Object("dir/big.bin").NewReader(ctx)
		if err != nil {
			t.Fatalf("%s NewReader big: %v", name, err)
		}
		got, _ = io.ReadAll(r)
		_ = r.Close()
		if !bytes.Equal(got, payload) {
			t.Errorf("%s big read: %d bytes, want %d", name, len(got), len(payload))
		}
		if _, err := c.Object("hello.txt").If(storage.Conditions{GenerationMatch: 1}).NewReader(ctx); status.Code(err) != codes.FailedPrecondition {
			t.Errorf("%s read with a stale generation = %v, want FAILED_PRECONDITION", name, err)
		}
		if _, err := c.Object("missing").NewReader(ctx); !errors.Is(err, storage.ErrObjectNotExist) {
			t.Errorf("%s read of a missing object = %v, want ErrObjectNotExist", name, err)
		}
	}

	// Listing, with prefix and delimiter.
	var names []string
	it := bh.Objects(ctx, &storage.Query{Delimiter: "/"})
	for {
		a, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("Objects: %v", err)
		}
		names = append(names, a.Name+a.Prefix)
	}
	// The gRPC iterator yields a page's objects before its prefixes.
	sort.Strings(names)
	if strings.Join(names, ",") != "dir/,hello.txt" {
		t.Errorf("list with delimiter = %v, want [dir/ hello.txt]", names)
	}
	names = nil
	it = bh.Objects(ctx, &storage.Query{Prefix: "dir/"})
	for {
		a, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("Objects: %v", err)
		}
		names = append(names, a.Name)
	}
	if strings.Join(names, ",") != "dir/big.bin,dir/one.txt" {
		t.Errorf("list with prefix = %v", names)
	}

	// Update, read back over JSON; a stale metageneration is refused.
	oa, err := bh.Object("hello.txt").Attrs(ctx)
	if err != nil {
		t.Fatalf("Attrs: %v", err)
	}
	if _, err := bh.Object("hello.txt").Update(ctx, storage.ObjectAttrsToUpdate{ContentType: "text/plain", Metadata: map[string]string{"k": "v"}}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if jo, _ := jb.Object("hello.txt").Attrs(ctx); jo == nil || jo.ContentType != "text/plain" || jo.Metadata["k"] != "v" {
		t.Errorf("JSON sees %+v", jo)
	}
	if _, err := bh.Object("hello.txt").If(storage.Conditions{MetagenerationMatch: oa.Metageneration}).Update(ctx, storage.ObjectAttrsToUpdate{ContentType: "x/y"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("Update with a stale metageneration = %v, want FAILED_PRECONDITION", err)
	}

	// Compose and rewrite (Copier).
	comp, err := bh.Object("composed").ComposerFrom(bh.Object("hello.txt"), bh.Object("dir/one.txt")).Run(ctx)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	r, _ := jb.Object("composed").NewReader(ctx)
	got, _ := io.ReadAll(r)
	_ = r.Close()
	if string(got) != "hello over grpcone shot" || comp.ComponentCount != 2 {
		t.Errorf("composed = %q (%d components)", got, comp.ComponentCount)
	}
	cp, err := bh.Object("copied.bin").CopierFrom(bh.Object("dir/big.bin")).Run(ctx)
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if cp.CRC32C != big.CRC32C || cp.Size != big.Size {
		t.Errorf("copy = %+v", cp)
	}

	// Move.
	if _, err := bh.Object("copied.bin").Move(ctx, storage.MoveObjectDestination{Object: "moved.bin"}); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if _, err := jb.Object("moved.bin").Attrs(ctx); err != nil {
		t.Errorf("JSON Attrs of the moved object: %v", err)
	}

	// Delete (soft, under the default policy), then restore.
	cur, _ := bh.Object("composed").Attrs(ctx)
	if err := bh.Object("composed").Delete(ctx); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := jb.Object("composed").Attrs(ctx); !errors.Is(err, storage.ErrObjectNotExist) {
		t.Errorf("JSON Attrs after a gRPC delete = %v", err)
	}
	if _, err := bh.Object("composed").Attrs(ctx); !errors.Is(err, storage.ErrObjectNotExist) {
		t.Errorf("gRPC Attrs after delete = %v", err)
	}
	if _, err := bh.Object("composed").Generation(cur.Generation).Restore(ctx, &storage.RestoreOptions{}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := jb.Object("composed").Attrs(ctx); err != nil {
		t.Errorf("JSON Attrs after restore: %v", err)
	}
}
