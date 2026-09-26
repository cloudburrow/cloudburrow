package storage

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	s, err := NewServer(Options{Hosts: []string{"storage.localhost"}})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(s)
	t.Cleanup(h.Close)
	return h
}

type jsonErr struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Errors  []struct {
			Domain  string `json:"domain"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"errors"`
	} `json:"error"`
}

func do(t *testing.T, method, url, host string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

// A discovery method that is not built answers 501 notImplemented naming it.
func TestStorageUnknownMethodIsNotImplemented(t *testing.T) {
	h := newTestServer(t)
	resp, b := do(t, "GET", h.URL+"/storage/v1/b/x/anywhereCaches", "")
	var e jsonErr
	_ = json.Unmarshal(b, &e)
	if resp.StatusCode != 501 || len(e.Error.Errors) != 1 || e.Error.Errors[0].Reason != "notImplemented" ||
		!strings.Contains(e.Error.Message, "storage.anywhereCaches.list") {
		t.Errorf("GET anywhereCaches = %d %s; want 501 notImplemented naming storage.anywhereCaches.list", resp.StatusCode, b)
	}
	for _, c := range []struct{ method, path, names string }{
		{"GET", "/storage/v1/b/x/o/dir%2Fobj/acl", "storage.objectAccessControls.list"},
	} {
		resp, b := do(t, c.method, h.URL+c.path, "")
		if resp.StatusCode != 501 || !strings.Contains(string(b), c.names) {
			t.Errorf("%s %s = %d %s; want 501 naming %q", c.method, c.path, resp.StatusCode, b, c.names)
		}
	}
	if resp, b := do(t, "GET", h.URL+"/storage/v1/nothing/here", ""); resp.StatusCode != 404 {
		t.Errorf("an unbound JSON path = %d %s; want 404", resp.StatusCode, b)
	}
	if resp, b := do(t, "GET", h.URL+"/storage/v1/b?alt=proto", ""); resp.StatusCode != 400 || !strings.Contains(string(b), "alt") {
		t.Errorf("alt=proto = %d %s; want 400 naming alt", resp.StatusCode, b)
	}
}

// JSON errors carry error.errors[0].{domain,reason,message}; XML errors are
// <Error><Code>…</Code><Message>…</Message></Error>.
func TestStorageErrorEnvelopeShapes(t *testing.T) {
	h := newTestServer(t)
	resp, b := do(t, "GET", h.URL+"/storage/v1/b/x/anywhereCaches", "")
	_ = resp
	var e jsonErr
	if err := json.Unmarshal(b, &e); err != nil || e.Error.Code != 501 || len(e.Error.Errors) != 1 ||
		e.Error.Errors[0].Domain != "global" || e.Error.Errors[0].Reason == "" || e.Error.Errors[0].Message == "" {
		t.Errorf("JSON error body = %s (%v)", b, err)
	}
	// A POST without x-goog-resumable (a form upload) and a bucket PUT are
	// not built.
	for name, c := range map[string]struct{ method, path, host, names string }{
		"path-style":     {"POST", "/my-bucket/dir/obj.txt", "", "my-bucket/dir/obj.txt"},
		"virtual-hosted": {"POST", "/dir/obj.txt", "my-bucket.storage.localhost", "my-bucket/dir/obj.txt"},
		"bucket":         {"PUT", "/my-bucket", "", "bucket my-bucket"},
	} {
		resp, b := do(t, c.method, h.URL+c.path, c.host)
		var xe xmlError
		if err := xml.Unmarshal(b, &xe); err != nil || resp.StatusCode != 501 || xe.Code != "NotImplemented" ||
			!strings.Contains(xe.Message, c.names) || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/xml") {
			t.Errorf("%s XML = %d %s (%v); want 501 <Error><Code>NotImplemented naming %q", name, resp.StatusCode, b, err, c.names)
		}
	}
	// XML GET of an object is built (#491): a missing bucket is NoSuchBucket.
	resp, b = do(t, "GET", h.URL+"/no-bucket/obj", "")
	var xe xmlError
	if err := xml.Unmarshal(b, &xe); err != nil || resp.StatusCode != 404 || xe.Code != "NoSuchBucket" {
		t.Errorf("XML GET in a missing bucket = %d %s", resp.StatusCode, b)
	}
}

