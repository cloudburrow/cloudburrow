package console

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// hostRequest sends method path to srv as a browser on the page origin
// would: Host and Origin name the page's host, and fetch metadata says
// same-origin, because to the browser it is.
func hostRequest(t *testing.T, url, method, path, host string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url+path, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	req.Header.Set("Origin", "http://"+host)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// DNS rebinding (#676): attacker.example resolves to the attacker, serves a
// page, then resolves to 127.0.0.1. The page's requests then reach the
// console with Host, Origin and Sec-Fetch-Site all consistent, so the
// same-origin check alone lets them through. The Host check refuses them.
func TestARebindingPageIsRefusedByHost(t *testing.T) {
	t.Parallel()
	srv := serve(t, fakeProvider{id: "secretmanager", title: "Secret Manager"})

	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/status"},
		{http.MethodGet, "/api/resources/secretmanager"},
		{http.MethodGet, "/api/requests"},
		{http.MethodPost, "/api/reveal/secretmanager"},
		{http.MethodPost, "/api/resources/secretmanager"},
		{http.MethodDelete, "/api/resources/secretmanager"},
		// The shell too: a rebound page must not even load the console.
		{http.MethodGet, "/"},
	} {
		code, body := hostRequest(t, srv.URL, c.method, c.path, "attacker.example:9090")
		if code != http.StatusMisdirectedRequest {
			t.Errorf("%s %s with Host attacker.example:9090 = %d, want 421", c.method, c.path, code)
		}
		if !strings.Contains(body, `"attacker.example:9090"`) {
			t.Errorf("%s %s refusal does not name the host: %.200q", c.method, c.path, body)
		}
	}
}

// The console's own page, opened at any loopback spelling, keeps working.
func TestTheConsoleAnswersItsLoopbackNames(t *testing.T) {
	t.Parallel()
	srv := serve(t)
	for _, host := range []string{"127.0.0.1:9090", "localhost:9090", "[::1]:9090", "LOCALHOST:9090"} {
		if code, body := hostRequest(t, srv.URL, http.MethodGet, "/api/status", host); code != http.StatusOK {
			t.Errorf("GET /api/status with Host %s = %d, want 200: %.200s", host, code, body)
		}
	}
}
