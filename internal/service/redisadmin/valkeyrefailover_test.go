package redisadmin

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	"google.golang.org/protobuf/proto"
)

// TestValkeyPromotable (#1657): a replica still loading the primary's
// dataset, one that has not synced since it started (a node that rejoined
// empty), one that lost the primary well before it failed, one that
// answers as a primary, and one far behind the replica with the most are
// never chosen; with none fit there is no failover.
func TestValkeyPromotable(t *testing.T) {
	down := 3 * time.Second
	up := Replication{LinkUp: true, Offset: 100, LinkDownSince: -1}
	for _, c := range []struct {
		name string
		st   Replication
		fit  bool
	}{
		{"linked", up, true},
		{"lost the primary as it failed", Replication{Offset: 100, LinkDownSince: 4}, true},
		{"syncing", Replication{LinkUp: false, SyncInProgress: true, LinkDownSince: -1}, false},
		{"syncing, link up", Replication{LinkUp: true, SyncInProgress: true}, false},
		{"never synced", Replication{Offset: 0, LinkDownSince: -1}, false},
		{"lost the primary long before", Replication{Offset: 100, LinkDownSince: 600}, false},
		{"a primary", Replication{Primary: true, Offset: 100}, false},
	} {
		if fit, why := promotable(c.st, down); fit != c.fit {
			t.Errorf("%s: promotable = %v (%s), want %v", c.name, fit, why, c.fit)
		}
	}

	// The rejoining node, syncing, has more of a stale offset than the
	// other: it is passed over all the same.
	states := map[int]Replication{
		1: {SyncInProgress: true, Offset: 500, LinkDownSince: -1},
		2: {LinkUp: true, Offset: 90},
	}
	if best, err := fittest([]int{1, 2}, states, down); err != nil || best != 2 {
		t.Errorf("fittest = %d, %v; want node 2", best, err)
	}
	// Far behind the replica with the most, which is not fit.
	states = map[int]Replication{
		1: {Offset: 2 * limitedDataLoss, LinkDownSince: 600},
		2: {LinkUp: true, Offset: 1},
	}
	if best, err := fittest([]int{1, 2}, states, down); !errors.Is(err, errNoneFit) {
		t.Errorf("fittest = %d, %v; want none fit", best, err)
	}
	// Only the rejoining node: none.
	if best, err := fittest([]int{1}, map[int]Replication{1: {SyncInProgress: true, LinkDownSince: -1}}, down); !errors.Is(err, errNoneFit) || !strings.Contains(err.Error(), "loading") {
		t.Errorf("fittest = %d, %v; want none fit, still loading", best, err)
	}
	if _, err := fittest([]int{1}, nil, down); !errors.Is(err, errNoneFit) {
		t.Errorf("fittest of none answering = %v, want none fit", err)
	}
}

// TestValkeyPrimaryHealthObserve (#1657): a promoted node's new
// replication ID, with the same run_id, is not a failure, settling or
// not; a new run_id is (the server started again); without a run_id a new
// replication ID is a start only once settled; and while settling only a
// primary that does not answer counts, not one answering as a replica.
func TestValkeyPrimaryHealthObserve(t *testing.T) {
	now := time.Now()
	h := &primaryHealth{server: "n1"}
	if !h.observe(Replication{Primary: true, ReplID: "a", RunID: "r1"}, nil, now) {
		t.Fatal("the first check is a failure")
	}
	h.replID, h.runID = "a", "r1"
	if !h.observe(Replication{Primary: true, ReplID: "b", RunID: "r1"}, nil, now) || h.failed != 0 {
		t.Errorf("a new replication ID of the same server is a failure (failed %d)", h.failed)
	}
	if h.observe(Replication{Primary: true, ReplID: "c", RunID: "r2"}, nil, now) || h.failed < failAfter {
		t.Errorf("a new run_id is not a failure (failed %d)", h.failed)
	}

	settling := &primaryHealth{server: "n1", replID: "a", settleUntil: now.Add(time.Minute)}
	if !settling.observe(Replication{Primary: true, ReplID: "b"}, nil, now) {
		t.Error("without a run_id, a new replication ID while settling is a failure")
	}
	settled := &primaryHealth{server: "n1", replID: "a"}
	if settled.observe(Replication{Primary: true, ReplID: "b"}, nil, now) || settled.failed < failAfter {
		t.Error("without a run_id, a new replication ID once settled is not a start")
	}
	settling = &primaryHealth{server: "n1", replID: "a", runID: "r1", settleUntil: now.Add(time.Minute)}
	for i := 0; i < failAfter+2; i++ {
		settling.observe(Replication{Primary: false, LinkUp: true}, nil, now)
	}
	if settling.failed != 0 {
		t.Errorf("answering as a replica while settling counted %d failures", settling.failed)
	}
	for i := 0; i < failAfter; i++ {
		settling.observe(Replication{}, errors.New("connection refused"), now)
	}
	if settling.failed < failAfter {
		t.Errorf("not answering while settling counted %d failures, want %d", settling.failed, failAfter)
	}
	if settling.observe(Replication{Primary: true, ReplID: "x", RunID: "r9"}, nil, now) {
		t.Error("starting again while settling is not a failure")
	}
}