// fields= trims a response to the selected fields, within arrays too.
func TestStorageFieldsPartialResponse(t *testing.T) {
	list := map[string]any{
		"kind":          "storage#objects",
		"nextPageToken": "tok",
		"prefixes":      []string{"dir/"},
		"items":         []map[string]any{{"name": "a", "size": "1", "metadata": map[string]string{"k": "v"}}, {"name": "b", "size": "2"}},
	}
	for spec, want := range map[string]string{
		"items/name,nextPageToken": `{"items":[{"name":"a"},{"name":"b"}],"nextPageToken":"tok"}`,
		"items(name,size)":         `{"items":[{"name":"a","size":"1"},{"name":"b","size":"2"}]}`,
		"items/metadata/k":         `{"items":[{"metadata":{"k":"v"}},{}]}`,
		"kind,*":                   `{"items":[{"metadata":{"k":"v"},"name":"a","size":"1"},{"name":"b","size":"2"}],"kind":"storage#objects","nextPageToken":"tok","prefixes":["dir/"]}`,
	} {
		rec := httptest.NewRecorder()
		writeResponse(rec, httptest.NewRequest("GET", "/storage/v1/b/x/o?prettyPrint=false&fields="+spec, nil), 200, list)
		if got := strings.TrimSpace(rec.Body.String()); got != want {
			t.Errorf("fields=%s:\n got %s\nwant %s", spec, got, want)
		}
	}
	for _, bad := range []string{"items(name", "items/", ",name", "a()"} {
		rec := httptest.NewRecorder()
		writeResponse(rec, httptest.NewRequest("GET", "/storage/v1/b/x/o?fields="+bad, nil), 200, list)
		if rec.Code != 400 {
			t.Errorf("fields=%s = %d, want 400", bad, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	writeResponse(rec, httptest.NewRequest("GET", "/storage/v1/b/x/o", nil), 200, list)
	if !bytes.Contains(rec.Body.Bytes(), []byte("\n  ")) {
		t.Error("the default response is not pretty-printed")
	}
}

// Links are built from the host the request reached, never storage.googleapis.com.
func TestStorageLinksUseRequestHost(t *testing.T) {
	for _, host := range []string{"127.0.0.1:9001", "storage.cloudburrow.svc.cluster.local:4443"} {
		r := httptest.NewRequest("GET", "/storage/v1/b/bkt/o/dir%2Fa%20b", nil)
		r.Host = host
		self, media := selfLink(r, "bkt", "dir/a b"), mediaLink(r, "bkt", "dir/a b", 7)
		if self != "http://"+host+"/storage/v1/b/bkt/o/dir%2Fa%20b" ||
			media != "http://"+host+"/download/storage/v1/b/bkt/o/dir%2Fa%20b?generation=7&alt=media" {
			t.Errorf("host %s: selfLink %s, mediaLink %s", host, self, media)
		}
		if strings.Contains(self+media, "googleapis.com") {
			t.Errorf("a link points at Google: %s %s", self, media)
		}
	}
}

// Every discovery method is in methodStatus, built or explicitly
// unimplemented, and the embedded document is the pinned module's.
func TestStorageDiscoveryDrift(t *testing.T) {
	ms, err := discoveryMethods()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, m := range ms {
		seen[m.ID] = true
		if _, ok := methodStatus[m.ID]; !ok {
			t.Errorf("%s is in the discovery document but not in methodStatus: build it or list it as unimplemented", m.ID)
		}
	}
	for id := range methodStatus {
		if !seen[id] {
			t.Errorf("methodStatus lists %s, which the discovery document does not", id)
		}
	}
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "google.golang.org/api").Output()
	if err != nil {
		t.Skipf("cannot locate google.golang.org/api: %v", err)
	}
	pinned, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(out)), "storage", "v1", "storage-api.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pinned, storageAPI) {
		t.Error("storage-api.json differs from the pinned google.golang.org/api copy: copy it again and update methodStatus")
	}
}

// The server keeps its last calls for a CLI to scrape (#513), with no query
// string in them.
func TestStorageEventsEndpoint(t *testing.T) {
	h := newTestServer(t)
	raw(t, "POST", h.URL+"/storage/v1/b?project=p", `{"name":"evt"}`)
	raw(t, "GET", h.URL+"/storage/v1/b/evt?secretparam=hidden", "")
	code, body := raw(t, "GET", h.URL+"/_cloudburrow/events", "")
	if code != 200 || !strings.Contains(body, `"method":"storage.buckets.insert"`) || !strings.Contains(body, `"method":"storage.buckets.get"`) ||
		strings.Contains(body, "hidden") || strings.Contains(body, "_cloudburrow") {
		t.Errorf("events = %d %s", code, body)
	}
	if _, body := raw(t, "GET", h.URL+"/_cloudburrow/events?after=2", ""); strings.Contains(body, "storage.buckets") {
		t.Errorf("after=2 = %s; want only later calls", body)
	}
}

// Every discovery method not built answers 501 notImplemented naming it
// (#520): the coverage report's Unimplemented rows rest on this.
func TestStorageEveryUnbuiltMethodIsNotImplemented(t *testing.T) {
	h := newTestServer(t)
	methods, err := discoveryMethods()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, m := range methods {
		if methodStatus[m.ID] == built {
			continue
		}
		n++
		var segs []string
		for _, s := range strings.Split(m.Path, "/") {
			if strings.HasPrefix(s, "{") {
				s = "x"
			}
			segs = append(segs, s)
		}
		body := ""
		if m.Verb != "GET" && m.Verb != "DELETE" {
			body = "{}"
		}
		resp, b := do(t, m.Verb, h.URL+"/storage/v1/"+strings.Join(segs, "/"), body)
		if resp.StatusCode != 501 || !strings.Contains(string(b), m.ID) {
			t.Errorf("%s %s (%s) = %d %s; want 501 naming %s", m.Verb, m.Path, m.ID, resp.StatusCode, b, m.ID)
		}
	}
	if n < 40 {
		t.Errorf("only %d unbuilt methods were checked", n)
	}
}
