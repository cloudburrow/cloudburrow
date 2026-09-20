package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(body)
}

// startCoordinator returns a ready coordinator with a control server on an
// OS-assigned port. Port 0 keeps tests parallel-safe.
func startCoordinator(t *testing.T, extra ...Component) (*Coordinator, *ControlServer) {
	t.Helper()
	c := New(2 * time.Second)
	cs := NewControlServer(0, c)
	c.Register(cs)
	c.Register(extra...)
	t.Cleanup(func() { _ = c.Stop(context.Background()) })
	return c, cs
}

func TestControlServerReportsReady(t *testing.T) {
	t.Parallel()
	c, cs := startCoordinator(t)
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start() = %v", err)
	}

	base := "http://" + cs.Addr()

	status, body := get(t, base+"/healthz")
	if status != http.StatusOK {
		t.Errorf("/healthz status = %d, want 200", status)
	}
	if !strings.Contains(body, "ok") {
		t.Errorf("/healthz body = %q", body)
	}

	status, body = get(t, base+"/readyz")
	if status != http.StatusOK {
		t.Errorf("/readyz status = %d, want 200; body=%s", status, body)
	}
	var resp readyResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode /readyz: %v (body=%s)", err, body)
	}
	if !resp.Ready {
		t.Error("ready = false, want true")
	}
	if !resp.Components["control"] {
		t.Errorf("components = %v, want control:true", resp.Components)
	}
}

// The important negative case: a process whose mandatory startup work failed
// must answer 503 on readiness while still answering health, so a caller can
// distinguish "alive but broken" from "not listening".
func TestControlServerReportsNotReadyAfterFailedStart(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	c := New(2 * time.Second)
	cs := NewControlServer(0, c)
	c.Register(cs)
	c.Register(&fakeComponent{name: "mandatory", rec: rec, startErr: errors.New("disk unavailable")})
	t.Cleanup(func() { _ = c.Stop(context.Background()) })

	if err := c.Start(context.Background()); err == nil {
		t.Fatal("Start() = nil, want error")
	}

	// The control server was unwound with everything else, so bring a fresh one
	// up against the failed coordinator to inspect what it reports.
	probe := NewControlServer(0, c)
	if err := probe.Start(context.Background()); err != nil {
		t.Fatalf("probe Start() = %v", err)
	}
	t.Cleanup(func() { _ = probe.Stop(context.Background()) })

	status, body := get(t, "http://"+probe.Addr()+"/readyz")
	if status != http.StatusServiceUnavailable {
		t.Errorf("/readyz status = %d, want 503; body=%s", status, body)
	}
	var resp readyResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Ready {
		t.Error("ready = true after a failed start")
	}
	if resp.State != "failed" {
		t.Errorf("state = %q, want failed", resp.State)
	}
	if !strings.Contains(resp.Error, "disk unavailable") {
		t.Errorf("error = %q, want the underlying cause", resp.Error)
	}
	if resp.Components["mandatory"] {
		t.Error("failed component reported ready")
	}

	// Liveness is still fine: the process is running.
	if status, _ := get(t, "http://"+probe.Addr()+"/healthz"); status != http.StatusOK {
		t.Errorf("/healthz status = %d, want 200 even when not ready", status)
	}
}

// An occupied port must fail at Start with an actionable error, not silently.
func TestControlServerOccupiedPort(t *testing.T) {
	t.Parallel()
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	port := blocker.Addr().(*net.TCPAddr).Port

	c := New(time.Second)
	cs := NewControlServer(port, c)
	c.Register(cs)

	err = c.Start(context.Background())
	if err == nil {
		t.Fatal("Start() = nil, want error for an occupied port")
	}
	if !strings.Contains(err.Error(), fmt.Sprint(port)) {
		t.Errorf("error should name the port %d, got: %v", port, err)
	}
	if c.Ready() {
		t.Error("Ready() = true despite a failed bind")
	}
}

// Port 0 must yield distinct ports, which is what allows parallel instances.
func TestControlServerOSAssignedPortsAreDistinct(t *testing.T) {
	t.Parallel()
	c1, cs1 := startCoordinator(t)
	c2, cs2 := startCoordinator(t)
	if err := c1.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c2.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cs1.Port() == 0 || cs2.Port() == 0 {
		t.Fatalf("ports not resolved: %d, %d", cs1.Port(), cs2.Port())
	}
	if cs1.Port() == cs2.Port() {
		t.Errorf("both instances got port %d; parallel instances would collide", cs1.Port())
	}
}

func TestControlServerRejectsNonGET(t *testing.T) {
	t.Parallel()
	c, cs := startCoordinator(t)
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post("http://"+cs.Addr()+"/readyz", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /readyz = %d, want 405", resp.StatusCode)
	}
}

// After Stop the listener must be closed, not merely ignored.
func TestControlServerStopClosesListener(t *testing.T) {
	t.Parallel()
	c, cs := startCoordinator(t)
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	addr := cs.Addr()
	if err := c.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() = %v", err)
	}
	client := &http.Client{Timeout: time.Second}
	if _, err := client.Get("http://" + addr + "/healthz"); err == nil {
		t.Error("control server still answering after Stop")
	}
}
