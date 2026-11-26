package remotecompute

import (
	"context"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Every port a container publishes on the host's loopback is one this
// process or the developer reaches on its own loopback: kind's API server
// (127.0.0.1:<random>->6443), the ingress gateway (extraPortMappings), a
// notebook runtime's Jupyter, a Cloud SQL instance's database, a Dataproc
// cluster's web UIs. kind and docker choose most of them as they run, so
// the mirror follows `docker ps` and forwards each one, at the same port,
// as soon as it appears, and drops it when it goes.

// publishedRE matches one published TCP mapping in `docker ps`'s Ports
// column: 127.0.0.1:32768->6443/tcp, 0.0.0.0:8000-8001->80-81/tcp.
var publishedRE = regexp.MustCompile(`^(\d+\.\d+\.\d+\.\d+):(\d+)(?:-(\d+))?->[\d-]+/tcp$`)

// PublishedPorts parses `docker ps --format {{.Ports}}` lines into the host
// ports published on an IPv4 loopback or wildcard address.
func PublishedPorts(out string) []int {
	seen := map[int]bool{}
	for _, line := range strings.Split(out, "\n") {
		for _, m := range strings.Split(line, ",") {
			g := publishedRE.FindStringSubmatch(strings.TrimSpace(m))
			if g == nil {
				continue
			}
			if g[1] != "127.0.0.1" && g[1] != "0.0.0.0" {
				continue
			}
			lo, _ := strconv.Atoi(g[2])
			hi := lo
			if g[3] != "" {
				hi, _ = strconv.Atoi(g[3])
			}
			for p := lo; p <= hi && p-lo < 1024; p++ {
				seen[p] = true
			}
		}
	}
	ports := make([]int, 0, len(seen))
	for p := range seen {
		ports = append(ports, p)
	}
	sort.Ints(ports)
	return ports
}

// Mirror keeps a local forward for every port the host's containers
// publish.
type Mirror struct {
	s *Session
	// List returns the ports published now; nil is `docker ps` on the host.
	List func(ctx context.Context) ([]int, error)
	// Interval between scans; zero is 500ms.
	Interval time.Duration
	// Logf, when set, says what the mirror did.
	Logf func(format string, args ...any)

	mu      sync.Mutex
	cancels map[int]func() error
	failed  map[int]bool
	stop    context.CancelFunc
	done    chan struct{}
}

// NewMirror returns a mirror of the session's published ports; Start runs
// it.
func (s *Session) NewMirror() *Mirror {
	return &Mirror{s: s, cancels: map[int]func() error{}, failed: map[int]bool{}}
}

func (m *Mirror) list(ctx context.Context) ([]int, error) {
	if m.List != nil {
		return m.List(ctx)
	}
	out, err := m.s.docker(ctx, "ps", "--format", "{{.Ports}}")
	if err != nil {
		return nil, err
	}
	return PublishedPorts(out), nil
}

// Sync forwards each newly published port and drops each gone one, once.
func (m *Mirror) Sync(ctx context.Context) error {
	ports, err := m.list(ctx)
	if err != nil {
		return err
	}
	want := map[int]bool{}
	for _, p := range ports {
		want[p] = true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for p, cancel := range m.cancels {
		if !want[p] {
			_ = cancel()
			delete(m.cancels, p)
			m.logf("remote compute: port %d is no longer published on the host; its forward is closed", p)
		}
	}
	for p := range m.failed {
		if !want[p] {
			delete(m.failed, p)
		}
	}
	for _, p := range ports {
		if m.cancels[p] != nil || m.failed[p] {
			continue
		}
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(p))
		cancel, err := m.s.Forward(ctx, addr, addr)
		if err != nil {
			// Taken here by something else: said once, not retried every
			// scan, until the host stops publishing it.
			m.failed[p] = true
			m.logf("remote compute: warning: the host publishes port %d, but it cannot be forwarded to this machine's %s: %v", p, addr, err)
			continue
		}
		m.cancels[p] = cancel
		m.logf("remote compute: forwarding %s to the host's port %d", addr, p)
	}
	return nil
}

func (m *Mirror) logf(format string, args ...any) {
	if m.Logf != nil {
		m.Logf(format, args...)
	}
}

// Forwarded is the ports forwarded now.
func (m *Mirror) Forwarded() []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]int, 0, len(m.cancels))
	for p := range m.cancels {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}

// Start syncs now and then every Interval until Stop.
func (m *Mirror) Start(ctx context.Context) error {
	if err := m.Sync(ctx); err != nil {
		return err
	}
	iv := m.Interval
	if iv == 0 {
		iv = 500 * time.Millisecond
	}
	loop, cancel := context.WithCancel(context.Background())
	m.stop, m.done = cancel, make(chan struct{})
	go func() {
		defer close(m.done)
		t := time.NewTicker(iv)
		defer t.Stop()
		for {
			select {
			case <-loop.Done():
				return
			case <-t.C:
				c, cancel := context.WithTimeout(loop, 20*time.Second)
				_ = m.Sync(c)
				cancel()
			}
		}
	}()
	return nil
}

// Stop ends the scans and closes every forward.
func (m *Mirror) Stop() {
	if m.stop != nil {
		m.stop()
		<-m.done
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for p, cancel := range m.cancels {
		_ = cancel()
		delete(m.cancels, p)
	}
}
