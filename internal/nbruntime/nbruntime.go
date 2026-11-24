// Package nbruntime runs the Python of BigQuery Studio's notebooks (#2094)
// on the host: CloudBurrow's stand-in for the Colab Enterprise runtime a
// notebook connects to in Google's console
// (https://docs.cloud.google.com/bigquery/docs/create-notebooks).
//
// Each connected notebook has a runtime of its own: a container of the
// image built from image/ (the Docker official python image, pinned, with
// Google's google-cloud-bigquery and bigquery-magics and IPython), running
// kernel.py, an IPython shell that keeps the notebook's state between
// cells. Cells are sent one per line on the container's stdin and their
// outputs (nbformat 4 output objects) read back from its stdout.
//
// Egress: the runtime is on a Docker network created --internal, which has
// no route out. Its one peer is a relay container (the same image, running
// relay.py) on that network and on the default bridge, aliased
// host.docker.internal on the internal network, port 8080, relaying to the
// instance's BigQuery REST port, the name and port the kernel's BigQuery
// client is given. The relay dials that host port alone, as
// internal/sparkproc's does for a Spark procedure.
package nbruntime

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/hostrelay"
)

//go:embed image/Dockerfile image/kernel.py image/relay.py image/requirements.txt image/build-requirements.txt
var context_ embed.FS

var contextFiles = []string{"Dockerfile", "kernel.py", "relay.py", "requirements.txt", "build-requirements.txt"}

// Tag is the runtime image's tag: its build context's hash, so a changed
// Dockerfile or kernel is a new image, never a stale one.
func Tag() string {
	h := sha256.New()
	for _, name := range contextFiles {
		b, _ := context_.ReadFile("image/" + name)
		h.Write([]byte(name))
		h.Write(b)
	}
	return "cloudburrow/notebook-runtime:" + hex.EncodeToString(h.Sum(nil))[:16]
}

// Tar is the build context as a tar, for `docker build -`.
func Tar() (io.Reader, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range contextFiles {
		b, err := context_.ReadFile("image/" + name)
		if err != nil {
			return nil, err
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(b))}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(b); err != nil {
			return nil, err
		}
	}
	return &buf, tw.Close()
}

// The runtime's BigQuery endpoint: the relay's alias and port on the
// runtime's network. The instance's REST ports answer to
// host.docker.internal beside loopback (hostguard, ADR-0004).
const (
	relayAlias = "host.docker.internal"
	relayPort  = "8080"
	endpoint   = "http://" + relayAlias + ":" + relayPort
	// dataprocEndpoint is the Dataproc API's, gRPC in plaintext (#2156).
	dataprocEndpoint = relayAlias + ":8081"
)

// IdleTimeout is how long a runtime is kept without a cell run: Colab
// Enterprise's default runtime template shuts an idle runtime down after
// 180 minutes.
const IdleTimeout = 180 * time.Minute

// The states a runtime reports, as the notebook's Connect button shows them.
const (
	StateDisconnected = "DISCONNECTED"
	StateBuilding     = "BUILDING"
	StateStarting     = "STARTING"
	StateConnected    = "CONNECTED"
	StateFailed       = "FAILED"
)

// Status is a notebook's runtime as the console shows it.
type Status struct {
	State   string    `json:"state"`
	Message string    `json:"message,omitempty"`
	Python  string    `json:"python,omitempty"`
	Project string    `json:"project,omitempty"`
	Image   string    `json:"image,omitempty"`
	Started time.Time `json:"started,omitempty"`
	Idle    string    `json:"idleShutdown,omitempty"`
	Notes   []string  `json:"notes,omitempty"`
}

// Result is one cell's run: its outputs, nbformat 4 output objects as the
// kernel wrote them, and the execution count.
type Result struct {
	Status         string            `json:"status"`
	ExecutionCount int               `json:"execution_count"`
	Outputs        []json.RawMessage `json:"outputs"`
}

// Manager keeps the instance's notebook runtimes.
type Manager struct {
	// Instance labels the containers and networks made.
	Instance string
	// BigQuery is the instance's BigQuery REST address on the host
	// (host:port), live at each connect.
	BigQuery func() string
	// Dataproc is the instance's Dataproc API address on the host
	// (host:port), "" when it is not served: an Apache Spark notebook's
	// DataprocSparkSession creates its Spark Connect session there and
	// reaches it through the same port (#2156).
	Dataproc func() string
	// Region is the Dataproc region a notebook's sessions are made in;
	// "" is us-central1.
	Region string
	// Logf receives the manager's own notes, or is nil.
	Logf func(format string, args ...any)
	// Docker is the docker command ("" for "docker"); tests swap it.
	Docker string

	mu       sync.Mutex
	sessions map[string]*session
	buildMu  sync.Mutex
	once     sync.Once
	desktop  bool
}

