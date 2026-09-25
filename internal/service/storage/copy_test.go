package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"strings"
	"testing"

	gcs "cloud.google.com/go/storage"
)

// Compose joins sources in order; the result has no MD5, a CRC32C combined
// from the components, and a componentCount.
func TestStorageComposeChecksums(t *testing.T) {
	c, bh, _ := sdkBucket(t, "compose")
	ctx := context.Background()
	write(t, bh.Object("a"), []byte("hello "), nil)
	write(t, bh.Object("b"), []byte("world"), nil)
	composer := bh.Object("ab").ComposerFrom(bh.Object("a"), bh.Object("b"))
	composer.ContentType = "text/plain"
	a, err := composer.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := crc32.Checksum([]byte("hello world"), crc32.MakeTable(crc32.Castagnoli))
	if len(a.MD5) != 0 || a.CRC32C != want || a.ComponentCount != 2 || a.ContentType != "text/plain" || a.Size != 11 {
		t.Errorf("composed attrs: md5 %x, crc %x (want %x), components %d, type %q, size %d", a.MD5, a.CRC32C, want, a.ComponentCount, a.ContentType, a.Size)
	}
	if got := read(t, bh.Object("ab"), 0, -1); string(got) != "hello world" {
		t.Errorf("composed bytes = %q", got)
	}
	// A composite of a composite counts every component.
	write(t, bh.Object("c"), []byte("!"), nil)
	abc, err := bh.Object("abc").ComposerFrom(bh.Object("ab"), bh.Object("c")).Run(ctx)
	if err != nil || abc.ComponentCount != 3 {
		t.Errorf("compose of a composite: %d components, %v", abc.ComponentCount, err)
	}
	_ = c
}

// 32 sources compose; 33 are refused with 400.
func TestStorageComposeLimits(t *testing.T) {
	_, bh, _ := sdkBucket(t, "limits")
	var srcs []*gcs.ObjectHandle
	for i := 0; i < 33; i++ {
		o := bh.Object(fmt.Sprintf("p%02d", i))
		write(t, o, []byte{byte('a' + i%26)}, nil)
		srcs = append(srcs, o)
	}
	ctx := context.Background()
	if _, err := bh.Object("32").ComposerFrom(srcs[:32]...).Run(ctx); err != nil {
		t.Errorf("32 sources: %v", err)
	}
	h := lastHTTP
	body, _ := json.Marshal(map[string]any{"sourceObjects": func() []map[string]string {
		var out []map[string]string
		for i := 0; i < 33; i++ {
			out = append(out, map[string]string{"name": fmt.Sprintf("p%02d", i)})
		}
		return out
	}()})
	if code, resp := raw(t, "POST", h.URL+"/storage/v1/b/limits/o/33/compose", string(body)); code != 400 {
		t.Errorf("33 sources = %d %s; want 400", code, resp)
	}
	if code, resp := raw(t, "POST", h.URL+"/storage/v1/b/limits/o/x/compose", `{"sourceObjects":[{"name":"p00"}],"kmsKeyName":"k"}`); code != 400 || !strings.Contains(resp, "kmsKeyName") {
		t.Errorf("kmsKeyName = %d %s", code, resp)
	}
	if code, resp := raw(t, "POST", h.URL+"/storage/v1/b/limits/o/x/compose", `{"sourceObjects":[{"name":"p00","objectPreconditions":{"ifGenerationMatch":"1"}}]}`); code != 412 {
		t.Errorf("a stale source precondition = %d %s", code, resp)
	}
	if code, _ := raw(t, "POST", h.URL+"/storage/v1/b/limits/o/x/compose?deleteSourceObjects=true", `{"sourceObjects":[{"name":"p00"},{"name":"p01"}]}`); code != 200 {
		t.Fatal("compose with deleteSourceObjects failed")
	}
	if code, _ := raw(t, "GET", h.URL+"/storage/v1/b/limits/o/p00", ""); code != 404 {
		t.Errorf("deleteSourceObjects kept p00 (%d)", code)
	}
}

