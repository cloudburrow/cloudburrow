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
	cluster "cloud.google.com/go/redis/cluster/apiv1"
	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
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

// redisClusterCreate creates a Redis cluster of no replicas, with cfg's
// further fields, and deletes it when the test ends.
func redisClusterCreate(t *testing.T, ctx context.Context, c *cluster.CloudRedisClusterClient, project, id string, cfg func(*clusterpb.Cluster)) *clusterpb.Cluster {
	t.Helper()
	parent := "projects/" + project + "/locations/us-central1"
	in := &clusterpb.Cluster{ShardCount: proto.Int32(2), ReplicaCount: proto.Int32(0), NodeType: clusterpb.NodeType_REDIS_SHARED_CORE_NANO,
		PscConfigs: []*clusterpb.PscConfig{{Network: "projects/" + project + "/global/networks/default"}}}
	if cfg != nil {
		cfg(in)
	}
	op, err := c.CreateCluster(ctx, &clusterpb.CreateClusterRequest{Parent: parent, ClusterId: id, Cluster: in})
	if err != nil {
		t.Fatalf("CreateCluster %s: %v", id, err)
	}
	name := parent + "/clusters/" + id
	t.Cleanup(func() {
		for i := 0; i < 3; i++ {
			// A primary is deleted once its secondaries are gone.
			if op, err := c.DeleteCluster(context.Background(), &clusterpb.DeleteClusterRequest{Name: name}); err == nil {
				_ = waitLRODone(context.Background(), op)
				return
			}
			time.Sleep(2 * time.Second)
		}
	})
	cl, err := waitLRO(ctx, op)
	if err != nil {
		t.Fatalf("CreateCluster %s operation: %v", id, err)
	}
	return cl
}

func redisClusterClientOf(t *testing.T, cl *clusterpb.Cluster) *redis.ClusterClient {
	t.Helper()
	e := cl.GetDiscoveryEndpoints()
	if len(e) == 0 {
		t.Fatalf("cluster %s has no discovery endpoint", cl.GetName())
	}
	rc := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{net.JoinHostPort(e[0].GetAddress(), strconv.Itoa(int(e[0].GetPort())))}, MaxRetries: -1})
	t.Cleanup(func() { _ = rc.Close() })
	return rc
}

// ccrUpdate is an UpdateCluster of cross_cluster_replication_config, as
// gcloud redis clusters switchover, detach and detach-secondaries send it.
func ccrUpdate(t *testing.T, ctx context.Context, c *cluster.CloudRedisClusterClient, name string, cfg *clusterpb.CrossClusterReplicationConfig) *clusterpb.Cluster {
	t.Helper()
	op, err := c.UpdateCluster(ctx, &clusterpb.UpdateClusterRequest{
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"cross_cluster_replication_config"}},
		Cluster:    &clusterpb.Cluster{Name: name, CrossClusterReplicationConfig: cfg}})
	if err != nil {
		t.Fatalf("UpdateCluster cross_cluster_replication_config of %s: %v", name, err)
	}
	cl, err := waitLRO(ctx, op)
	if err != nil {
		t.Fatalf("UpdateCluster cross_cluster_replication_config of %s operation: %v", name, err)
	}
	return cl
}

