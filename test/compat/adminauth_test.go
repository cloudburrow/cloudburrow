//go:build compat

package compat

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// EnvAdminToken carries the running instance's admin token (#553). When it
// is unset, the token is read from the instance's state directory named by
// EnvCLIArgs.
const EnvAdminToken = "CLOUDBURROW_TEST_ADMIN_TOKEN"

// adminToken is the token every /admin request must carry, or "" when the
// suite has no way to know it, in which case the admin calls are refused
// and the tests that make them fail, saying why.
func adminToken(t *testing.T) string {
	t.Helper()
	if tok := strings.TrimSpace(os.Getenv(EnvAdminToken)); tok != "" {
		return tok
	}
	if os.Getenv(EnvCLI) != "" {
		if b, err := os.ReadFile(filepath.Join(instanceDirFrom(t, strings.Fields(os.Getenv(EnvCLIArgs))), "admin-token")); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

// adminDo sends one admin request with the token.
func adminDo(t *testing.T, method, url, contentType string, body io.Reader) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if tok := adminToken(t); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		t.Logf("%s %s was refused: the suite needs %s or the instance's admin-token file", method, url, EnvAdminToken)
	}
	return resp.StatusCode, string(b)
}

// TestAdminAPIRequiresTheToken (#553): a request to any /admin route without
// the instance's token, or with a wrong one, is 401 and changes nothing; the
// health and readiness endpoints stay open; the token works.
func TestAdminAPIRequiresTheToken(t *testing.T) {
	h := New(t)
	control := h.Endpoint(EnvControl)
	if adminToken(t) == "" {
		t.Skipf("%s is not set and no instance directory is known; the token cannot be tested", EnvAdminToken)
	}
	q, tp, sub, sec := seedProject(t, h)
	bare := func(method, path, auth string) int {
		t.Helper()
		req, _ := http.NewRequest(method, "http://"+control+path, nil)
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
		{"POST", "/admin/reset?project=" + h.Project()}, {"POST", "/admin/seed"}, {"GET", "/admin/events"},
		{"POST", "/admin/state/export"}, {"GET", "/admin/faults"},
	} {
		if code := bare(c.method, c.path, ""); code != http.StatusUnauthorized {
			t.Errorf("%s %s without the token = %d, want 401", c.method, c.path, code)
		}
		if code := bare(c.method, c.path, "Bearer not-the-token"); code != http.StatusUnauthorized {
			t.Errorf("%s %s with a wrong token = %d, want 401", c.method, c.path, code)
		}
	}
	if kept := present(t, h, q, tp, sub, sec); len(kept) != 4 {
		t.Errorf("a refused reset still removed state: only %v remain", kept)
	}
	for _, path := range []string{"/healthz", "/readyz"} {
		if code := bare("GET", path, ""); code == http.StatusUnauthorized {
			t.Errorf("GET %s asks for the admin token; health must stay open", path)
		}
	}
	if code, body := adminReset(t, control, "service=tasks,pubsub,secretmanager&project="+h.Project()); code != http.StatusOK {
		t.Fatalf("reset with the token = %d %s", code, body)
	}
}
