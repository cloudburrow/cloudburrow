package k8s

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"
)

// TTY is a kubectl running on a pseudo-terminal: what `kubectl exec -it`
// needs to allocate a terminal in the container (#781).
//
// Reading returns what the remote program wrote to its terminal, writing
// types into it. Close ends kubectl, which ends the remote side's session
// with it.
type TTY interface {
	io.ReadWriteCloser
	// Resize sets the terminal's size. kubectl sees the change as a
	// SIGWINCH on its controlling terminal and forwards it to the container.
	Resize(cols, rows uint16) error
	// Wait blocks until kubectl exits and returns how it exited.
	Wait() error
}

// TerminalStarter is an Invoker that can start kubectl on a
// pseudo-terminal. Subprocess is one; a test Invoker that is not makes
// Terminal fail with ErrCannotTerminal.
type TerminalStarter interface {
	StartTerminal(cols, rows uint16, args ...string) (TTY, error)
}

// ErrCannotTerminal means the Runner's Invoker is not a TerminalStarter.
var ErrCannotTerminal = errors.New("k8s: the invoker cannot start kubectl on a terminal")

// Terminal starts kubectl [globals] ARGS on a pseudo-terminal of cols by
// rows, for an interactive exec: `kubectl exec -it POD -- bash` refuses to
// allocate a terminal in the container unless its own stdin is one. Like
// PortForward it takes no context: the terminal lives until Close. A
// failure to start is the Invoker's error, unclassified.
func (r *Runner) Terminal(cols, rows uint16, args ...string) (TTY, error) {
	s, ok := r.inv.(TerminalStarter)
	if !ok {
		return nil, ErrCannotTerminal
	}
	return s.StartTerminal(cols, rows, r.global(args...)...)
}

// StartTerminal starts kubectl on a new pseudo-terminal.
func (s Subprocess) StartTerminal(cols, rows uint16, args ...string) (TTY, error) {
	return startOnPTY(s.withEnv(exec.CommandContext(context.Background(), "kubectl", args...)), cols, rows)
}

// startOnPTY starts cmd as the session leader of a new pseudo-terminal,
// which is its controlling terminal, so a resize reaches it as SIGWINCH.
func startOnPTY(cmd *exec.Cmd, cols, rows uint16) (TTY, error) {
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		return nil, err
	}
	t := &ptyProcess{f: f, cmd: cmd, done: make(chan struct{})}
	go func() {
		t.err = cmd.Wait()
		close(t.done)
	}()
	return t, nil
}

type ptyProcess struct {
	f    *os.File
	cmd  *exec.Cmd
	once sync.Once
	done chan struct{}
	err  error
}

func (p *ptyProcess) Read(b []byte) (int, error) {
	n, err := p.f.Read(b)
	// Once the program exits, Linux reports EIO on the terminal's master
	// rather than EOF; to a reader both mean there is no more output.
	if err != nil && !errors.Is(err, io.EOF) {
		select {
		case <-p.done:
			return n, io.EOF
		default:
			if errors.Is(err, os.ErrClosed) {
				return n, io.EOF
			}
		}
	}
	return n, err
}

func (p *ptyProcess) Write(b []byte) (int, error) { return p.f.Write(b) }

func (p *ptyProcess) Resize(cols, rows uint16) error {
	return pty.Setsize(p.f, &pty.Winsize{Cols: cols, Rows: rows})
}

func (p *ptyProcess) Wait() error {
	<-p.done
	return p.err
}

// Close kills kubectl, if it is still running, and releases the terminal.
func (p *ptyProcess) Close() error {
	p.once.Do(func() {
		select {
		case <-p.done:
		default:
			_ = p.cmd.Process.Kill()
		}
		<-p.done
		_ = p.f.Close()
	})
	return nil
}
