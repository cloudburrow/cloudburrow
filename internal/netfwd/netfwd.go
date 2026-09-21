// Package netfwd publishes in-cluster Services on host loopback addresses.
//
// Two addresses exist for every backend and they are not interchangeable
// (docs/architecture.md §4.2):
//
//   - host -> service: 127.0.0.1:<port>, established here
//   - in-cluster -> service: <name>.<namespace>.svc.cluster.local
//
// A workload running in the cluster must be given the second form. Handing it
// the host form produces a connection that cannot be made.
package netfwd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrForwardFailed means the tunnel could not be established.
var ErrForwardFailed = errors.New("port-forward failed")

// Target is one Service to publish on the host.
type Target struct {
	// Name is the Kubernetes Service name.
	Name string
	// Namespace holds the Service.
	Namespace string
	// ServicePort is the port on the Service.
	ServicePort int
	// HostPort is the requested host port. 0 asks the OS to choose, which is
	// what lets two instances run without colliding.
	HostPort int
}

// InClusterHost returns the DNS name a pod uses to reach this Service.
func (t Target) InClusterHost() string {
	return fmt.Sprintf("%s.%s.svc.cluster.local", t.Name, t.Namespace)
}

// InClusterAddr returns host:port for in-cluster clients.
func (t Target) InClusterAddr() string {
	return net.JoinHostPort(t.InClusterHost(), strconv.Itoa(t.ServicePort))
}

// Forwarder maintains one host-to-Service tunnel.
type Forwarder struct {
	target     Target
	kubeconfig string
	bindAddr   string

	mu       sync.Mutex
	cmd      *exec.Cmd
	hostPort int
	done     chan struct{}
	// cancelSupervisor stops the goroutine that re-establishes the tunnel.
	cancelSupervisor context.CancelFunc
	// restarts counts re-establishments, so a flapping backend is visible
	// rather than merely survivable.
	restarts int
}

// New returns a Forwarder for one target.
//
// When HostPort is 0 a free port is reserved immediately rather than at Start.
// Callers need the host address *before* the backend is deployed: a backend
// that advertises a download URL must be told the address its clients will
// use, and that cannot be discovered after the fact without replacing the pod
// and breaking this very tunnel.
func New(target Target, kubeconfig, bindAddr string) *Forwarder {
	if bindAddr == "" {
		bindAddr = "127.0.0.1"
	}
	f := &Forwarder{target: target, kubeconfig: kubeconfig, bindAddr: bindAddr}
	f.hostPort = target.HostPort
	if f.hostPort == 0 {
		if p, err := freePort(bindAddr); err == nil {
			f.hostPort = p
		}
	}
	return f
}

func (f *Forwarder) Name() string { return "forward:" + f.target.Name }

// HostAddr returns the resolved host address, or "" before Start.
func (f *Forwarder) HostAddr() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hostPort == 0 {
		return ""
	}
	return net.JoinHostPort(f.bindAddr, strconv.Itoa(f.hostPort))
}

// InClusterAddr returns the address in-cluster clients must use.
func (f *Forwarder) InClusterAddr() string { return f.target.InClusterAddr() }

// Start establishes the tunnel and keeps it established.
//
// Readiness is confirmed by connecting, not by assuming the child process is
// ready — kubectl prints its "Forwarding from" line before the listener is
// necessarily usable.
//
// The tunnel is then supervised. `kubectl port-forward` binds one pod, and it
// exits when that pod goes away: a crash, an OOM kill, an eviction or a
// rollout all end it. Without supervision the host endpoint CloudBurrow told
// the developer to use stays refused for the life of the instance while the
// cluster looks perfectly healthy — the pod is Running, the service exists,
// and the address in the startup banner is dead. That was observed, not
// imagined: restarting a backend's pod left its advertised port refused
// indefinitely.
func (f *Forwarder) Start(ctx context.Context) error {
	if err := f.launch(ctx); err != nil {
		return err
	}

	// The supervisor outlives the start context on purpose: ctx here bounds
	// startup, while supervision must last until Stop.
	superCtx, cancel := context.WithCancel(context.Background())
	f.mu.Lock()
	f.cancelSupervisor = cancel
	f.mu.Unlock()
	go f.supervise(superCtx)
	return nil
}

