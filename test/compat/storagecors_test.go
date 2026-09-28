//go:build compat

package compat

import (
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
)

// CORS (#502), to docs.cloud.google.com/storage/docs/cross-origin: XML
// endpoints follow the bucket's cors configuration, JSON endpoints always
// allow. Both only for an origin that passes CloudBurrow's own check first
// (#677, ADR-0004): a loopback origin, or one the server was started with
// --cors-allow-origin for. Any other is refused with 403, because the
// server checks no credentials.

// EnvCORSOrigin names an origin the instance under test was started with
// --cors-allow-origin for (#677). scripts/compat-env.sh sets it from the
// instance's flags; CI's storage shard and builtin-storage step set one.
const EnvCORSOrigin = "CLOUDBURROW_TEST_CORS_ORIGIN"

// appOrigin is the web app the bucket's cors rule is for: a local dev
// server, which every instance answers without configuration.
const appOrigin = "http://localhost:5173"

func corsBucketFor(t *testing.T, h *Harness, c *storage.Client) *storage.BucketHandle {
	t.Helper()
	bh := bucket(t, h, c)
	if _, err := bh.Update(h.Context(), storage.BucketAttrsToUpdate{CORS: []storage.CORS{{
		Origins: []string{appOrigin}, Methods: []string{"GET", "PUT"},
		ResponseHeaders: []string{"Content-Type"}, MaxAge: 10 * time.Minute,
	}}}); err != nil {
		t.Fatal(err)
	}
	return bh
}

func preflight(t *testing.T, h *Harness, path string, hdr map[string]string) *http.Response {
	t.Helper()
	return corsDo(t, h, http.MethodOptions, path, "", hdr)
}

