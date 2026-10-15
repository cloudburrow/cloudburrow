//go:build compat

package compat

import (
	"errors"
	"io"
	"testing"

	"cloud.google.com/go/storage"
)

// TestStorageGRPCComposeDeletesSources (#1345): ComposeObject with
// delete_source_objects, over gRPC with the official Go client
// (Composer.DeleteSourceObjects), writes the composite and deletes its
// sources, as the JSON API's compose does. Measured first: it was refused
// UNIMPLEMENTED.
//
// covers: google.storage.v2.Storage/ComposeObject
func TestStorageGRPCComposeDeletesSources(t *testing.T) {
	h := New(t)
	g := storageGRPCClient(t, h, false)
	j := storageClient(t, h)
	ctx := h.Context()
	jb := bucket(t, h, j)
	bh := g.Bucket(jb.BucketName())
	for name, body := range map[string]string{"a": "hello, ", "b": "world"} {
		w := bh.Object(name).NewWriter(ctx)
		if _, err := io.WriteString(w, body); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	c := bh.Object("ab").ComposerFrom(bh.Object("a"), bh.Object("b"))
	c.DeleteSourceObjects = true
	attrs, err := c.Run(ctx)
	if err != nil {
		t.Fatalf("compose with DeleteSourceObjects: %v", err)
	}
	if attrs.Size != 12 || attrs.ComponentCount != 2 {
		t.Errorf("composite attrs: size %d, components %d; want 12, 2", attrs.Size, attrs.ComponentCount)
	}
	r, err := jb.Object("ab").NewReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	_ = r.Close()
	if string(got) != "hello, world" {
		t.Errorf("composite reads %q", got)
	}
	for _, name := range []string{"a", "b"} {
		if _, err := jb.Object(name).Attrs(ctx); !errors.Is(err, storage.ErrObjectNotExist) {
			t.Errorf("source %s after compose: %v; want it deleted", name, err)
		}
	}
}
