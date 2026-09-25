package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gcs "cloud.google.com/go/storage"
	"google.golang.org/api/option"
)

// The Go client uploads anything over its chunk size resumably, with every
// chunk a POST carrying X-GUploader-No-308.
func TestStorageResumableUploadAndRangedRead(t *testing.T) {
	_, bh, _ := sdkBucket(t, "resumable")
	body := make([]byte, 3*chunkUnit+12345)
	_, _ = rand.Read(body)
	a := write(t, bh.Object("big.bin"), body, func(w *gcs.Writer) {
		w.ChunkSize = chunkUnit
		w.Metadata = map[string]string{"kind": "resumable"}
	})
	if a.Size != int64(len(body)) || a.Metadata["kind"] != "resumable" || a.Generation == 0 {
		t.Errorf("attrs = size %d, metadata %v, generation %d", a.Size, a.Metadata, a.Generation)
	}
	if got := read(t, bh.Object("big.bin"), 0, -1); !bytes.Equal(got, body) {
		t.Errorf("read back %d bytes, want the %d written", len(got), len(body))
	}
	if got := read(t, bh.Object("big.bin"), chunkUnit-5, 10); !bytes.Equal(got, body[chunkUnit-5:chunkUnit+5]) {
		t.Error("a range across a chunk boundary differs")
	}
	// A resumable upload whose preconditions fail at the end is refused.
	w := bh.Object("big.bin").If(gcs.Conditions{DoesNotExist: true}).NewWriter(context.Background())
	w.ChunkSize = chunkUnit
	_, _ = w.Write(body)
	if err := w.Close(); httpCode(err) != 412 {
		t.Errorf("a resumable DoesNotExist over an existing object = %v, want 412", err)
	}
}

type session struct {
	t   *testing.T
	h   *httptest.Server
	loc string
}

