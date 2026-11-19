package redisadmin

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"google.golang.org/grpc/codes"
)

// rotNodes is a ClusterRuntime whose nodes take new certificates, and a
// dialer that records what the nodes are sent.
type rotNodes struct {
	mu     sync.Mutex
	staged []string
	sent   []string
}

func (f *rotNodes) StartClusterNode(context.Context, Spec) (string, string, int, error) {
	return "", "", 0, fmt.Errorf("not started here")
}
func (f *rotNodes) Remove(context.Context, string) error { return nil }
func (f *rotNodes) StageNodeCert(_ context.Context, spec Spec) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.staged = append(f.staged, spec.Server)
	return "/tmp/tls/rotated-" + spec.Server + ".crt", "/tmp/tls/rotated-" + spec.Server + ".key", nil
}

type rotConn struct {
	f    *rotNodes
	addr string
}

func (c rotConn) Do(_ context.Context, args ...any) (any, error) {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.sent = append(c.f.sent, fmt.Sprint(c.addr, args))
	return "OK", nil
}
func (c rotConn) Pipe(context.Context, [][]any) ([]any, error) { return nil, nil }
func (c rotConn) Close() error                                 { return nil }

// TestClusterCertRotation (#1918): a week after creation, by the
// Memorystore clock, every node of a TLS cluster loads a new certificate
// without a restart, and not again until a week later; a cluster without
// in-transit encryption is left alone; on demand is customer-managed only.
func TestClusterCertRotation(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	var clockMu sync.Mutex
	clock := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
	advance := func(d time.Duration) { clockMu.Lock(); now = now.Add(d); clockMu.Unlock() }
	f := &rotNodes{}
	c := &clusters{rt: f, now: clock, bg: context.Background(), byName: map[string]*clusterRecord{}, busy: map[string]string{},
		dial: func(addr, _, _ string) ClusterNode { return rotConn{f, addr} }}
	tlsCA, _, err := newCA("c")
	if err != nil {
		t.Fatal(err)
	}
	r := &clusterRecord{Name: "projects/p/locations/us-central1/clusters/c", Created: now, state: clusterpb.Cluster_ACTIVE, TransitCA: tlsCA,
		Nodes: []*clusterNode{{Server: "n0", addr: "a0"}, {Server: "n1", addr: "a1"}}}
	plain := &clusterRecord{Name: "projects/p/locations/us-central1/clusters/plain", Created: now, state: clusterpb.Cluster_ACTIVE,
		Nodes: []*clusterNode{{Server: "m0", addr: "b0"}}}
	c.byName[r.Name], c.byName[plain.Name] = r, plain
	wait := func(n int) {
		t.Helper()
		for i := 0; i < 200; i++ {
			c.mu.Lock()
			busy := len(c.rotating)
			c.mu.Unlock()
			f.mu.Lock()
			got := len(f.sent)
			f.mu.Unlock()
			if got >= n && busy == 0 {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	advance(6 * 24 * time.Hour)
	c.rotateDue()
	wait(0)
	if len(f.sent) != 0 {
		t.Fatalf("rotated before a week: %v", f.sent)
	}
	advance(24 * time.Hour)
	c.rotateDue()
	wait(2)
	if len(f.sent) != 2 || f.sent[0] != "a0[CONFIG SET tls-cert-file /tmp/tls/rotated-n0.crt tls-key-file /tmp/tls/rotated-n0.key]" {
		t.Fatalf("a week on: sent %v, staged %v", f.sent, f.staged)
	}
	if !r.CertsIssued.Equal(clock()) {
		t.Errorf("CertsIssued %v, want %v", r.CertsIssued, clock())
	}
	c.rotateDue()
	wait(2)
	if len(f.sent) != 2 {
		t.Errorf("rotated again within the week: %v", f.sent)
	}
	advance(certRotateEvery)
	c.rotateDue()
	wait(4)
	if len(f.sent) != 4 {
		t.Errorf("not rotated the second week: %v", f.sent)
	}
	if errRotateNotCustomer.Error() == "" || code(errRotateNotCustomer) != codes.FailedPrecondition {
		t.Errorf("on demand refusal %v", errRotateNotCustomer)
	}
}
