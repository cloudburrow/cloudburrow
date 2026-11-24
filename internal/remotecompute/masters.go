package remotecompute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Every SSH connection's master process is recorded in the instance's
// state (remote/masters/<pid>.json) while it runs. A master forwards the
// host's published ports to this machine's loopback, so a master that
// outlived the process that started it (an `up` killed rather than
// stopped) would go on holding those ports here. A move back to this
// machine closes such masters, then waits until every port the instance's
// containers publish is free, before it starts them here (#2209).

// masterRecord is one master's file.
type masterRecord struct {
	PID    int    `json:"pid"`
	Owner  int    `json:"owner"`
	Ctl    string `json:"ctl"`
	Target string `json:"target"`
}

// mastersDir is where the instance's masters are recorded; "" when the
// session has no state directory.
func mastersDir(stateDir string) string {
	if stateDir == "" {
		return ""
	}
	return filepath.Join(stateDir, "remote", "masters")
}

func writeMasterPID(dir string, pid int, ctl string, t Target) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(masterRecord{PID: pid, Owner: os.Getpid(), Ctl: ctl, Target: t.String()})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, strconv.Itoa(pid)+".json"), b, 0o600)
}

func removeMasterPID(dir string, pid int) {
	_ = os.Remove(filepath.Join(dir, strconv.Itoa(pid)+".json"))
}

// processAlive reports whether pid is a live process.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// isSSH reports whether pid runs ssh, so a recorded pid that the system
// gave to another process since is never signalled.
func isSSH(pid int) bool {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	if err != nil {
		return false
	}
	return filepath.Base(strings.TrimSpace(string(out))) == "ssh"
}

// CloseOrphanedMasters ends every SSH master of the instance at stateDir
// whose own process is gone, and waits for each to exit. It returns the
// pids it ended. A master whose process still runs is left alone: that
// process closes its own.
func CloseOrphanedMasters(ctx context.Context, stateDir string) ([]int, error) {
	dir := mastersDir(stateDir)
	if dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ended []int
	var errs []error
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var r masterRecord
		if json.Unmarshal(b, &r) != nil || r.PID <= 0 {
			_ = os.Remove(path)
			continue
		}
		if !processAlive(r.PID) || !isSSH(r.PID) {
			_ = os.Remove(path)
			continue
		}
		if r.Owner == os.Getpid() || processAlive(r.Owner) {
			continue
		}
		if r.Ctl != "" {
			c, cancel := context.WithTimeout(ctx, 10*time.Second)
			_ = exec.CommandContext(c, "ssh", "-F", "/dev/null", "-S", r.Ctl, "-O", "exit", "cloudburrow-master").Run()
			cancel()
		}
		if processAlive(r.PID) {
			_ = syscall.Kill(r.PID, syscall.SIGTERM)
		}
		deadline := time.Now().Add(masterExitWait)
		for processAlive(r.PID) && isSSH(r.PID) && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
		if processAlive(r.PID) && isSSH(r.PID) {
			_ = syscall.Kill(r.PID, syscall.SIGKILL)
			time.Sleep(200 * time.Millisecond)
		}
		if processAlive(r.PID) && isSSH(r.PID) {
			errs = append(errs, fmt.Errorf("the SSH connection to %s (pid %d) did not exit", r.Target, r.PID))
			continue
		}
		_ = os.Remove(path)
		ended = append(ended, r.PID)
	}
	return ended, errors.Join(errs...)
}

// PortsHeldError names the ports of this machine that stayed taken.
type PortsHeldError struct {
	Ports   []int
	Holders map[int]string
	Waited  time.Duration
}

func (e *PortsHeldError) Error() string {
	parts := make([]string, 0, len(e.Ports))
	for _, p := range e.Ports {
		s := strconv.Itoa(p)
		if h := e.Holders[p]; h != "" {
			s += " (held by " + h + ")"
		}
		parts = append(parts, s)
	}
	word := "port"
	if len(e.Ports) > 1 {
		word = "ports"
	}
	return fmt.Sprintf("this machine's %s %s stayed in use for %s after the connection to the VM closed, so the instance cannot publish them here",
		word, strings.Join(parts, ", "), e.Waited.Round(time.Second))
}

// portFree reports whether this machine's TCP port can be listened on at
// both the loopback and the wildcard address, as Docker publishes it.
func portFree(port int) bool {
	for _, host := range []string{"127.0.0.1", "0.0.0.0"} {
		l, err := net.Listen("tcp4", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			return false
		}
		_ = l.Close()
	}
	return true
}

// portFreeCheck is portFree; tests replace it.
var portFreeCheck = portFree

// portHolder names the process listening on port, when lsof can say.
var portHolder = func(port int) string {
	out, err := exec.Command("lsof", "-nP", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN", "-Fpc").Output()
	if err != nil {
		return ""
	}
	var pid, cmd string
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "p") && pid == "":
			pid = line[1:]
		case strings.HasPrefix(line, "c") && cmd == "":
			cmd = line[1:]
		}
	}
	if pid == "" {
		return ""
	}
	if cmd == "" {
		return "pid " + pid
	}
	return cmd + ", pid " + pid
}

// WaitPortsFree waits until each of ports is free on this machine, for at
// most wait, checking every interval. It returns a *PortsHeldError naming
// the ports still taken, and what holds each, when the wait runs out.
func WaitPortsFree(ctx context.Context, ports []int, wait, interval time.Duration) error {
	if interval <= 0 {
		interval = 200 * time.Millisecond
	}
	start := time.Now()
	pending := append([]int(nil), ports...)
	sort.Ints(pending)
	for {
		var still []int
		for _, p := range pending {
			if !portFreeCheck(p) {
				still = append(still, p)
			}
		}
		if len(still) == 0 {
			return nil
		}
		pending = still
		if time.Since(start) >= wait {
			e := &PortsHeldError{Ports: still, Holders: map[int]string{}, Waited: time.Since(start)}
			for _, p := range still {
				if h := portHolder(p); h != "" {
					e.Holders[p] = h
				}
			}
			return e
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}
