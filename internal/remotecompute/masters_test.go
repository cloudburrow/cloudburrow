package remotecompute

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A port held when the wait starts and freed during it is waited for; one
// held throughout is named, with what holds it, when the wait runs out.
func TestWaitPortsFree(t *testing.T) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	go func() { time.Sleep(300 * time.Millisecond); l.Close() }()
	if err := WaitPortsFree(context.Background(), []int{port}, 5*time.Second, 50*time.Millisecond); err != nil {
		t.Fatalf("a port freed during the wait: %v", err)
	}

	held, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	hp := held.Addr().(*net.TCPAddr).Port
	prev := portHolder
	portHolder = func(p int) string {
		if p == hp {
			return "ssh, pid 4242"
		}
		return ""
	}
	defer func() { portHolder = prev }()
	start := time.Now()
	err = WaitPortsFree(context.Background(), []int{hp}, 400*time.Millisecond, 50*time.Millisecond)
	var e *PortsHeldError
	if !errors.As(err, &e) || len(e.Ports) != 1 || e.Ports[0] != hp {
		t.Fatalf("a port held throughout = %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("the wait was not bounded: %s", time.Since(start))
	}
	if !strings.Contains(err.Error(), "held by ssh, pid 4242") || !strings.Contains(err.Error(), "stayed in use") {
		t.Fatalf("the error reads %q", err)
	}
}

// A master whose process is gone is ended; one whose process runs is left
// to it; a recorded pid that is no ssh is never signalled.
func TestCloseOrphanedMasters(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep")
	}
	// A link named ssh runs as "ssh" (a copied system binary would not
	// run on macOS, its signature broken).
	bin := filepath.Join(t.TempDir(), "ssh")
	if err := os.Symlink(sleep, bin); err != nil {
		t.Fatal(err)
	}
	start := func() *exec.Cmd {
		c := exec.Command(bin, "60")
		if err := c.Start(); err != nil {
			t.Skipf("cannot run a copy of sleep: %v", err)
		}
		t.Cleanup(func() { _ = c.Process.Kill(); _, _ = c.Process.Wait() })
		return c
	}
	orphan, kept := start(), start()
	other := exec.Command(sleep, "60")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Process.Kill(); _, _ = other.Process.Wait() })
	exited := make(chan struct{})
	go func() { _, _ = orphan.Process.Wait(); close(exited) }()

	// A finished process's pid stands for a process that is gone.
	gone := exec.Command(sleep, "0")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	dir := mastersDir(state)
	writeOwned := func(pid, owner int) {
		if err := writeMasterPID(dir, pid, "", Target{User: "cb", Host: "127.0.0.1"}); err != nil {
			t.Fatal(err)
		}
		// writeMasterPID records this process; the owner is set here.
		path := filepath.Join(dir, itoa(pid)+".json")
		b, _ := os.ReadFile(path)
		s := strings.Replace(string(b), `"owner":`+itoa(os.Getpid()), `"owner":`+itoa(owner), 1)
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeOwned(orphan.Process.Pid, gone.Process.Pid)
	writeOwned(kept.Process.Pid, other.Process.Pid)
	writeOwned(other.Process.Pid, gone.Process.Pid)

	ended, err := CloseOrphanedMasters(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	if len(ended) != 1 || ended[0] != orphan.Process.Pid {
		t.Fatalf("ended %v; want the orphan %d alone", ended, orphan.Process.Pid)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the orphaned master still runs")
	}
	if !processAlive(kept.Process.Pid) || !processAlive(other.Process.Pid) {
		t.Fatal("a master with a live owner, or a process that is no ssh, was signalled")
	}
	if _, err := os.Stat(filepath.Join(dir, itoa(orphan.Process.Pid)+".json")); !os.IsNotExist(err) {
		t.Fatalf("the orphan's record stays: %v", err)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
