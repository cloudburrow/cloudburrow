package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

// A move back to this machine waits, bounded, for each port the VM's
// connection forwarded here to be free: one freed during the wait lets the
// move go on; one still held fails it with the port named, which rolls the
// move back to the VM.
func TestMoveBackWaitsForTheForwardedPorts(t *testing.T) {
	cfg := config.Config{Name: "mvports", StateDir: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(cfg.StateDir, cfg.Name), 0o700); err != nil {
		t.Fatal(err)
	}
	src := moveEnd{machine: "lab-aws"}
	var steps []string
	step := func(s, msg string) { steps = append(steps, s+": "+msg) }

	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	go func() { time.Sleep(300 * time.Millisecond); l.Close() }()
	if err := freeLocalPorts(context.Background(), cfg, src, []int{port}, step); err != nil {
		t.Fatalf("a port freed during the wait: %v", err)
	}
	if !strings.Contains(strings.Join(steps, "\n"), "are free on this machine") {
		t.Errorf("the steps read %v", steps)
	}

	held, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	prev := movePortWait
	movePortWait = 500 * time.Millisecond
	t.Cleanup(func() { movePortWait = prev })
	hp := held.Addr().(*net.TCPAddr).Port
	err = freeLocalPorts(context.Background(), cfg, src, []int{hp}, step)
	if err == nil || !strings.Contains(err.Error(), "stayed in use") || !strings.Contains(err.Error(), "the instance stays on lab-aws") ||
		!strings.Contains(err.Error(), strconv.Itoa(hp)) {
		t.Fatalf("a port held throughout = %v", err)
	}
}
