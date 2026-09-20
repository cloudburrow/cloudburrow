package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var addrRE = regexp.MustCompile(`127\.0\.0\.1:\d+`)

// runUpAsync starts runUp in the background and returns its control address
// once it reports listening, plus a stop function.
func runUpAsync(t *testing.T, args ...string) (addr string, out *syncBuffer, stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stdout := &syncBuffer{}
	errCh := make(chan error, 1)

	go func() { errCh <- runUp(ctx, args, stdout, io.Discard) }()

	deadline := time.Now().Add(5 * time.Second)
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
		time.Sleep(10 * time.Millisecond)
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
		case <-time.After(10 * time.Second):
			t.Fatal("runUp did not return after cancellation")
			return nil
		}
	}
}

// syncBuffer is a concurrency-safe buffer: runUp writes from its own goroutine
// while the test reads.
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

// Invalid configuration must fail before anything is bound. The test proves the
// "before" part by holding the control port: if validation ran after binding,
// the error would be a bind failure instead.
func TestUpRejectsInvalidConfigBeforeBinding(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"bad log level", []string{"--log-level", "chatty"}, "logLevel"},
		{"non-loopback without confirmation", []string{"--bind-address", "0.0.0.0"}, "allow-remote"},
		{"duplicate ports", []string{"--port-control", "9500", "--port-run", "9500"}, "duplicates"},
		{"bad mode", []string{"--mode", "sometimes"}, "mode"},
		{"unpinned node image", []string{"--node-image", "kindest/node"}, "mutable target"},
		{"invalid instance name", []string{"--name", "Bad Name"}, "name"},
		{"port out of range", []string{"--port-run", "99999"}, "65535"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			err := runUp(context.Background(), tt.args, &out, io.Discard)
			if err == nil {
				t.Fatal("runUp() = nil, want validation error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want containing %q", err, tt.want)
			}
			if out.Len() != 0 {
				t.Errorf("runUp wrote output despite invalid config: %q", out.String())
			}
		})
	}
}

// The end-to-end path: start, become ready, shut down cleanly on cancellation.
func TestUpStartsBecomesReadyAndStopsCleanly(t *testing.T) {
	t.Parallel()
	addr, _, stop := runUpAsync(t, "--port-control", "0")

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
	_, out, stop := runUpAsync(t, "--port-control", "0")
	text := out.String()
	if !strings.Contains(text, "NOT STARTED") {
		t.Errorf("startup output must state that services are not started; got:\n%s", text)
	}
	if err := stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

// A port already in use must fail with an actionable error naming the port.
func TestUpFailsOnOccupiedControlPort(t *testing.T) {
	t.Parallel()
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	port := blocker.Addr().(*net.TCPAddr).Port

	var out bytes.Buffer
	err = runUp(context.Background(), []string{"--port-control", strconv.Itoa(port)}, &out, io.Discard)
	if err == nil {
		t.Fatal("runUp() = nil, want bind error")
	}
	if !strings.Contains(err.Error(), strconv.Itoa(port)) {
		t.Errorf("error = %v, want it to name port %d", err, port)
	}
}

// stop, reset and delete must fail honestly rather than silently doing nothing.
// A command that reported success while performing no cluster operation would
// be worse than one that says what is missing.
func TestDestructiveCommandsReportUnimplemented(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		fn   func([]string, io.Writer, io.Writer) error
	}{
		{"stop", runStop},
		{"reset", runReset},
		{"delete", runDelete},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			err := tt.fn(nil, &out, io.Discard)
			if !errors.Is(err, errNotImplemented) {
				t.Fatalf("%s() = %v, want errNotImplemented", tt.name, err)
			}
			if !strings.Contains(err.Error(), "#9") {
				t.Errorf("%s() error should name the tracking issue, got: %v", tt.name, err)
			}
		})
	}
}

// Invalid configuration must be rejected before a command claims to act.
func TestDestructiveCommandsValidateFirst(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := runDelete([]string{"--name", "Not Valid"}, &out, io.Discard)
	if errors.Is(err, errNotImplemented) {
		t.Fatal("delete reached the unimplemented path with invalid config; validation must come first")
	}
	if err == nil || !strings.Contains(err.Error(), "name") {
		t.Fatalf("runDelete() = %v, want a validation error naming the field", err)
	}
}

// status must state plainly which services never survive a restart.
func TestStatusReportsNonPersistentServices(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := runStatus([]string{"--mode", "persistent"}, &out, io.Discard); err != nil {
		t.Fatalf("runStatus() = %v", err)
	}
	text := out.String()
	if !strings.Contains(text, "pubsub") || !strings.Contains(text, "never survives") {
		t.Errorf("status must say pubsub never survives restart; got:\n%s", text)
	}
}