// launch starts one kubectl process and waits for its listener.
func (f *Forwarder) launch(ctx context.Context) error {
	f.mu.Lock()
	hostPort := f.hostPort
	f.mu.Unlock()
	if hostPort == 0 {
		p, err := freePort(f.bindAddr)
		if err != nil {
			return fmt.Errorf("%w: reserve host port: %w", ErrForwardFailed, err)
		}
		hostPort = p
	}

	spec := fmt.Sprintf("%d:%d", hostPort, f.target.ServicePort)
	cmd := exec.Command("kubectl", "--kubeconfig", f.kubeconfig,
		"port-forward", "--address", f.bindAddr,
		"-n", f.target.Namespace, "svc/"+f.target.Name, spec)
	var errOut strings.Builder
	cmd.Stdout = io.Discard
	cmd.Stderr = &errOut
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%w: start kubectl port-forward: %w", ErrForwardFailed, err)
	}

	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()

	f.mu.Lock()
	f.cmd, f.hostPort, f.done = cmd, hostPort, done
	f.mu.Unlock()

	addr := net.JoinHostPort(f.bindAddr, strconv.Itoa(hostPort))
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			return fmt.Errorf("%w: kubectl exited: %s", ErrForwardFailed, strings.TrimSpace(errOut.String()))
		case <-ctx.Done():
			f.stopProcess(context.Background())
			return ctx.Err()
		default:
		}
		if c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond); err == nil {
			c.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	f.stopProcess(context.Background())
	return fmt.Errorf("%w: %s never accepted a connection: %s", ErrForwardFailed, addr, strings.TrimSpace(errOut.String()))
}

// supervise re-establishes the tunnel whenever kubectl exits unexpectedly.
//
// The same host port is reused, because the address was already printed, may
// already be in an application's configuration, and for storage was baked into
// the backend's advertised download URL. Reconnecting on a different port
// would be a different kind of broken.
func (f *Forwarder) supervise(ctx context.Context) {
	for {
		f.mu.Lock()
		done := f.done
		f.mu.Unlock()
		if done == nil {
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-done:
		}

		// The pod behind the tunnel went away. Retry until it comes back,
		// backing off so a backend that never returns does not spin.
		backoff := 500 * time.Millisecond
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if err := f.launch(ctx); err == nil {
				f.mu.Lock()
				f.restarts++
				f.mu.Unlock()
				break
			}
			if backoff < 10*time.Second {
				backoff *= 2
			}
		}
	}
}

// Restarts reports how many times the tunnel has been re-established.
//
// Surviving a restart and never noticing one are different states, and a
// backend that flaps should be visible as flapping.
func (f *Forwarder) Restarts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.restarts
}

// Stop tears the tunnel down.
//
// Supervision is cancelled first: otherwise killing kubectl would look like
// the pod dying and the supervisor would immediately rebuild the tunnel we are
// trying to remove.
func (f *Forwarder) Stop(ctx context.Context) error {
	f.mu.Lock()
	cancel := f.cancelSupervisor
	f.cancelSupervisor = nil
	f.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return f.stopProcess(ctx)
}

// stopProcess kills the current kubectl without touching supervision, so that
// a failed launch can clean up after itself and still be retried.
func (f *Forwarder) stopProcess(ctx context.Context) error {
	f.mu.Lock()
	cmd, done := f.cmd, f.done
	f.cmd = nil
	f.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return nil
	}
	_ = cmd.Process.Kill()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
	return nil
}

// freePort asks the OS for an unused port on the bind address.
func freePort(bindAddr string) (int, error) {
	l, err := net.Listen("tcp", net.JoinHostPort(bindAddr, "0"))
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
