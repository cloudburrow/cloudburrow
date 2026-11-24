package remotecompute

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Stage is a step of connecting, or a change of the connection's state.
type Stage string

// The stages an Event reports, in the order a first Connect goes through
// them; disconnected, reconnecting and closed come later.
const (
	StageKey           Stage = "key"
	StageConnecting    Stage = "connecting"
	StageAuthorizing   Stage = "authorizing"
	StagePrerequisites Stage = "prerequisites"
	StageConnected     Stage = "connected"
	StageForward       Stage = "forward"
	StageDisconnected  Stage = "disconnected"
	StageReconnecting  Stage = "reconnecting"
	StageFailed        Stage = "failed"
	StageClosed        Stage = "closed"
)

// Event is one progress or state change, in words a console can show as
// they are.
type Event struct {
	Time    time.Time `json:"time"`
	Stage   Stage     `json:"stage"`
	Message string    `json:"message"`
	Error   string    `json:"error,omitempty"`
}

// Options configure Connect.
type Options struct {
	Target Target
	// StateDir is the instance's state directory: CloudBurrow's key for the
	// remote and its known_hosts live under StateDir/remote/<host>/.
	StateDir string
	// Instance names the key's comment, so the remote's authorized_keys says
	// whose it is.
	Instance string
	// Progress receives every Event, synchronously; nil drops them.
	Progress func(Event)
	// SSH is the ssh binary; empty is "ssh" on PATH.
	SSH string
	// SSHConfig, when set, is passed as ssh -F: a config file used in place
	// of the user's ~/.ssh/config.
	SSHConfig string
	// RemoteDockerSocket is the Docker socket on the host; empty is
	// /var/run/docker.sock.
	RemoteDockerSocket string
	// SkipPrerequisites leaves the host's checks to the caller (doctor
	// reports them itself).
	SkipPrerequisites bool

	// transport replaces the system ssh (tests).
	transport transport
	// backoff is the delay before reconnect attempt n (tests).
	backoff func(n int) time.Duration
}

// ErrClosed is returned by calls on a closed Session.
var ErrClosed = errors.New("remote compute session is closed")

// Status is the connection as it is now.
type Status struct {
	Target     string    `json:"target"`
	Connected  bool      `json:"connected"`
	Reconnects int       `json:"reconnects"`
	Since      time.Time `json:"since,omitempty"`
	LastError  string    `json:"lastError,omitempty"`
	Host       HostInfo  `json:"host"`
	Forwards   int       `json:"forwards"`
	Events     []Event   `json:"events,omitempty"`
}

// forward is one forward the connection carries.
type forward struct {
	reverse bool
	bind    string // -L: the local listen; -R: the remote listen
	target  string // -L: the remote target; -R: the local target
	// asked is the listen a reverse forward was first asked for, port 0,
	// when the server chose the port bind now names: the connection that
	// allocated it knows the forward by that.
	asked string
}

func (f forward) spec() []string {
	flag := "-L"
	if f.reverse {
		flag = "-R"
	}
	return []string{flag, f.bind + ":" + f.target}
}

// Session is one supervised connection to a remote compute host.
type Session struct {
	opts   Options
	tr     transport
	runDir string
	sock   string

	mu         sync.Mutex
	forwards   []*forward
	connected  bool
	since      time.Time
	reconnects int
	lastErr    string
	host       HostInfo
	events     []Event
	closed     bool
	done       <-chan error
	kick       chan struct{}
	stop       context.CancelFunc
	loopDone   chan struct{}
}

// shortDir is a directory for the Unix sockets whose path is short enough
// for sun_path (104 bytes on macOS): the ControlMaster socket and the
// forwarded Docker socket.
func shortDir(seed string) (string, error) {
	sum := sha256.Sum256([]byte(seed + "\x00" + strconv.Itoa(os.Getpid()) + "\x00" + strconv.FormatInt(time.Now().UnixNano(), 10)))
	base := os.TempDir()
	if len(base) > 48 {
		base = "/tmp"
	}
	dir := filepath.Join(base, "cb-rc-"+hex.EncodeToString(sum[:5]))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, os.Chmod(dir, 0o700)
}

