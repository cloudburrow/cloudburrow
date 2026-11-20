package storage

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// waitRequests polls the request log until it holds n records.
func waitRequests(t *testing.T, base string, n int) []Request {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, body := raw(t, "GET", base+"/_cloudburrow/events?requests=1", "")
		var page struct {
			Requests []Request `json:"requests"`
		}
		if err := json.Unmarshal([]byte(body), &page); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		if len(page.Requests) >= n || time.Now().After(deadline) {
			return page.Requests
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The request log (#1979) keeps one record per request, with the bucket's
// project and location, the bytes each way, the permissions checked and
// the JSON method an XML request is served as; admin paths are left out.
func TestStorageRequestLog(t *testing.T) {
	h := newTestServer(t)
	raw(t, "POST", h.URL+"/storage/v1/b?project=p", `{"name":"rlog","location":"us-central1"}`)
	raw(t, "POST", h.URL+"/upload/storage/v1/b/rlog/o?uploadType=media&name=dir/a.txt", "hello")
	raw(t, "GET", h.URL+"/storage/v1/b/rlog/o/dir%2Fa.txt?alt=media", "")
	raw(t, "GET", h.URL+"/rlog/dir/a.txt", "")
	raw(t, "POST", h.URL+"/storage/v1/b/rlog/o/dir%2Fa.txt/copyTo/b/rlog/o/b.txt", "")
	raw(t, "GET", h.URL+"/storage/v1/b/rlog/o/missing", "")
	reqs := waitRequests(t, h.URL, 6)
	if len(reqs) != 6 {
		t.Fatalf("records = %+v", reqs)
	}
	ins, upload, get, xml, cp, missing := reqs[0], reqs[1], reqs[2], reqs[3], reqs[4], reqs[5]
	if ins.Method != "storage.buckets.insert" || ins.Project != "p" || ins.Bucket != "rlog" || ins.Location != "US-CENTRAL1" || ins.ProjectNumber == 0 || ins.Permissions[0] != "storage.buckets.create" {
		t.Errorf("insert = %+v", ins)
	}
	if upload.Method != "storage.objects.insert" || upload.Bucket != "rlog" || upload.Object != "dir/a.txt" || upload.RequestBytes != 5 ||
		upload.Project != "p" || upload.Location != "US-CENTRAL1" || upload.Permissions[0] != "storage.objects.create" ||
		upload.Surface != "json" || upload.RequestID == "" || upload.Status != 200 {
		t.Errorf("upload = %+v", upload)
	}
	if get.ResponseBytes != 5 || get.Permissions[0] != "storage.objects.get" || get.HTTPMethod != "GET" {
		t.Errorf("get = %+v", get)
	}
	if xml.Method != "storage.objects.get" || xml.Surface != "xml" || xml.ResponseBytes != 5 || xml.Object != "dir/a.txt" {
		t.Errorf("xml = %+v", xml)
	}
	if cp.Method != "storage.objects.copy" || len(cp.Permissions) != 2 || cp.DestBucket != "rlog" || cp.DestObject != "b.txt" {
		t.Errorf("copy = %+v", cp)
	}
	if missing.Status != 404 {
		t.Errorf("missing = %+v", missing)
	}
	for _, r := range reqs {
		if strings.Contains(r.Method, "admin") || strings.Contains(r.Object, "_cloudburrow") {
			t.Errorf("an admin path was logged: %+v", r)
		}
	}
	if ids := map[string]bool{}; true {
		for _, r := range reqs {
			if ids[r.RequestID] {
				t.Errorf("request ID %s twice", r.RequestID)
			}
			ids[r.RequestID] = true
		}
	}
	if _, body := raw(t, "GET", h.URL+"/_cloudburrow/events?requests=1&after=5", ""); strings.Contains(body, "storage.buckets.insert") ||
		!strings.Contains(body, `"status":404`) {
		t.Errorf("after=5 = %s", body)
	}
}

// A full queue drops records rather than wait.
func TestStorageRequestLogDropsWhenFull(t *testing.T) {
	s, err := NewServer(Options{})
	if err != nil {
		t.Fatal(err)
	}
	s.stopRequestLog()
	for i := 0; i <= requestQueueSize; i++ {
		s.logRequest(Request{})
	}
	if s.requests.dropped.Load() != 1 {
		t.Errorf("dropped = %d", s.requests.dropped.Load())
	}
}

// BenchmarkStorageObjectGet measures a small object download through the
// whole handler, with the request log on (as served) and off, which is the
// cost the log adds to the hot path (#1979).
func BenchmarkStorageObjectGet(b *testing.B) {
	for _, mode := range []string{"log", "nolog"} {
		b.Run(mode, func(b *testing.B) {
			s, err := NewServer(Options{})
			if err != nil {
				b.Fatal(err)
			}
			defer s.stopRequestLog()
			s.noRequestLog = mode == "nolog"
			call := func(method, path, body string) int {
				req := httptest.NewRequest(method, path, strings.NewReader(body))
				req.RemoteAddr = "127.0.0.1:1"
				w := httptest.NewRecorder()
				s.ServeHTTP(w, req)
				_, _ = io.Copy(io.Discard, w.Body)
				return w.Code
			}
			if c := call("POST", "/storage/v1/b?project=p", `{"name":"bench"}`); c != http.StatusOK {
				b.Fatalf("bucket: %d", c)
			}
			if c := call("POST", "/upload/storage/v1/b/bench/o?uploadType=media&name=a", "0123456789"); c != http.StatusOK {
				b.Fatalf("object: %d", c)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if c := call("GET", "/storage/v1/b/bench/o/a?alt=media", ""); c != http.StatusOK {
					b.Fatalf("get: %d", c)
				}
			}
		})
	}
}
