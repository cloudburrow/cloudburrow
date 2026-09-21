package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/identity-wael/cloudburrow/internal/cluster"
	"github.com/identity-wael/cloudburrow/internal/config"
)

// Invalid configuration must be rejected before a command touches a cluster.
func TestDestructiveCommandsValidateFirst(t *testing.T) {
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
			err := tt.fn([]string{"--name", "Not Valid", "--state-dir", t.TempDir()}, &out, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "name") {
				t.Fatalf("%s() = %v, want a validation error naming the field", tt.name, err)
			}
			if out.Len() != 0 {
				t.Errorf("%s() wrote output despite invalid config: %q", tt.name, out.String())
			}
		})
	}
}

// A destructive command must never be constructible against a cluster
// CloudBurrow does not own. The guard lives in internal/cluster; this asserts
// the CLI actually routes through it.
func TestDestructiveCommandsRefuseUnownedCluster(t *testing.T) {
	t.Parallel()
	cfg := config.Default()
	cfg.Name = "dev"
	c, err := newCluster(cfg)
	if err != nil {
		t.Fatalf("newCluster() = %v", err)
	}
	if !strings.HasPrefix(c.Name(), cluster.OwnerPrefix) {
		t.Fatalf("cluster name %q lacks the ownership prefix", c.Name())
	}
}

// status must state plainly which services never survive a restart.
func TestStatusReportsNonPersistentServices(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := runStatus([]string{"--mode", "persistent", "--state-dir", t.TempDir()}, &out, io.Discard); err != nil {
		t.Fatalf("runStatus() = %v", err)
	}
	text := out.String()
	if !strings.Contains(text, "pubsub") || !strings.Contains(text, "never survives") {
		t.Errorf("status must say pubsub never survives restart; got:\n%s", text)
	}
}

// Invalid configuration must fail before anything is bound or created. The
// test proves the "before" part by checking nothing was written to stdout: if
// validation ran later, startup output would already have appeared.
func TestUpRejectsInvalidConfigBeforeActing(t *testing.T) {
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
			args := append([]string{"--state-dir", t.TempDir()}, tt.args...)
			err := runUp(context.Background(), args, &out, io.Discard)
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

// An occupied control port must fail with an actionable error naming the port.
//
// The control server is registered before the cluster, so this fails without
// creating anything — which is also why it needs no Docker.
func TestUpFailsOnOccupiedControlPort(t *testing.T) {
	t.Parallel()
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	port := blocker.Addr().(*net.TCPAddr).Port

	// Every other port is OS-assigned, so the control port is the only
	// contended one. Leaving them at their defaults made this test fail for
	// whichever unrelated process happened to hold one of them — and it then
	// reported the wrong port, which is worse than failing.
	args := append([]string{
		"--state-dir", t.TempDir(),
		"--port-control", strconv.Itoa(port),
	}, osAssignedPorts()...)

	var out bytes.Buffer
	err = runUp(context.Background(), args, &out, io.Discard)
	if err == nil {
		t.Fatal("runUp() = nil, want bind error")
	}
	if !strings.Contains(err.Error(), strconv.Itoa(port)) {
		t.Errorf("error = %v, want it to name port %d", err, port)
	}
}

// osAssignedPorts asks the OS to choose every port but the control port, so a
// test is never affected by what else is running on the machine.
func osAssignedPorts() []string {
	var args []string
	for _, flag := range []string{
		"--port-storage", "--port-pubsub", "--port-tasks", "--port-run",
		"--port-secrets", "--port-metadata", "--port-console", "--port-ingress",
	} {
		args = append(args, flag, "0")
	}
	return args
}

// Unit tests must never touch the developer's real state directory. This
// failed once: runUp opened ~/.cloudburrow during a test and left a lock file
// behind, which then broke unrelated runs.
func TestUnitTestsDoNotTouchTheHomeStateDir(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	realDir := filepath.Join(home, ".cloudburrow")
	before := dirExists(realDir)

	var out bytes.Buffer
	_ = runUp(context.Background(), []string{"--state-dir", t.TempDir(), "--log-level", "nonsense"}, &out, io.Discard)

	if !before && dirExists(realDir) {
		t.Errorf("a unit test created %s; tests must stay inside their temp dirs", realDir)
	}
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}
