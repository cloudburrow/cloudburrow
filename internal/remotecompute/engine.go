package remotecompute

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/byoc"
)

// Engine is phase 1 as the provisioner (internal/byoc, phases 2 and 3)
// reaches it: byoc.RemoteEngine. When a VM the console's Create, Start or
// Scale made is running, the manager hands it over; Connect signs in with
// the VM's key from the keychain (served by an in-process agent, never
// written to disk) against the host key the provisioner pinned, checks the
// VM's Docker, and records the VM as the instance's compute, so the
// instance's up, stop, delete, reset, status and doctor run their Docker
// there.
type Engine struct {
	// StateDir is the instances' root: an instance's directory is
	// StateDir/<instance>, as config.Config.InstanceDir has it.
	StateDir string
	// SSH and SSHConfig are as in Options.
	SSH, SSHConfig string
	// LocalState, when set, reports whether an instance's cluster is on
	// this machine's Docker. Such an instance is not handed a new VM
	// silently: its state would stay behind, so Connect answers
	// byoc.ErrMoveNeeded and the instance moves with `cloud move`.
	LocalState func(instance string) bool

	mu    sync.Mutex
	conns map[string]*engineConn
}

type engineConn struct {
	session *Session
	subs    []chan byoc.RemoteEvent
}

var _ byoc.RemoteEngine = (*Engine)(nil)

func (e *Engine) conn(instance string) *engineConn {
	if e.conns == nil {
		e.conns = map[string]*engineConn{}
	}
	c := e.conns[instance]
	if c == nil {
		c = &engineConn{}
		e.conns[instance] = c
	}
	return c
}

// eventKind maps a session's stage to the provisioner's event kinds.
func eventKind(s Stage) string {
	switch s {
	case StageConnected:
		return "connected"
	case StageDisconnected, StageFailed:
		return "lost"
	case StageReconnecting:
		return "connecting"
	case StageClosed:
		return "closed"
	}
	return "connecting"
}

func (e *Engine) publish(instance string, ev Event) {
	re := byoc.RemoteEvent{Time: ev.Time, Instance: instance, Kind: eventKind(ev.Stage), Message: ev.Message}
	if ev.Error != "" {
		re.Message += ": " + ev.Error
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ch := range e.conn(instance).subs {
		select {
		case ch <- re:
		default: // a slow reader misses events rather than stalling ssh
		}
	}
}

// Connect makes the VM t the instance's compute.
func (e *Engine) Connect(ctx context.Context, t byoc.RemoteTarget) error {
	if t.Instance == "" || t.Host == "" {
		return errors.New("the remote target names no instance or host")
	}
	if len(t.PrivateKey) == 0 || t.HostKey == "" {
		return errors.New("a provisioned VM is reached with its own key and pinned host key; the provisioner gave neither")
	}
	target := Target{User: t.User, Host: t.Host, Port: t.Port}
	if target.Port == 0 {
		target.Port = 22
	}
	dir := filepath.Join(e.StateDir, t.Instance)
	if _, had, _ := LoadRecord(dir); !had && e.LocalState != nil && e.LocalState(t.Instance) {
		return fmt.Errorf("%s: %w", t.Instance, byoc.ErrMoveNeeded)
	}
	_ = e.Close(t.Instance)
	s, err := Connect(ctx, Options{Target: target, StateDir: dir, Instance: t.Instance, SSH: e.SSH, SSHConfig: e.SSHConfig,
		Key: PEMKey(t.PrivateKey), HostKey: t.HostKey,
		Progress: func(ev Event) { e.publish(t.Instance, ev) }})
	if err != nil {
		return err
	}
	rec := Record{Target: target, Created: time.Now().UTC(), Host: s.Status().Host, Provisioned: true}
	if old, had, _ := LoadRecord(dir); had && old.Provisioned {
		rec.Created, rec.Machine = old.Created, old.Machine
	} else if had {
		_ = s.Close()
		return fmt.Errorf("instance %s's compute already runs on %s; `cloudburrow delete` it before moving it to the VM", t.Instance, old.Target)
	}
	if err := SaveRecord(dir, rec); err != nil {
		_ = s.Close()
		return err
	}
	e.mu.Lock()
	e.conn(t.Instance).session = s
	e.mu.Unlock()
	return nil
}

func (e *Engine) session(instance string) *Session {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c := e.conns[instance]; c != nil {
		return c.session
	}
	return nil
}

// Status is the instance's connection to its VM.
func (e *Engine) Status(_ context.Context, instance string) (byoc.RemoteStatus, error) {
	s := e.session(instance)
	if s == nil {
		if rec, had, err := LoadRecord(filepath.Join(e.StateDir, instance)); err == nil && had && rec.Provisioned {
			return byoc.RemoteStatus{Message: "The instance's Docker runs on " + rec.Target.String() + "; not connected from this process"}, nil
		}
		return byoc.RemoteStatus{Message: "The instance's Docker is not on a VM"}, nil
	}
	st := s.Status()
	msg := "Connected to " + st.Target
	if h := st.Host; h.CPUs > 0 {
		msg += " (" + h.Size() + ")"
	}
	if !st.Connected {
		msg = "Reconnecting to " + st.Target
		if st.LastError != "" {
			msg += ": " + st.LastError
		}
	}
	return byoc.RemoteStatus{Connected: st.Connected, Message: msg, Since: st.Since}, nil
}

// Reconnect drops the instance's connection and makes it again.
func (e *Engine) Reconnect(ctx context.Context, instance string) error {
	s := e.session(instance)
	if s == nil {
		return fmt.Errorf("instance %s is not connected to a VM", instance)
	}
	return s.Reconnect(ctx)
}

// Close ends the instance's connection (the VM stops or goes); its record
// stays until the instance is deleted.
func (e *Engine) Close(instance string) error {
	e.mu.Lock()
	c := e.conns[instance]
	var s *Session
	if c != nil {
		s, c.session = c.session, nil
	}
	e.mu.Unlock()
	if s == nil {
		return nil
	}
	return s.Close()
}

// Forget closes the instance's connection and removes its record of a VM,
// once the provisioner has deleted the VM; a record of another host stays.
func (e *Engine) Forget(instance string) error {
	_ = e.Close(instance)
	dir := filepath.Join(e.StateDir, instance)
	if rec, had, err := LoadRecord(dir); err != nil || !had || !rec.Provisioned {
		return err
	}
	return RemoveRecord(dir)
}

// Events streams the instance's connection events until the process ends.
func (e *Engine) Events(instance string) <-chan byoc.RemoteEvent {
	ch := make(chan byoc.RemoteEvent, 64)
	e.mu.Lock()
	c := e.conn(instance)
	c.subs = append(c.subs, ch)
	e.mu.Unlock()
	return ch
}
