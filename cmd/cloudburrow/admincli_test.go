package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

const testAdminToken = "0123456789abcdef-test-token"

// adminCallSeen is one request the fake control endpoint received.
type adminCallSeen struct {
	method, path, query, auth, body string
}

// fakeAdmin stands in for a running `up`'s control endpoint: a runtime file
// naming this (cloudburrow.test) process and the server, and the admin-token
// file beside it. Every request is recorded and answered with status and
// reply.
func fakeAdmin(t *testing.T, cfg config.Config, status int, reply string) func() []adminCallSeen {
	t.Helper()
	var mu sync.Mutex
	var seen []adminCallSeen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, adminCallSeen{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), string(b)})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	b, _ := json.Marshal(runtimeInfo{PID: os.Getpid(), Control: strings.TrimPrefix(srv.URL, "http://")})
	if err := os.MkdirAll(cfg.InstanceDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtimePath(cfg), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(adminTokenPath(cfg), []byte(testAdminToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return func() []adminCallSeen {
		mu.Lock()
		defer mu.Unlock()
		return append([]adminCallSeen(nil), seen...)
	}
}

// noNamespaceReset fails the test if the namespace path is reached.
func noNamespaceReset(t *testing.T) *bool {
	t.Helper()
	called := false
	prev := resetNamespace
	resetNamespace = func(config.Config, io.Writer) error {
		called = true
		return errors.New("the namespace was deleted")
	}
	t.Cleanup(func() { resetNamespace = prev })
	return &called
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, errUsage) {
		return 2
	}
	var exit *exitError
	if errors.As(err, &exit) {
		return exit.code
	}
	return 1
}

// With an `up` running, reset is POST /admin/reset with the token, scoped
// as asked, and never deletes the namespace beneath it (#586).
func TestResetUnderARunningUpUsesTheAdminAPI(t *testing.T) {
	called := noNamespaceReset(t)
	args, cfg := fakeConfigArgs(t)
	calls := fakeAdmin(t, cfg, http.StatusOK, `{"reset":["tasks","pubsub"],"reseeded":["pubsub"]}`)

	var out, errOut strings.Builder
	err := run(append([]string{"reset", "--service", "tasks", "--service=pubsub", "--project", "p", "--reseed"}, args...), &out, &errOut)
	if err != nil {
		t.Fatalf("reset = %v\n%s", err, errOut.String())
	}
	if *called {
		t.Fatal("reset with a running up deleted the managed namespace")
	}
	got := calls()
	if len(got) != 1 {
		t.Fatalf("%d admin calls, want 1: %+v", len(got), got)
	}
	c := got[0]
	if c.method != http.MethodPost || c.path != "/admin/reset" {
		t.Errorf("request %s %s, want POST /admin/reset", c.method, c.path)
	}
	if c.auth != "Bearer "+testAdminToken {
		t.Errorf("Authorization = %q, want the instance's token", c.auth)
	}
	if c.query != "project=p&reseed=true&service=tasks&service=pubsub" {
		t.Errorf("query = %q", c.query)
	}
	for _, want := range []string{"reset: tasks, pubsub in project p", "reseeded from the startup seed file: pubsub", "untouched"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String()+errOut.String(), testAdminToken) {
		t.Error("reset printed the admin token")
	}
}

func TestResetUnderARunningUpReportsFailures(t *testing.T) {
	for _, c := range []struct {
		name, reply string
		status      int
		says        string
	}{
		{"refused", `{"error":"unknown service(s); nothing was reset","unknown":["nope"]}`, http.StatusBadRequest, "unknown service(s); nothing was reset"},
		{"partial", `{"reset":["tasks"],"failed":{"storage":"backend down"}}`, http.StatusInternalServerError, "1 component(s) failed"},
		{"token", `{"error":"needs the token"}`, http.StatusUnauthorized, "refused the token"},
	} {
		t.Run(c.name, func(t *testing.T) {
			called := noNamespaceReset(t)
			args, cfg := fakeConfigArgs(t)
			fakeAdmin(t, cfg, c.status, c.reply)
			var out, errOut strings.Builder
			err := run(append([]string{"reset"}, args...), &out, &errOut)
			if code := exitCode(err); code != 1 {
				t.Fatalf("exit %d (%v), want 1", code, err)
			}
			if !strings.Contains(err.Error(), c.says) {
				t.Errorf("error %q lacks %q", err, c.says)
			}
			if *called {
				t.Error("a failed admin reset fell back to deleting the namespace")
			}
			if strings.Contains(err.Error()+out.String(), testAdminToken) {
				t.Error("the admin token was printed")
			}
		})
	}
}

// With no `up` running, reset keeps the ownership-checked namespace path,
// and a scoped reset is refused rather than widened into a full wipe.
func TestResetWithoutARunningUp(t *testing.T) {
	args, _ := fakeConfigArgs(t)
	var reached bool
	prev := resetNamespace
	resetNamespace = func(config.Config, io.Writer) error { reached = true; return nil }
	t.Cleanup(func() { resetNamespace = prev })

	var out, errOut strings.Builder
	if err := run(append([]string{"reset"}, args...), &out, &errOut); err != nil {
		t.Fatalf("reset = %v", err)
	}
	if !reached {
		t.Error("reset with no up running did not take the namespace path")
	}

	reached = false
	err := run(append([]string{"reset", "--service", "tasks"}, args...), &out, &errOut)
	if code := exitCode(err); code != 1 || !strings.Contains(err.Error(), "need a running `cloudburrow up`") {
		t.Errorf("scoped reset with no up = exit %d, %v", code, err)
	}
	if reached {
		t.Error("a reset scoped to one service deleted the whole namespace")
	}
}

func TestSeedPostsTheFileWithTheToken(t *testing.T) {
	args, cfg := fakeConfigArgs(t)
	calls := fakeAdmin(t, cfg, http.StatusOK, `{"seeded":["tasks"]}`)
	doc := `{"components": {"tasks": {"queues": ["projects/p/locations/us-central1/queues/work"]}}}`
	file := filepath.Join(t.TempDir(), "seed.json")
	if err := os.WriteFile(file, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut strings.Builder
	if err := run(append([]string{"seed", file, "--if-not-exists"}, args...), &out, &errOut); err != nil {
		t.Fatalf("seed = %v\n%s", err, errOut.String())
	}
	got := calls()
	if len(got) != 1 {
		t.Fatalf("%d admin calls, want 1", len(got))
	}
	c := got[0]
	if c.method != http.MethodPost || c.path != "/admin/seed" || c.query != "ifNotExists=true" {
		t.Errorf("request %s %s?%s, want POST /admin/seed?ifNotExists=true", c.method, c.path, c.query)
	}
	if c.auth != "Bearer "+testAdminToken {
		t.Errorf("Authorization = %q", c.auth)
	}
	if c.body != doc {
		t.Errorf("body = %q, want the file", c.body)
	}
	if !strings.Contains(out.String(), "seeded "+file+": tasks") {
		t.Errorf("output: %s", out.String())
	}
}

func TestSeedReportsAConflict(t *testing.T) {
	args, cfg := fakeConfigArgs(t)
	fakeAdmin(t, cfg, http.StatusConflict, `{"error":"queue work already exists; set ifNotExists to skip it","component":"tasks","seeded":["storage"]}`)
	file := filepath.Join(t.TempDir(), "seed.json")
	_ = os.WriteFile(file, []byte(`{}`), 0o600)
	var out, errOut strings.Builder
	err := run(append([]string{"seed", file}, args...), &out, &errOut)
	if code := exitCode(err); code != 1 || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("seed conflict = exit %d, %v", code, err)
	}
	if !strings.Contains(out.String(), "seeded before the failure: storage") {
		t.Errorf("output: %s", out.String())
	}
}

func TestAdminCommandsNeedARunningUp(t *testing.T) {
	args, _ := fakeConfigArgs(t)
	file := filepath.Join(t.TempDir(), "seed.json")
	_ = os.WriteFile(file, []byte(`{}`), 0o600)
	for _, cmd := range [][]string{{"seed", file}, {"events"}} {
		var out, errOut strings.Builder
		err := run(append(cmd, args...), &out, &errOut)
		if code := exitCode(err); code != 1 || !strings.Contains(err.Error(), "is not running") {
			t.Errorf("%s with no up = exit %d, %v", cmd[0], code, err)
		}
	}
	var out, errOut strings.Builder
	if code := exitCode(run(append([]string{"seed"}, args...), &out, &errOut)); code != 2 {
		t.Errorf("seed without a file = exit %d, want 2", code)
	}
}

func TestEventsQueriesAndPrints(t *testing.T) {
	args, cfg := fakeConfigArgs(t)
	reply := `{"count":1,"events":[{"time":"2026-09-26T10:00:00Z","service":"tasks","kind":"request",` +
		`"target":"CreateQueue","detail":{"code":"OK","resource":"projects/p/locations/l/queues/work"}}]}`
	calls := fakeAdmin(t, cfg, http.StatusOK, reply)

	var out, errOut strings.Builder
	if err := run(append([]string{"events", "--service", "tasks", "--limit", "5", "--since", "2026-09-26T09:00:00Z", "--format", "json"}, args...), &out, &errOut); err != nil {
		t.Fatalf("events = %v\n%s", err, errOut.String())
	}
	c := calls()[0]
	if c.method != http.MethodGet || c.path != "/admin/events" || c.auth != "Bearer "+testAdminToken {
		t.Errorf("request %s %s auth %q", c.method, c.path, c.auth)
	}
	if c.query != "limit=5&service=tasks&since=2026-09-26T09%3A00%3A00Z" {
		t.Errorf("query = %q", c.query)
	}
	var e struct{ Service, Target string }
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &e); err != nil || e.Target != "CreateQueue" {
		t.Errorf("json output %q: %v", out.String(), err)
	}

	out.Reset()
	if err := run(append([]string{"events"}, args...), &out, &errOut); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"tasks", "request", "CreateQueue", "OK", "queues/work"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("text output lacks %q: %s", want, out.String())
		}
	}
}

func TestEventsFlags(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	o, _, err := eventsFlags([]string{"--since", "10m"}, now)
	if err != nil || !o.since.Equal(now.Add(-10*time.Minute)) {
		t.Errorf("-since 10m = %v, %v", o.since, err)
	}
	for _, bad := range [][]string{{"--since", "yesterday"}, {"--limit", "0"}, {"--format", "yaml"}} {
		if _, _, err := eventsFlags(bad, now); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}
