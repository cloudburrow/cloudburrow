package storage

import (
	"context"
	"net/http"
	"testing"

	gcs "cloud.google.com/go/storage"
)

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
	_, h := sdk(t)
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

// An XML preflight that matches no rule gets 200 with no CORS headers.
func TestStorageXMLPreflightMismatch200NoHeadersInProcess(t *testing.T) {
	base := corsBucket(t)
	for name, h := range map[string]map[string]string{
		"origin": {"Origin": "https://evil.example", "Access-Control-Request-Method": "GET"},
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

// JSON API endpoints allow any origin whatever the bucket says.
//
// unverified: storage.objects.get 200: the JSON API's defaults beyond Allow-Origin, Allow-Methods and Max-Age (Allow-Headers echoed, Allow-Credentials on simple requests)
func TestStorageJSONAlwaysAllowsCORSInProcess(t *testing.T) {
	base := corsBucket(t)
	for _, path := range []string{"/storage/v1/b/cors-bucket/o", "/upload/storage/v1/b/cors-bucket/o", "/download/storage/v1/b/cors-bucket/o/x"} {
		resp := corsRequest(t, "OPTIONS", base+path, map[string]string{
			"Origin": "https://evil.example", "Access-Control-Request-Method": "PATCH", "Access-Control-Request-Headers": "authorization, content-type",
		})
		h := resp.Header
		if resp.StatusCode != 200 || h.Get("Access-Control-Allow-Origin") != "https://evil.example" || h.Get("Access-Control-Allow-Methods") != jsonCORSMethods ||
			h.Get("Access-Control-Max-Age") != "3600" || h.Get("Access-Control-Allow-Headers") != "authorization, content-type" {
			t.Errorf("%s preflight = %d %v", path, resp.StatusCode, h)
		}
	}
	actual := corsRequest(t, "GET", base+"/storage/v1/b/cors-bucket/o", map[string]string{"Origin": "https://evil.example"})
	if actual.StatusCode != 200 || actual.Header.Get("Access-Control-Allow-Origin") != "https://evil.example" {
		t.Errorf("an actual JSON request = %d %v", actual.StatusCode, actual.Header)
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
}
