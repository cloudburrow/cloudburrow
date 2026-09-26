package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	gcsbuiltin "github.com/cloudburrow/cloudburrow/internal/service/storage"
)

func eventServer(t *testing.T) (*admin.Recorder, *admin.API, string) {
	t.Helper()
	rec := admin.NewRecorder(100, time.Now)
	api := admin.NewAPI(rec)
	api.Faults().Interpose("storage")
	srv, err := gcsbuiltin.NewServer(gcsbuiltin.Options{Observe: storageEvents(rec, nil), Faults: storageFaults{api.Faults()}})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(srv)
	t.Cleanup(h.Close)
	return rec, api, h.URL
}

func send(t *testing.T, method, url, body string, hdr map[string]string) (*http.Response, string) {
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
	return resp, string(b)
}

// Every request the builtin server serves is an /admin/events entry with
// its API method, bucket, object and status (#513).
func TestStorageEventsRecorded(t *testing.T) {
	rec, _, base := eventServer(t)
	send(t, "POST", base+"/storage/v1/b?project=p", `{"name":"ev-bucket"}`, nil)
	send(t, "POST", base+"/upload/storage/v1/b/ev-bucket/o?uploadType=media&name=dir%2Fa.txt", "x", nil)
	send(t, "GET", base+"/storage/v1/b/ev-bucket/o/missing", "", nil)
	send(t, "PUT", base+"/ev-bucket/b.txt", "y", nil)
	got := rec.Events("storage", 10)
	var lines []string
	for _, e := range got {
		lines = append(lines, e.Target+" "+e.Detail["bucket"]+"/"+e.Detail["object"]+" "+e.Detail["code"])
	}
	// A bucket insert names its bucket in the body, which an event never
	// reads.
	want := []string{
		"storage.buckets.insert / 200",
		"storage.objects.insert ev-bucket/dir/a.txt 200",
		"storage.objects.get ev-bucket/missing 404",
		"xml.PUT ev-bucket/b.txt 200",
	}
	for _, w := range want {
		if !contains(lines, w) {
			t.Errorf("no event %q in %v", w, lines)
		}
	}
}

// No event carries a resumable upload_id, a rewrite token, a signed URL's
// signature or an HMAC secret.
func TestStorageEventsRedactUploadIDs(t *testing.T) {
	rec, _, base := eventServer(t)
	send(t, "POST", base+"/storage/v1/b?project=p", `{"name":"redact"}`, nil)
	resp, _ := send(t, "POST", base+"/upload/storage/v1/b/redact/o?uploadType=resumable&name=r", "{}", nil)
	id := resp.Header.Get("X-GUploader-UploadID")
	send(t, "PUT", resp.Header.Get("Location"), "0123456789", map[string]string{"Content-Range": "bytes 0-9/10"})
	_, body := send(t, "POST", base+"/storage/v1/projects/p/hmacKeys?serviceAccountEmail=a@p.iam.gserviceaccount.com", "", nil)
	var k struct {
		Secret string `json:"secret"`
	}
	_ = json.Unmarshal([]byte(body), &k)
	send(t, "GET", base+"/redact/r?X-Goog-Signature=deadbeefcafe&X-Goog-Algorithm=GOOG4-HMAC-SHA256", "", nil)
	all, _ := json.Marshal(rec.Events("storage", 100))
	for what, secret := range map[string]string{"upload_id": id, "HMAC secret": k.Secret, "signature": "deadbeefcafe"} {
		if secret == "" {
			t.Fatalf("the test has no %s to look for", what)
		}
		if strings.Contains(string(all), secret) {
			t.Errorf("an event holds the %s: %s", what, all)
		}
	}
}

// A storage fault rule answers with storage's own error body, JSON or XML
// by the surface, and records the fault.
func TestStorageFaultsUseStorageErrorBodies(t *testing.T) {
	rec, api, base := eventServer(t)
	send(t, "POST", base+"/storage/v1/b?project=p", `{"name":"faulted"}`, nil)
	mux := http.NewServeMux()
	api.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	send(t, "POST", srv.URL+"/admin/faults", `{"service":"storage","method":"storage.objects.list","httpStatus":503,"count":1}`, nil)
	send(t, "POST", srv.URL+"/admin/faults", `{"service":"storage","method":"xml.GET","httpStatus":500,"count":1}`, nil)
	if resp, body := send(t, "GET", base+"/storage/v1/b/faulted/o", "", nil); resp.StatusCode != 503 || !strings.Contains(body, `"code": 503`) && !strings.Contains(body, `"code":503`) {
		t.Errorf("a faulted JSON call = %d %s", resp.StatusCode, body)
	}
	if resp, body := send(t, "GET", base+"/faulted", "", nil); resp.StatusCode != 500 || !strings.Contains(body, "<Error>") {
		t.Errorf("a faulted XML call = %d %s", resp.StatusCode, body)
	}
	if resp, _ := send(t, "GET", base+"/storage/v1/b/faulted/o", "", nil); resp.StatusCode != 200 {
		t.Errorf("after the count-1 rule = %d", resp.StatusCode)
	}
	faults := 0
	for _, e := range rec.Events("storage", 50) {
		if e.Kind == "fault" {
			faults++
		}
	}
	if faults != 2 {
		t.Errorf("recorded %d faults, want 2", faults)
	}
}

// The scraper feeds the in-cluster server's calls into /admin/events once
// each, and starts over when the server restarts and numbers from 1 again
// (#514).
func TestStorageEventScraper(t *testing.T) {
	srv, err := gcsbuiltin.NewServer(gcsbuiltin.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(srv)
	defer h.Close()
	rec := admin.NewRecorder(100, time.Now)
	s := newStorageEventScraper(forwarderAt(t, h.Listener.Addr().String()), rec, nil)
	send(t, "POST", h.URL+"/storage/v1/b?project=p", `{"name":"scraped"}`, nil)
	send(t, "GET", h.URL+"/storage/v1/b/scraped", "", nil)
	s.scrape(context.Background())
	s.scrape(context.Background())
	if n := len(rec.Events("storage", 100)); n != 2 {
		t.Fatalf("after two scrapes of two calls: %d events, want 2 (each once)", n)
	}
	// A restarted server numbers its calls from 1 again.
	srv2, _ := gcsbuiltin.NewServer(gcsbuiltin.Options{})
	h.Config.Handler = srv2
	send(t, "GET", h.URL+"/storage/v1/b?project=p", "", nil)
	s.scrape(context.Background()) // finds nothing after its cursor, notices the restart
	s.scrape(context.Background())
	if n := len(rec.Events("storage", 100)); n != 3 {
		t.Errorf("after the server restarted: %d events, want 3", n)
	}
}
