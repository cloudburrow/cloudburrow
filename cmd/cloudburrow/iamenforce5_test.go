package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestStripForMatch (#1879): Valkey's JSON requests are checked by their
// path without /memorystore, and served with it, body intact; other paths
// are served unchecked.
func TestStripForMatch(t *testing.T) {
	var checked, served, body string
	wrap := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			checked = r.URL.Path
			next.ServeHTTP(w, r)
		})
	}
	h := stripForMatch("/memorystore", wrap, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		served = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/memorystore/v1/projects/p/locations/l/instances", strings.NewReader("{}")))
	if checked != "/v1/projects/p/locations/l/instances" || served != "/memorystore/v1/projects/p/locations/l/instances" || body != "{}" {
		t.Fatalf("checked %q, served %q, body %q", checked, served, body)
	}
	checked = ""
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/projects/p/locations/l/instances", nil))
	if checked != "" || served != "/v1/projects/p/locations/l/instances" {
		t.Fatalf("an unprefixed path: checked %q, served %q", checked, served)
	}
}
