package remotecompute

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// transport is the SSH connection as the supervisor drives it. The real one
// is the system ssh with a ControlMaster socket; the supervisor's unit tests
// inject a fake that drops and refuses connections on cue.
type transport interface {
	// Authorize makes sure the remote accepts CloudBurrow's key, installing
	// its public half with the user's own ssh agent and config when it does
	// not yet. It reports whether it installed the key.
	Authorize(ctx context.Context) (installed bool, err error)
	// Master starts the one connection, carrying the initial forwards, and
	// returns once it accepts control commands. done receives the
	// connection's end (nil after a requested exit) and is then closed.
	Master(ctx context.Context, initial []string) (done <-chan error, err error)
	// Control runs ssh -O op with forward arguments (-L/-R spec) over the
	// connection; stdout is what ssh prints, a remote forward's allocated
	// port for a listen port of 0.
	Control(ctx context.Context, op string, args ...string) (string, error)
	// Run runs a shell command on the remote over the connection.
	Run(ctx context.Context, command string) (string, error)
}

// sshTransport is the system ssh.
type sshTransport struct {
	ssh        string
	target     Target
	key        Key
	ctl        string // ControlPath
	knownHosts string
	config     string // -F, when set
	// agent serves a stored key (Options.Key); key is unused then.
	agent *keyAgent
	// hostAlias names the host in a known_hosts that pins its one key
	// (Options.HostKey); empty is trust on first use.
	hostAlias string

	mu  sync.Mutex
	cmd *exec.Cmd
	// exited is closed when the master process has exited, and with it
	// every listener its forwards held.
	exited chan struct{}
	// pidDir, when set, records the master's process while it runs, so a
	// master that outlived its process (killed) is found and closed.
	pidDir string
}

// common are the options every ssh invocation carries.
func (s *sshTransport) common() []string {
	var a []string
	if s.config != "" {
		a = append(a, "-F", s.config)
	}
	if s.target.Port != 0 {
		a = append(a, "-p", strconv.Itoa(s.target.Port))
	}
	a = append(a, "-o", "BatchMode=yes", "-o", "ConnectTimeout=15", "-o", "LogLevel=ERROR")
	if s.hostAlias != "" {
		// The provisioner's host key and no other.
		return append(a, "-o", "StrictHostKeyChecking=yes", "-o", "HostKeyAlias="+s.hostAlias,
			"-o", "UserKnownHostsFile="+s.knownHosts, "-o", "GlobalKnownHostsFile=/dev/null")
	}
	// Trust on first use, recorded beside the key; the user's own
	// known_hosts is read too, and a changed host key is refused.
	return append(a, "-o", "StrictHostKeyChecking=accept-new", "-o", "UserKnownHostsFile="+s.knownHosts+" ~/.ssh/known_hosts")
}

// own are the options that sign in with CloudBurrow's key and no other.
func (s *sshTransport) own() []string {
	if s.agent != nil {
		return []string{"-o", "IdentitiesOnly=yes", "-o", "IdentityFile=" + s.agent.pubFile, "-o", "IdentityAgent=" + s.agent.sock,
			"-o", "PasswordAuthentication=no", "-o", "KbdInteractiveAuthentication=no"}
	}
	return []string{"-o", "IdentitiesOnly=yes", "-o", "IdentityFile=" + s.key.Path, "-o", "PasswordAuthentication=no",
		"-o", "KbdInteractiveAuthentication=no"}
}

func (s *sshTransport) run(ctx context.Context, stdin io.Reader, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, s.ssh, args...)
	cmd.Stdin = stdin
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(errOut.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		return out.String(), fmt.Errorf("%w: %s", err, msg)
	}
	return out.String(), nil
}

