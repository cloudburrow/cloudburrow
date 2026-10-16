package metadata

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A Cloud Run workload reaches the metadata server under its account's
// cloudburrow-sa- host, and is told that account (#1388): the email, the
// listing, the account path and the identity token's email claim. Any other
// host is answered as the instance, and a cloudburrow-sa- label the
// resolver never issued, or one under a name outside the cluster, is
// refused as any foreign host is.
func TestWorkloadHostNamesItsOwnAccount(t *testing.T) {
	c, _ := testCreds(t)
	const sa = "runner@demo.iam.gserviceaccount.com"
	srv := httptest.NewServer(NewServer(c, "demo", "127.0.0.1", 0).WithAccounts(func(label string) (string, bool) {
		return sa, label == "cloudburrow-sa-0123"
	}).Handler())
	t.Cleanup(srv.Close)
	get := func(host, path string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		req.Host = host
		req.Header.Set(FlavorHeader, FlavorValue)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	const email = "/computeMetadata/v1/instance/service-accounts/default/email"
	for _, host := range []string{"cloudburrow-sa-0123.cloudburrow-run.svc.cluster.local:9004",
		"cloudburrow-sa-0123.cloudburrow-run.svc", "cloudburrow-sa-0123:9004"} {
		if code, body := get(host, email); code != http.StatusOK || body != sa {
			t.Errorf("%s: email = %d %q, want %s", host, code, body, sa)
		}
	}
	if code, body := get("cloudburrow-host.cloudburrow.svc.cluster.local:9004", email); code != http.StatusOK || body != c.Email {
		t.Errorf("cloudburrow-host: email = %d %q, want the instance's %s", code, body, c.Email)
	}
	host := "cloudburrow-sa-0123.cloudburrow-run.svc.cluster.local:9004"
	if code, body := get(host, "/computeMetadata/v1/instance/service-accounts/"); code != http.StatusOK || !strings.Contains(body, sa+"/") {
		t.Errorf("listing = %d %q, want %s", code, body, sa)
	}
	if code, body := get(host, "/computeMetadata/v1/instance/service-accounts/"+sa+"/email"); code != http.StatusOK || body != sa {
		t.Errorf("by email = %d %q", code, body)
	}
	if code, _ := get(host, "/computeMetadata/v1/instance/service-accounts/"+c.Email+"/email"); code != http.StatusNotFound {
		t.Errorf("the instance's account under a workload's host = %d, want 404", code)
	}
	code, tok := get(host, "/computeMetadata/v1/instance/service-accounts/default/identity?audience=https://x")
	parts := strings.Split(tok, ".")
	if code != http.StatusOK || len(parts) != 3 {
		t.Fatalf("identity = %d %q", code, tok)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil || claims["email"] != sa {
		t.Errorf("identity token claims = %v (%v), want email %s", claims, err, sa)
	}
	for _, host := range []string{"cloudburrow-sa-9999.cloudburrow-run.svc.cluster.local", "cloudburrow-sa-0123.attacker.example",
		"cloudburrow-sa-0123.com"} {
		if code, _ := get(host, email); code != http.StatusMisdirectedRequest {
			t.Errorf("%s: %d, want 421", host, code)
		}
	}
}