// TestValkeyRejoiningNodeNotFailedOverTo (#1657): an instance of one
// replica fails over once when its primary's node goes; the old node
// rejoins empty as a replica of the promoted one, which then takes a new
// replication ID (it gained a replica): that is not a failure, and there
// is no second failover. While the rejoined node still syncs, the promoted
// node going is not failed over to it; once it has synced, it is.
func TestValkeyRejoiningNodeNotFailedOverTo(t *testing.T) {
	oldEvery, oldTend, oldSettle := healthEvery, tendEvery, settleFor
	healthEvery, tendEvery, settleFor = 10*time.Millisecond, 10*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { healthEvery, tendEvery, settleFor = oldEvery, oldTend, oldSettle })
	s, _ := newHA(t)
	f := &vkNodes{t: t, nodes: map[string]*vkNode{}, byAddr: map[string]*vkNode{}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var mu sync.Mutex
	var failovers []string
	s.RecordEvents(func(service, kind, target string, detail map[string]string) {
		if service == "memorystore" && kind == "failover" {
			mu.Lock()
			failovers = append(failovers, detail["from"]+">"+detail["to"])
			mu.Unlock()
		}
	})
	s.EnableValkey(ctx, f, func(addr, _, _ string) ClusterNode { return vkConn{f, addr} }, nil)
	ms := NewMemorystoreServer(s)
	name := haParent + "/instances/vk-rejoin"
	if _, err := ms.CreateInstance(ctx, &memorystorepb.CreateInstanceRequest{Parent: haParent, InstanceId: "vk-rejoin", Instance: &memorystorepb.Instance{
		Mode: memorystorepb.Instance_CLUSTER_DISABLED, ReplicaCount: proto.Int32(1), NodeType: memorystorepb.Instance_SHARED_CORE_NANO,
		PscAutoConnections: []*memorystorepb.PscAutoConnection{{ProjectId: "test-project", Network: "projects/test-project/global/networks/default"}},
	}}); err != nil {
		t.Fatal(err)
	}
	base := ValkeyServerBase(name)
	n0, n1 := base+"-0", base+"-1"
	until := func(what string, cond func() bool) {
		t.Helper()
		for i := 0; i < 500; i++ {
			f.mu.Lock()
			ok := cond()
			f.mu.Unlock()
			if ok {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("%s: roles %v", what, f.roles)
	}
	got := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), failovers...)
	}
	until("node 0 is labelled the primary", func() bool { return f.roles[n0] == rolePrimary })

	// Node 0's pod goes: node 1 is promoted.
	f.mu.Lock()
	f.nodes[n0].down = true
	f.mu.Unlock()
	until("node 1 is promoted", func() bool { return f.roles[n1] == rolePrimary })
	// Node 0 comes back empty, a primary of its own: it is made a replica
	// of node 1 and syncs; node 1 takes a new replication ID.
	f.mu.Lock()
	f.nodes[n0].down, f.nodes[n0].replID, f.nodes[n0].runID, f.nodes[n0].offset = false, "fresh", "run-fresh", 0
	f.mu.Unlock()
	until("node 0 replicates node 1", func() bool { return f.nodes[n0].primary != "" })
	f.mu.Lock()
	f.nodes[n0].syncing = true
	f.nodes[n1].replID = "gained-a-replica"
	f.mu.Unlock()
	time.Sleep(settleFor + 30*healthEvery)
	if fo := got(); len(fo) != 1 || fo[0] != n0+">"+n1 {
		t.Fatalf("failovers %v, want one, from node 0 to node 1", fo)
	}
	f.mu.Lock()
	primary := f.roles[n1]
	f.mu.Unlock()
	if primary != rolePrimary {
		t.Fatalf("node 1 is %s after node 0 rejoined, want the primary", primary)
	}

	// Node 1 goes while node 0 still syncs: no failover to it.
	f.mu.Lock()
	f.nodes[n1].down = true
	f.mu.Unlock()
	time.Sleep(30 * healthEvery)
	if fo := got(); len(fo) != 1 {
		t.Fatalf("failovers %v while the only replica syncs, want none more", fo)
	}
	f.mu.Lock()
	n0Primary := f.nodes[n0].primary
	f.mu.Unlock()
	if n0Primary == "" {
		t.Fatal("the syncing node 0 was promoted")
	}
	// Once node 0 has synced, it is promoted.
	f.mu.Lock()
	f.nodes[n0].syncing, f.nodes[n0].offset = false, 5
	f.mu.Unlock()
	until("node 0 is promoted once synced", func() bool { return f.roles[n0] == rolePrimary })
	if fo := got(); len(fo) != 2 || fo[1] != n1+">"+n0 {
		t.Fatalf("failovers %v, want a second, from node 1 to node 0", fo)
	}
}
