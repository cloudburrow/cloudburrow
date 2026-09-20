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

// Start establishes the tunnel and waits until it accepts a connection.
//
// Readiness is confirmed by connecting, not by assuming the child process is
// ready — kubectl prints its "Forwarding from" line before the listener is
// necessarily usable.
func (f *Forwarder) Start(ctx context.Context) error {
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
			_ = f.Stop(context.Background())
			return ctx.Err()
		default:
		}
		if c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond); err == nil {
			c.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = f.Stop(context.Background())
	return fmt.Errorf("%w: %s never accepted a connection: %s", ErrForwardFailed, addr, strings.TrimSpace(errOut.String()))
}

// Stop tears the tunnel down.
func (f *Forwarder) Stop(ctx context.Context) error {
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
