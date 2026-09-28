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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/k8s"
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
	// Label names the forwarder when it is not the only one to its Service.
	// Empty means the Service name, which is what every single-port backend
	// uses and what everything that looks a forwarder up by service expects.
	Label string
	// Guarded puts the Host check (internal/hostguard) in front of the
	// tunnel: kubectl listens on a loopback port nothing publishes, and the
	// host address is a proxy that refuses a request for any other name
	// (guard.go, #725). It is for ports that carry HTTP a browser could
	// send: raw TCP and gRPC-only ports stay a plain tunnel.
	Guarded bool
	// Front, when set on a Guarded target, wraps the path to the emulator
	// behind the Host check: a handler that sees each REST request before
	// the emulator does. BigQuery's is internal/bigqueryfront (#861). gRPC
	// requests do not pass through it.
	Front func(http.Handler) http.Handler
	// Service is the Service the tunnel forwards to, when it is not Name's.
	// Name's is still the in-cluster address pods are given (#881).
	Service string
	// Hosts are more names a Guarded target's Host check accepts, beyond
	// hostguard's own. BigQuery's REST tunnel accepts its Service's
	// in-cluster names when that Service is routed to it (#881).
	Hosts []string
}

// service is the Service the tunnel forwards to.
func (t Target) service() string {
	if t.Service != "" {
		return t.Service
	}
	return t.Name
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
	target   Target
	bindAddr string
	// kube runs kubectl against the Target's namespace, with the
	// kubeconfig New was given and kubectl's default context.
	kube *k8s.Runner

	mu sync.Mutex
	// proc is the running kubectl port-forward.
	proc     k8s.Process
	hostPort int
	// kubePort is where kubectl itself listens on 127.0.0.1 when the
	// tunnel is Guarded, and guard is the proxy on hostPort in front of it.
	// Unguarded, kubectl listens on hostPort and both are unused.
	kubePort int
	guard    *guardProxy
	done     chan struct{}
	// cancelSupervisor stops the goroutine that re-establishes the tunnel,
	// and supervisorDone is closed when it has stopped: Stop waits for it,
	// so nothing of a stopped tunnel runs on after Stop returns.
	cancelSupervisor context.CancelFunc
	supervisorDone   chan struct{}
	// restarts counts re-establishments, so a flapping backend is visible
	// rather than merely survivable.
	restarts int
	// pod is the pod the tunnel is bound to, "" for the Service fallback;
	// containers are its container IDs at bind time, so a restarted
	// container or a recreated sandbox (a node restart, #566) is noticed
	// though the pod's name is unchanged; stderr is kubectl's, for the
	// words behind a failure.
	pod        string
	containers string
	stderr     *syncBuffer

	// Logf, when set, receives one line per exit and re-establishment, so a
	// diagnose bundle says what the tunnel did (#526).
	Logf func(format string, args ...any)
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
	f := &Forwarder{target: target, bindAddr: bindAddr, kube: k8s.New(kubeconfig, "", target.Namespace)}
	f.hostPort = target.HostPort
	if f.hostPort == 0 {
		if p, err := freePort(bindAddr); err == nil {
			f.hostPort = p
		}
	}
	if target.Guarded {
		// Always loopback, whatever the bind: only the proxy dials it.
		if p, err := freePort(kubeBind); err == nil {
			f.kubePort = p
		}
	}
	return f
}

// kubeBind is where kubectl listens behind a guarded tunnel.
const kubeBind = "127.0.0.1"

