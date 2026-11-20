//go:build compat

package compat

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	memorystore "cloud.google.com/go/memorystore/apiv1"
	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// valkeyServerVersion is valkey_version from INFO server of a node, or of
// every node of a ClusterClient (the first answer).
func valkeyServerVersion(t *testing.T, ctx context.Context, info func(ctx context.Context, section ...string) *redis.StringCmd) string {
	t.Helper()
	s, err := info(ctx, "server").Result()
	if err != nil {
		t.Fatalf("INFO server: %v", err)
	}
	for _, l := range strings.Split(s, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "valkey_version:"); ok {
			return v
		}
	}
	return ""
}

// valkeyCluster creates a CLUSTER mode instance of 2 shards of no replica
// at version, deleted when t ends.
func valkeyCluster(t *testing.T, ctx context.Context, c *memorystore.Client, project, id, version string) *memorystorepb.Instance {
	t.Helper()
	parent := "projects/" + project + "/locations/us-central1"
	name := parent + "/instances/" + id
	op, err := c.CreateInstance(ctx, &memorystorepb.CreateInstanceRequest{Parent: parent, InstanceId: id, Instance: &memorystorepb.Instance{
		ShardCount: 2, ReplicaCount: proto.Int32(0), NodeType: memorystorepb.Instance_SHARED_CORE_NANO, EngineVersion: version,
		PscAutoConnections: []*memorystorepb.PscAutoConnection{{ProjectId: project, Network: "projects/" + project + "/global/networks/default"}}}})
	if err != nil {
		t.Fatalf("CreateInstance %s: %v", id, err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(context.Background(), &memorystorepb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = waitLRODone(context.Background(), op)
		}
	})
	inst, err := waitLRO(ctx, op)
	if err != nil {
		t.Fatalf("CreateInstance %s operation: %v", id, err)
	}
	return inst
}

// valkeyClusterClient is a ClusterClient of inst's discovery endpoint.
func valkeyClusterClient(t *testing.T, inst *memorystorepb.Instance) *redis.ClusterClient {
	t.Helper()
	e := inst.GetDiscoveryEndpoints()[0]
	rc := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{net.JoinHostPort(e.GetAddress(), strconv.Itoa(int(e.GetPort())))}})
	t.Cleanup(func() { _ = rc.Close() })
	return rc
}

// covers: google.cloud.memorystore.v1.Memorystore/CreateInstance, google.cloud.memorystore.v1.Memorystore/GetInstance
//
// TestMemorystoreValkey91 (#2019), with cloud.google.com/go/memorystore/
// apiv1 and a go-redis ClusterClient: a CLUSTER mode instance created at
// engine_version VALKEY_9_1 is ACTIVE at VALKEY_9_1 and runs Valkey 9.1,
// which serves the commands Google's supported versions page lists for 9.1:
// HGETDEL, MSETEX (multiple keys with one expiration; the page names it
// MSETNX), HSETEX's NX and XX, and CLUSTERSCAN.
func TestMemorystoreValkey91(t *testing.T) {
	h := New(t)
	c, project := valkeyAdminClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	inst := valkeyCluster(t, ctx, c, project, fmt.Sprintf("v91-%d", time.Now().UnixNano()%1e8), "VALKEY_9_1")
	if inst.GetEngineVersion() != "VALKEY_9_1" || inst.GetState() != memorystorepb.Instance_ACTIVE {
		t.Fatalf("created instance: engine_version %q, state %v; want VALKEY_9_1, ACTIVE", inst.GetEngineVersion(), inst.GetState())
	}
	if got, err := c.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: inst.GetName()}); err != nil || got.GetEngineVersion() != "VALKEY_9_1" {
		t.Errorf("GetInstance = %q, %v; want VALKEY_9_1", got.GetEngineVersion(), err)
	}
	rc := valkeyClusterClient(t, inst)
	if v := valkeyServerVersion(t, ctx, rc.Info); !strings.HasPrefix(v, "9.1.") {
		t.Errorf("valkey_version = %q, want 9.1.x", v)
	}
	if err := rc.HSet(ctx, "{h}:hash", "a", "1", "b", "2").Err(); err != nil {
		t.Fatalf("HSET: %v", err)
	}
	if got, err := rc.Do(ctx, "HGETDEL", "{h}:hash", "FIELDS", "1", "a").StringSlice(); err != nil || len(got) != 1 || got[0] != "1" {
		t.Errorf("HGETDEL = %v, %v; want [1]", got, err)
	}
	if got, err := rc.HGetAll(ctx, "{h}:hash").Result(); err != nil || len(got) != 1 || got["b"] != "2" {
		t.Errorf("after HGETDEL, HGETALL = %v, %v; want only b", got, err)
	}
	if n, err := rc.Do(ctx, "MSETEX", "2", "{m}:a", "1", "{m}:b", "2", "EX", "100").Int(); err != nil || n != 1 {
		t.Errorf("MSETEX = %v, %v; want 1", n, err)
	}
	if ttl, err := rc.TTL(ctx, "{m}:b").Result(); err != nil || ttl <= 0 || ttl > 100*time.Second {
		t.Errorf("TTL after MSETEX = %v, %v; want up to 100s", ttl, err)
	}
	hsetex := func(flag, v string) (int, error) {
		return rc.Do(ctx, "HSETEX", "{h}:ex", flag, "EX", "100", "FIELDS", "1", "f", v).Int()
	}
	if n, err := hsetex("NX", "v1"); err != nil || n != 1 {
		t.Errorf("HSETEX NX of a new field = %v, %v; want 1", n, err)
	}
	if n, err := hsetex("NX", "v2"); err != nil || n != 0 {
		t.Errorf("HSETEX NX of an existing field = %v, %v; want 0", n, err)
	}
	if n, err := hsetex("XX", "v3"); err != nil || n != 1 {
		t.Errorf("HSETEX XX of an existing field = %v, %v; want 1", n, err)
	}
	if got, err := rc.HGet(ctx, "{h}:ex", "f").Result(); got != "v3" {
		t.Errorf("HGET after HSETEX = %q, %v; want v3", got, err)
	}
	if info, err := rc.Do(ctx, "COMMAND", "INFO", "CLUSTERSCAN").Slice(); err != nil || len(info) != 1 || info[0] == nil {
		t.Errorf("COMMAND INFO CLUSTERSCAN = %v, %v; want the command", info, err)
	}
}

