package bigqueryfront

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/service/storage"
)

// The URLs signURL writes, as a service agent whose certificates the
// front serves, are what the instance's Cloud Storage verifies (#1786):
// the object's bytes for 6 hours, 403 when tampered with or expired.
func TestSignURLVerifiedByStorage(t *testing.T) {
	const email = "bqcx-1-ab12" + agentDomain
	var agents agentKeys
	agents.keep(t.TempDir(), t.Logf)
	k, err := agents.get(email)
	if err != nil {
		t.Fatal(err)
	}
	certs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !agents.serveCerts(w, r) {
			http.NotFound(w, r)
		}
	}))
	defer certs.Close()
	srv, err := storage.NewServer(storage.Options{CertSources: map[string]string{strings.TrimPrefix(agentDomain, "@"): certs.URL}})
	if err != nil {
		t.Fatal(err)
	}
	gcs := httptest.NewServer(srv)
	defer gcs.Close()
	do := func(method, u, body string) (int, string) {
		req, _ := http.NewRequest(method, u, strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := do("POST", gcs.URL+"/storage/v1/b?project=p", `{"name":"bkt"}`); code != 200 {
		t.Fatalf("create bucket = %d %s", code, body)
	}
	if code, body := do("PUT", gcs.URL+"/bkt/dir/a%20b.txt", "bytes"); code != 200 {
		t.Fatalf("put = %d %s", code, body)
	}
	u, err := signURL(k, email, gcs.URL, "bkt", "dir/a b.txt", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(u, "X-Goog-Expires=21600") || !strings.Contains(u, "X-Goog-Algorithm=GOOG4-RSA-SHA256") {
		t.Errorf("url = %s", u)
	}
	if code, body := do("GET", u, ""); code != 200 || body != "bytes" {
		t.Errorf("signed GET = %d %s", code, body)
	}
	if code, _ := do("GET", strings.Replace(u, "a%20b.txt", "c.txt", 1), ""); code != 403 {
		t.Errorf("tampered GET = %d", code)
	}
	old, _ := signURL(k, email, gcs.URL, "bkt", "dir/a b.txt", time.Now().Add(-6*time.Hour-time.Second))
	if code, _ := do("GET", old, ""); code != 403 {
		t.Errorf("expired GET = %d", code)
	}
	// The key survives a restart of the front.
	var again agentKeys
	again.keep(agents.path[:strings.LastIndex(agents.path, "/")], t.Logf)
	if k2, _ := again.get(email); k2.ID != k.ID {
		t.Errorf("key after restart = %s, want %s", k2.ID, k.ID)
	}
}
