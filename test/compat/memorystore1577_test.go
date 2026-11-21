//go:build compat

package compat

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"testing"
	"time"

	memorystore "cloud.google.com/go/memorystore/apiv1"
	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func valkeyClusterMode(t *testing.T, ctx context.Context, c *memorystore.Client, project, id string, shards int32, cfg func(*memorystorepb.Instance)) *memorystorepb.Instance {
	t.Helper()
	return valkeyDisabled(t, ctx, c, project, id, func(in *memorystorepb.Instance) {
		in.Mode, in.ShardCount = memorystorepb.Instance_CLUSTER, shards
		if cfg != nil {
			cfg(in)
		}
	})
}

func crrClusterClient(t *testing.T, inst *memorystorepb.Instance) *redis.ClusterClient {
	t.Helper()
	e := inst.GetDiscoveryEndpoints()
	if len(e) == 0 {
		t.Fatalf("instance %s has no discovery endpoint", inst.GetName())
	}
	rc := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{net.JoinHostPort(e[0].GetAddress(), strconv.Itoa(int(e[0].GetPort())))}, MaxRetries: -1})
	t.Cleanup(func() { _ = rc.Close() })
	return rc
}

func clusterEventually(t *testing.T, ctx context.Context, rc *redis.ClusterClient, k, want string) {
	t.Helper()
	var got string
	var err error
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if got, err = rc.Get(ctx, k).Result(); got == want {
			return
		}
	}
	t.Errorf("GET %s = %q, %v; want %q", k, got, err, want)
}

