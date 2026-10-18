package metadata

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// The metadata server hands out tokens and the project's identity, so a
// rebound page must not read it (#676). Metadata-Flavor does not stop one:
// a same-origin page may set any header. Pods and the host keep working.
func TestMetadataRefusesForeignHosts(t *testing.T) {
	t.Parallel()
	c, _ := testCreds(t)
	srv := serve(t, c)
	for host, want := range map[string]int{
		"attacker.example:9004": http.StatusMisdirectedRequest,
		"127.0.0.1:9004":        http.StatusOK,
		"cloudburrow-host.cloudburrow.svc.cluster.local:9004": http.StatusOK,
		"host.docker.internal:9004":                           http.StatusOK,
		"metadata.google.internal":                            http.StatusOK,
	} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/computeMetadata/v1/project/project-id", nil)
		req.Host = host
		req.Header.Set("Metadata-Flavor", "Google")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("project-id with Host %s = %d, want %d", host, resp.StatusCode, want)
		}
	}
}

// A page on another site cannot read a token (#1586). Metadata-Flavor is a
// custom header, so a browser preflights it, and the preflight is refused;
// the token exchange at /token, which takes no Metadata-Flavor and is a
// form POST a page can send without a preflight, is refused on its Origin.
func TestMetadataRefusesCrossSitePages(t *testing.T) {
	t.Parallel()
	c, _ := testCreds(t)
	srv := serve(t, c)
	for _, r := range []struct{ method, path, ctype, body string }{
		{http.MethodOptions, "/computeMetadata/v1/instance/service-accounts/default/token", "", ""},
		{http.MethodGet, "/computeMetadata/v1/instance/service-accounts/default/token", "", ""},
		{http.MethodPost, "/token", "application/x-www-form-urlencoded", "assertion=x"},
	} {
		req, _ := http.NewRequest(r.method, srv.URL+r.path, strings.NewReader(r.body))
		req.Header.Set("Origin", "https://evil.example")
		req.Header.Set("Metadata-Flavor", "Google")
		if r.ctype != "" {
			req.Header.Set("Content-Type", r.ctype)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden || strings.Contains(string(body), "access_token") {
			t.Errorf("%s %s from https://evil.example = %d %q, want 403 with no token", r.method, r.path, resp.StatusCode, body)
		}
		if v := resp.Header.Get("Access-Control-Allow-Origin"); v != "" {
			t.Errorf("%s %s: Access-Control-Allow-Origin %q, want none", r.method, r.path, v)
		}
	}
}