func startSession(t *testing.T, h *httptest.Server, name string) *session {
	t.Helper()
	req, _ := http.NewRequest("POST", h.URL+"/upload/storage/v1/b/raw/o?uploadType=resumable&name="+name, strings.NewReader(`{"contentType":"application/octet-stream"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Location") == "" {
		t.Fatalf("initiate = %d, Location %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	return &session{t: t, h: h, loc: resp.Header.Get("Location")}
}

func (s *session) put(method, contentRange string, body []byte, hdr map[string]string) *http.Response {
	s.t.Helper()
	req, _ := http.NewRequest(method, s.loc, bytes.NewReader(body))
	if contentRange != "" {
		req.Header.Set("Content-Range", contentRange)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(b))
	return resp
}

// The PUT path without X-GUploader-No-308 gets a real 308 with Range.
func TestStorageResumableReal308WithRange(t *testing.T) {
	h := rawServer(t)
	s := startSession(t, h, "r308")
	chunk := bytes.Repeat([]byte{7}, chunkUnit)
	resp := s.put("PUT", fmt.Sprintf("bytes 0-%d/*", chunkUnit-1), chunk, nil)
	if resp.StatusCode != 308 || resp.Header.Get("Range") != "bytes=0-262143" {
		t.Errorf("an incomplete chunk = %d, Range %q; want 308 bytes=0-262143", resp.StatusCode, resp.Header.Get("Range"))
	}
	// Not a multiple of 256 KiB and not final: refused.
	if resp := s.put("PUT", fmt.Sprintf("bytes %d-%d/*", chunkUnit, chunkUnit+99), make([]byte, 100), nil); resp.StatusCode != 400 {
		t.Errorf("a short non-final chunk = %d, want 400", resp.StatusCode)
	}
	// Re-sending persisted bytes is harmless: they are skipped.
	resp = s.put("PUT", fmt.Sprintf("bytes 0-%d/%d", chunkUnit+9, chunkUnit+10), append(bytes.Repeat([]byte{9}, chunkUnit), []byte("0123456789")...), nil)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), fmt.Sprintf(`"size": "%d"`, chunkUnit+10)) {
		t.Fatalf("the final chunk = %d %s", resp.StatusCode, b)
	}
	code, got := raw(t, "GET", h.URL+"/raw/r308", "")
	if code != 200 || !strings.HasPrefix(got, string(chunk[:10])) || !strings.HasSuffix(got, "0123456789") {
		t.Errorf("the re-sent prefix replaced persisted bytes: %d, %q…", code, got[:10])
	}
	// The completed session answers the object again.
	if resp := s.put("PUT", "bytes */*", nil, nil); resp.StatusCode != 200 {
		t.Errorf("a status query after completion = %d", resp.StatusCode)
	}
}

// "bytes */*" asks how much is persisted: nothing yet is 308 with no Range,
// and the No-308 form is 200 with the override header.
func TestStorageResumableStatusQueryUnknownTotal(t *testing.T) {
	h := rawServer(t)
	s := startSession(t, h, "status")
	if resp := s.put("PUT", "bytes */*", nil, nil); resp.StatusCode != 308 || resp.Header.Get("Range") != "" {
		t.Errorf("an empty session's status = %d, Range %q", resp.StatusCode, resp.Header.Get("Range"))
	}
	s.put("POST", fmt.Sprintf("bytes 0-%d/*", chunkUnit-1), make([]byte, chunkUnit), map[string]string{"X-GUploader-No-308": "yes"})
	resp := s.put("POST", "bytes */*", nil, map[string]string{"X-GUploader-No-308": "yes"})
	if resp.StatusCode != 200 || resp.Header.Get("X-Http-Status-Code-Override") != "308" || resp.Header.Get("Range") != "bytes=0-262143" {
		t.Errorf("No-308 status = %d, override %q, Range %q", resp.StatusCode, resp.Header.Get("X-Http-Status-Code-Override"), resp.Header.Get("Range"))
	}
	// "bytes */TOTAL" with everything persisted finalizes (the Go client's
	// empty last chunk).
	resp = s.put("POST", fmt.Sprintf("bytes */%d", chunkUnit), nil, nil)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("bytes */%d = %d %s", chunkUnit, resp.StatusCode, b)
	}
	if resp := s.put("PUT", "bytes 0-1/x", nil, nil); resp.StatusCode != 200 && resp.StatusCode != 400 {
		t.Errorf("a malformed Content-Range after completion = %d", resp.StatusCode)
	}
}

// DELETE cancels with 499, and the session is gone afterwards.
func TestStorageResumableCancel499(t *testing.T) {
	h := rawServer(t)
	s := startSession(t, h, "cancelled")
	if resp := s.put("DELETE", "", nil, nil); resp.StatusCode != 499 {
		t.Errorf("cancel = %d, want 499", resp.StatusCode)
	}
	if resp := s.put("PUT", "bytes */*", nil, nil); resp.StatusCode < 400 || resp.StatusCode >= 500 {
		t.Errorf("a call after cancel = %d, want 4xx", resp.StatusCode)
	}
	if code, _ := raw(t, "GET", h.URL+"/storage/v1/b/raw/o/cancelled", ""); code != 404 {
		t.Errorf("a cancelled upload made an object (%d)", code)
	}
}

// A session lasts a week, then answers 410 for a week, then 404.
func TestStorageResumableSessionExpires(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	srv, err := NewServer(Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(srv)
	defer h.Close()
	raw(t, "POST", h.URL+"/storage/v1/b?project=p", `{"name":"raw"}`)
	s := startSession(t, h, "expiring")
	now = now.Add(sessionLife - time.Minute)
	if resp := s.put("PUT", "bytes */*", nil, nil); resp.StatusCode != 308 {
		t.Errorf("before a week = %d, want 308", resp.StatusCode)
	}
	now = now.Add(2 * time.Minute)
	if resp := s.put("PUT", "bytes */*", nil, nil); resp.StatusCode != 410 {
		t.Errorf("after a week = %d, want 410", resp.StatusCode)
	}
	now = now.Add(sessionGoneFor)
	if resp := s.put("PUT", "bytes */*", nil, nil); resp.StatusCode != 404 {
		t.Errorf("after two weeks = %d, want 404", resp.StatusCode)
	}
}

// A session and its persisted chunks survive a restart in persistent mode.
func TestStorageResumableSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	open := func() (*Server, *LogMetaStore) {
		meta, err := OpenLogMetaStore(filepath.Join(dir, "meta"))
		if err != nil {
			t.Fatal(err)
		}
		blobs, err := OpenFileBlobStore(filepath.Join(dir, "objects"), Limits{})
		if err != nil {
			t.Fatal(err)
		}
		srv, err := NewServer(Options{Meta: meta, Blobs: blobs})
		if err != nil {
			t.Fatal(err)
		}
		return srv, meta
	}
	srv, meta := open()
	h := httptest.NewServer(srv)
	raw(t, "POST", h.URL+"/storage/v1/b?project=p", `{"name":"raw"}`)
	s := startSession(t, h, "durable")
	first := bytes.Repeat([]byte{1}, chunkUnit)
	s.put("PUT", fmt.Sprintf("bytes 0-%d/*", chunkUnit-1), first, nil)
	h.Close()
	meta.Close()

	srv, meta = open()
	defer meta.Close()
	h2 := httptest.NewServer(srv)
	defer h2.Close()
	s.loc = strings.Replace(s.loc, h.URL, h2.URL, 1)
	if resp := s.put("PUT", "bytes */*", nil, nil); resp.Header.Get("Range") != "bytes=0-262143" {
		t.Fatalf("after a restart the session reports Range %q", resp.Header.Get("Range"))
	}
	resp := s.put("PUT", fmt.Sprintf("bytes %d-%d/%d", chunkUnit, chunkUnit+2, chunkUnit+3), []byte("end"), nil)
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("finishing after a restart = %d %s", resp.StatusCode, b)
	}
	c, err := gcs.NewClient(context.Background(), option.WithEndpoint(h2.URL+"/storage/v1/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := read(t, c.Bucket("raw").Object("durable"), 0, -1); !bytes.Equal(got, append(first, []byte("end")...)) {
		t.Errorf("read back %d bytes", len(got))
	}
}