// Connect prepares CloudBurrow's key, signs in (installing the key once
// with the user's own agent and config), checks the host's prerequisites,
// and opens the supervised connection with the Docker socket forwarded.
// It returns once Docker on the host answers through it.
func Connect(ctx context.Context, opts Options) (*Session, error) {
	if opts.Target.Host == "" {
		return nil, fmt.Errorf("%w: no host", ErrBadTarget)
	}
	if opts.RemoteDockerSocket == "" {
		opts.RemoteDockerSocket = "/var/run/docker.sock"
	}
	if opts.SSH == "" {
		opts.SSH = "ssh"
	}
	if opts.backoff == nil {
		opts.backoff = defaultBackoff
	}
	s := &Session{opts: opts, kick: make(chan struct{}, 1)}
	s.emit(StageKey, "Preparing CloudBurrow's SSH key for "+opts.Target.String(), nil)
	key, err := EnsureKey(opts.StateDir, opts.Target, "cloudburrow-"+opts.Instance)
	if err != nil {
		s.emit(StageFailed, "CloudBurrow's SSH key could not be prepared", err)
		return nil, err
	}
	dir, err := shortDir(opts.StateDir + opts.Target.String())
	if err != nil {
		return nil, err
	}
	s.runDir, s.sock = dir, filepath.Join(dir, "docker.sock")
	s.tr = opts.transport
	if s.tr == nil {
		s.tr = &sshTransport{ssh: opts.SSH, target: opts.Target, key: key, ctl: filepath.Join(dir, "ctl"),
			knownHosts: filepath.Join(remoteDir(opts.StateDir, opts.Target), "known_hosts"), config: opts.SSHConfig}
	}
	fail := func(msg string, err error) (*Session, error) {
		s.emit(StageFailed, msg, err)
		_ = os.RemoveAll(s.runDir)
		return nil, err
	}
	s.emit(StageConnecting, "Connecting to "+opts.Target.String(), nil)
	installed, err := s.tr.Authorize(ctx)
	if err != nil {
		return fail("Could not sign in to "+opts.Target.String(), err)
	}
	if installed {
		s.emit(StageAuthorizing, "Added CloudBurrow's own key to the remote account; your keys are not used again", nil)
	}
	s.forwards = []*forward{{bind: s.sock, target: opts.RemoteDockerSocket}}
	done, err := s.tr.Master(ctx, s.forwards[0].spec())
	if err != nil {
		return fail("The SSH connection to "+opts.Target.String()+" did not come up", err)
	}
	s.mu.Lock()
	s.done, s.connected, s.since = done, true, time.Now()
	s.mu.Unlock()
	if !opts.SkipPrerequisites {
		s.emit(StagePrerequisites, "Checking the host: Docker, CPU, memory and disk", nil)
		info, problems := s.Prerequisites(ctx)
		s.mu.Lock()
		s.host = info
		s.mu.Unlock()
		if len(problems) > 0 {
			_, _ = s.tr.Control(context.Background(), "exit")
			return fail("The host is not ready for CloudBurrow", errors.New(strings.Join(problems, "; ")))
		}
	}
	loopCtx, cancel := context.WithCancel(context.Background())
	s.stop, s.loopDone = cancel, make(chan struct{})
	go s.supervise(loopCtx)
	s.emit(StageConnected, "Connected to "+opts.Target.String()+s.hostSummary(), nil)
	return s, nil
}

func (s *Session) hostSummary() string {
	s.mu.Lock()
	h := s.host
	s.mu.Unlock()
	if h.CPUs == 0 {
		return ""
	}
	return fmt.Sprintf(" (%s)", h.Size())
}

func defaultBackoff(n int) time.Duration {
	d := time.Second << min(n, 5)
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

// supervise reconnects whenever the connection ends, until Close.
func (s *Session) supervise(ctx context.Context) {
	defer close(s.loopDone)
	for {
		s.mu.Lock()
		done := s.done
		s.mu.Unlock()
		var cause error
		select {
		case <-ctx.Done():
			return
		case err, ok := <-done:
			if !ok {
				err = errors.New("connection closed")
			}
			if err == nil {
				err = errors.New("the SSH connection ended")
			}
			cause = err
		case <-s.kick:
			_, _ = s.tr.Control(context.Background(), "exit")
			// Drain the end of the old connection before the new one.
			select {
			case <-done:
			case <-time.After(10 * time.Second):
			case <-ctx.Done():
				return
			}
			cause = errors.New("reconnect requested")
		}
		s.mu.Lock()
		s.connected, s.lastErr = false, cause.Error()
		s.mu.Unlock()
		s.emit(StageDisconnected, "Lost the connection to "+s.opts.Target.String(), cause)
		for attempt := 0; ; attempt++ {
			select {
			case <-ctx.Done():
				return
			case <-time.After(s.opts.backoff(attempt)):
			}
			s.emit(StageReconnecting, fmt.Sprintf("Reconnecting to %s (attempt %d)", s.opts.Target, attempt+1), nil)
			if err := s.connectOnce(ctx); err != nil {
				s.mu.Lock()
				s.lastErr = err.Error()
				s.mu.Unlock()
				continue
			}
			break
		}
	}
}

// connectOnce opens a new connection and replays every forward on it.
func (s *Session) connectOnce(ctx context.Context) error {
	s.mu.Lock()
	specs := make([][]string, len(s.forwards))
	for i, f := range s.forwards {
		specs[i] = f.spec()
	}
	s.mu.Unlock()
	done, err := s.tr.Master(ctx, specs[0])
	if err != nil {
		return err
	}
	for _, spec := range specs[1:] {
		if _, err := s.tr.Control(ctx, "forward", spec...); err != nil {
			// One forward the new connection cannot carry (its port taken
			// meanwhile) is said, and the rest are still carried.
			s.emit(StageForward, "Could not restore the forward "+strings.Join(spec, " "), err)
		}
	}
	s.mu.Lock()
	s.done, s.connected, s.since = done, true, time.Now()
	s.reconnects++
	s.lastErr = ""
	s.mu.Unlock()
	s.emit(StageConnected, "Reconnected to "+s.opts.Target.String(), nil)
	return nil
}

func (s *Session) emit(stage Stage, msg string, err error) {
	e := Event{Time: time.Now(), Stage: stage, Message: msg}
	if err != nil {
		e.Error = err.Error()
	}
	s.mu.Lock()
	s.events = append(s.events, e)
	if len(s.events) > 50 {
		s.events = s.events[len(s.events)-50:]
	}
	s.mu.Unlock()
	if s.opts.Progress != nil {
		s.opts.Progress(e)
	}
}

// DockerHost is the DOCKER_HOST value that reaches the host's Docker
// through the connection.
func (s *Session) DockerHost() string { return "unix://" + s.sock }

// Target is the host this session reaches.
func (s *Session) Target() Target { return s.opts.Target }

// Status is the connection's state now.
func (s *Session) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Status{Target: s.opts.Target.String(), Connected: s.connected, Reconnects: s.reconnects, Since: s.since,
		LastError: s.lastErr, Host: s.host, Forwards: len(s.forwards), Events: append([]Event(nil), s.events...)}
}

