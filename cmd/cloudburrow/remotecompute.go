package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/byoc"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/hostrelay"
	"github.com/cloudburrow/cloudburrow/internal/remotecompute"
)

// Remote compute (#2209, phase 1; docs/remote-compute.md): with
// --compute ssh://user@host, or an instance whose up recorded one, every
// docker and kind command this process runs reaches the host's Docker
// through one supervised SSH connection, every relay a workload reaches this
// machine through is a reverse forward on the host, and every port the
// host's containers publish on its loopback is forwarded back to this
// machine's. The CLI, the console and every endpoint stay here.

// computeConn is this process's connection to its instance's compute host,
// or nil when the compute is this machine's Docker.
type computeConn struct {
	session *remotecompute.Session
	mirror  *remotecompute.Mirror
	record  remotecompute.Record
	prev    string
	hadPrev bool
}

var (
	activeComputeMu sync.Mutex
	activeCompute   *computeConn
)

// currentCompute is the process's compute connection, or nil.
func currentCompute() *computeConn {
	activeComputeMu.Lock()
	defer activeComputeMu.Unlock()
	return activeCompute
}

// computeTarget is where an instance's compute runs: --compute (or
// CLOUDBURROW_COMPUTE, or the file's compute), else what its up recorded.
// A --compute naming another host than the recorded one is refused: the
// cluster and its state are on the recorded host.
func computeTarget(cfg config.Config) (remotecompute.Target, bool, error) {
	rec, recorded, err := remotecompute.LoadRecord(cfg.InstanceDir())
	if err != nil {
		return remotecompute.Target{}, false, err
	}
	if cfg.Compute == "" {
		return rec.Target, recorded, nil
	}
	t, err := remotecompute.ParseTarget(cfg.Compute)
	if err != nil {
		return remotecompute.Target{}, false, err
	}
	if recorded && rec.Target != t {
		return remotecompute.Target{}, false, fmt.Errorf("instance %q's compute runs on %s, not %s: its cluster and state are there. "+
			"`cloudburrow delete` it first to move it", cfg.Name, rec.Target, t)
	}
	return t, true, nil
}

// connectCompute connects to the instance's compute host, when it has one,
// and points this process's docker and kind at it. up passes record, so the
// placement is remembered for the commands that follow; Close undoes it.
// For an instance on this machine's Docker it returns nil and does nothing.
func connectCompute(ctx context.Context, cfg config.Config, out io.Writer, record bool) (*computeConn, error) {
	return connectComputeWith(ctx, cfg, out, record, false)
}

// connectComputeWith is connectCompute; skipChecks leaves the host's
// prerequisites to the caller, doctor, which reports each one.
func connectComputeWith(ctx context.Context, cfg config.Config, out io.Writer, record, skipChecks bool) (*computeConn, error) {
	t, ok, err := computeTarget(cfg)
	if err != nil || !ok {
		return nil, err
	}
	if record {
		if _, had, _ := remotecompute.LoadRecord(cfg.InstanceDir()); !had && localClusterExists(ctx, cfg) {
			return nil, fmt.Errorf("instance %q already has a cluster on this machine's Docker (%s); "+
				"`cloudburrow delete` it, or choose another --name, to run it on %s", cfg.Name, cfg.ClusterName(), t)
		}
	}
	opts := remotecompute.Options{Target: t, StateDir: cfg.InstanceDir(), Instance: cfg.Name, SkipPrerequisites: skipChecks}
	rec, _, _ := remotecompute.LoadRecord(cfg.InstanceDir())
	if rec.Provisioned {
		// A VM `cloudburrow cloud create` made (#2209): its key is in the
		// keychain and its host key pinned, and its address is the
		// provisioner's latest.
		vm, err := provisionedTarget(cfg)
		if err != nil {
			return nil, fmt.Errorf("instance %q's compute is the VM at %s: %w", cfg.Name, t, err)
		}
		t = remotecompute.Target{User: vm.User, Host: vm.Host, Port: vm.Port}
		if t.Port == 0 {
			t.Port = 22
		}
		opts.Target, opts.Key, opts.HostKey = t, remotecompute.PEMKey(vm.PrivateKey), vm.HostKey
		// SSH mode: the VM's firewall follows this machine's address,
		// checked before each connection. SSM mode: t is this process's
		// Session Manager tunnel.
		opts.BeforeConnect = vm.BeforeConnect
		rec.Via = vm.Via
	}
	opts.Progress = func(e remotecompute.Event) {
		if e.Error != "" {
			fmt.Fprintf(out, "compute: %s: %s\n", e.Message, e.Error)
			return
		}
		fmt.Fprintf(out, "compute: %s\n", e.Message)
	}
	s, err := remotecompute.Connect(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("compute host %s: %w", t, err)
	}
	c := &computeConn{session: s, record: remotecompute.Record{Target: t, Created: time.Now().UTC(), Host: s.Status().Host, Provisioned: rec.Provisioned, Via: rec.Via}}
	if rec, had, _ := remotecompute.LoadRecord(cfg.InstanceDir()); had {
		c.record.Created = rec.Created
	}
	if record {
		if err := remotecompute.SaveRecord(cfg.InstanceDir(), c.record); err != nil {
			_ = s.Close()
			return nil, err
		}
	}
	c.prev, c.hadPrev = os.LookupEnv("DOCKER_HOST")
	// Every docker and kind this process runs inherits it; DOCKER_CONTEXT
	// would override it, so it goes.
	_ = os.Setenv("DOCKER_HOST", s.DockerHost())
	_ = os.Unsetenv("DOCKER_CONTEXT")
	hostrelay.SetRemote(s)
	c.mirror = s.NewMirror()
	if record {
		// up's log says what each forward is; a one-off command is quiet.
		c.mirror.Logf = func(format string, args ...any) { fmt.Fprintf(out, format+"\n", args...) }
	}
	if err := c.mirror.Start(ctx); err != nil {
		c.Close()
		return nil, fmt.Errorf("compute host %s: list its published ports: %w", t, err)
	}
	activeComputeMu.Lock()
	activeCompute = c
	activeComputeMu.Unlock()
	return c, nil
}