// covers: google.cloud.redis.cluster.v1.CloudRedisCluster/CreateCluster, google.cloud.redis.cluster.v1.CloudRedisCluster/UpdateCluster, google.cloud.redis.cluster.v1.CloudRedisCluster/GetCluster, google.cloud.redis.cluster.v1.CloudRedisCluster/DeleteCluster
//
// TestRedisClusterCrossClusterReplication (#1577), with cloud.google.com/
// go/redis/cluster/apiv1 and go-redis's ClusterClient: a cluster created
// with cross_cluster_replication_config SECONDARY and a primary_cluster
// copies its primary's shard count and node type, serves the primary's
// keys and refuses writes, and both report the membership; a secondary's
// shard count is refused, a cascading secondary and the primary's delete
// too; a shard count change of the primary scales the secondary with it;
// a switchover and then a detach-secondaries (as gcloud sends them) make
// the former secondary the primary and then both writable.
func TestRedisClusterCrossClusterReplication(t *testing.T) {
	h := New(t)
	c, project := redisClusterAdminClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	sfx := time.Now().UnixNano() % 1e7
	p := redisClusterCreate(t, ctx, c, project, fmt.Sprintf("ccr-p-%d", sfx), nil)
	pc := redisClusterClientOf(t, p)
	for i := 0; i < 30; i++ {
		if err := pc.Set(ctx, fmt.Sprintf("before-%d", i), strconv.Itoa(i), 0).Err(); err != nil {
			t.Fatalf("SET on the primary: %v", err)
		}
	}
	// shard_count 1 and another node type asked: a secondary's are its
	// primary's.
	s := redisClusterCreate(t, ctx, c, project, fmt.Sprintf("ccr-s-%d", sfx), func(in *clusterpb.Cluster) {
		in.ShardCount, in.NodeType = proto.Int32(1), clusterpb.NodeType_REDIS_STANDARD_SMALL
		in.CrossClusterReplicationConfig = &clusterpb.CrossClusterReplicationConfig{ClusterRole: clusterpb.CrossClusterReplicationConfig_SECONDARY,
			PrimaryCluster: &clusterpb.CrossClusterReplicationConfig_RemoteCluster{Cluster: p.GetName()}}
	})
	if s.GetShardCount() != 2 || s.GetNodeType() != clusterpb.NodeType_REDIS_SHARED_CORE_NANO {
		t.Errorf("secondary's shard_count %d, node_type %v; want its primary's 2, REDIS_SHARED_CORE_NANO", s.GetShardCount(), s.GetNodeType())
	}
	if cfg := s.GetCrossClusterReplicationConfig(); cfg.GetClusterRole() != clusterpb.CrossClusterReplicationConfig_SECONDARY ||
		cfg.GetPrimaryCluster().GetCluster() != p.GetName() || cfg.GetPrimaryCluster().GetUid() != p.GetUid() ||
		cfg.GetMembership().GetPrimaryCluster().GetCluster() != p.GetName() ||
		len(cfg.GetMembership().GetSecondaryClusters()) != 1 || cfg.GetMembership().GetSecondaryClusters()[0].GetCluster() != s.GetName() {
		t.Errorf("secondary's cross_cluster_replication_config = %v", cfg)
	}
	got, err := c.GetCluster(ctx, &clusterpb.GetClusterRequest{Name: p.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if cfg := got.GetCrossClusterReplicationConfig(); cfg.GetClusterRole() != clusterpb.CrossClusterReplicationConfig_PRIMARY ||
		len(cfg.GetSecondaryClusters()) != 1 || cfg.GetSecondaryClusters()[0].GetCluster() != s.GetName() ||
		cfg.GetMembership().GetPrimaryCluster().GetCluster() != p.GetName() || cfg.GetUpdateTime() == nil {
		t.Errorf("primary's cross_cluster_replication_config = %v", cfg)
	}
	sc := redisClusterClientOf(t, s)
	for i := 0; i < 30; i++ {
		clusterEventually(t, ctx, sc, fmt.Sprintf("before-%d", i), strconv.Itoa(i))
	}
	if err := pc.Set(ctx, "after", "1", 0).Err(); err != nil {
		t.Fatal(err)
	}
	clusterEventually(t, ctx, sc, "after", "1")
	if err := sc.Set(ctx, "x", "y", 0).Err(); err == nil {
		t.Error("SET on the secondary succeeded, want it refused")
	}

	// Refusals.
	if _, err := c.UpdateCluster(ctx, &clusterpb.UpdateClusterRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"shard_count"}},
		Cluster: &clusterpb.Cluster{Name: s.GetName(), ShardCount: proto.Int32(3)}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("UpdateCluster shard_count of a secondary = %v, want FAILED_PRECONDITION", err)
	}
	if _, err := c.DeleteCluster(ctx, &clusterpb.DeleteClusterRequest{Name: p.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeleteCluster of a primary with a secondary = %v, want FAILED_PRECONDITION", err)
	}
	if _, err := c.CreateCluster(ctx, &clusterpb.CreateClusterRequest{Parent: "projects/" + project + "/locations/us-central1", ClusterId: fmt.Sprintf("ccr-c-%d", sfx),
		Cluster: &clusterpb.Cluster{ShardCount: proto.Int32(2), CrossClusterReplicationConfig: &clusterpb.CrossClusterReplicationConfig{
			ClusterRole: clusterpb.CrossClusterReplicationConfig_SECONDARY, PrimaryCluster: &clusterpb.CrossClusterReplicationConfig_RemoteCluster{Cluster: s.GetName()}}}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("CreateCluster of a secondary of a secondary = %v, want FAILED_PRECONDITION", err)
	}

	// The primary's shard count: the secondary is scaled with it.
	op, err := c.UpdateCluster(ctx, &clusterpb.UpdateClusterRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"shard_count"}},
		Cluster: &clusterpb.Cluster{Name: p.GetName(), ShardCount: proto.Int32(3)}})
	if err != nil {
		t.Fatalf("UpdateCluster shard_count of the primary: %v", err)
	}
	if _, err := waitLRO(ctx, op); err != nil {
		t.Fatalf("UpdateCluster shard_count of the primary operation: %v", err)
	}
	if s, err = c.GetCluster(ctx, &clusterpb.GetClusterRequest{Name: s.GetName()}); err != nil || s.GetShardCount() != 3 || s.GetState() != clusterpb.Cluster_ACTIVE {
		t.Fatalf("secondary after the primary's reshard: %v, %v; want 3 shards, ACTIVE", s, err)
	}
	if err := pc.Set(ctx, "scaled", "2", 0).Err(); err != nil {
		t.Fatal(err)
	}
	clusterEventually(t, ctx, sc, "scaled", "2")
	clusterEventually(t, ctx, sc, "before-7", "7")

	// Switchover, as gcloud redis clusters switchover sends it.
	s = ccrUpdate(t, ctx, c, s.GetName(), &clusterpb.CrossClusterReplicationConfig{ClusterRole: clusterpb.CrossClusterReplicationConfig_PRIMARY,
		SecondaryClusters: []*clusterpb.CrossClusterReplicationConfig_RemoteCluster{{Cluster: p.GetName()}}})
	if cfg := s.GetCrossClusterReplicationConfig(); cfg.GetClusterRole() != clusterpb.CrossClusterReplicationConfig_PRIMARY ||
		len(cfg.GetSecondaryClusters()) != 1 || cfg.GetSecondaryClusters()[0].GetCluster() != p.GetName() {
		t.Errorf("after the switchover, the new primary's config = %v", cfg)
	}
	if err := sc.Set(ctx, "switched", "3", 0).Err(); err != nil {
		t.Fatalf("SET on the new primary: %v", err)
	}
	clusterEventually(t, ctx, pc, "switched", "3")
	if err := pc.Set(ctx, "x", "y", 0).Err(); err == nil {
		t.Error("SET on the former primary succeeded, want it refused")
	}

	// detach-secondaries of every secondary: cluster_role NONE.
	s = ccrUpdate(t, ctx, c, s.GetName(), &clusterpb.CrossClusterReplicationConfig{ClusterRole: clusterpb.CrossClusterReplicationConfig_NONE})
	if s.GetCrossClusterReplicationConfig() != nil {
		t.Errorf("detached primary's config = %v, want none", s.GetCrossClusterReplicationConfig())
	}
	if p, err = c.GetCluster(ctx, &clusterpb.GetClusterRequest{Name: p.GetName()}); err != nil || p.GetCrossClusterReplicationConfig() != nil {
		t.Errorf("detached secondary: %v, %v; want no config", p.GetCrossClusterReplicationConfig(), err)
	}
	if err := pc.Set(ctx, "detached", "4", 0).Err(); err != nil {
		t.Errorf("SET on the detached cluster: %v", err)
	}
}

