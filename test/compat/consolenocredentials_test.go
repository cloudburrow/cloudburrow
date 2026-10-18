//go:build compat

package compat

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
)

// TestConsoleReadsHoldNoCredentials (#1576): what the browser is sent holds
// none of the instance's credentials. Every screen the console lists is
// read (its listing, its page, and the detail of its first row), with the
// console's status, instance, settings, connect, about, terminal, logs,
// operations, requests, metrics and search answers, and none holds the
// instance kubeconfig's client key, certificate or token, the ADC
// fixture's private key, the admin token, or a Secret Manager payload. No
// route forwards the Docker Engine API.
func TestConsoleReadsHoldNoCredentials(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	if os.Getenv(EnvCLI) == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	forbidden := diagnoseForbidden(t, h, instanceDirFrom(t, cliArgs()))
	if token := adminToken(t); token != "" {
		forbidden["the admin token"] = token
	}
	project := url.QueryEscape(h.Project())

	paths := []string{"/api/status", "/api/services", "/api/instance", "/api/settings", "/api/connect", "/api/about",
		"/api/terminal", "/api/logs?limit=1000", "/api/operations", "/api/requests", "/api/metrics", "/api/search?q=kube",
		"/api/faults"}
	_, body := consoleDo(t, addr, http.MethodGet, "/api/services", "")
	var resp struct {
		Services []map[string]any `json:"services"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil || len(resp.Services) == 0 {
		t.Fatalf("/api/services = %.300s", body)
	}
	offers := func(s map[string]any, k string) bool { v, ok := s[k]; return ok && v != nil && v != false }
	for _, s := range resp.Services {
		id, _ := s["id"].(string)
		listing := "/api/resources/" + id + "?project=" + project
		paths = append(paths, listing)
		if offers(s, "page") {
			paths = append(paths, "/api/page/"+id+"?project="+project)
		}
		if !offers(s, "detail") {
			continue
		}
		code, lb := consoleDo(t, addr, http.MethodGet, listing, "")
		var l struct {
			Items []struct {
				Name string `json:"name"`
			} `json:"items"`
		}
		if code == http.StatusOK && json.Unmarshal([]byte(lb), &l) == nil && len(l.Items) > 0 {
			paths = append(paths, "/api/detail/"+id+"?project="+project+"&name="+url.QueryEscape(l.Items[0].Name))
		}
	}
	for _, p := range paths {
		code, body := consoleDo(t, addr, http.MethodGet, p, "")
		for what, value := range forbidden {
			if value != "" && strings.Contains(body, value) {
				t.Errorf("GET %s (%d) holds %s", p, code, what)
			}
		}
	}
	for _, p := range []string{"/_ping", "/version", "/v1.41/containers/json", "/containers/json", "/api/docker/containers/json"} {
		code, body := consoleDo(t, addr, http.MethodGet, p, "")
		if strings.Contains(body, `"ApiVersion"`) || strings.Contains(body, `"Containers"`) || strings.Contains(body, `"Image":`) || body == "OK" {
			t.Errorf("GET %s = %d %.200s: the console forwards the Docker Engine API", p, code, body)
		}
	}
	t.Logf("%d console reads checked against %d credentials", len(paths), len(forbidden))
}