type session struct {
	key, project string
	opts         Options
	status       Status
	names        []string // network last
	stop         func()
	cmd          *exec.Cmd
	stdin        io.WriteCloser
	out          *bufio.Reader
	run          sync.Mutex // one cell at a time, as a kernel runs them
	last         time.Time
	timer        *time.Timer
}

var nameSafe = regexp.MustCompile(`[^a-z0-9]+`)

func (m *Manager) logf(format string, args ...any) {
	if m.Logf != nil {
		m.Logf(format, args...)
	}
}

func (m *Manager) docker(ctx context.Context, args ...string) (string, error) {
	bin := m.Docker
	if bin == "" {
		bin = "docker"
	}
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// Status is the runtime of the notebook key.
func (m *Manager) Status(key string) Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[key]
	if s == nil {
		return Status{State: StateDisconnected}
	}
	return s.status
}

// Connect starts the notebook's runtime, building its image first when the
// host has none, and returns at once: Status follows it to CONNECTED or
// FAILED. A runtime already connected or starting is kept.
func (m *Manager) Connect(key, project string) Status {
	return m.ConnectWith(key, project, Options{})
}

// Options are a runtime's settings from its Colab Enterprise runtime
// template (#2105): its idle shutdown and its environment variables.
type Options struct {
	// IdleTimeout replaces the default IdleTimeout when positive.
	IdleTimeout time.Duration
	// IdleDisabled keeps the runtime however long it is idle.
	IdleDisabled bool
	// Env is set in the kernel's environment.
	Env map[string]string
	// Token, when set, is the bearer token the kernel's Google clients
	// send (CB_ACCESS_TOKEN): the identity a run executes as (#2173),
	// instead of the anonymous credentials the instance takes as its own.
	Token string
}

// idleTimeout is the session's idle shutdown, 0 when it has none.
func (s *session) idleTimeout() time.Duration {
	switch {
	case s.opts.IdleDisabled:
		return 0
	case s.opts.IdleTimeout > 0:
		return s.opts.IdleTimeout
	}
	return IdleTimeout
}

// ConnectWith is Connect with a runtime template's options.
func (m *Manager) ConnectWith(key, project string, opts Options) Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions == nil {
		m.sessions = map[string]*session{}
	}
	if s := m.sessions[key]; s != nil && s.status.State != StateFailed {
		return s.status
	}
	s := &session{key: key, project: project, opts: opts, status: Status{State: StateStarting, Project: project, Image: Tag()}}
	m.sessions[key] = s
	go m.start(s)
	return s.status
}

func (m *Manager) set(s *session, f func(*Status)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f(&s.status)
}

func (m *Manager) fail(s *session, err error) {
	m.logf("notebook runtime %s: %v", s.key, err)
	m.teardown(s)
	m.set(s, func(st *Status) { st.State, st.Message = StateFailed, err.Error() })
}

func (m *Manager) start(s *session) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if err := m.ensureImage(ctx, s); err != nil {
		m.fail(s, err)
		return
	}
	m.set(s, func(st *Status) { st.State = StateStarting })
	if err := m.launch(ctx, s); err != nil {
		m.fail(s, err)
		return
	}
}

// ensureImage builds the runtime image unless the host has it. One build
// at a time; a second caller waits for the first's image.
func (m *Manager) ensureImage(ctx context.Context, s *session) error {
	tag := Tag()
	if _, err := m.docker(ctx, "image", "inspect", "--format", "{{.Id}}", tag); err == nil {
		return nil
	}
	m.buildMu.Lock()
	defer m.buildMu.Unlock()
	if _, err := m.docker(ctx, "image", "inspect", "--format", "{{.Id}}", tag); err == nil {
		return nil
	}
	m.set(s, func(st *Status) {
		st.State, st.Message = StateBuilding, "Building the notebook runtime image "+tag+" (the first connect only)"
	})
	tr, err := Tar()
	if err != nil {
		return err
	}
	bin := m.Docker
	if bin == "" {
		bin = "docker"
	}
	cmd := exec.CommandContext(ctx, bin, "build", "--label", "cloudburrow.notebook-runtime=1", "-t", tag, "-")
	cmd.Stdin = tr
	out, err := cmd.CombinedOutput()
	if err != nil {
		tail := string(out)
		if len(tail) > 2000 {
			tail = tail[len(tail)-2000:]
		}
		return fmt.Errorf("the notebook runtime image could not be built: %s", strings.TrimSpace(tail))
	}
	m.logf("notebook runtime: built %s", tag)
	return nil
}