// provisionedTarget reads the instance's VM from the provisioner: its
// address, user, key (from the keychain) and pinned host key.
var provisionedTarget = func(cfg config.Config) (byoc.RemoteTarget, error) {
	m, err := processCloudManager(cfg)
	if err != nil {
		return byoc.RemoteTarget{}, err
	}
	return m.RemoteTarget(cfg.Name)
}

// cloudManagers is this process's one manager per state directory, so the
// Session Manager tunnels it opens, and its record of the firewall's
// address, are shared by up, the console and the compute connection.
var cloudManagers sync.Map

// processCloudManager is the process's manager for cfg's state directory.
func processCloudManager(cfg config.Config) (*byoc.Manager, error) {
	if v, ok := cloudManagers.Load(cfg.StateDir); ok {
		return v.(*byoc.Manager), nil
	}
	m, err := newCloudManager(cfg)
	if err != nil {
		return nil, err
	}
	v, _ := cloudManagers.LoadOrStore(cfg.StateDir, m)
	return v.(*byoc.Manager), nil
}

// localClusterExists reports whether this machine's own Docker has the
// instance's kind cluster, before DOCKER_HOST is pointed away from it.
func localClusterExists(ctx context.Context, cfg config.Config) bool {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "kind", "get", "clusters").Output()
	if err != nil {
		return false
	}
	for _, l := range strings.Fields(string(out)) {
		if l == cfg.ClusterName() {
			return true
		}
	}
	return false
}

// Close ends the connection and restores this process's DOCKER_HOST.
func (c *computeConn) Close() {
	if c == nil {
		return
	}
	activeComputeMu.Lock()
	if activeCompute == c {
		activeCompute = nil
	}
	activeComputeMu.Unlock()
	hostrelay.SetRemote(nil)
	if c.mirror != nil {
		c.mirror.Stop()
	}
	_ = c.session.Close()
	if c.hadPrev {
		_ = os.Setenv("DOCKER_HOST", c.prev)
	} else {
		_ = os.Unsetenv("DOCKER_HOST")
	}
}

// computeSummary is the one line status and the console show: where the
// compute runs, and the host's size.
func computeSummary(cfg config.Config) (string, error) {
	rec, ok, err := remotecompute.LoadRecord(cfg.InstanceDir())
	if err != nil {
		return "", err
	}
	if !ok {
		return "this machine's Docker", nil
	}
	line := rec.Target.String()
	if rec.Host.CPUs > 0 {
		line += " (" + rec.Host.Size() + ")"
	}
	return line, nil
}

// ComputeView is what the console's Instance page shows about where the
// instance's compute runs.
type computeView struct {
	Location   string                 `json:"location"` // "local" or "ssh"
	Target     string                 `json:"target,omitempty"`
	Host       remotecompute.HostInfo `json:"host"`
	Size       string                 `json:"size,omitempty"`
	Connected  bool                   `json:"connected"`
	Reconnects int                    `json:"reconnects"`
	LastError  string                 `json:"lastError,omitempty"`
	Forwards   int                    `json:"forwards"`
	Published  []int                  `json:"published,omitempty"`
	Events     []remotecompute.Event  `json:"events,omitempty"`
}

func currentComputeView() computeView {
	c := currentCompute()
	if c == nil {
		return computeView{Location: "local", Connected: true}
	}
	st := c.session.Status()
	h := st.Host
	if h.CPUs == 0 {
		h = c.record.Host
	}
	v := computeView{Location: "ssh", Target: st.Target, Host: h, Connected: st.Connected, Reconnects: st.Reconnects,
		LastError: st.LastError, Forwards: st.Forwards, Events: st.Events}
	if h.CPUs > 0 {
		v.Size = h.Size()
	}
	if c.mirror != nil {
		v.Published = c.mirror.Forwarded()
	}
	return v
}

// errNoCompute is the console's answer to a reconnect on local compute.
var errNoCompute = errors.New("this instance's compute is this machine's Docker; there is no connection to reconnect")

// reconnectCompute is the console's Reconnect.
func reconnectCompute(ctx context.Context) error {
	c := currentCompute()
	if c == nil {
		return errNoCompute
	}
	return c.session.Reconnect(ctx)
}

// consoleCompute is the console's ComputeSource.
type consoleCompute struct{}

func (consoleCompute) Compute() any                        { return currentComputeView() }
func (consoleCompute) Reconnect(ctx context.Context) error { return reconnectCompute(ctx) }