// corsDo sends one request to the storage endpoint (or to an absolute URL
// on it, a session URI) with the given headers.
func corsDo(t *testing.T, h *Harness, method, path, body string, hdr map[string]string) *http.Response {
	t.Helper()
	u := path
	if strings.HasPrefix(path, "/") {
		u = h.Endpoint(EnvStorage) + path
	}
	req, err := http.NewRequestWithContext(h.Context(), method, u, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

// TestStorageCORSRoundTrip: a cors configuration set through the official
// client comes back as set.
// covers: storage.buckets.patch, storage.buckets.get
func TestStorageCORSRoundTrip(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	bh := corsBucketFor(t, h, c)
	a, err := bh.Attrs(h.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(a.CORS) != 1 || a.CORS[0].Origins[0] != appOrigin || len(a.CORS[0].Methods) != 2 || a.CORS[0].MaxAge != 10*time.Minute {
		t.Errorf("cors = %+v", a.CORS)
	}
}

// TestStorageXMLPreflightMatches: an XML preflight matching a rule gets its
// origin, methods and max age back.
func TestStorageXMLPreflightMatches(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	bh := corsBucketFor(t, h, c)
	resp := preflight(t, h, "/"+bh.BucketName()+"/o.txt", map[string]string{
		"Origin": appOrigin, "Access-Control-Request-Method": "PUT", "Access-Control-Request-Headers": "content-type",
	})
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Access-Control-Allow-Origin") != appOrigin ||
		resp.Header.Get("Access-Control-Allow-Methods") != "GET, PUT" || resp.Header.Get("Access-Control-Max-Age") != "600" {
		t.Errorf("a matching preflight = %d %v", resp.StatusCode, resp.Header)
	}
}

// TestStorageXMLPreflightMismatch200NoHeaders: an XML preflight from an
// origin CloudBurrow answers (loopback) that matches no rule gets 200 with
// no CORS headers, as documented.
func TestStorageXMLPreflightMismatch200NoHeaders(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	bh := corsBucketFor(t, h, c)
	resp := preflight(t, h, "/"+bh.BucketName()+"/o.txt", map[string]string{
		"Origin": "http://127.0.0.1:8080", "Access-Control-Request-Method": "GET",
	})
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Access-Control-Allow-Origin") != "" || resp.Header.Get("Access-Control-Allow-Methods") != "" {
		t.Errorf("a mismatched preflight = %d %v; want 200 with no CORS headers", resp.StatusCode, resp.Header)
	}
}

// TestStorageJSONAlwaysAllowsCORS: JSON API endpoints allow an origin that
// passes CloudBurrow's check whatever the bucket says (#502, #677): a
// loopback origin the bucket's rule does not name, echoed, with DELETE, GET,
// HEAD, PATCH, POST and PUT, and no Allow-Credentials.
//
// unverified: storage.objects.list 200: the JSON API's CORS defaults beyond Allow-Origin, Allow-Methods and Max-Age (the requested headers echoed)
func TestStorageJSONAlwaysAllowsCORS(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	bh := corsBucketFor(t, h, c)
	const origin = "http://[::1]:3000"
	resp := preflight(t, h, "/storage/v1/b/"+bh.BucketName()+"/o", map[string]string{
		"Origin": origin, "Access-Control-Request-Method": "PATCH",
	})
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Access-Control-Allow-Origin") != origin ||
		resp.Header.Get("Access-Control-Allow-Methods") != "DELETE, GET, HEAD, PATCH, POST, PUT" || resp.Header.Get("Access-Control-Max-Age") != "3600" {
		t.Errorf("a JSON preflight = %d %v", resp.StatusCode, resp.Header)
	}
	list := corsDo(t, h, http.MethodGet, "/storage/v1/b/"+bh.BucketName()+"/o", "", map[string]string{"Origin": origin})
	if list.StatusCode != http.StatusOK || list.Header.Get("Access-Control-Allow-Origin") != origin || list.Header.Get("Access-Control-Allow-Credentials") != "" {
		t.Errorf("a JSON list = %d %v; want 200, the origin allowed and no Allow-Credentials", list.StatusCode, list.Header)
	}
}

// corsHeaders lists the Access-Control-* headers of a response.
func corsHeaders(hdr http.Header) string {
	var out []string
	for k := range hdr {
		if strings.HasPrefix(k, "Access-Control-") {
			out = append(out, k)
		}
	}
	return strings.Join(out, ", ")
}

// TestStorageCORSRefusesForeignOrigin: a browser request from
// https://evil.example, neither loopback nor allowed, is refused on the JSON
// and XML APIs, however the bucket is configured: a PATCH or DELETE
// preflight gets 403 and no CORS allow headers, and an actual request gets
// 403 with no Access-Control-Allow-Origin and changes nothing (#677).
func TestStorageCORSRefusesForeignOrigin(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	bh := corsBucketFor(t, h, c)
	// Even a bucket whose rule allows any origin does not open it up.
	if _, err := bh.Update(h.Context(), storage.BucketAttrsToUpdate{CORS: []storage.CORS{{Origins: []string{"*"}, Methods: []string{"GET", "PUT", "DELETE"}}}}); err != nil {
		t.Fatal(err)
	}
	b := bh.BucketName()
	evil := map[string]string{"Origin": "https://evil.example"}
	for _, p := range []struct{ path, method string }{
		{"/storage/v1/b/" + b + "/o", "PATCH"},
		{"/storage/v1/b/" + b, "DELETE"},
		{"/upload/storage/v1/b/" + b + "/o", "POST"},
		{"/" + b + "/o.txt", "PUT"},
	} {
		resp := preflight(t, h, p.path, map[string]string{"Origin": "https://evil.example", "Access-Control-Request-Method": p.method, "Access-Control-Request-Headers": "content-type"})
		if resp.StatusCode != http.StatusForbidden || corsHeaders(resp.Header) != "" {
			t.Errorf("a %s preflight of %s from evil.example = %d with %q; want 403 and no CORS headers", p.method, p.path, resp.StatusCode, corsHeaders(resp.Header))
		}
	}
	for _, r := range []struct{ method, path, body string }{
		{http.MethodGet, "/storage/v1/b/" + b + "/o", ""},
		{http.MethodGet, "/storage/v1/b?project=" + h.Project(), ""},
		{http.MethodPost, "/upload/storage/v1/b/" + b + "/o?uploadType=media&name=evil.txt", "pwned"},
		{http.MethodPost, "/upload/storage/v1/b/" + b + "/o?uploadType=resumable&name=evil2.txt", "{}"},
		{http.MethodPut, "/" + b + "/evil3.txt", "pwned"},
		{http.MethodGet, "/" + b, ""},
		{http.MethodDelete, "/storage/v1/b/" + b, ""},
	} {
		resp := corsDo(t, h, r.method, r.path, r.body, evil)
		if resp.StatusCode != http.StatusForbidden || resp.Header.Get("Access-Control-Allow-Origin") != "" || resp.Header.Get("Location") != "" {
			t.Errorf("%s %s from evil.example = %d %v; want 403 with no Allow-Origin", r.method, r.path, resp.StatusCode, resp.Header)
		}
	}
	// Nothing the refused requests asked for happened.
	if _, err := bh.Attrs(h.Context()); err != nil {
		t.Errorf("the bucket after a refused DELETE: %v", err)
	}
	for _, name := range []string{"evil.txt", "evil2.txt", "evil3.txt"} {
		if _, err := bh.Object(name).Attrs(h.Context()); !errors.Is(err, storage.ErrObjectNotExist) {
			t.Errorf("%s after a refused write: %v, want ErrObjectNotExist", name, err)
		}
	}
}

// corsResumableUpload uploads name through a resumable session a browser at
// origin starts and finishes, checking every answer allows origin, and
// returns what the SDK reads back.
func corsResumableUpload(t *testing.T, h *Harness, bh *storage.BucketHandle, origin, name, data string) string {
	t.Helper()
	hdr := map[string]string{"Origin": origin, "Content-Type": "application/json"}
	pre := preflight(t, h, "/upload/storage/v1/b/"+bh.BucketName()+"/o", map[string]string{"Origin": origin, "Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "content-type, x-upload-content-type"})
	if pre.StatusCode != http.StatusOK || pre.Header.Get("Access-Control-Allow-Origin") != origin {
		t.Fatalf("the upload preflight from %s = %d %v", origin, pre.StatusCode, pre.Header)
	}
	start := corsDo(t, h, http.MethodPost, "/upload/storage/v1/b/"+bh.BucketName()+"/o?uploadType=resumable&name="+name, "{}", hdr)
	uri := start.Header.Get("Location")
	if start.StatusCode != http.StatusOK || uri == "" || start.Header.Get("Access-Control-Allow-Origin") != origin {
		t.Fatalf("starting a session from %s = %d %v", origin, start.StatusCode, start.Header)
	}
	put := corsDo(t, h, http.MethodPut, uri, data, map[string]string{"Origin": origin, "Content-Range": "bytes 0-" + strconv.Itoa(len(data)-1) + "/" + strconv.Itoa(len(data))})
	if put.StatusCode != http.StatusOK || put.Header.Get("Access-Control-Allow-Origin") != origin {
		t.Fatalf("finishing the session from %s = %d %v", origin, put.StatusCode, put.Header)
	}
	r, err := bh.Object(name).NewReader(h.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(got)
}

// TestStorageCORSLoopbackOriginWorks: a web app on a loopback origin, the
// usual local dev server, needs no setting: its preflight and a resumable
// upload it starts and finishes are allowed, and a foreign page holding the
// session URI still cannot write to it (#677).
func TestStorageCORSLoopbackOriginWorks(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	bh := corsBucketFor(t, h, c)
	if got := corsResumableUpload(t, h, bh, appOrigin, "loopback.txt", "from localhost"); got != "from localhost" {
		t.Errorf("read back %q", got)
	}
	start := corsDo(t, h, http.MethodPost, "/upload/storage/v1/b/"+bh.BucketName()+"/o?uploadType=resumable&name=held.txt", "{}", map[string]string{"Origin": appOrigin, "Content-Type": "application/json"})
	if resp := corsDo(t, h, http.MethodPut, start.Header.Get("Location"), "x", map[string]string{"Origin": "https://evil.example", "Content-Range": "bytes 0-0/1"}); resp.StatusCode != http.StatusForbidden || resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("a foreign origin on a session URI = %d %v; want 403", resp.StatusCode, resp.Header)
	}
}

// TestStorageCORSAllowlistedOriginWorks: an origin the server was started
// with --cors-allow-origin for is answered like a loopback one, a resumable
// upload included, and a session it started answers with its origin when a
// loopback page on the same machine sends the next chunk (#677).
func TestStorageCORSAllowlistedOriginWorks(t *testing.T) {
	h := New(t)
	origin := strings.TrimSpace(os.Getenv(EnvCORSOrigin))
	if origin == "" {
		t.Skipf("%s is not set; start the server with --cors-allow-origin and name that origin", EnvCORSOrigin)
	}
	c := storageClient(t, h)
	bh := corsBucketFor(t, h, c)
	pre := preflight(t, h, "/storage/v1/b/"+bh.BucketName()+"/o/x", map[string]string{"Origin": origin, "Access-Control-Request-Method": "PATCH"})
	if pre.StatusCode != http.StatusOK || pre.Header.Get("Access-Control-Allow-Origin") != origin || pre.Header.Get("Access-Control-Allow-Methods") != "DELETE, GET, HEAD, PATCH, POST, PUT" {
		t.Errorf("a PATCH preflight from %s = %d %v", origin, pre.StatusCode, pre.Header)
	}
	if got := corsResumableUpload(t, h, bh, origin, "allowed.txt", "from the allowlist"); got != "from the allowlist" {
		t.Errorf("read back %q", got)
	}
	start := corsDo(t, h, http.MethodPost, "/upload/storage/v1/b/"+bh.BucketName()+"/o?uploadType=resumable&name=shared.txt", "{}", map[string]string{"Origin": origin, "Content-Type": "application/json"})
	status := corsDo(t, h, http.MethodPut, start.Header.Get("Location"), "", map[string]string{"Origin": appOrigin, "Content-Range": "bytes */*"})
	if got := status.Header.Get("Access-Control-Allow-Origin"); got != origin {
		t.Errorf("the session URI answered Allow-Origin %q, want the origin that started it, %s", got, origin)
	}
}
