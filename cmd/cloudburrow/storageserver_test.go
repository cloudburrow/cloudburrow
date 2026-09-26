package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	gcsbuiltin "github.com/cloudburrow/cloudburrow/internal/service/storage"
	"github.com/cloudburrow/cloudburrow/internal/storageserver"
)

// storage-server binds loopback unless told otherwise (ADR-0004).
func TestStorageServerRefusesARemoteListenAddress(t *testing.T) {
	var out, errb bytes.Buffer
	err := runStorageServer(context.Background(), []string{"--listen", "0.0.0.0:0"}, &out, &errb)
	if err == nil || !strings.Contains(err.Error(), "--allow-remote") {
		t.Errorf("a non-loopback --listen = %v; want a refusal naming --allow-remote", err)
	}
}

func TestStorageServerServesUntilCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var out, errb bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- runStorageServer(ctx, []string{"--listen", "127.0.0.1:0"}, &out, &errb) }()
	cancel()
	if err := <-done; err != nil {
		t.Errorf("storage-server after cancel = %v", err)
	}
}

// openDir opens a builtin server on dir the way storage-server does, after
// preparing it for the mode, and returns it with a close.
func openDir(t *testing.T, dir string, ephemeral bool) (*httptest.Server, func()) {
	t.Helper()
	if err := storageserver.PrepareDir(dir, ephemeral); err != nil {
		t.Fatal(err)
	}
	meta, err := gcsbuiltin.OpenLogMetaStore(filepath.Join(dir, "meta"))
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := gcsbuiltin.OpenFileBlobStore(filepath.Join(dir, "objects"), gcsbuiltin.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := gcsbuiltin.NewServer(gcsbuiltin.Options{Meta: meta, Blobs: blobs})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(srv)
	return h, func() { h.Close(); _ = meta.Close() }
}

func hit(t *testing.T, method, url, body string, hdr map[string]string) (int, string, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

// Persistent mode keeps a bucket, a versioned object and an in-progress
// resumable session across a restart; ephemeral mode, on the same
// directory, starts with nothing (#512, the #481 lesson: by mode, not by
// what exists).
func TestStorageEphemeralClearsLeftoverStore(t *testing.T) {
	dir := t.TempDir()
	h, closeFn := openDir(t, dir, false)
	hit(t, "POST", h.URL+"/storage/v1/b?project=p", `{"name":"kept","versioning":{"enabled":true}}`, nil)
	hit(t, "POST", h.URL+"/upload/storage/v1/b/kept/o?uploadType=media&name=v", "one", nil)
	hit(t, "POST", h.URL+"/upload/storage/v1/b/kept/o?uploadType=media&name=v", "two", nil)
	_, _, hdr := hit(t, "POST", h.URL+"/upload/storage/v1/b/kept/o?uploadType=resumable&name=r", "{}", nil)
	id := hdr.Get("X-GUploader-UploadID")
	chunk := strings.Repeat("x", 256<<10)
	if code, _, _ := hit(t, "PUT", h.URL+"/upload/storage/v1/b/kept/o?uploadType=resumable&upload_id="+id, chunk, map[string]string{"Content-Range": "bytes 0-262143/*"}); code != 308 {
		t.Fatalf("first chunk = %d", code)
	}
	closeFn()

	h, closeFn = openDir(t, dir, false)
	if _, body, _ := hit(t, "GET", h.URL+"/storage/v1/b/kept/o?versions=true", "", nil); strings.Count(body, `"name": "v"`) != 2 {
		t.Errorf("persistent restart: versions = %s", body)
	}
	if code, _, rh := hit(t, "PUT", h.URL+"/upload/storage/v1/b/kept/o?uploadType=resumable&upload_id="+id, "", map[string]string{"Content-Range": "bytes */*"}); code != 308 || rh.Get("Range") != "bytes=0-262143" {
		t.Errorf("persistent restart: the session = %d %q; want its 256 KiB", code, rh.Get("Range"))
	}
	closeFn()

	h, closeFn = openDir(t, dir, true)
	defer closeFn()
	if code, _, _ := hit(t, "GET", h.URL+"/storage/v1/b/kept", "", nil); code != 404 {
		t.Errorf("ephemeral mode found the earlier run's bucket: %d", code)
	}
	if code, _, _ := hit(t, "PUT", h.URL+"/upload/storage/v1/b/kept/o?uploadType=resumable&upload_id="+id, "", map[string]string{"Content-Range": "bytes */*"}); code != 404 {
		t.Errorf("ephemeral mode found the earlier run's session: %d", code)
	}
}

func TestStorageServerRefusesAnUnknownMode(t *testing.T) {
	var out, errb bytes.Buffer
	err := runStorageServer(context.Background(), []string{"--listen", "127.0.0.1:0", "--mode", "sometimes"}, &out, &errb)
	if err == nil || !strings.Contains(err.Error(), "--mode") {
		t.Errorf("--mode sometimes = %v", err)
	}
}
