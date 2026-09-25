package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	gcsbuiltin "github.com/cloudburrow/cloudburrow/internal/service/storage"
)

// memEntries is an in-memory admin.EntryWriter and EntryReader.
type memEntries map[string][]byte

func (m memEntries) Add(name string, size int64, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err == nil && int64(len(b)) != size {
		return io.ErrShortWrite
	}
	m[name] = b
	return err
}

func (m memEntries) Open(name string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(m[name])), nil
}

func (m memEntries) List(prefix string) []string {
	var out []string
	for k := range m {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func builtinAt(t *testing.T) (*httptest.Server, func(method, path string, body io.Reader) (int, string)) {
	t.Helper()
	srv, err := gcsbuiltin.NewServer(gcsbuiltin.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(srv)
	t.Cleanup(h.Close)
	return h, func(method, path string, body io.Reader) (int, string) {
		req, _ := http.NewRequest(method, h.URL+path, body)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
}

// A state save and load through the builtin snapshotter keeps a 10 MiB
// object byte for byte, its versions and its hold, and marks no secret.
func TestBuiltinStorageSnapshotRoundTrip(t *testing.T) {
	src, call := builtinAt(t)
	big := make([]byte, 10<<20)
	_, _ = rand.Read(big)
	call("POST", "/storage/v1/b?project=p", strings.NewReader(`{"name":"state","versioning":{"enabled":true}}`))
	call("POST", "/upload/storage/v1/b/state/o?uploadType=media&name=big", bytes.NewReader(big))
	call("POST", "/upload/storage/v1/b/state/o?uploadType=media&name=v", strings.NewReader("one"))
	call("POST", "/upload/storage/v1/b/state/o?uploadType=media&name=v", strings.NewReader("two"))
	call("PATCH", "/storage/v1/b/state/o/v", strings.NewReader(`{"eventBasedHold":true}`))
	s := &builtinStorageSnapshotter{tunnel: forwarderAt(t, src.Listener.Addr().String())}
	if s.Secret() {
		t.Error("the builtin snapshot claims to hold secrets; HMAC secrets are excluded")
	}
	entries := memEntries{}
	if err := s.Export(context.Background(), entries); err != nil {
		t.Fatal(err)
	}
	dst, dcall := builtinAt(t)
	d := &builtinStorageSnapshotter{tunnel: forwarderAt(t, dst.Listener.Addr().String())}
	if err := d.Import(context.Background(), entries); err != nil {
		t.Fatal(err)
	}
	if code, body := dcall("GET", "/download/storage/v1/b/state/o/big?alt=media", nil); code != 200 || !bytes.Equal([]byte(body), big) {
		t.Errorf("the 10 MiB object = %d, %d bytes", code, len(body))
	}
	if _, body := dcall("GET", "/storage/v1/b/state/o?versions=true", nil); strings.Count(body, `"name": "v"`) != 2 {
		t.Errorf("versions after the load = %s", body)
	}
	if code, _ := dcall("DELETE", "/storage/v1/b/state/o/v", nil); code != 403 {
		t.Errorf("the hold did not survive: delete = %d", code)
	}
}