func (f *Forwarder) Name() string {
	if f.target.Label != "" {
		return "forward:" + f.target.Label
	}
	return "forward:" + f.target.Name
}

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
// The tunnel is then supervised. `kubectl port-forward` binds one pod. When
// that pod goes away (a crash, an OOM kill, an eviction, a rollout) kubectl
// does not exit on its own: it keeps listening, fails the next connection's
// stream, and exits only then (#526, measured). Without supervision the host
// endpoint CloudBurrow told the developer to use stays dead for the life of
// the instance while the cluster looks perfectly healthy. So the supervisor
// watches the bound pod as well as the process, binds only a Ready pod when
// it re-establishes, and counts a tunnel as up only once a connection
// through it is carried, not merely accepted.
func (f *Forwarder) Start(ctx context.Context) error {
	// A pod first, waiting a little for one to be Ready: a tunnel bound to
	// the Service cannot be watched, so a pod restart behind it is noticed
	// only by a failed connection (#526). The Service is the fallback when
	// none is Ready in time, as it was before.
	deadline := time.Now().Add(startPodWait)
	var err error
	for {
		err = f.launch(ctx, true)
		if err == nil || !(errors.Is(err, errNoReadyPod) || errors.Is(err, ErrForwardFailed)) {
			break
		}
		if time.Now().After(deadline) {
			if errors.Is(err, errNoReadyPod) {
				err = f.launch(ctx, false)
			}
			break
		}
		// A launch that bound a Ready pod and did not carry is the same
		// wait as no Ready pod: after stop and up the kubelet recreates
		// every pod's sandbox while the pod keeps its name and, for a
		// moment, its Ready condition (#566), and the next launch finds
		// the pod that is actually there (#572).
		if errors.Is(err, ErrForwardFailed) {
			f.logf("tunnel %s: %v; retrying", f.Name(), err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	if err != nil {
		return err
	}
	if f.target.Guarded {
		f.mu.Lock()
		public := net.JoinHostPort(f.bindAddr, strconv.Itoa(f.hostPort))
		kube := net.JoinHostPort(kubeBind, strconv.Itoa(f.kubePort))
		f.mu.Unlock()
		g, err := startGuard(public, kube, f.logf, f.target.Front, f.target.Hosts...)
		if err != nil {
			f.stopProcess(context.Background())
			return fmt.Errorf("%w: listen on %s: %w", ErrForwardFailed, public, err)
		}
		f.mu.Lock()
		f.guard = g
		f.mu.Unlock()
	}
	f.mu.Lock()
	bound := f.pod
	f.mu.Unlock()
	if bound == "" {
		f.logf("tunnel %s: bound to the Service, no Ready pod to bind; a pod restart is noticed only by a failed connection", f.Name())
	} else {
		f.logf("tunnel %s: bound to pod/%s", f.Name(), bound)
	}

	// The supervisor outlives the start context on purpose: ctx here bounds
	// startup, while supervision must last until Stop.
	superCtx, cancel := context.WithCancel(context.Background())
	superDone := make(chan struct{})
	f.mu.Lock()
	f.cancelSupervisor, f.supervisorDone = cancel, superDone
	f.mu.Unlock()
	go func() {
		defer close(superDone)
		f.supervise(superCtx)
	}()
	return nil
}

// launch starts one kubectl process bound to a Ready pod and proves the
// tunnel carries a connection before reporting it up.
//
// requirePod refuses the Service fallback: after a restart the Service
// would resolve to whichever pod kubectl finds, Ready or not, and a tunnel
// bound to a pod that is not listening accepts connections it cannot serve.
func (f *Forwarder) launch(ctx context.Context, requirePod bool) error {
	f.mu.Lock()
	hostPort, kubePort := f.hostPort, f.kubePort
	f.mu.Unlock()
	if hostPort == 0 {
		p, err := freePort(f.bindAddr)
		if err != nil {
			return fmt.Errorf("%w: reserve host port: %w", ErrForwardFailed, err)
		}
		hostPort = p
	}
	// kubectl listens where the tunnel is published, or, behind a guard,
	// on its own loopback port.
	listenAddr, listenPort := f.bindAddr, hostPort
	if f.target.Guarded {
		if kubePort == 0 {
			p, err := freePort(kubeBind)
			if err != nil {
				return fmt.Errorf("%w: reserve kubectl port: %w", ErrForwardFailed, err)
			}
			kubePort = p
		}
		listenAddr, listenPort = kubeBind, kubePort
	}

	// A Ready pod that is not being deleted, never one on its way out
	// (#381); the Service only at first start, when none can be resolved.
	resource, remote, pod := f.forwardTarget(ctx)
	if requirePod && pod == "" {
		return errNoReadyPod
	}
	spec := fmt.Sprintf("%d:%d", listenPort, remote)
	errOut := &syncBuffer{}
	proc, err := f.kube.PortForward(listenAddr, resource, spec, io.Discard, errOut)
	if err != nil {
		return fmt.Errorf("%w: start kubectl port-forward: %w", ErrForwardFailed, err)
	}

	done := make(chan struct{})
	go func() { _ = proc.Wait(); close(done) }()

	containers := ""
	if pod != "" {
		containers, _ = f.podIdentity(ctx, pod)
	}
	f.mu.Lock()
	f.proc, f.hostPort, f.kubePort, f.done, f.stderr, f.pod, f.containers = proc, hostPort, kubePort, done, errOut, pod, containers
	f.mu.Unlock()

	addr := net.JoinHostPort(listenAddr, strconv.Itoa(listenPort))
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			return fmt.Errorf("%w: kubectl exited: %s", ErrForwardFailed, errOut.tail())
		case <-ctx.Done():
			f.stopProcess(context.Background())
			return ctx.Err()
		default:
		}
		if err := f.carries(addr, errOut); err == nil {
			return nil
		} else if !errors.Is(err, errNotListening) {
			f.stopProcess(context.Background())
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
	f.stopProcess(context.Background())
	return fmt.Errorf("%w: %s never accepted a connection: %s", ErrForwardFailed, addr, errOut.tail())
}

// carries opens one connection through the tunnel and holds it briefly:
// kubectl opens the stream to the pod on accept, and reports on stderr when
// the pod refuses or is gone. A listener that accepts is not a tunnel that
// carries (#526): every restart failure in CI had kubectl accepting
// connections whose streams never answered.
func (f *Forwarder) carries(addr string, errOut *syncBuffer) error {
	c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return errNotListening
	}
	defer c.Close()
	deadline := time.Now().Add(streamCheck)
	for time.Now().Before(deadline) {
		if msg, bad := errOut.streamError(); bad {
			return fmt.Errorf("%w: the tunnel accepts but does not carry: %s", ErrForwardFailed, msg)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil
}

// streamCheck is how long a probe connection waits for kubectl to report a
// failed stream before the tunnel counts as carrying.
var streamCheck = 750 * time.Millisecond

var (
	errNotListening = errors.New("not listening yet")
	errNoReadyPod   = errors.New("no Ready pod to bind")
)

// startPodWait is how long Start waits for a Ready pod before binding the
// Service instead.
var startPodWait = 15 * time.Second

// podWatch is how often the supervisor checks that the bound pod still
// exists. kubectl does not exit when its idle pod is deleted: it notices
// on the next connection, fails it, and exits then (#526). Watching the
// pod replaces the tunnel within this interval instead of on a client's
// failure.
var podWatch = 3 * time.Second

// supervise re-establishes the tunnel whenever kubectl exits, and whenever
// the pod it is bound to is gone or terminating.
//
// The same host port is reused, because the address was already printed, may
// already be in an application's configuration, and for storage was baked into
// the backend's advertised download URL. Reconnecting on a different port
// would be a different kind of broken.
func (f *Forwarder) supervise(ctx context.Context) {
	ticker := time.NewTicker(podWatch)
	defer ticker.Stop()
	for {
		f.mu.Lock()
		done, pod, containers, errOut := f.done, f.pod, f.containers, f.stderr
		f.mu.Unlock()
		if done == nil {
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-done:
			f.logf("tunnel %s: kubectl exited: %s; re-establishing", f.Name(), errOut.tail())
		case <-ticker.C:
			if pod == "" {
				continue
			}
			alive, now := f.podAlive(ctx, pod)
			switch {
			case !alive:
				f.logf("tunnel %s: pod/%s is gone; re-establishing", f.Name(), pod)
			case containers != "" && now != "" && now != containers:
				// The pod is back with new containers: a crash, or the node
				// restarting under `stop`/`up` (#566). The stream to the old
				// ones hangs rather than fails, so nothing else would notice.
				f.logf("tunnel %s: pod/%s restarted its containers; re-establishing", f.Name(), pod)
			default:
				continue
			}
			f.stopProcess(ctx)
		}

		// Retry until the backend comes back: at once, then every half
		// second while no pod is Ready, backing off only on a launch that
		// failed for another reason, so a backend that never returns does
		// not spin and a restart never costs a client its deadline. The
		// port is refused while this runs, which a client retries; the old
		// tunnel would have accepted and hung. Behind a guard the port
		// stays open and the proxy answers 502, or UNAVAILABLE to gRPC,
		// the code gRPC clients treat as retryable.
		backoff := time.Duration(0)
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			err := f.launch(ctx, true)
			if err == nil {
				f.mu.Lock()
				f.restarts++
				n, bound := f.restarts, f.pod
				f.mu.Unlock()
				f.logf("tunnel %s: re-established to pod/%s (restart %d)", f.Name(), bound, n)
				break
			}
			if errors.Is(err, errNoReadyPod) {
				backoff = 500 * time.Millisecond
				continue
			}
			f.logf("tunnel %s: %v", f.Name(), err)
			if backoff == 0 {
				backoff = 500 * time.Millisecond
			} else if backoff < 4*time.Second {
				backoff *= 2
			}
		}
	}
}

// podAlive reports whether the bound pod exists and is not terminating, and
// its container identity now. A failed lookup (an API server under load)
// counts as alive and unchanged: a tunnel is replaced on evidence, not on a
// timeout.
func (f *Forwarder) podAlive(ctx context.Context, pod string) (alive bool, containers string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := f.kube.Get(ctx, "pod", pod, "json")
	if err != nil {
		if k8s.IsNotFound(err) {
			return false, ""
		}
		return true, ""
	}
	p, ok := parsePod([]byte(out))
	if !ok {
		return true, ""
	}
	return p.Metadata.DeletionTimestamp == nil, p.identity()
}

// podIdentity is the pod's container IDs, joined, as bound.
func (f *Forwarder) podIdentity(ctx context.Context, pod string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := f.kube.Get(ctx, "pod", pod, "json")
	if err != nil {
		return "", err
	}
	p, ok := parsePod([]byte(out))
	if !ok {
		return "", errors.New("unreadable pod")
	}
	return p.identity(), nil
}

type podJSON struct {
	Metadata struct {
		DeletionTimestamp *time.Time `json:"deletionTimestamp"`
	} `json:"metadata"`
	Status struct {
		ContainerStatuses []struct {
			Name        string `json:"name"`
			ContainerID string `json:"containerID"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

func parsePod(b []byte) (podJSON, bool) {
	var p podJSON
	if json.Unmarshal(b, &p) != nil {
		return p, false
	}
	return p, true
}

// identity is the container IDs, in name order; "" while none is running.
func (p podJSON) identity() string {
	var ids []string
	for _, c := range p.Status.ContainerStatuses {
		if c.ContainerID != "" {
			ids = append(ids, c.Name+"="+c.ContainerID)
		}
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

func (f *Forwarder) logf(format string, args ...any) {
	if f.Logf != nil {
		f.Logf(format, args...)
	}
}

// syncBuffer is kubectl's stderr, readable while the process writes it.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// tail is the last line, the one that says why.
func (s *syncBuffer) tail() string {
	lines := strings.Split(strings.TrimSpace(s.String()), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// streamError reports kubectl's own words for a stream it could not open.
func (s *syncBuffer) streamError() (string, bool) {
	for _, line := range strings.Split(s.String(), "\n") {
		if strings.Contains(line, "error forwarding port") || strings.Contains(line, "lost connection to pod") {
			return strings.TrimSpace(line), true
		}
	}
	return "", false
}

// Running reports whether the tunnel's process is up right now.
//
// Readiness latched at startup answers "did this ever work", which is a
// different question from the one a dashboard is asked. A tunnel whose pod
// went away is not ready, however well it started.
func (f *Forwarder) Running() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.proc == nil || f.done == nil {
		return false
	}
	select {
	case <-f.done:
		// The supervisor has not re-established it yet.
		return false
	default:
		return true
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
	cancel, superDone := f.cancelSupervisor, f.supervisorDone
	f.cancelSupervisor, f.supervisorDone = nil, nil
	f.mu.Unlock()
	if cancel != nil {
		cancel()
		// The supervisor may be mid-relaunch; let it notice the cancel and
		// return before its process is killed under it.
		select {
		case <-superDone:
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
	err := f.stopProcess(ctx)
	f.mu.Lock()
	g := f.guard
	f.guard = nil
	f.mu.Unlock()
	g.close()
	return err
}

// stopProcess kills the current kubectl without touching supervision, so that
// a failed launch can clean up after itself and still be retried.
func (f *Forwarder) stopProcess(ctx context.Context) error {
	f.mu.Lock()
	proc, done := f.proc, f.done
	f.proc = nil
	f.mu.Unlock()

	if proc == nil {
		return nil
	}
	_ = proc.Kill()
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