// covers: google.cloud.memorystore.v1.Memorystore/CreateInstance, google.cloud.memorystore.v1.Memorystore/UpdateInstance, google.cloud.memorystore.v1.Memorystore/GetInstance, google.cloud.memorystore.v1.Memorystore/DeleteInstance
//
// TestMemorystoreValkeyClusterModeCrossRegionReplication (#1577), with
// cloud.google.com/go/memorystore/apiv1 and go-redis's ClusterClient: a
// CLUSTER mode secondary takes its primary's shard count, holds the keys
// written before it was created (TTLs kept) and those written after,
// each once, and refuses writes; a switchover makes it the primary and the
// former primary its read-only secondary; a detach makes both writable.
func TestMemorystoreValkeyClusterModeCrossRegionReplication(t *testing.T) {
	h := New(t)
	c, project := valkeyAdminClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	sfx := time.Now().UnixNano() % 1e7
	p := valkeyClusterMode(t, ctx, c, project, fmt.Sprintf("crr-p-%d", sfx), 2, nil)
	pc := crrClusterClient(t, p)
	for i := 0; i < 40; i++ {
		if err := pc.Set(ctx, fmt.Sprintf("before-%d", i), strconv.Itoa(i), 0).Err(); err != nil {
			t.Fatalf("SET on the primary: %v", err)
		}
	}
	if err := pc.Set(ctx, "ttl", "x", time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := pc.Incr(ctx, "counter").Err(); err != nil {
			t.Fatal(err)
		}
	}
	// shard_count 1 asked: a secondary's is copied from its primary.
	s := valkeyClusterMode(t, ctx, c, project, fmt.Sprintf("crr-s-%d", sfx), 1, func(in *memorystorepb.Instance) {
		in.CrossInstanceReplicationConfig = &memorystorepb.CrossInstanceReplicationConfig{
			InstanceRole:    memorystorepb.CrossInstanceReplicationConfig_SECONDARY,
			PrimaryInstance: &memorystorepb.CrossInstanceReplicationConfig_RemoteInstance{Instance: p.GetName()}}
	})
	if s.GetShardCount() != 2 {
		t.Errorf("secondary's shard_count = %d, want its primary's 2", s.GetShardCount())
	}
	if cfg := s.GetCrossInstanceReplicationConfig(); cfg.GetInstanceRole() != memorystorepb.CrossInstanceReplicationConfig_SECONDARY ||
		cfg.GetPrimaryInstance().GetInstance() != p.GetName() || cfg.GetMembership().GetPrimaryInstance().GetInstance() != p.GetName() {
		t.Errorf("secondary's cross_instance_replication_config = %v", cfg)
	}
	got, err := c.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: p.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if pcfg := got.GetCrossInstanceReplicationConfig(); pcfg.GetInstanceRole() != memorystorepb.CrossInstanceReplicationConfig_PRIMARY ||
		len(pcfg.GetSecondaryInstances()) != 1 || pcfg.GetSecondaryInstances()[0].GetInstance() != s.GetName() {
		t.Errorf("primary's cross_instance_replication_config = %v", pcfg)
	}

	sc := crrClusterClient(t, s)
	for i := 0; i < 40; i++ {
		clusterEventually(t, ctx, sc, fmt.Sprintf("before-%d", i), strconv.Itoa(i))
	}
	if ttl, err := sc.TTL(ctx, "ttl").Result(); err != nil || ttl <= 0 || ttl > time.Hour {
		t.Errorf("TTL of a copied key = %v, %v; want under an hour", ttl, err)
	}
	for i := 0; i < 5; i++ {
		if err := pc.Incr(ctx, "counter").Err(); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 20; i++ {
		if err := pc.Set(ctx, fmt.Sprintf("after-%d", i), strconv.Itoa(i), 0).Err(); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 20; i++ {
		clusterEventually(t, ctx, sc, fmt.Sprintf("after-%d", i), strconv.Itoa(i))
	}
	clusterEventually(t, ctx, sc, "counter", "10")
	if err := sc.Set(ctx, "x", "y", 0).Err(); err == nil {
		t.Error("SET on the secondary succeeded, want it refused")
	}
	if _, err := c.DeleteInstance(ctx, &memorystorepb.DeleteInstanceRequest{Name: p.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeleteInstance of a primary with a secondary = %v, want FAILED_PRECONDITION", err)
	}

	// Switchover: the secondary becomes the primary.
	s = cirUpdate(t, ctx, c, s.GetName(), &memorystorepb.CrossInstanceReplicationConfig{
		InstanceRole:       memorystorepb.CrossInstanceReplicationConfig_PRIMARY,
		SecondaryInstances: []*memorystorepb.CrossInstanceReplicationConfig_RemoteInstance{{Instance: p.GetName()}}})
	if cfg := s.GetCrossInstanceReplicationConfig(); cfg.GetInstanceRole() != memorystorepb.CrossInstanceReplicationConfig_PRIMARY ||
		len(cfg.GetSecondaryInstances()) != 1 || cfg.GetSecondaryInstances()[0].GetInstance() != p.GetName() {
		t.Errorf("after the switchover, the new primary's config = %v", cfg)
	}
	if err := sc.Set(ctx, "switched", "3", 0).Err(); err != nil {
		t.Fatalf("SET on the new primary: %v", err)
	}
	clusterEventually(t, ctx, pc, "switched", "3")
	clusterEventually(t, ctx, pc, "counter", "10")
	if err := pc.Set(ctx, "x", "y", 0).Err(); err == nil {
		t.Error("SET on the former primary succeeded, want it refused")
	}

	// Detach the former primary, now a secondary.
	p = cirUpdate(t, ctx, c, p.GetName(), &memorystorepb.CrossInstanceReplicationConfig{
		InstanceRole: memorystorepb.CrossInstanceReplicationConfig_NONE})
	if p.GetCrossInstanceReplicationConfig() != nil {
		t.Errorf("detached instance's config = %v, want none", p.GetCrossInstanceReplicationConfig())
	}
	if err := pc.Set(ctx, "detached", "4", 0).Err(); err != nil {
		t.Errorf("SET on the detached instance: %v", err)
	}
	if err := sc.Set(ctx, "detached", "5", 0).Err(); err != nil {
		t.Errorf("SET on the primary left: %v", err)
	}
	if v, _ := pc.Get(ctx, "detached").Result(); v != "4" {
		t.Errorf("detached instance's key = %q, want its own 4", v)
	}
}