// covers: google.cloud.memorystore.v1.Memorystore/UpdateInstance
//
// TestMemorystoreValkeyClusterModeReplicaScaleAndUpgrade (#1577), with
// cloud.google.com/go/memorystore/apiv1 and go-redis's ClusterClient: a
// CLUSTER mode secondary's shard_count and engine_version are refused, and
// the primary's are copied to it: the secondary has the new shard count,
// then the new engine version, and keeps replicating.
func TestMemorystoreValkeyClusterModeReplicaScaleAndUpgrade(t *testing.T) {
	h := New(t)
	c, project := valkeyAdminClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	sfx := time.Now().UnixNano() % 1e7
	p := valkeyClusterMode(t, ctx, c, project, fmt.Sprintf("crs-p-%d", sfx), 1, func(in *memorystorepb.Instance) { in.EngineVersion = "VALKEY_8_0" })
	pc := crrClusterClient(t, p)
	for i := 0; i < 20; i++ {
		if err := pc.Set(ctx, fmt.Sprintf("k-%d", i), strconv.Itoa(i), 0).Err(); err != nil {
			t.Fatal(err)
		}
	}
	s := valkeyClusterMode(t, ctx, c, project, fmt.Sprintf("crs-s-%d", sfx), 1, func(in *memorystorepb.Instance) {
		in.CrossInstanceReplicationConfig = &memorystorepb.CrossInstanceReplicationConfig{
			InstanceRole:    memorystorepb.CrossInstanceReplicationConfig_SECONDARY,
			PrimaryInstance: &memorystorepb.CrossInstanceReplicationConfig_RemoteInstance{Instance: p.GetName()}}
	})
	sc := crrClusterClient(t, s)
	clusterEventually(t, ctx, sc, "k-3", "3")
	update := func(name, path string, in *memorystorepb.Instance) (*memorystorepb.Instance, error) {
		in.Name = name
		op, err := c.UpdateInstance(ctx, &memorystorepb.UpdateInstanceRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{path}}, Instance: in})
		if err != nil {
			return nil, err
		}
		return waitLRO(ctx, op)
	}
	if _, err := update(s.GetName(), "shard_count", &memorystorepb.Instance{ShardCount: 2}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("UpdateInstance shard_count of a secondary = %v, want FAILED_PRECONDITION", err)
	}
	if _, err := update(s.GetName(), "engine_version", &memorystorepb.Instance{EngineVersion: "VALKEY_9_0"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("UpdateInstance engine_version of a secondary = %v, want FAILED_PRECONDITION", err)
	}

	if _, err := update(p.GetName(), "shard_count", &memorystorepb.Instance{ShardCount: 2}); err != nil {
		t.Fatalf("UpdateInstance shard_count of the primary: %v", err)
	}
	got, err := c.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: s.GetName()})
	if err != nil || got.GetShardCount() != 2 || got.GetState() != memorystorepb.Instance_ACTIVE {
		t.Fatalf("secondary after the primary's reshard: %v, %v; want 2 shards, ACTIVE", got, err)
	}
	if err := pc.Set(ctx, "scaled", "1", 0).Err(); err != nil {
		t.Fatal(err)
	}
	clusterEventually(t, ctx, sc, "scaled", "1")
	clusterEventually(t, ctx, sc, "k-11", "11")

	if _, err := update(p.GetName(), "engine_version", &memorystorepb.Instance{EngineVersion: "VALKEY_9_0"}); err != nil {
		t.Fatalf("UpdateInstance engine_version of the primary: %v", err)
	}
	if got, err = c.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: s.GetName()}); err != nil || got.GetEngineVersion() != "VALKEY_9_0" || got.GetShardCount() != 2 {
		t.Fatalf("secondary after the primary's upgrade: %v, %v; want VALKEY_9_0, 2 shards", got, err)
	}
	// Every node is new after a rolling upgrade: the clients start again
	// from the instances' discovery endpoints.
	if p, err = c.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: p.GetName()}); err != nil {
		t.Fatal(err)
	}
	pc, sc = crrClusterClient(t, p), crrClusterClient(t, got)
	if err := pc.Set(ctx, "upgraded", "2", 0).Err(); err != nil {
		t.Fatal(err)
	}
	clusterEventually(t, ctx, sc, "upgraded", "2")
	clusterEventually(t, ctx, sc, "scaled", "1")
	if err := sc.Set(ctx, "x", "y", 0).Err(); err == nil {
		t.Error("SET on the secondary after the upgrade succeeded, want it refused")
	}
}

