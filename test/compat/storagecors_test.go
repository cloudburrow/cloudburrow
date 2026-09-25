//go:build compat

package compat

import (
	"net/http"
	"testing"
	"time"

	"cloud.google.com/go/storage"
)

// CORS (#502), to docs.cloud.google.com/storage/docs/cross-origin: XML
// endpoints follow the bucket's cors configuration, JSON endpoints always
// allow. fake-gcs-server does neither (#374), so these run against the
// builtin server.

func corsBucketFor(t *testing.T, h *Harness, c *storage.Client) *storage.BucketHandle {
	t.Helper()
	bh := bucket(t, h, c)
	if _, err := bh.Update(h.Context(), storage.BucketAttrsToUpdate{CORS: []storage.CORS{{
		Origins: []string{"https://app.example"}, Methods: []string{"GET", "PUT"},
		ResponseHeaders: []string{"Content-Type"}, MaxAge: 10 * time.Minute,
	}}}); err != nil {
		t.Fatal(err)
	}
	return bh
}

func preflight(t *testing.T, h *Harness, path string, hdr map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(h.Context(), http.MethodOptions, h.Endpoint(EnvStorage)+path, nil)
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
	builtinOnly(t, "CORS configuration, #374")
	h := New(t)
	c := storageClient(t, h)
	bh := corsBucketFor(t, h, c)
	a, err := bh.Attrs(h.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(a.CORS) != 1 || a.CORS[0].Origins[0] != "https://app.example" || len(a.CORS[0].Methods) != 2 || a.CORS[0].MaxAge != 10*time.Minute {
		t.Errorf("cors = %+v", a.CORS)
	}
}

// TestStorageXMLPreflightMatches: an XML preflight matching a rule gets its
// origin, methods and max age back.
func TestStorageXMLPreflightMatches(t *testing.T) {
	builtinOnly(t, "CORS on the XML API, #374")
	h := New(t)
	c := storageClient(t, h)
	bh := corsBucketFor(t, h, c)
	resp := preflight(t, h, "/"+bh.BucketName()+"/o.txt", map[string]string{
		"Origin": "https://app.example", "Access-Control-Request-Method": "PUT", "Access-Control-Request-Headers": "content-type",
	})
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Access-Control-Allow-Origin") != "https://app.example" ||
		resp.Header.Get("Access-Control-Allow-Methods") != "GET, PUT" || resp.Header.Get("Access-Control-Max-Age") != "600" {
		t.Errorf("a matching preflight = %d %v", resp.StatusCode, resp.Header)
	}
}

// TestStorageXMLPreflightMismatch200NoHeaders: an XML preflight matching no
// rule gets 200 with no CORS headers, as documented.
func TestStorageXMLPreflightMismatch200NoHeaders(t *testing.T) {
	builtinOnly(t, "CORS on the XML API, #374")
	h := New(t)
	c := storageClient(t, h)
	bh := corsBucketFor(t, h, c)
	resp := preflight(t, h, "/"+bh.BucketName()+"/o.txt", map[string]string{
		"Origin": "https://evil.example", "Access-Control-Request-Method": "GET",
	})
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Access-Control-Allow-Origin") != "" || resp.Header.Get("Access-Control-Allow-Methods") != "" {
		t.Errorf("a mismatched preflight = %d %v; want 200 with no CORS headers", resp.StatusCode, resp.Header)
	}
}

// TestStorageJSONAlwaysAllowsCORS: JSON API endpoints allow any origin,
// echoing it, with DELETE, GET, HEAD, PATCH, POST and PUT.
//
// unverified: storage.objects.list 200: the JSON API's CORS defaults beyond Allow-Origin, Allow-Methods and Max-Age (the requested headers echoed, Allow-Credentials on simple requests)
func TestStorageJSONAlwaysAllowsCORS(t *testing.T) {
	builtinOnly(t, "CORS on the JSON API, #374")
	h := New(t)
	c := storageClient(t, h)
	bh := corsBucketFor(t, h, c)
	resp := preflight(t, h, "/storage/v1/b/"+bh.BucketName()+"/o", map[string]string{
		"Origin": "https://evil.example", "Access-Control-Request-Method": "PATCH",
	})
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Access-Control-Allow-Origin") != "https://evil.example" ||
		resp.Header.Get("Access-Control-Allow-Methods") != "DELETE, GET, HEAD, PATCH, POST, PUT" || resp.Header.Get("Access-Control-Max-Age") != "3600" {
		t.Errorf("a JSON preflight = %d %v", resp.StatusCode, resp.Header)
	}
}