// installScript appends the key to authorized_keys once.
func installScript(pub string) string {
	q := shellQuote(pub)
	return "umask 077; mkdir -p ~/.ssh && touch ~/.ssh/authorized_keys && " +
		"(grep -qxF " + q + " ~/.ssh/authorized_keys || printf '%s\\n' " + q + " >> ~/.ssh/authorized_keys)"
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func (s *sshTransport) Authorize(ctx context.Context) (bool, error) {
	if s.agent != nil {
		// A stored key is in place already (the VM's cloud-init put it
		// there): signing in is all there is to check.
		args := append(append(s.common(), s.own()...), s.target.destination(), "true")
		if _, err := s.run(ctx, nil, args...); err != nil {
			if msg := err.Error(); strings.Contains(msg, "Host key verification failed") || strings.Contains(msg, "IDENTIFICATION HAS CHANGED") {
				return false, fmt.Errorf("%s answered with a host key other than the one CloudBurrow made for it, so CloudBurrow did not sign in: "+
					"the address may now be another machine's", s.target)
			}
			return false, fmt.Errorf("could not sign in to %s with CloudBurrow's key: %w", s.target, err)
		}
		return false, nil
	}
	// The key is installed once, and remembered beside it: whether the
	// host accepts it cannot be asked of ssh alone, since keys the user's
	// config names are offered too.
	marker := s.key.Path + ".authorized"
	if _, err := os.Stat(marker); err == nil {
		args := append(append(s.common(), s.own()...), s.target.destination(), "true")
		_, err := s.run(ctx, nil, args...)
		if err == nil {
			return false, nil
		}
		if !strings.Contains(err.Error(), "Permission denied") {
			return false, fmt.Errorf("could not reach %s over SSH: %w", s.target, err)
		}
		// Removed from authorized_keys since: installed again below.
	}
	// The user's own agent and config sign in, once, and put CloudBurrow's
	// public key in place. From then on CloudBurrow's key is offered first.
	args := append(s.common(), s.target.destination(), installScript(s.key.Public))
	if _, err := s.run(ctx, nil, args...); err != nil {
		if !strings.Contains(err.Error(), "Permission denied") {
			return false, fmt.Errorf("could not reach %s over SSH: %w", s.target, err)
		}
		return false, fmt.Errorf("%s does not accept CloudBurrow's key yet, and your ssh agent and config could not sign in to add it (%v). "+
			"Add this line to the remote account's ~/.ssh/authorized_keys, then retry:\n  %s", s.target, err, s.key.Public)
	}
	args = append(append(s.common(), s.own()...), s.target.destination(), "true")
	if _, err := s.run(ctx, nil, args...); err != nil {
		return true, fmt.Errorf("%s still refuses CloudBurrow's key after it was added to ~/.ssh/authorized_keys: %w", s.target, err)
	}
	if err := os.WriteFile(marker, []byte(s.target.String()+"\n"), 0o600); err != nil {
		return true, err
	}
	return true, nil
}

func (s *sshTransport) Master(ctx context.Context, initial []string) (<-chan error, error) {
	_ = os.Remove(s.ctl)
	args := append(s.common(), s.own()...)
	args = append(args, "-M", "-S", s.ctl, "-N", "-T",
		"-o", "ControlPersist=no",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=5",
		"-o", "ServerAliveCountMax=3",
		"-o", "StreamLocalBindUnlink=yes",
		"-o", "StreamLocalBindMask=0177",
	)
	args = append(args, initial...)
	args = append(args, s.target.destination())
	cmd := exec.Command(s.ssh, args...)
	var errOut lockedBuffer
	cmd.Stderr = &errOut
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	exited := make(chan struct{})
	s.mu.Lock()
	s.cmd, s.exited = cmd, exited
	s.mu.Unlock()
	if s.pidDir != "" {
		_ = writeMasterPID(s.pidDir, cmd.Process.Pid, s.ctl, s.target)
	}
	go func() {
		err := cmd.Wait()
		if err != nil {
			if msg := strings.TrimSpace(errOut.String()); msg != "" {
				err = fmt.Errorf("%w: %s", err, msg)
			}
		}
		if s.pidDir != "" {
			removeMasterPID(s.pidDir, cmd.Process.Pid)
		}
		done <- err
		close(done)
		close(exited)
	}()
	deadline := time.Now().Add(30 * time.Second)
	for {
		check := exec.CommandContext(ctx, s.ssh, "-S", s.ctl, "-O", "check", s.target.destination())
		if check.Run() == nil {
			return done, nil
		}
		select {
		case <-exited:
			err := <-done
			if err == nil {
				err = errors.New("ssh exited")
			}
			return nil, err
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			return nil, fmt.Errorf("the SSH connection to %s did not come up in 30s: %s", s.target, strings.TrimSpace(errOut.String()))
		}
	}
}

func (s *sshTransport) Control(ctx context.Context, op string, args ...string) (string, error) {
	a := append([]string{"-S", s.ctl, "-O", op}, args...)
	a = append(a, s.target.destination())
	out, err := s.run(ctx, nil, a...)
	if op == "exit" {
		// The master may be gone already; exit then has nothing to do.
		// Otherwise ssh -O exit returns as the master starts to exit, not
		// once it has: its forwards' listeners are closed only then, so
		// the master is waited for (and killed if it lingers). A port it
		// held is free when Close returns.
		s.mu.Lock()
		cmd, exited := s.cmd, s.exited
		s.mu.Unlock()
		if cmd != nil && cmd.Process != nil && exited != nil {
			if err != nil {
				_ = cmd.Process.Kill()
			}
			select {
			case <-exited:
			case <-time.After(masterExitWait):
				_ = cmd.Process.Kill()
				select {
				case <-exited:
				case <-time.After(5 * time.Second):
				}
			}
		}
		return out, nil
	}
	return out, err
}

func (s *sshTransport) Run(ctx context.Context, command string) (string, error) {
	a := append(append(s.common(), s.own()...), "-S", s.ctl, s.target.destination(), command)
	return s.run(ctx, nil, a...)
}

// masterExitWait bounds the wait for the master to exit after -O exit.
const masterExitWait = 10 * time.Second

// lockedBuffer is a bytes.Buffer safe for exec's writer goroutine and a
// reader.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.b.Len() > 64<<10 {
		l.b.Reset()
	}
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