// covers: google.cloud.memorystore.v1.Memorystore/CreateInstance, google.cloud.memorystore.v1.Memorystore/CreateAclPolicy
//
// TestMemorystoreValkeyDefaultVersion (#2019), with cloud.google.com/go/
// memorystore/apiv1 and go-redis: an instance created with no
// engine_version is VALKEY_9_1, as Google documents ("If you don't
// specify a value, then VALKEY_9_1 is the default value"), and runs Valkey
// 9.1. Its ACL policy restricts a user to database 1 (Valkey 9.1's
// database-level access control): the user writes there and is refused
// database 0.
func TestMemorystoreValkeyDefaultVersion(t *testing.T) {
	h := New(t)
	c, project := valkeyAdminClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	sfx := time.Now().UnixNano() % 1e7
	parent := "projects/" + project + "/locations/us-central1"
	pid := fmt.Sprintf("db1-%d", sfx)
	policy := parent + "/aclPolicies/" + pid
	if _, err := c.CreateAclPolicy(ctx, &memorystorepb.CreateAclPolicyRequest{Parent: parent, AclPolicyId: pid,
		AclPolicy: &memorystorepb.AclPolicy{Rules: []*memorystorepb.AclRule{{Username: "db1user", Rule: "on >db1pass ~* +@all db=1"}}}}); err != nil {
		t.Fatalf("CreateAclPolicy: %v", err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteAclPolicy(context.Background(), &memorystorepb.DeleteAclPolicyRequest{Name: policy}); err == nil {
			_ = waitLRODone(context.Background(), op)
		}
	})
	inst := valkeyDisabled(t, ctx, c, project, fmt.Sprintf("vdef-%d", sfx), func(in *memorystorepb.Instance) {
		in.EngineVersion = ""
		in.AclPolicy = proto.String(policy)
	})
	if inst.GetEngineVersion() != "VALKEY_9_1" {
		t.Errorf("engine_version of an instance created without one = %q, want VALKEY_9_1", inst.GetEngineVersion())
	}
	if v := valkeyServerVersion(t, ctx, valkeyPrimary(t, inst).Info); !strings.HasPrefix(v, "9.1.") {
		t.Errorf("valkey_version = %q, want 9.1.x", v)
	}
	valkeyAuthEventually(t, ctx, inst, "db1user", "db1pass", true)
	db := func(n int) *redis.Client {
		rc := redis.NewClient(&redis.Options{Addr: valkeyEndpoint(t, inst, memorystorepb.ConnectionType_CONNECTION_TYPE_PRIMARY),
			Username: "db1user", Password: "db1pass", DB: n, MaxRetries: -1})
		t.Cleanup(func() { _ = rc.Close() })
		return rc
	}
	if err := db(1).Set(ctx, "k", "v", 0).Err(); err != nil {
		t.Errorf("SET in database 1 as db1user: %v", err)
	}
	if err := db(0).Set(ctx, "k", "v", 0).Err(); err == nil || !strings.Contains(err.Error(), "NOPERM") {
		t.Errorf("SET in database 0 as db1user = %v; want NOPERM", err)
	}
}

