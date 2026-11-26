package main

import (
	"fmt"
	"strings"
	"testing"
)

// TestTunnelLogSummarizesTheBoot (#2250): while the guest boots, an SSH
// tunnel's failed launches are counted, not written one by one; up.log
// gets the last when it comes up, the first and the last when it gives
// up, and every line once it is up.
func TestTunnelLogSummarizesTheBoot(t *testing.T) {
	var got []string
	logf := func(f string, a ...any) { got = append(got, fmt.Sprintf(f, a...)) }
	l := &tunnelLog{logf: logf}
	for i := 1; i <= 3; i++ {
		l.Logf("launch %d refused", i)
	}
	if len(got) != 0 {
		t.Fatalf("written while booting: %q", got)
	}
	l.up("web")
	l.Logf("tunnel re-established")
	want := []string{"instance web's SSH tunnel is up after 3 lines while the guest booted; the last: launch 3 refused", "tunnel re-established"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("up: %q, want %q", got, want)
	}
	got = nil
	f := &tunnelLog{logf: logf}
	f.Logf("first")
	f.Logf("second")
	f.failed("db")
	if len(got) != 2 || !strings.HasSuffix(got[0], "2 lines; the first: first") || !strings.HasSuffix(got[1], "the last: second") {
		t.Errorf("failed: %q", got)
	}
}

// TestBootFailureHintNamesInotify: a guest whose systemd exited for want
// of inotify instances (CI's run shard, main run 45753347963) gets a line
// naming the host limit; any other serial console none.
func TestBootFailureHintNamesInotify(t *testing.T) {
	serial := "Welcome to Debian GNU/Linux 12 (bookworm)!\nFailed to create control group inotify object: Too many open files\n" +
		"Failed to allocate manager object: Too many open files\n[!!!!!!] Failed to allocate manager object.\nExiting PID 1..."
	if h := bootFailureHint(serial); !strings.Contains(h, "fs.inotify.max_user_instances") {
		t.Errorf("hint %q", h)
	}
	if h := bootFailureHint("Welcome to Debian GNU/Linux 12 (bookworm)!\nReached target multi-user.target"); h != "" {
		t.Errorf("hint for a normal boot: %q", h)
	}
}