// covers: google.cloud.memorystore.v1.Memorystore/StartMigration, google.cloud.memorystore.v1.Memorystore/FinishMigration
//
// TestMemorystoreValkeyClusterModeMigration (#1577), with cloud.google.com/
// go/memorystore/apiv1 and go-redis's ClusterClient: StartMigration of a
// CLUSTER mode instance from a cluster-mode source of the same shard count
// (another instance here, named by its discovery endpoint) serves the
// source's keys, old and new, refuses writes while MIGRATING, and
// FinishMigration makes it ACTIVE, MIGRATED and writable; a source of
// another shard count is ROLLED_BACK.
func TestMemorystoreValkeyClusterModeMigration(t *testing.T) {
	h := New(t)
	c, project := valkeyAdminClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	sfx := time.Now().UnixNano() % 1e7
	src := valkeyClusterMode(t, ctx, c, project, fmt.Sprintf("cmig-src-%d", sfx), 2, nil)
	dst := valkeyClusterMode(t, ctx, c, project, fmt.Sprintf("cmig-dst-%d", sfx), 2, nil)
	one := valkeyClusterMode(t, ctx, c, project, fmt.Sprintf("cmig-one-%d", sfx), 1, nil)
	sc, dc := crrClusterClient(t, src), crrClusterClient(t, dst)
	for i := 0; i < 30; i++ {
		if err := sc.Set(ctx, fmt.Sprintf("old-%d", i), strconv.Itoa(i), 0).Err(); err != nil {
			t.Fatal(err)
		}
	}
	ep := src.GetDiscoveryEndpoints()[0]
	start := func(name string) (*memorystorepb.Instance, error) {
		op, err := c.StartMigration(ctx, &memorystorepb.StartMigrationRequest{Name: name,
			Source: &memorystorepb.StartMigrationRequest_SelfManagedSource{SelfManagedSource: &memorystorepb.SelfManagedSource{
				IpAddress: ep.GetAddress(), Port: ep.GetPort(), NetworkAttachment: "projects/" + project + "/regions/us-central1/networkAttachments/mig"}}})
		if err != nil {
			t.Fatalf("StartMigration of %s: %v", name, err)
		}
		return waitLRO(ctx, op)
	}
	if _, err := start(one.GetName()); err == nil {
		t.Error("StartMigration of a 1-shard instance from a 2-shard source succeeded, want it to fail")
	}
	if got, err := c.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: one.GetName()}); err != nil ||
		got.GetMigrationConfig().GetState() != memorystorepb.MigrationConfig_ROLLED_BACK {
		t.Errorf("1-shard target after the failed migration: %v, %v; want ROLLED_BACK", got.GetMigrationConfig(), err)
	}

	inst, err := start(dst.GetName())
	if err != nil {
		t.Fatalf("StartMigration operation: %v", err)
	}
	if inst.GetState() != memorystorepb.Instance_MIGRATING || inst.GetMigrationConfig().GetState() != memorystorepb.MigrationConfig_REPLICATION_ESTABLISHED {
		t.Errorf("migrating: state %v, migration_config %v", inst.GetState(), inst.GetMigrationConfig())
	}
	for i := 0; i < 30; i++ {
		clusterEventually(t, ctx, dc, fmt.Sprintf("old-%d", i), strconv.Itoa(i))
	}
	for i := 0; i < 10; i++ {
		if err := sc.Set(ctx, fmt.Sprintf("new-%d", i), strconv.Itoa(i), 0).Err(); err != nil {
			t.Fatal(err)
		}
	}
	if err := dc.Set(ctx, "x", "y", 0).Err(); err == nil {
		t.Error("SET on the migration's target succeeded, want it refused")
	}
	fop, err := c.FinishMigration(ctx, &memorystorepb.FinishMigrationRequest{Name: dst.GetName()})
	if err != nil {
		t.Fatalf("FinishMigration: %v", err)
	}
	if inst, err = waitLRO(ctx, fop); err != nil {
		t.Fatalf("FinishMigration operation: %v", err)
	}
	if inst.GetState() != memorystorepb.Instance_ACTIVE || inst.GetMigrationConfig().GetState() != memorystorepb.MigrationConfig_MIGRATED {
		t.Errorf("migrated: state %v, migration_config %v", inst.GetState(), inst.GetMigrationConfig())
	}
	// FinishMigration waited for every write the source took before it.
	for i := 0; i < 10; i++ {
		if v, err := dc.Get(ctx, fmt.Sprintf("new-%d", i)).Result(); v != strconv.Itoa(i) {
			t.Errorf("GET new-%d after FinishMigration = %q, %v", i, v, err)
		}
	}
	if err := dc.Set(ctx, "mine", "3", 0).Err(); err != nil {
		t.Errorf("SET after the migration: %v", err)
	}
	if err := sc.Set(ctx, "later", "4", 0).Err(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if v, _ := dc.Get(ctx, "later").Result(); v != "" {
		t.Errorf("a write to the source after FinishMigration reached the instance: %q", v)
	}
}