// target is the relay's target as a container reaches the host: Docker
// Desktop's host.docker.internal is the host's loopback; on Docker Engine
// the port is relayed from the default bridge's gateway for the session.
func (m *Manager) target(ctx context.Context, rest string) (string, func(), error) {
	m.once.Do(func() {
		out, _ := m.docker(ctx, "info", "--format", "{{.OperatingSystem}}")
		m.desktop = strings.Contains(out, "Docker Desktop")
	})
	_, port, err := net.SplitHostPort(rest)
	if err != nil {
		return "", nil, fmt.Errorf("the BigQuery address %q: %w", rest, err)
	}
	if m.desktop {
		return "host.docker.internal:" + port, func() {}, nil
	}
	gw, err := m.docker(ctx, "network", "inspect", "bridge", "--format", "{{range .IPAM.Config}}{{.Gateway}} {{end}}")
	if err != nil {
		return "", nil, errors.New("find the default bridge's gateway: " + gw)
	}
	var ip string
	for _, f := range strings.Fields(gw) {
		if p := net.ParseIP(f); p != nil && p.To4() != nil {
			ip = f
			break
		}
	}
	if ip == "" {
		return "", nil, fmt.Errorf("the default bridge has no IPv4 gateway (%q)", gw)
	}
	r := &hostrelay.Relay{Listen: net.JoinHostPort(ip, "0"), Target: rest}
	if err := r.Start(context.Background()); err != nil {
		return "", nil, err
	}
	return r.Addr(), func() { _ = r.Close() }, nil
}

