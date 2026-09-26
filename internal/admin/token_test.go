package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// With a token set (#553), every admin route refuses a request without it,
// or with the wrong one, before touching anything; the right one is served.
func TestAdminRoutesRequireTheToken(t *testing.T) {
	var calls []string
	a := NewAPI(NewRecorder(10, nil))
	a.RegisterResetter(fakeResetter{name: "tasks", calls: &calls, ordered: &calls})
	a.RequireToken("s3cret")
	srv := serve(a)
	defer srv.Close()

	do := func(method, path, auth string) int {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(""))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, c := range []struct{ method, path string }{
		{"POST", "/admin/reset"}, {"POST", "/admin/seed"}, {"GET", "/admin/events"},
		{"POST", "/admin/state/export"}, {"POST", "/admin/state/import"},
		{"POST", "/admin/faults"}, {"GET", "/admin/faults"}, {"DELETE", "/admin/faults"},
	} {
		if code := do(c.method, c.path, ""); code != http.StatusUnauthorized {
			t.Errorf("%s %s without a token = %d, want 401", c.method, c.path, code)
		}
		if code := do(c.method, c.path, "Bearer wrong"); code != http.StatusUnauthorized {
			t.Errorf("%s %s with the wrong token = %d, want 401", c.method, c.path, code)
		}
	}
	if len(calls) != 0 {
		t.Fatalf("a refused reset still reset %v", calls)
	}
	if code := do("POST", "/admin/reset", "Bearer s3cret"); code != http.StatusOK {
		t.Errorf("reset with the token = %d, want 200", code)
	}
	if len(calls) != 1 {
		t.Errorf("the authorised reset ran %d resets, want 1", len(calls))
	}
}

// Without a token the routes are open, as the in-process tests rely on.
func TestAdminRoutesAreOpenWithoutAToken(t *testing.T) {
	a := NewAPI(NewRecorder(10, nil))
	srv := serve(a)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/admin/events")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /admin/events with no token configured = %d, want 200", resp.StatusCode)
	}
	_ = httptest.NewRecorder
}
