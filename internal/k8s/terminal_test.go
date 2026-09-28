//go:build unix

package k8s

import (
	"errors"
	"io"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

type terminalInvoker struct {
	recorder
	cols, rows uint16
	args       []string
}

func (f *terminalInvoker) StartTerminal(cols, rows uint16, args ...string) (TTY, error) {
	f.cols, f.rows, f.args = cols, rows, args
	return nil, nil
}

// The exec runs against the Runner's cluster, context and namespace, like
// every other call, and the size it was asked for is the size it starts at.
func TestTerminalRunsWithTheGlobalFlagsAndSize(t *testing.T) {
	t.Parallel()
	inv := &terminalInvoker{}
	r := NewWith(inv, "/k/config", "kind-x", "cloudburrow")
	if _, err := r.Terminal(120, 40, "exec", "-it", "pod", "--", "bash"); err != nil {
		t.Fatal(err)
	}
	want := []string{"--kubeconfig", "/k/config", "--context", "kind-x", "-n", "cloudburrow", "exec", "-it", "pod", "--", "bash"}
	if !reflect.DeepEqual(inv.args, want) || inv.cols != 120 || inv.rows != 40 {
		t.Errorf("started %v at %dx%d; want %v at 120x40", inv.args, inv.cols, inv.rows, want)
	}
}

func TestTerminalNeedsATerminalStarter(t *testing.T) {
	t.Parallel()
	r := NewWith(&recorder{}, "", "", "ns")
	if _, err := r.Terminal(80, 24, "exec"); !errors.Is(err, ErrCannotTerminal) {
		t.Errorf("err = %v, want ErrCannotTerminal", err)
	}
}

// The program really is on a terminal of the requested size, which is what
// `kubectl exec -t` checks before it allocates one in the container, and a
// resize changes the size the program reads.
func TestStartOnPTYGivesTheProgramATerminal(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("stty"); err != nil {
		t.Skip("no stty")
	}
	tty, err := startOnPTY(exec.Command("sh", "-c", "stty size; read line; stty size"), 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer tty.Close()
	read := func(want string) {
		t.Helper()
		var got strings.Builder
		buf := make([]byte, 256)
		deadline := time.Now().Add(10 * time.Second)
		for !strings.Contains(got.String(), want) && time.Now().Before(deadline) {
			n, err := tty.Read(buf)
			got.Write(buf[:n])
			if err != nil {
				break
			}
		}
		if !strings.Contains(got.String(), want) {
			t.Fatalf("read %q; want %q", got.String(), want)
		}
	}
	read("30 100")
	if err := tty.Resize(90, 20); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(tty, "go\n"); err != nil {
		t.Fatal(err)
	}
	read("20 90")
	if err := tty.Wait(); err != nil {
		t.Errorf("wait: %v", err)
	}
}

// Close ends a program that is still running rather than waiting for it.
func TestTerminalCloseEndsTheProgram(t *testing.T) {
	t.Parallel()
	tty, err := startOnPTY(exec.Command("sleep", "60"), 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = tty.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not end the program")
	}
}