func (m *Manager) launch(ctx context.Context, s *session) error {
	rest := ""
	if m.BigQuery != nil {
		rest = m.BigQuery()
	}
	if rest == "" {
		return errors.New("BigQuery is not served by this instance, so a notebook runtime has nothing to query")
	}
	target, stop, err := m.target(ctx, rest)
	if err != nil {
		return err
	}
	s.stop = stop
	// The Dataproc API's port (#2156), on the relay's 8081.
	dpTarget := ""
	if m.Dataproc != nil {
		if dp := m.Dataproc(); dp != "" {
			t, stopDP, err := m.target(ctx, dp)
			if err != nil {
				stop()
				return err
			}
			dpTarget, s.stop = t, func() { stop(); stopDP() }
		}
	}
	suffix := nameSafe.ReplaceAllString(strings.ToLower(s.key), "-")
	if len(suffix) > 30 {
		suffix = suffix[len(suffix)-30:]
	}
	suffix = strings.Trim(suffix, "-") + fmt.Sprintf("-%d", time.Now().UnixNano()%1e6)
	network, relay, kernel := "cb-nb-net-"+suffix, "cb-nb-relay-"+suffix, "cb-nb-"+suffix
	label := "io.cloudburrow.instance=" + m.Instance // sparkproc.InstanceLabel: swept by stop
	image := Tag()

	if out, err := m.docker(ctx, "network", "create", "--internal", "--label", label, network); err != nil {
		return errors.New("create the runtime's internal network: " + out)
	}
	m.mu.Lock()
	s.names = []string{network}
	m.mu.Unlock()
	relayArgs := []string{"run", "-d", "--name", relay, "--label", label, "--entrypoint", "python3"}
	if !m.desktop {
		relayArgs = append(relayArgs, "--add-host", "host.docker.internal:host-gateway")
	}
	relayArgs = append(relayArgs, image, "-I", "/opt/cloudburrow/relay.py", target, dpTarget)
	if out, err := m.docker(ctx, relayArgs...); err != nil {
		return errors.New("start the runtime's relay: " + out)
	}
	m.mu.Lock()
	s.names = []string{relay, network}
	m.mu.Unlock()
	if err := m.waitReady(ctx, relay); err != nil {
		return err
	}
	if out, err := m.docker(ctx, "network", "connect", "--alias", relayAlias, network, relay); err != nil {
		return errors.New("connect the runtime's relay: " + out)
	}
	bin := m.Docker
	if bin == "" {
		bin = "docker"
	}
	var cmdEnv []string
	runArgs := []string{"run", "-i", "--rm", "--init", "--name", kernel, "--label", label, "--network", network,
		"-e", "GOOGLE_CLOUD_PROJECT=" + s.project, "-e", "CB_BIGQUERY_ENDPOINT=" + endpoint, "-e", "NO_GCE_CHECK=True"}
	for _, k := range sortedKeys(s.opts.Env) {
		runArgs = append(runArgs, "-e", k+"="+s.opts.Env[k])
	}
	if s.opts.Token != "" {
		// Passed by name, so the token is not on docker's command line.
		runArgs = append(runArgs, "-e", "CB_ACCESS_TOKEN")
		cmdEnv = append(cmdEnv, "CB_ACCESS_TOKEN="+s.opts.Token)
	}
	if dpTarget != "" {
		region := m.Region
		if region == "" {
			region = "us-central1"
		}
		// Google's DataprocSparkSession reads its API endpoint and region
		// from these; kernel.py points its clients at the relay.
		runArgs = append(runArgs, "-e", "CB_DATAPROC_ENDPOINT="+dataprocEndpoint,
			"-e", "GOOGLE_CLOUD_DATAPROC_API_ENDPOINT="+dataprocEndpoint, "-e", "GOOGLE_CLOUD_REGION="+region)
	}
	cmd := exec.Command(bin, append(runArgs, image)...)
	if len(cmdEnv) > 0 {
		cmd.Env = append(os.Environ(), cmdEnv...)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &limited{w: &stderr, n: 64 << 10}
	if err := cmd.Start(); err != nil {
		return errors.New("start the runtime: " + err.Error())
	}
	m.mu.Lock()
	s.names = []string{kernel, relay, network}
	s.cmd, s.stdin, s.out = cmd, stdin, bufio.NewReaderSize(stdout, 1<<20)
	m.mu.Unlock()
	type ready struct {
		Ready  bool     `json:"ready"`
		Python string   `json:"python"`
		Notes  []string `json:"notes"`
	}
	got := make(chan ready, 1)
	go func() {
		line, err := s.out.ReadBytes('\n')
		var r ready
		if err == nil {
			_ = json.Unmarshal(line, &r)
		}
		got <- r
	}()
	select {
	case r := <-got:
		if !r.Ready {
			return fmt.Errorf("the runtime did not start: %s", strings.TrimSpace(stderr.String()))
		}
		m.mu.Lock()
		idle := s.idleTimeout()
		s.status = Status{State: StateConnected, Python: r.Python, Project: s.project, Image: image,
			Started: time.Now().UTC(), Notes: r.Notes}
		if idle > 0 {
			s.status.Idle = idle.String()
			s.timer = time.AfterFunc(idle, func() { m.idle(s) })
		}
		s.last = time.Now()
		m.mu.Unlock()
		m.logf("notebook runtime %s: connected (%s)", s.key, kernel)
		return nil
	case <-time.After(3 * time.Minute):
		return errors.New("the runtime did not start in 3 minutes: " + strings.TrimSpace(stderr.String()))
	}
}

func (m *Manager) waitReady(ctx context.Context, name string) error {
	deadline := time.Now().Add(time.Minute)
	for {
		logs, _ := m.docker(ctx, "logs", name)
		if strings.Contains(logs, "ready") {
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("the runtime's relay did not start: %s", logs)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// idle shuts a runtime down after its idle timeout without a cell run.
func (m *Manager) idle(s *session) {
	m.mu.Lock()
	idle := s.idleTimeout()
	quiet := time.Since(s.last) >= idle
	m.mu.Unlock()
	if !quiet {
		m.mu.Lock()
		if s.timer != nil {
			s.timer.Reset(idle - time.Since(s.last))
		}
		m.mu.Unlock()
		return
	}
	m.logf("notebook runtime %s: idle shutdown", s.key)
	_ = m.Disconnect(s.key)
}

// ErrNotConnected is a cell sent to a notebook with no runtime.
var ErrNotConnected = errors.New("the notebook is not connected to a runtime: click Connect first")

// Execute runs one cell's code on the notebook's runtime and returns its
// outputs. A context that ends first interrupts the cell.
func (m *Manager) Execute(ctx context.Context, key, code string) (Result, error) {
	m.mu.Lock()
	s := m.sessions[key]
	connected := s != nil && s.status.State == StateConnected
	m.mu.Unlock()
	if !connected {
		return Result{}, ErrNotConnected
	}
	s.run.Lock()
	defer s.run.Unlock()
	m.mu.Lock()
	s.last = time.Now()
	stdin, out := s.stdin, s.out
	m.mu.Unlock()
	line, _ := json.Marshal(map[string]string{"code": code})
	if _, err := stdin.Write(append(line, '\n')); err != nil {
		m.fail(s, errors.New("the runtime stopped"))
		return Result{}, errors.New("the runtime stopped: connect again")
	}
	type answer struct {
		b   []byte
		err error
	}
	got := make(chan answer, 1)
	go func() {
		b, err := out.ReadBytes('\n')
		got <- answer{b, err}
	}()
	var a answer
	select {
	case a = <-got:
	case <-ctx.Done():
		_ = m.Interrupt(key)
		select {
		case a = <-got:
		case <-time.After(15 * time.Second):
			m.fail(s, errors.New("the runtime did not answer an interrupt"))
			return Result{}, errors.New("the cell did not stop when interrupted, so the runtime was shut down")
		}
	}
	if a.err != nil {
		m.fail(s, errors.New("the runtime stopped"))
		return Result{}, errors.New("the runtime stopped while the cell ran: connect again")
	}
	var r Result
	if err := json.Unmarshal(a.b, &r); err != nil {
		return Result{}, fmt.Errorf("the runtime's answer cannot be read: %w", err)
	}
	m.mu.Lock()
	s.last = time.Now()
	m.mu.Unlock()
	return r, nil
}

// Interrupt stops the running cell (Interrupt execution): SIGINT to the
// kernel, a KeyboardInterrupt in the cell.
func (m *Manager) Interrupt(key string) error {
	m.mu.Lock()
	s := m.sessions[key]
	m.mu.Unlock()
	if s == nil || len(s.names) < 3 {
		return ErrNotConnected
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if out, err := m.docker(ctx, "kill", "--signal", "INT", s.names[0]); err != nil {
		return errors.New("interrupt: " + out)
	}
	return nil
}

// Restart restarts the runtime's session: a new kernel, its variables
// gone (Restart session).
func (m *Manager) Restart(key string) Status {
	m.mu.Lock()
	s := m.sessions[key]
	m.mu.Unlock()
	if s == nil {
		return Status{State: StateDisconnected}
	}
	project := s.project
	_ = m.Disconnect(key)
	return m.Connect(key, project)
}

// Disconnect deletes the notebook's runtime (Disconnect and delete
// runtime).
func (m *Manager) Disconnect(key string) error {
	m.mu.Lock()
	s := m.sessions[key]
	delete(m.sessions, key)
	m.mu.Unlock()
	if s == nil {
		return nil
	}
	m.teardown(s)
	return nil
}

func (m *Manager) teardown(s *session) {
	m.mu.Lock()
	names, stdin, cmd, stop, timer := s.names, s.stdin, s.cmd, s.stop, s.timer
	s.names, s.stdin, s.cmd, s.stop, s.timer = nil, nil, nil, nil, nil
	m.mu.Unlock()
	if timer != nil {
		timer.Stop()
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for i, n := range names {
		if i == len(names)-1 && strings.HasPrefix(n, "cb-nb-net-") {
			_, _ = m.docker(ctx, "network", "rm", n)
			continue
		}
		_, _ = m.docker(ctx, "rm", "-f", "-v", n)
	}
	if cmd != nil {
		_ = cmd.Wait()
	}
	if stop != nil {
		stop()
	}
}

// Close deletes every runtime.
func (m *Manager) Close() {
	m.mu.Lock()
	keys := make([]string, 0, len(m.sessions))
	for k := range m.sessions {
		keys = append(keys, k)
	}
	m.mu.Unlock()
	for _, k := range keys {
		_ = m.Disconnect(k)
	}
}

type limited struct {
	w io.Writer
	n int
}

func (l *limited) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := len(p)
		if k > l.n {
			k = l.n
		}
		_, _ = l.w.Write(p[:k])
		l.n -= k
	}
	return len(p), nil
}