// covers: google.cloud.memorystore.v1.Memorystore/CreateInstance, google.cloud.memorystore.v1.Memorystore/UpdateInstance
//
// TestMemorystoreValkeyUpgrade90To91 (#2019), with cloud.google.com/go/
// memorystore/apiv1 and go-redis: a CLUSTER_DISABLED instance of 1 replica
// at VALKEY_9_0 upgraded to VALKEY_9_1 runs Valkey 9.1 with every key it
// held before (strings, a hash and a key's TTL) and every write
// acknowledged during the rolling upgrade; a downgrade back to VALKEY_9_0
// is INVALID_ARGUMENT, as Google allows only upgrades.
func TestMemorystoreValkeyUpgrade90To91(t *testing.T) {
	h := New(t)
	c, project := valkeyAdminClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	inst := valkeyDisabled(t, ctx, c, project, fmt.Sprintf("u91-%d", time.Now().UnixNano()%1e7), func(in *memorystorepb.Instance) {
		in.EngineVersion, in.ReplicaCount = "VALKEY_9_0", proto.Int32(1)
	})
	rc := valkeyPrimary(t, inst)
	if v := valkeyServerVersion(t, ctx, rc.Info); !strings.HasPrefix(v, "9.0.") {
		t.Errorf("before the upgrade, valkey_version = %q, want 9.0.x", v)
	}
	before := map[string]string{}
	for i := 0; i < 50; i++ {
		k := fmt.Sprintf("before-%d", i)
		before[k] = strconv.Itoa(i * 7)
		if err := rc.Set(ctx, k, before[k], 0).Err(); err != nil {
			t.Fatalf("SET %s: %v", k, err)
		}
	}
	if err := rc.HSet(ctx, "hash", "f1", "a", "f2", "b").Err(); err != nil {
		t.Fatalf("HSET: %v", err)
	}
	if err := rc.Set(ctx, "ttl", "x", time.Hour).Err(); err != nil {
		t.Fatalf("SET with TTL: %v", err)
	}
	acked, inst := upgradeWhileWriting(t, ctx, c, inst.GetName(), "VALKEY_9_1", func(k, v string) error {
		return rc.Set(ctx, k, v, 0).Err()
	})
	after := valkeyPrimary(t, inst)
	if v := valkeyServerVersion(t, ctx, after.Info); !strings.HasPrefix(v, "9.1.") {
		t.Errorf("after the upgrade, valkey_version = %q, want 9.1.x", v)
	}
	for _, m := range []map[string]string{before, acked} {
		for k, want := range m {
			if got, err := after.Get(ctx, k).Result(); got != want {
				t.Errorf("after the upgrade, GET %s = %q, %v; want %q", k, got, err, want)
			}
		}
	}
	if got, err := after.HGetAll(ctx, "hash").Result(); err != nil || got["f1"] != "a" || got["f2"] != "b" {
		t.Errorf("after the upgrade, HGETALL hash = %v, %v", got, err)
	}
	if ttl, err := after.TTL(ctx, "ttl").Result(); err != nil || ttl <= 0 || ttl > time.Hour {
		t.Errorf("after the upgrade, TTL ttl = %v, %v; want under an hour", ttl, err)
	}
	if _, err := c.UpdateInstance(ctx, &memorystorepb.UpdateInstanceRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"engine_version"}},
		Instance: &memorystorepb.Instance{Name: inst.GetName(), EngineVersion: "VALKEY_9_0"}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a downgrade to VALKEY_9_0 = %v, want INVALID_ARGUMENT", err)
	}
}

// covers: google.cloud.memorystore.v1.Memorystore/CreateInstance, google.cloud.memorystore.v1.Memorystore/UpdateInstance
//
// TestMemorystoreValkeyUpgrade80To91 (#2019), with cloud.google.com/go/
// memorystore/apiv1 and a go-redis ClusterClient: a CLUSTER mode instance
// of 2 shards at VALKEY_8_0 upgraded straight to VALKEY_9_1 (Google: "you
// can upgrade to any newer version") runs Valkey 9.1 on every shard, with
// the keys of every shard written before and during the upgrade.
func TestMemorystoreValkeyUpgrade80To91(t *testing.T) {
	h := New(t)
	c, project := valkeyAdminClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	inst := valkeyCluster(t, ctx, c, project, fmt.Sprintf("c91-%d", time.Now().UnixNano()%1e8), "VALKEY_8_0")
	rc := valkeyClusterClient(t, inst)
	before := map[string]string{}
	for i := 0; i < 40; i++ {
		k := fmt.Sprintf("before-%d", i)
		before[k] = strconv.Itoa(i)
		if err := rc.Set(ctx, k, before[k], 0).Err(); err != nil {
			t.Fatalf("SET %s: %v", k, err)
		}
	}
	acked, inst := upgradeWhileWriting(t, ctx, c, inst.GetName(), "VALKEY_9_1", func(k, v string) error {
		return rc.Set(ctx, k, v, 0).Err()
	})
	after := valkeyClusterClient(t, inst)
	if err := after.ForEachMaster(ctx, func(ctx context.Context, n *redis.Client) error {
		if v := valkeyServerVersion(t, ctx, n.Info); !strings.HasPrefix(v, "9.1.") {
			t.Errorf("after the upgrade, a primary's valkey_version = %q, want 9.1.x", v)
		}
		return nil
	}); err != nil {
		t.Errorf("each primary: %v", err)
	}
	for _, m := range []map[string]string{before, acked} {
		for k, want := range m {
			if got, err := after.Get(ctx, k).Result(); got != want {
				t.Errorf("after the upgrade, GET %s = %q, %v; want %q", k, got, err, want)
			}
		}
	}
}
