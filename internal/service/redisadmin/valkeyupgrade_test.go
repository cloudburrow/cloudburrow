package redisadmin

import (
	"context"
	"sync"
	"testing"
	"time"

	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestValkeyUpgradeRepointsPrimaryEndpoint (#1669): once a
// CLUSTER_DISABLED instance's engine upgrade is done, its primary endpoint
// reaches the promoted new node, with no health check in between, and a
// fence a check put up before the upgrade is gone. The old nodes are
// removed as the upgrade ends; an endpoint still pointing at the old
// primary closed every connection (EOF) until the next check repointed it.
func TestValkeyUpgradeRepointsPrimaryEndpoint(t *testing.T) {
	oldEvery := healthEvery
	healthEvery = time.Hour // no check runs: the upgrade alone must repoint
	t.Cleanup(func() { healthEvery = oldEvery })
	s, _ := newHA(t)
	f := &vkNodes{t: t, nodes: map[string]*vkNode{}, byAddr: map[string]*vkNode{}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s.EnableValkey(ctx, f, func(addr, _, _ string) ClusterNode { return vkConn{f, addr} }, nil)
	ms := NewMemorystoreServer(s)
	c := s.valkey
	name := haParent + "/instances/vk-upg"
	if _, err := ms.CreateInstance(ctx, &memorystorepb.CreateInstanceRequest{Parent: haParent, InstanceId: "vk-upg", Instance: &memorystorepb.Instance{
		Mode: memorystorepb.Instance_CLUSTER_DISABLED, ReplicaCount: proto.Int32(1), NodeType: memorystorepb.Instance_SHARED_CORE_NANO,
		EngineVersion:      "VALKEY_7_2",
		PscAutoConnections: []*memorystorepb.PscAutoConnection{{ProjectId: "test-project", Network: "projects/test-project/global/networks/default"}},
	}}); err != nil {
		t.Fatal(err)
	}
	var inst *memorystorepb.Instance
	for i := 0; i < 500; i++ {
		if inst, _ = ms.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: name}); inst.GetState() == memorystorepb.Instance_ACTIVE {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if inst.GetState() != memorystorepb.Instance_ACTIVE {
		t.Fatalf("instance %v", inst)
	}
	base := ValkeyServerBase(name)
	ph, pp := vkEndpoint(t, inst, memorystorepb.ConnectionType_CONNECTION_TYPE_PRIMARY)
	if got := greet(t, ph, pp); got != base+"-0" {
		t.Fatalf("the primary endpoint reaches %s, want %s", got, base+"-0")
	}
	// A check fenced the primary endpoint just before the upgrade began.
	c.mu.Lock()
	c.eps[name].fenced = true
	c.openEndpoints(c.byName[name])
	c.mu.Unlock()

	op, err := ms.UpdateInstance(ctx, &memorystorepb.UpdateInstanceRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"engine_version"}},
		Instance: &memorystorepb.Instance{Name: name, EngineVersion: "VALKEY_8_0"}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; ; i++ {
		got, err := s.created.ops.Get(op.GetName())
		if err != nil {
			t.Fatal(err)
		}
		if got.Done() {
			break
		}
		if i == 1000 {
			t.Fatal("the upgrade did not complete")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Before any GetInstance, which points the endpoints too.
	c.mu.Lock()
	fenced := c.eps[name].fenced
	c.mu.Unlock()
	if got := greetOrEmpty(ph, pp); fenced || got != base+"-2" {
		t.Fatalf("after the upgrade the primary endpoint reaches %q (fenced %v), want the promoted new node %s", got, fenced, base+"-2")
	}
	after, _ := ms.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: name})
	if after.GetEngineVersion() != "VALKEY_8_0" {
		t.Fatalf("after the upgrade the instance runs %s, want VALKEY_8_0", after.GetEngineVersion())
	}
}

// TestValkeyKeptPrimaryLiftsFence (#1669): a primary that starts again
// while no replica is fit to promote (the only one still syncing) is kept,
// and the fence its restart put up is lifted: the primary endpoint reaches
// it again, with no failover.
func TestValkeyKeptPrimaryLiftsFence(t *testing.T) {
	oldEvery, oldTend := healthEvery, tendEvery
	healthEvery, tendEvery = 10*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { healthEvery, tendEvery = oldEvery, oldTend })
	s, _ := newHA(t)
	f := &vkNodes{t: t, nodes: map[string]*vkNode{}, byAddr: map[string]*vkNode{}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var mu sync.Mutex
	failovers := 0
	s.RecordEvents(func(service, kind, _ string, _ map[string]string) {
		if service == "memorystore" && kind == "failover" {
			mu.Lock()
			failovers++
			mu.Unlock()
		}
	})
	s.EnableValkey(ctx, f, func(addr, _, _ string) ClusterNode { return vkConn{f, addr} }, nil)
	ms := NewMemorystoreServer(s)
	c := s.valkey
	name := haParent + "/instances/vk-kept"
	if _, err := ms.CreateInstance(ctx, &memorystorepb.CreateInstanceRequest{Parent: haParent, InstanceId: "vk-kept", Instance: &memorystorepb.Instance{
		Mode: memorystorepb.Instance_CLUSTER_DISABLED, ReplicaCount: proto.Int32(1), NodeType: memorystorepb.Instance_SHARED_CORE_NANO,
		PscAutoConnections: []*memorystorepb.PscAutoConnection{{ProjectId: "test-project", Network: "projects/test-project/global/networks/default"}},
	}}); err != nil {
		t.Fatal(err)
	}
	base := ValkeyServerBase(name)
	n0 := base + "-0"
	for i := 0; ; i++ {
		f.mu.Lock()
		labelled := f.roles[n0] == rolePrimary
		f.mu.Unlock()
		if labelled {
			break
		}
		if i == 500 {
			t.Fatal("node 0 is not labelled the primary")
		}
		time.Sleep(10 * time.Millisecond)
	}
	inst, _ := ms.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: name})
	ph, pp := vkEndpoint(t, inst, memorystorepb.ConnectionType_CONNECTION_TYPE_PRIMARY)

	// Node 0 starts again while node 1 is still loading its dataset.
	fencedOnce := false
	f.mu.Lock()
	f.nodes[base+"-1"].syncing = true
	f.nodes[n0].replID, f.nodes[n0].runID = "started-again", "run-again"
	f.mu.Unlock()
	for i := 0; i < 50; i++ {
		c.mu.Lock()
		fencedOnce = fencedOnce || c.eps[name].fenced
		c.mu.Unlock()
		time.Sleep(healthEvery)
	}
	c.mu.Lock()
	fenced := c.eps[name].fenced
	c.mu.Unlock()
	mu.Lock()
	fo := failovers
	mu.Unlock()
	if got := greetOrEmpty(ph, pp); fenced || got != n0 || fo != 0 {
		t.Fatalf("after the primary started again with no replica fit, the primary endpoint reaches %q (fenced %v, fenced once %v), %d failovers; want %s, unfenced, none",
			got, fenced, fencedOnce, fo, n0)
	}
}
