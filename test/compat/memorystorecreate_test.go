//go:build compat

package compat

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	redisadmin "cloud.google.com/go/redis/apiv1"
	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"github.com/redis/go-redis/v9"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func memorystoreAdminClient(t *testing.T, h *Harness) (*redisadmin.CloudRedisClient, string) {
	t.Helper()
	project := strings.TrimSpace(os.Getenv(EnvMemorystoreProject))
	if project == "" {
		t.Skipf("%s is not set", EnvMemorystoreProject)
	}
	c, err := redisadmin.NewCloudRedisClient(h.Context(), option.WithEndpoint(h.Endpoint(EnvMemorystoreAdmin)),
		option.WithoutAuthentication(), option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, project
}

// covers: google.cloud.redis.v1.CloudRedis/CreateInstance, google.cloud.redis.v1.CloudRedis/UpdateInstance, google.cloud.redis.v1.CloudRedis/DeleteInstance
//
// TestMemorystoreCreateConnectUpdateDelete (#1302), with cloud.google.com/
// go/redis/apiv1: CreateInstance's operation completes with a READY
// instance of its own Valkey server, at the host and port it reports; a
// go-redis client there needs the AUTH string GetInstanceAuthString
// returns, and sees memory_size_gb as maxmemory and redis_configs as the
// server's config. UpdateInstance resizes it and changes its labels and
// configs on the running server, which keeps its keys; DeleteInstance
// removes it, and the server with it.
func TestMemorystoreCreateConnectUpdateDelete(t *testing.T) {
	h := New(t)
	c, project := memorystoreAdminClient(t, h)
	ctx := memorystoreWait(t)
	parent := "projects/" + project + "/locations/us-central1"
	id := fmt.Sprintf("compat-%d", time.Now().UnixNano()%1e9)
	name := parent + "/instances/" + id

	op, err := c.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent, InstanceId: id,
		Instance: &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 2, AuthEnabled: true,
			DisplayName: "compat cache", Labels: map[string]string{"team": "a"},
			RedisConfigs: map[string]string{"maxmemory-policy": "allkeys-lru"}}})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(h.Context(), &redispb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = waitLRODone(h.Context(), op)
		}
	})
	inst, err := waitLRO(ctx, op)
	if err != nil {
		t.Fatalf("CreateInstance operation: %v", err)
	}
	if md, err := op.Metadata(); err != nil || md.GetVerb() != "create" || md.GetTarget() != name {
		t.Errorf("operation metadata = %v, %v", md, err)
	}
	if inst.GetName() != name || inst.GetState() != redispb.Instance_READY || inst.GetMemorySizeGb() != 2 ||
		!inst.GetAuthEnabled() || inst.GetRedisVersion() != "REDIS_7_2" || inst.GetDisplayName() != "compat cache" {
		t.Errorf("created instance = %v", inst)
	}
	if inst.GetPort() == 0 || inst.GetHost() == "" || h.Endpoint(EnvMemorystore) == net.JoinHostPort(inst.GetHost(), strconv.Itoa(int(inst.GetPort()))) {
		t.Fatalf("created instance's address %s:%d: want a server of its own", inst.GetHost(), inst.GetPort())
	}
	got, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
	if err != nil || got.GetPort() != inst.GetPort() {
		t.Errorf("GetInstance = %v, %v", got, err)
	}
	var listed bool
	it := c.ListInstances(ctx, &redispb.ListInstancesRequest{Parent: parent})
	for {
		l, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListInstances: %v", err)
		}
		listed = listed || l.GetName() == name
	}
	if !listed {
		t.Errorf("ListInstances does not list %s", name)
	}

	auth, err := c.GetInstanceAuthString(ctx, &redispb.GetInstanceAuthStringRequest{Name: name})
	if err != nil || auth.GetAuthString() == "" {
		t.Fatalf("GetInstanceAuthString = %v, %v", auth, err)
	}
	addr := net.JoinHostPort(inst.GetHost(), strconv.Itoa(int(inst.GetPort())))
	anon := redis.NewClient(&redis.Options{Addr: addr})
	defer anon.Close()
	if err := anon.Set(ctx, "k", "v", 0).Err(); err == nil || !strings.Contains(err.Error(), "NOAUTH") {
		t.Errorf("SET without AUTH = %v, want NOAUTH", err)
	}
	rc := redis.NewClient(&redis.Options{Addr: addr, Password: auth.GetAuthString()})
	defer rc.Close()
	if err := rc.Set(ctx, "k", "v", 0).Err(); err != nil {
		t.Fatalf("SET with AUTH: %v", err)
	}
	cfg, err := rc.ConfigGet(ctx, "max*").Result()
	if err != nil {
		t.Fatal(err)
	}
	if cfg["maxmemory"] != strconv.Itoa(2<<30) || cfg["maxmemory-policy"] != "allkeys-lru" {
		t.Errorf("server config = maxmemory %s, policy %s; want 2 GB, allkeys-lru", cfg["maxmemory"], cfg["maxmemory-policy"])
	}
	// Not the server `up` runs: the key is not there.
	if n, err := redisClient(t, h).Exists(ctx, "k").Result(); err == nil && n != 0 {
		t.Errorf("the key is on the server `up` runs too")
	}

	uop, err := c.UpdateInstance(ctx, &redispb.UpdateInstanceRequest{
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"memory_size_gb", "labels", "display_name", "redis_configs"}},
		Instance: &redispb.Instance{Name: name, MemorySizeGb: 3, DisplayName: "resized", Labels: map[string]string{"team": "b"},
			RedisConfigs: map[string]string{"notify-keyspace-events": "Ex"}}})
	if err != nil {
		t.Fatalf("UpdateInstance: %v", err)
	}
	updated, err := waitLRO(ctx, uop)
	if err != nil {
		t.Fatalf("UpdateInstance operation: %v", err)
	}
	if updated.GetMemorySizeGb() != 3 || updated.GetDisplayName() != "resized" || updated.GetLabels()["team"] != "b" ||
		updated.GetRedisConfigs()["notify-keyspace-events"] != "Ex" {
		t.Errorf("updated instance = %v", updated)
	}
	cfg, err = rc.ConfigGet(ctx, "*").Result()
	if err != nil {
		t.Fatal(err)
	}
	if cfg["maxmemory"] != strconv.Itoa(3<<30) || cfg["maxmemory-policy"] != "volatile-lru" || !strings.ContainsAny(cfg["notify-keyspace-events"], "xE") {
		t.Errorf("server config after update = maxmemory %s, policy %s, events %q", cfg["maxmemory"], cfg["maxmemory-policy"], cfg["notify-keyspace-events"])
	}
	if v, err := rc.Get(ctx, "k").Result(); err != nil || v != "v" {
		t.Errorf("GET after update = %q, %v; the running server keeps its keys", v, err)
	}

	if _, err := c.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent, InstanceId: id + "-ha",
		Instance: &redispb.Instance{Tier: redispb.Instance_STANDARD_HA, MemorySizeGb: 1, ReplicaCount: 3}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateInstance(STANDARD_HA, 3 replicas without read replicas) = %v, want INVALID_ARGUMENT", err)
	}

	dop, err := c.DeleteInstance(ctx, &redispb.DeleteInstanceRequest{Name: name})
	if err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	if err := waitLRODone(ctx, dop); err != nil {
		t.Fatalf("DeleteInstance operation: %v", err)
	}
	if _, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetInstance after delete = %v, want NOT_FOUND", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for rc.Ping(ctx).Err() == nil {
		if time.Now().After(deadline) {
			t.Fatal("the deleted instance's server still answers")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// restartProbeInstance is the created instance TestMemorystoreCreatedAcrossRestart
// leaves for the run after CI's stop and up.
const restartProbeInstance = "restart-probe"

// covers: google.cloud.redis.v1.CloudRedis/CreateInstance, google.cloud.redis.v1.CloudRedis/GetInstance, google.cloud.redis.v1.CloudRedis/GetInstanceAuthString
//
// TestMemorystoreCreatedAcrossRestart (#1302): a created instance follows
// the CloudBurrow instance's mode, as the server `up` runs does. Without
// CLOUDBURROW_TEST_MEMORYSTORE_EXPECT it creates restart-probe, with AUTH,
// and writes a key; CI then restarts the instance and runs it again:
// "present" (persistent mode) wants the instance back, READY, at an address
// of its own with the same AUTH string and the key; "absent" (ephemeral)
// wants no such instance.
func TestMemorystoreCreatedAcrossRestart(t *testing.T) {
	createdAcrossRestart(t, restartProbeInstance, nil)
}

// covers: google.cloud.redis.v1.CloudRedis/CreateInstance, google.cloud.redis.v1.CloudRedis/GetInstance
//
// TestMemorystoreRDBAcrossRestart (#1384) is TestMemorystoreCreatedAcrossRestart
// for an instance with RDB persistence (persistence_config RDB, ONE_HOUR),
// restart-probe-rdb: its server keeps no append-only file, and the key
// comes back after stop and up in persistent mode from the RDB snapshot
// the server writes on its volume when it stops.
func TestMemorystoreRDBAcrossRestart(t *testing.T) {
	createdAcrossRestart(t, restartProbeInstance+"-rdb",
		&redispb.PersistenceConfig{PersistenceMode: redispb.PersistenceConfig_RDB, RdbSnapshotPeriod: redispb.PersistenceConfig_ONE_HOUR})
}

func createdAcrossRestart(t *testing.T, id string, pc *redispb.PersistenceConfig) {
	h := New(t)
	c, project := memorystoreAdminClient(t, h)
	ctx := memorystoreWait(t)
	parent := "projects/" + project + "/locations/us-central1"
	name := parent + "/instances/" + id
	expect := os.Getenv(envMemorystoreExpect)
	if expect == "absent" {
		if _, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name}); status.Code(err) != codes.NotFound {
			t.Fatalf("ephemeral mode after stop/up: GetInstance = %v, want NOT_FOUND", err)
		}
		return
	}
	if expect == "" {
		if _, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name}); status.Code(err) == codes.NotFound {
			op, err := c.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent, InstanceId: id,
				Instance: &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1, AuthEnabled: true, PersistenceConfig: pc}})
			if err != nil {
				t.Fatalf("CreateInstance: %v", err)
			}
			if _, err := waitLRO(ctx, op); err != nil {
				t.Fatalf("CreateInstance operation: %v", err)
			}
		}
	}
	var inst *redispb.Instance
	deadline := time.Now().Add(3 * time.Minute)
	for {
		var err error
		if inst, err = c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name}); err != nil {
			t.Fatalf("GetInstance: %v", err)
		}
		if inst.GetState() == redispb.Instance_READY {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("instance %s is %s, not READY, after 3 minutes: %s", name, inst.GetState(), inst.GetStatusMessage())
		}
		time.Sleep(time.Second)
	}
	auth, err := c.GetInstanceAuthString(ctx, &redispb.GetInstanceAuthStringRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	rc := redis.NewClient(&redis.Options{Addr: net.JoinHostPort(inst.GetHost(), strconv.Itoa(int(inst.GetPort()))), Password: auth.GetAuthString()})
	defer rc.Close()
	if expect == "" {
		if err := rc.Set(ctx, "probe", "written-before-stop", 0).Err(); err != nil {
			t.Fatal(err)
		}
		return
	}
	if got, err := rc.Get(ctx, "probe").Result(); err != nil || got != "written-before-stop" {
		t.Fatalf("persistent mode after stop/up: GET = %q, %v; want the key written before stop", got, err)
	}
	if pc != nil {
		if inst.GetPersistenceConfig().GetPersistenceMode() != pc.GetPersistenceMode() {
			t.Errorf("after stop/up persistence_config = %v", inst.GetPersistenceConfig())
		}
		if cfg, err := rc.ConfigGet(ctx, "appendonly").Result(); err != nil || cfg["appendonly"] != "no" {
			t.Errorf("after stop/up appendonly = %v, %v; want no: the key came from the RDB snapshot", cfg, err)
		}
	}
}
