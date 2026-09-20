//go:build integration

package main

// Integration tests that create a real Kubernetes cluster. They need Docker,
// kind and kubectl, and are excluded from `make check`:
//
//	make test-integration
//
// Unit tests must never require a cluster (docs/architecture.md §3), which is
// why these live behind a build tag.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var addrRE = regexp.MustCompile(`127\.0\.0\.1:\d+`)

// uniqueInstance returns an instance name and state dir that cannot collide
// with a developer's real environment.
//
// This matters: an early run of these tests used the default name and left an
// orphaned "cloudburrow" cluster behind. Integration tests must never touch the
// environment a developer actually uses.
func uniqueInstance(t *testing.T) (name, stateDir string) {
	t.Helper()
	name = fmt.Sprintf("it-%d", time.Now().UnixNano()%1e9)
	stateDir = t.TempDir()
	t.Cleanup(func() {
		// Always remove the cluster, even when the test failed part-way.
		cmd := exec.Command("kind", "delete", "cluster", "--name", "cloudburrow-"+name)
		_ = cmd.Run()
	})
	return name, stateDir
}

// runUpAsync starts runUp in the background and returns its control address
// once it reports listening, plus a stop function.
func runUpAsync(t *testing.T, args ...string) (addr string, out *syncBuffer, stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stdout := &syncBuffer{}
	errCh := make(chan error, 1)

	go func() { errCh <- runUp(ctx, args, stdout, io.Discard) }()

	// Creating a cluster pulls a node image and waits for the API server, so
	// the budget here is minutes, not seconds.
	deadline := time.Now().Add(6 * time.Minute)
	for time.Now().Before(deadline) {
		if m := addrRE.FindString(stdout.String()); m != "" {
			addr = m
			break
		}
		select {
		case err := <-errCh:
			cancel()
			t.Fatalf("runUp exited early: %v (output: %s)", err, stdout.String())
		default:
		}
		time.Sleep(200 * time.Millisecond)
	}
	if addr == "" {
		cancel()
		t.Fatalf("runUp never reported a listening address; output: %s", stdout.String())
	}

	return addr, stdout, func() error {
		cancel()
		select {
		case err := <-errCh:
			return err
		case <-time.After(2 * time.Minute):
			t.Fatal("runUp did not return after cancellation")
			return nil
		}
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// The end-to-end path: start, become ready, shut down cleanly on cancellation.
func TestUpStartsBecomesReadyAndStopsCleanly(t *testing.T) {
	t.Parallel()
	name, stateDir := uniqueInstance(t)
	addr, _, stop := runUpAsync(t, "--name", name, "--state-dir", stateDir, "--port-control", "0")

	resp, err := http.Get("http://" + addr + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/readyz = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Ready      bool            `json:"ready"`
		Components map[string]bool `json:"components"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Ready || !body.Components["control"] {
		t.Errorf("readiness = %+v, want ready with control true", body)
	}

	if err := stop(); err != nil {
		t.Fatalf("shutdown returned %v, want nil", err)
	}

	// The listener must actually be released, so a restart can rebind.
	client := &http.Client{Timeout: time.Second}
	if _, err := client.Get("http://" + addr + "/readyz"); err == nil {
		t.Error("control port still answering after shutdown")
	}
}

// Output must not imply the emulated services are running, because none are.
func TestUpDoesNotClaimServicesAreRunning(t *testing.T) {
	t.Parallel()
	name, stateDir := uniqueInstance(t)
	_, out, stop := runUpAsync(t, "--name", name, "--state-dir", stateDir, "--port-control", "0")
	text := out.String()
	if !strings.Contains(text, "NOT STARTED") {
		t.Errorf("startup output must state that services are not started; got:\n%s", text)
	}
	if err := stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
}
