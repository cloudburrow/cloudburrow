//go:build compat

package compat

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"testing"

	"cloud.google.com/go/storage"
)

// TestStorageCSEKParity (#1345): customer-supplied encryption keys, with
// the official Go client (ObjectHandle.Key) on both transports. An object
// written over gRPC with a key reads over the JSON API with the same key,
// and one written over the JSON API reads over gRPC; a read without the key
// or with another key is refused on both; the metadata names the key's
// SHA-256 and, read without the key, has no checksums; a rewrite rotates
// the key, and a compose takes sources encrypted with its key. Measured
// first: gRPC refused CommonObjectRequestParams UNIMPLEMENTED, and the JSON
// API ignored the key headers.
//
// covers: google.storage.v2.Storage/WriteObject, google.storage.v2.Storage/ReadObject, google.storage.v2.Storage/GetObject
// covers: google.storage.v2.Storage/RewriteObject, google.storage.v2.Storage/ComposeObject
// covers: storage.objects.insert, storage.objects.get, storage.objects.rewrite
func TestStorageCSEKParity(t *testing.T) {
	h := New(t)
	g := storageGRPCClient(t, h, false)
	j := storageClient(t, h)
	ctx := h.Context()
	jb := bucket(t, h, j)
	gb := g.Bucket(jb.BucketName())
	key := bytes.Repeat([]byte{7}, 32)
	other := bytes.Repeat([]byte{9}, 32)
	sum := sha256.Sum256(key)
	wantSum := base64.StdEncoding.EncodeToString(sum[:])

	put := func(o *storage.ObjectHandle, body string) {
		t.Helper()
		w := o.NewWriter(ctx)
		if _, err := io.WriteString(w, body); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("write %s with a key: %v", o.ObjectName(), err)
		}
		if a := w.Attrs(); a.CustomerKeySHA256 != wantSum {
			t.Errorf("%s written: CustomerKeySHA256 %q", o.ObjectName(), a.CustomerKeySHA256)
		}
	}
	get := func(o *storage.ObjectHandle) (string, error) {
		r, err := o.NewReader(ctx)
		if err != nil {
			return "", err
		}
		defer r.Close()
		b, err := io.ReadAll(r)
		return string(b), err
	}

	put(gb.Object("by-grpc").Key(key), "written over gRPC")
	put(jb.Object("by-json").Key(key), "written over JSON")
	for _, c := range []struct {
		name string
		bh   *storage.BucketHandle
		obj  string
		want string
	}{
		{"JSON reads the gRPC write", jb, "by-grpc", "written over gRPC"},
		{"gRPC reads the JSON write", gb, "by-json", "written over JSON"},
		{"gRPC reads its own write", gb, "by-grpc", "written over gRPC"},
	} {
		if got, err := get(c.bh.Object(c.obj).Key(key)); err != nil || got != c.want {
			t.Errorf("%s: %q, %v", c.name, got, err)
		}
	}
	for name, bh := range map[string]*storage.BucketHandle{"JSON": jb, "gRPC": gb} {
		if _, err := get(bh.Object("by-grpc")); err == nil {
			t.Errorf("%s read without the key succeeded", name)
		}
		if _, err := get(bh.Object("by-json").Key(other)); err == nil {
			t.Errorf("%s read with another key succeeded", name)
		}
		a, err := bh.Object("by-json").Attrs(ctx)
		if err != nil {
			t.Fatalf("%s metadata without the key: %v", name, err)
		}
		if a.CustomerKeySHA256 == "" || len(a.MD5) != 0 || a.CRC32C != 0 {
			t.Errorf("%s metadata without the key: keySha256 %q, md5 %x, crc32c %x; want the hash and no checksums", name, a.CustomerKeySHA256, a.MD5, a.CRC32C)
		}
		if a, err := bh.Object("by-json").Key(key).Attrs(ctx); err != nil || len(a.MD5) == 0 {
			t.Errorf("%s metadata with the key: %+v, %v; want the checksums", name, a, err)
		}
	}

	// A rewrite rotates the key, over each transport.
	for name, bh := range map[string]*storage.BucketHandle{"JSON": jb, "gRPC": gb} {
		dst := bh.Object("rotated-" + name).Key(other)
		if _, err := dst.CopierFrom(bh.Object("by-grpc").Key(key)).Run(ctx); err != nil {
			t.Fatalf("%s rewrite to a new key: %v", name, err)
		}
		if got, err := get(jb.Object("rotated-" + name).Key(other)); err != nil || got != "written over gRPC" {
			t.Errorf("%s rotated object reads %q, %v", name, got, err)
		}
		if _, err := get(gb.Object("rotated-" + name).Key(key)); err == nil {
			t.Errorf("%s rotated object reads with the old key", name)
		}
		if _, err := bh.Object("no-key").CopierFrom(bh.Object("by-grpc")).Run(ctx); err == nil {
			t.Errorf("%s rewrite of an encrypted source without its key succeeded", name)
		}
	}

	// A compose over gRPC takes sources encrypted with its key.
	ab := gb.Object("ab").Key(key)
	if _, err := ab.ComposerFrom(gb.Object("by-grpc"), gb.Object("by-json")).Run(ctx); err != nil {
		t.Fatalf("compose with a key: %v", err)
	}
	if got, err := get(jb.Object("ab").Key(key)); err != nil || got != "written over gRPCwritten over JSON" {
		t.Errorf("composite reads %q, %v", got, err)
	}
}
