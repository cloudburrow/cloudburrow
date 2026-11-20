package firestoreadmin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
)

// TestRulesGateDeniesClientsBeforeAnyRelease (#2050): a client call in a
// project the front has seen no rules for first sets its deny-all rules;
// a project whose rules the CLI set, and server calls, are left alone.
func TestRulesGateDeniesClientsBeforeAnyRelease(t *testing.T) {
	var mu sync.Mutex
	var puts []string
	emu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			puts = append(puts, r.URL.Path)
			mu.Unlock()
			if !strings.Contains(string(b), "if ") || r.Header.Get("Authorization") != ownerToken {
				t.Errorf("the front's rules: %s (%s)", b, r.Header.Get("Authorization"))
			}
		}
	}))
	defer emu.Close()
	a := New(strings.TrimPrefix(emu.URL, "http://"))
	h := ClientAuth(a.RulesGate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})))
	call := func(method, path, client string) {
		r := httptest.NewRequest(method, path, strings.NewReader(`{"rules":{"files":[{"content":"allow read: if true;"}]}}`))
		if client != "" {
			r.Header.Set("X-Goog-Api-Client", client)
		}
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	fire := "gl-js/ fire/12.0.0"
	call(http.MethodGet, "/v1/projects/p/databases/(default)/documents/users/a", fire)
	call(http.MethodGet, "/v1/projects/p/databases/(default)/documents/users/b", fire)
	call(http.MethodGet, "/v1/projects/s/databases/(default)/documents/users/a", "gl-go/1.26")
	call(http.MethodPut, "/emulator/v1/projects/q:securityRules", "")
	call(http.MethodGet, "/v1/projects/q/databases/(default)/documents/users/a", fire)
	call(http.MethodPost, "/google.firestore.v1.Firestore/Listen/channel?database=projects%2Fw%2Fdatabases%2F(default)", "")
	mu.Lock()
	if got := strings.Join(puts, " "); got != "/emulator/v1/projects/p:securityRules /emulator/v1/projects/w:securityRules" {
		t.Errorf("the front set rules for %s; want p (once) and w", got)
	}
	puts = nil
	mu.Unlock()

	// An emulator that restarted empty gets every project's rules again.
	a.restoreRules(context.Background())
	mu.Lock()
	defer mu.Unlock()
	sort.Strings(puts)
	if got := strings.Join(puts, " "); got != "/emulator/v1/projects/p:securityRules /emulator/v1/projects/q:securityRules /emulator/v1/projects/w:securityRules" {
		t.Errorf("after a restart, the front set rules for %s; want p, q and w", got)
	}
}