// Copy and rewrite keep the bytes, cross buckets and honour ifSource*;
// rewrite changes the storage class.
func TestStorageCopyAndRewriteInProcess(t *testing.T) {
	c, bh, _ := sdkBucket(t, "src")
	ctx := context.Background()
	if err := c.Bucket("dst").Create(ctx, "demo-project", nil); err != nil {
		t.Fatal(err)
	}
	a := write(t, bh.Object("orig"), []byte("copy me"), func(w *gcs.Writer) { w.Metadata = map[string]string{"k": "v"} })
	copied, err := c.Bucket("dst").Object("copied").CopierFrom(bh.Object("orig")).Run(ctx)
	if err != nil || copied.Metadata["k"] != "v" || copied.Size != 7 || !bytes.Equal(copied.MD5, a.MD5) {
		t.Fatalf("CopierFrom = %+v, %v", copied, err)
	}
	if got := read(t, c.Bucket("dst").Object("copied"), 0, -1); string(got) != "copy me" {
		t.Errorf("copied bytes = %q", got)
	}
	cp := c.Bucket("dst").Object("cold").CopierFrom(bh.Object("orig"))
	cp.StorageClass = "COLDLINE"
	cold, err := cp.Run(ctx)
	if err != nil || cold.StorageClass != "COLDLINE" {
		t.Errorf("rewrite to COLDLINE = %v, %v", cold.StorageClass, err)
	}
	if _, err := c.Bucket("dst").Object("x").CopierFrom(bh.Object("orig").If(gcs.Conditions{GenerationMatch: a.Generation + 1})).Run(ctx); httpCode(err) != 412 {
		t.Errorf("a stale ifSourceGenerationMatch = %v, want 412", err)
	}
	if _, err := c.Bucket("dst").Object("x").CopierFrom(bh.Object("absent")).Run(ctx); !errors.Is(err, gcs.ErrObjectNotExist) && httpCode(err) != 404 {
		t.Errorf("copying a missing source = %v", err)
	}
}

// maxBytesRewrittenPerCall=1 MiB on a 3 MiB object takes three calls; a
// used token is 410.
func TestStorageRewriteTokenLoop(t *testing.T) {
	_, bh, h := sdkBucket(t, "loop")
	write(t, bh.Object("big"), bytes.Repeat([]byte("x"), 3<<20), nil)
	url := h.URL + "/storage/v1/b/loop/o/big/rewriteTo/b/loop/o/copy?maxBytesRewrittenPerCall=1048576"
	token, calls := "", 0
	var last map[string]any
	for calls < 5 {
		u := url
		if token != "" {
			u += "&rewriteToken=" + token
		}
		code, body := raw(t, "POST", u, "")
		if code != 200 {
			t.Fatalf("call %d = %d %s", calls+1, code, body)
		}
		calls++
		last = map[string]any{}
		_ = json.Unmarshal([]byte(body), &last)
		if last["done"] == true {
			break
		}
		token, _ = last["rewriteToken"].(string)
	}
	if calls != 3 || last["totalBytesRewritten"] != "3145728" || last["objectSize"] != "3145728" || last["resource"] == nil {
		t.Errorf("after %d calls: %v", calls, last)
	}
	if code, _ := raw(t, "POST", url+"&rewriteToken="+token, ""); code != 410 {
		t.Errorf("a used token = %d, want 410", code)
	}
	if code, _ := raw(t, "POST", url+"&rewriteToken=never-issued", ""); code != 410 {
		t.Errorf("an unknown token = %d, want 410", code)
	}
}

// maxBytesRewrittenPerCall must be a multiple of 1 MiB.
func TestStorageRewriteBadChunk400(t *testing.T) {
	_, bh, h := sdkBucket(t, "badchunk")
	write(t, bh.Object("o"), []byte("x"), nil)
	for _, v := range []string{"1000", "1048577", "-1048576", "abc"} {
		if code, _ := raw(t, "POST", h.URL+"/storage/v1/b/badchunk/o/o/rewriteTo/b/badchunk/o/c?maxBytesRewrittenPerCall="+v, ""); code != 400 {
			t.Errorf("maxBytesRewrittenPerCall=%s = %d, want 400", v, code)
		}
	}
}

// Move renames atomically within a bucket.
func TestStorageMove(t *testing.T) {
	_, bh, _ := sdkBucket(t, "move")
	ctx := context.Background()
	write(t, bh.Object("from"), []byte("moving"), nil)
	a, err := bh.Object("from").Move(ctx, gcs.MoveObjectDestination{Object: "to"})
	if err != nil || a.Name != "to" {
		t.Fatalf("Move = %+v, %v", a, err)
	}
	if _, err := bh.Object("from").Attrs(ctx); !errors.Is(err, gcs.ErrObjectNotExist) {
		t.Errorf("the source survived a move: %v", err)
	}
	if got := read(t, bh.Object("to"), 0, -1); string(got) != "moving" {
		t.Errorf("moved bytes = %q", got)
	}
	write(t, bh.Object("taken"), []byte("x"), nil)
	if _, err := bh.Object("to").Move(ctx, gcs.MoveObjectDestination{Object: "taken", Conditions: &gcs.Conditions{DoesNotExist: true}}); httpCode(err) != 412 {
		t.Errorf("a move onto an existing object with DoesNotExist = %v, want 412", err)
	}
	if _, err := bh.Object("to").Attrs(ctx); err != nil {
		t.Errorf("a refused move lost the source: %v", err)
	}
}