// Reconnect drops the connection and waits until a new one carries every
// forward again, or ctx ends.
func (s *Session) Reconnect(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	before := s.reconnects
	s.mu.Unlock()
	select {
	case s.kick <- struct{}{}:
	default:
	}
	for {
		s.mu.Lock()
		ok := s.connected && s.reconnects > before
		s.mu.Unlock()
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Close ends the connection and every forward. It is safe to call twice.
func (s *Session) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	if s.stop != nil {
		s.stop()
		<-s.loopDone
	}
	_, _ = s.tr.Control(context.Background(), "exit")
	s.mu.Lock()
	s.connected = false
	s.mu.Unlock()
	s.emit(StageClosed, "Disconnected from "+s.opts.Target.String(), nil)
	return os.RemoveAll(s.runDir)
}

func (s *Session) add(ctx context.Context, f *forward) (string, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return "", ErrClosed
	}
	s.mu.Unlock()
	out, err := s.tr.Control(ctx, "forward", f.spec()...)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.forwards = append(s.forwards, f)
	s.mu.Unlock()
	return strings.TrimSpace(out), nil
}

func (s *Session) remove(ctx context.Context, f *forward) error {
	s.mu.Lock()
	for i, g := range s.forwards {
		if g == f {
			s.forwards = append(s.forwards[:i], s.forwards[i+1:]...)
			break
		}
	}
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nil
	}
	_, err := s.tr.Control(ctx, "cancel", f.spec()...)
	if err != nil && f.asked != "" {
		g := *f
		g.bind = f.asked
		_, err = s.tr.Control(ctx, "cancel", g.spec()...)
	}
	return err
}

// Forward listens on local (host:port on this machine) and carries each
// connection to remote (host:port as the remote host sees it). cancel
// removes it.
func (s *Session) Forward(ctx context.Context, local, remote string) (cancel func() error, err error) {
	f := &forward{bind: local, target: remote}
	if _, err := s.add(ctx, f); err != nil {
		return nil, fmt.Errorf("forward %s to %s on %s: %w", local, remote, s.opts.Target, err)
	}
	return func() error { return s.remove(context.Background(), f) }, nil
}

// Reverse listens on listen (an address on the remote host, such as a
// Docker network's gateway, with port 0 for any free port) and carries
// each connection to target on this machine. It returns the address bound.
func (s *Session) Reverse(ctx context.Context, listen, target string) (bound string, cancel func() error, err error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", nil, err
	}
	f := &forward{reverse: true, bind: listen, target: target}
	out, err := s.add(ctx, f)
	if err != nil {
		return "", nil, fmt.Errorf("listen on %s on %s for %s: %w", listen, s.opts.Target, target, err)
	}
	if port == "0" {
		// ssh prints the port the server chose. The forward is kept under
		// that port so a reconnect asks for the same one again: the
		// containers already hold it.
		p, perr := strconv.Atoi(strings.TrimSpace(firstLine(out)))
		if perr != nil || p <= 0 {
			_ = s.remove(context.Background(), f)
			return "", nil, fmt.Errorf("listen on %s on %s: ssh reported no allocated port (%q)", listen, s.opts.Target, out)
		}
		port = strconv.Itoa(p)
		s.mu.Lock()
		f.asked, f.bind = f.bind, net.JoinHostPort(host, port)
		s.mu.Unlock()
	}
	return net.JoinHostPort(host, port), func() error { return s.remove(context.Background(), f) }, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Run runs a shell command on the host over the connection.
func (s *Session) Run(ctx context.Context, command string) (string, error) {
	return s.tr.Run(ctx, command)
}
