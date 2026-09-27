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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// controlRE finds the control address in the banner. Anchored to the
// control line, so an address printed earlier cannot be taken for it.
var controlRE = regexp.MustCompile(`control: +http://(127\.0\.0\.1:\d+)`)

// instances counts upArgs calls, so each instance a run starts gets its own
// port block.
var instances atomic.Int32

// upArgs are the flags for an instance of these tests: its own name and
// state dir, an OS-assigned control port, and its own port block, well
// clear of the defaults (from 9000), which a developer's instance may hold,
// and of the blocks the port-base and offline tests use (below 50000).
// Storage and Pub/Sub, the two in-cluster backends, rather than the
// defaults: Cloud Run's Knative install is minutes on its own, and the
// compatibility suite's run shard covers it.
func upArgs(t *testing.T) []string {
	t.Helper()
	name, stateDir := uniqueInstance(t)
	base := 50000 + int(time.Now().UnixNano()%60)*200 + int(instances.Add(1)%2)*100
	return []string{"--name", name, "--state-dir", stateDir, "--port-control", "0",
		"--port-base", strconv.Itoa(base), "--services", "storage,pubsub"}
}

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
		if m := controlRE.FindStringSubmatch(stdout.String()); m != nil {
			addr = m[1]
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

// waitFor returns the output once it contains want. runUpAsync returns at the
// banner's control line, before the rest of the banner is written.
func waitFor(t *testing.T, out *syncBuffer, want string) string {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		text := out.String()
		if strings.Contains(text, want) {
			return text
		}
		if time.Now().After(deadline) {
			t.Fatalf("output never contained %q; got:\n%s", want, text)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// The end-to-end path: start, become ready, shut down cleanly on cancellation.
//
// These two tests are not parallel: each creates a kind cluster, and two runUp
// calls in one process are not something a user runs.
func TestUpStartsBecomesReadyAndStopsCleanly(t *testing.T) {
	addr, _, stop := runUpAsync(t, upArgs(t)...)

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

// Up must not claim a service is running before it is. Nothing names the
// services or an address they serve until every component has started, which
// the "ready in" line reports; and the banner then says that running is not
// the same as supported.
//
// It used to require "NOT STARTED", from before up started any service
// (#53); up has printed no such line since the services were started for real,
// and the test did not run in CI to notice (#685).
func TestUpDoesNotClaimServicesAreRunning(t *testing.T) {
	addr, out, stop := runUpAsync(t, upArgs(t)...)
	text := waitFor(t, out, "press Ctrl-C to stop")

	ready := regexp.MustCompile(`(?m)^ready in \d`).FindStringIndex(text)
	if ready == nil {
		t.Fatalf("startup output has no \"ready in\" line; got:\n%s", text)
	}
	for _, claim := range []string{
		"\n  services: ",                // the enabled services
		"\n  endpoints:",                // their addresses
		"export STORAGE_EMULATOR_HOST=", // an SDK told where one is
		"export PUBSUB_EMULATOR_HOST=",
		addr, // the control address
	} {
		i := strings.Index(text, claim)
		switch {
		case i < 0:
			t.Errorf("the banner has no %q; got:\n%s", claim, text)
		case i < ready[0]:
			t.Errorf("%q is printed before startup finished; got:\n%s", claim, text)
		}
	}
	if !strings.Contains(text, "Running is not the same as supported.") {
		t.Errorf("the banner does not say that running is not supported; got:\n%s", text)
	}
	if err := stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
}
