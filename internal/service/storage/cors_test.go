package storage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gcs "cloud.google.com/go/storage"
)

// corsAllowed is the allowlist the CORS tests' server is started with, as
// `up --cors-allow-origin` would give it (#677).
var corsAllowed = []string{"https://app.example", "https://other.example", "HTTPS://Elsewhere.Example:443/"}

func corsRequest(t *testing.T, method, url string, h map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	for k, v := range h {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func corsBucket(t *testing.T) string {
	t.Helper()
	s, err := NewServer(Options{CORSAllowOrigins: corsAllowed})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(s)
	t.Cleanup(h.Close)
	body := `{"name":"cors-bucket","cors":[{"origin":["https://app.example"],"method":["GET","PUT"],"responseHeader":["Content-Type","x-goog-meta-a"],"maxAgeSeconds":600},` +
		`{"origin":["*"],"method":["HEAD"]}]}`
	if code, resp := raw(t, "POST", h.URL+"/storage/v1/b?project=p", body); code != 200 {
		t.Fatal(resp)
	}
	return h.URL
}

// The official client's cors round-trips.
func TestStorageCORSRoundTripInProcess(t *testing.T) {
	_, bh, _ := sdkBucket(t, "cors-sdk")
	ctx := context.Background()
	want := []gcs.CORS{{Origins: []string{"https://app.example"}, Methods: []string{"GET"}, ResponseHeaders: []string{"Content-Type"}, MaxAge: 600e9}}
	if _, err := bh.Update(ctx, gcs.BucketAttrsToUpdate{CORS: want}); err != nil {
		t.Fatal(err)
	}
	a, err := bh.Attrs(ctx)
	if err != nil || len(a.CORS) != 1 || a.CORS[0].Origins[0] != "https://app.example" || a.CORS[0].MaxAge != want[0].MaxAge {
		t.Errorf("cors = %+v, %v", a.CORS, err)
	}
	if code, _ := raw(t, "PATCH", lastHTTP.URL+"/storage/v1/b/cors-sdk", `{"cors":[{"origins":["x"]}]}`); code != 400 {
		t.Errorf("a misspelt cors field = %d", code)
	}
}

// An XML preflight matching a rule gets that rule's methods and max age,
// and the requested headers it allows.
func TestStorageXMLPreflightMatchesInProcess(t *testing.T) {
	base := corsBucket(t)
	resp := corsRequest(t, "OPTIONS", base+"/cors-bucket/o.txt", map[string]string{
		"Origin": "https://app.example", "Access-Control-Request-Method": "PUT", "Access-Control-Request-Headers": "content-type",
	})
	h := resp.Header
	if resp.StatusCode != 200 || h.Get("Access-Control-Allow-Origin") != "https://app.example" ||
		h.Get("Access-Control-Allow-Methods") != "GET, PUT" || h.Get("Access-Control-Max-Age") != "600" ||
		h.Get("Access-Control-Allow-Headers") != "content-type" || h.Get("Access-Control-Allow-Credentials") != "" {
		t.Errorf("preflight = %d %v", resp.StatusCode, h)
	}
	star := corsRequest(t, "OPTIONS", base+"/cors-bucket/o.txt", map[string]string{"Origin": "https://other.example", "Access-Control-Request-Method": "HEAD"})
	if star.Header.Get("Access-Control-Allow-Origin") != "https://other.example" || star.Header.Get("Access-Control-Max-Age") != "3600" {
		t.Errorf("a * origin rule = %v", star.Header)
	}
	actual := corsRequest(t, "GET", base+"/cors-bucket/missing", map[string]string{"Origin": "https://app.example"})
	if actual.Header.Get("Access-Control-Allow-Origin") != "https://app.example" || actual.Header.Get("Access-Control-Expose-Headers") != "Content-Type, x-goog-meta-a" {
		t.Errorf("an actual XML request = %v", actual.Header)
	}
}

// An XML preflight from an allowed origin that matches no rule gets 200
// with no CORS headers.
func TestStorageXMLPreflightMismatch200NoHeadersInProcess(t *testing.T) {
	base := corsBucket(t)
	for name, h := range map[string]map[string]string{
		"origin": {"Origin": "https://other.example", "Access-Control-Request-Method": "GET"},
		"method": {"Origin": "https://app.example", "Access-Control-Request-Method": "DELETE"},
		"header": {"Origin": "https://app.example", "Access-Control-Request-Method": "GET", "Access-Control-Request-Headers": "authorization"},
	} {
		resp := corsRequest(t, "OPTIONS", base+"/cors-bucket/o.txt", h)
		if resp.StatusCode != 200 || resp.Header.Get("Access-Control-Allow-Origin") != "" || resp.Header.Get("Access-Control-Allow-Methods") != "" {
			t.Errorf("%s mismatch = %d %v", name, resp.StatusCode, resp.Header)
		}
	}
	if resp := corsRequest(t, "OPTIONS", base+"/no-such-bucket/o", map[string]string{"Origin": "https://app.example", "Access-Control-Request-Method": "GET"}); resp.StatusCode != 200 || resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("a bucket with no config = %d %v", resp.StatusCode, resp.Header)
	}
}

// JSON API endpoints allow an allowed origin whatever the bucket says, and
// send no Allow-Credentials (#677).
//
// unverified: storage.objects.get 200: the JSON API's defaults beyond Allow-Origin, Allow-Methods and Max-Age (Allow-Headers echoed)
func TestStorageJSONAllowsAllowedOriginsInProcess(t *testing.T) {
	base := corsBucket(t)
	for _, origin := range []string{"https://app.example", "http://localhost:5173", "http://127.0.0.1:3000", "http://[::1]:8080", "https://my-app.localhost"} {
		for _, path := range []string{"/storage/v1/b/cors-bucket/o", "/upload/storage/v1/b/cors-bucket/o", "/download/storage/v1/b/cors-bucket/o/x"} {
			resp := corsRequest(t, "OPTIONS", base+path, map[string]string{
				"Origin": origin, "Access-Control-Request-Method": "PATCH", "Access-Control-Request-Headers": "authorization, content-type",
			})
			h := resp.Header
			if resp.StatusCode != 200 || h.Get("Access-Control-Allow-Origin") != origin || h.Get("Access-Control-Allow-Methods") != jsonCORSMethods ||
				h.Get("Access-Control-Max-Age") != "3600" || h.Get("Access-Control-Allow-Headers") != "authorization, content-type" {
				t.Errorf("%s %s preflight = %d %v", origin, path, resp.StatusCode, h)
			}
		}
		actual := corsRequest(t, "GET", base+"/storage/v1/b/cors-bucket/o", map[string]string{"Origin": origin})
		if actual.StatusCode != 200 || actual.Header.Get("Access-Control-Allow-Origin") != origin || actual.Header.Get("Access-Control-Allow-Credentials") != "" {
			t.Errorf("%s: an actual JSON request = %d %v", origin, actual.StatusCode, actual.Header)
		}
	}
}

// A browser request from an origin that is neither loopback nor allowed is
// refused on every surface: 403 with no CORS headers, preflights included,
// and the request a refused preflight was for never runs (#677).
func TestStorageRefusesForeignOrigins(t *testing.T) {
	base := corsBucket(t)
	cors := func(h http.Header) string {
		var out []string
		for k := range h {
			if strings.HasPrefix(k, "Access-Control-") {
				out = append(out, k)
			}
		}
		return strings.Join(out, ",")
	}
	for _, origin := range []string{
		"https://evil.example", "null", "http://localhost.evil.example", "http://127.0.0.1.nip.io",
		"https://app.example:8443", "http://app.example", "file://", "http://10.0.0.1:9001",
	} {
		for _, c := range []struct{ method, path, reqMethod string }{
			{"OPTIONS", "/storage/v1/b/cors-bucket/o", "PATCH"},
			{"OPTIONS", "/storage/v1/b/cors-bucket", "DELETE"},
			{"OPTIONS", "/upload/storage/v1/b/cors-bucket/o", "PUT"},
			{"OPTIONS", "/cors-bucket/o.txt", "GET"},
			{"GET", "/storage/v1/b/cors-bucket/o", ""},
			{"GET", "/storage/v1/b?project=p", ""},
			{"POST", "/upload/storage/v1/b/cors-bucket/o?uploadType=media&name=evil", ""},
			{"DELETE", "/storage/v1/b/cors-bucket", ""},
			{"GET", "/cors-bucket/o.txt", ""},
			{"PUT", "/cors-bucket/evil.txt", ""},
			{"POST", "/_cloudburrow/reset", ""},
		} {
			hdr := map[string]string{"Origin": origin}
			if c.reqMethod != "" {
				hdr["Access-Control-Request-Method"] = c.reqMethod
			}
			resp := corsRequest(t, c.method, base+c.path, hdr)
			if resp.StatusCode != http.StatusForbidden || cors(resp.Header) != "" {
				t.Errorf("%s %s from %s = %d, CORS headers %q; want 403 and none", c.method, c.path, origin, resp.StatusCode, cors(resp.Header))
			}
		}
	}
	// Nothing the refused requests asked for happened.
	if code, body := raw(t, "GET", base+"/storage/v1/b/cors-bucket/o", ""); code != 200 || strings.Contains(body, "evil") {
		t.Errorf("after refused requests the bucket lists %d %s", code, body)
	}
	// The refusal says why, in each API's error shape.
	req, _ := http.NewRequest("GET", base+"/storage/v1/b/cors-bucket/o", nil)
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"code": 403`) && !strings.Contains(string(b), `"code":403`) || !strings.Contains(string(b), "--cors-allow-origin") {
		t.Errorf("JSON refusal body = %s", b)
	}
	if resp.Header.Get("Vary") != "Origin" {
		t.Errorf("a refusal varies on %q, want Origin", resp.Header.Get("Vary"))
	}
}

// A request with no Origin is not a browser's cross-origin call and is
// served as before; one from the server's own origin is served too.
func TestStorageNoOriginAndSameOriginServed(t *testing.T) {
	base := corsBucket(t)
	if code, body := raw(t, "GET", base+"/storage/v1/b/cors-bucket/o", ""); code != 200 {
		t.Errorf("no Origin = %d %s", code, body)
	}
	s, err := NewServer(Options{})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/storage/v1/b?project=p", nil)
	r.Host = "storage.cb.svc.cluster.local:4443"
	r.Header.Set("Origin", "http://storage.cb.svc.cluster.local:4443")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || w.Header().Get("Access-Control-Allow-Origin") != "http://storage.cb.svc.cluster.local:4443" {
		t.Errorf("same origin = %d %v", w.Code, w.Header())
	}
	r.Header.Set("Origin", "http://storage.cb.svc.cluster.local:4444")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("another port of the same name = %d, want 403", w.Code)
	}
}

// An allowlist entry that is not an origin is refused when the server is
// built, not ignored.
func TestStorageCORSAllowOriginsValidated(t *testing.T) {
	for _, bad := range []string{"*", "https://*.example", "app.example", "https://app.example/path", "ftp://app.example", "https://user@app.example"} {
		if _, err := NewServer(Options{CORSAllowOrigins: []string{bad}}); err == nil {
			t.Errorf("NewServer accepted allowed origin %q", bad)
		}
	}
}

// A resumable session answers with the Origin that started it.
func TestStorageResumableSessionKeepsOrigin(t *testing.T) {
	base := corsBucket(t)
	req, _ := http.NewRequest("POST", base+"/upload/storage/v1/b/cors-bucket/o?uploadType=resumable&name=r", nil)
	req.Header.Set("Origin", "https://app.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	uri := resp.Header.Get("Location")
	put := corsRequest(t, "PUT", uri, map[string]string{"Origin": "https://elsewhere.example", "Content-Range": "bytes */*"})
	if put.Header.Get("Access-Control-Allow-Origin") != "https://app.example" {
		t.Errorf("session URI answered Allow-Origin %q, want the initiating origin", put.Header.Get("Access-Control-Allow-Origin"))
	}
	// The session URI is still behind the origin check (#677): holding the
	// URI does not let a foreign page write to it.
	if evil := corsRequest(t, "PUT", uri, map[string]string{"Origin": "https://evil.example", "Content-Range": "bytes */*"}); evil.StatusCode != http.StatusForbidden || evil.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("a foreign origin on the session URI = %d %v", evil.StatusCode, evil.Header)
	}
	// A loopback page completes an upload it started.
	req, _ = http.NewRequest("POST", base+"/upload/storage/v1/b/cors-bucket/o?uploadType=resumable&name=l", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	req, _ = http.NewRequest("PUT", resp.Header.Get("Location"), strings.NewReader("hello"))
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Content-Range", "bytes 0-4/5")
	done, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	done.Body.Close()
	if done.StatusCode != 200 || done.Header.Get("Access-Control-Allow-Origin") != "http://localhost:5173" {
		t.Errorf("a loopback resumable upload = %d %v", done.StatusCode, done.Header)
	}
}
