//go:build compat

package compat

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	memorystore "cloud.google.com/go/memorystore/apiv1"
	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	"github.com/redis/go-redis/v9"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func valkeyAdminClient(t *testing.T, h *Harness) (*memorystore.Client, string) {
	t.Helper()
	endpoint := h.Endpoint(EnvMemorystoreAdmin)
	project := strings.TrimSpace(os.Getenv(EnvMemorystoreProject))
	if project == "" {
		t.Skipf("%s is not set", EnvMemorystoreProject)
	}
	c, err := memorystore.NewClient(h.Context(), option.WithEndpoint(endpoint),
		option.WithoutAuthentication(), option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, project
}

// waitValkeyOp polls an update until it is done, each poll under a fresh
// harness context: a reshard of a real cluster can outlast one.
func waitValkeyOp(t *testing.T, h *Harness, op *memorystore.UpdateInstanceOperation) (*memorystorepb.Instance, error) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Minute); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		inst, err := op.Poll(h.Context())
		if err != nil || op.Done() {
			return inst, err
		}
	}
	return nil, fmt.Errorf("operation %s is still running after 5 minutes", op.Name())
}

func valkeyEndpoint(t *testing.T, inst *memorystorepb.Instance, kind memorystorepb.ConnectionType) string {
	t.Helper()
	conns := inst.GetPscAutoConnections()
	for _, e := range inst.GetEndpoints() {
		for _, c := range e.GetConnections() {
			conns = append(conns, c.GetPscAutoConnection())
		}
	}
	for _, p := range conns {
		if p != nil && p.GetConnectionType() == kind {
			return net.JoinHostPort(p.GetIpAddress(), strconv.Itoa(int(p.GetPort())))
		}
	}
	t.Fatalf("instance %s has no %s endpoint: %v", inst.GetName(), kind, inst.GetEndpoints())
	return ""
}

// covers: google.cloud.memorystore.v1.Memorystore/CreateInstance, google.cloud.memorystore.v1.Memorystore/GetInstance, google.cloud.memorystore.v1.Memorystore/ListInstances, google.cloud.memorystore.v1.Memorystore/UpdateInstance, google.cloud.memorystore.v1.Memorystore/DeleteInstance
//
// TestMemorystoreValkeyInstance (#1519), with cloud.google.com/go/
// memorystore/apiv1: CreateInstance's operation completes with an ACTIVE
// instance in CLUSTER mode of 3 shards of 1 replica, whose endpoints carry
// a DISCOVERY connection, and discovery_endpoints the same address (no
// psc_auto_connections, as they were asked as endpoints); a go-redis ClusterClient started there sets and gets
// keys across every slot range. UpdateInstance to 4 shards moves slots,
// keys and all, to a fourth primary; to 0 replicas removes the replicas;
// node_type, engine_configs and persistence_config are set on every node.
// DeleteInstance removes it.
func TestMemorystoreValkeyInstance(t *testing.T) {
	h := New(t)
	c, project := valkeyAdminClient(t, h)
	parent := "projects/" + project + "/locations/us-central1"
	id := fmt.Sprintf("compat-vk-%d", time.Now().UnixNano()%1e8)
	name := parent + "/instances/" + id
	network := "projects/" + project + "/global/networks/default"

	op, err := c.CreateInstance(h.Context(), &memorystorepb.CreateInstanceRequest{Parent: parent, InstanceId: id, Instance: &memorystorepb.Instance{
		ShardCount: 3, ReplicaCount: proto.Int32(1), NodeType: memorystorepb.Instance_SHARED_CORE_NANO,
		Labels: map[string]string{"env": "compat"},
		Endpoints: []*memorystorepb.Instance_InstanceEndpoint{{Connections: []*memorystorepb.Instance_ConnectionDetail{{
			Connection: &memorystorepb.Instance_ConnectionDetail_PscAutoConnection{PscAutoConnection: &memorystorepb.PscAutoConnection{
				ProjectId: project, Network: network}}}}}},
	}})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(h.Context(), &memorystorepb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = op.Wait(h.Context())
		}
	})
	inst, err := op.Wait(h.Context())
	if err != nil {
		t.Fatalf("CreateInstance operation: %v", err)
	}
	if inst.GetName() != name || inst.GetState() != memorystorepb.Instance_ACTIVE || inst.GetShardCount() != 3 || inst.GetReplicaCount() != 1 ||
		inst.GetMode() != memorystorepb.Instance_CLUSTER || inst.GetEngineVersion() != "VALKEY_8_0" || inst.GetNodeConfig().GetSizeGb() != 1.4 ||
		len(inst.GetDiscoveryEndpoints()) != 1 || len(inst.GetEndpoints()) != 1 || len(inst.GetPscAutoConnections()) != 0 || inst.GetLabels()["env"] != "compat" {
		t.Fatalf("created instance = %v", inst)
	}
	addr := valkeyEndpoint(t, inst, memorystorepb.ConnectionType_CONNECTION_TYPE_DISCOVERY)
	de := inst.GetDiscoveryEndpoints()[0]
	if got := net.JoinHostPort(de.GetAddress(), strconv.Itoa(int(de.GetPort()))); got != addr || de.GetNetwork() != network {
		t.Errorf("discovery_endpoints[0] = %v, want %s on %s", de, addr, network)
	}
	if p := inst.GetEndpoints()[0].GetConnections()[0].GetPscAutoConnection(); p.GetNetwork() != network || p.GetPscConnectionStatus() != memorystorepb.PscConnectionStatus_ACTIVE {
		t.Errorf("endpoints[0].connections[0] = %v", p)
	}
	rc := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{addr}})
	defer rc.Close()
	if n := len(clusterMasters(t, h, rc)); n != 3 {
		t.Errorf("CLUSTER SLOTS names %d primaries, want 3", n)
	}
	clusterKeys(t, h, rc, "vk", 60, true)

	got, err := c.GetInstance(h.Context(), &memorystorepb.GetInstanceRequest{Name: name})
	if err != nil || got.GetUid() != inst.GetUid() {
		t.Errorf("GetInstance = %v, %v", got, err)
	}
	listed := false
	it := c.ListInstances(h.Context(), &memorystorepb.ListInstancesRequest{Parent: "projects/" + project + "/locations/-"})
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

	uop, err := c.UpdateInstance(h.Context(), &memorystorepb.UpdateInstanceRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"shard_count"}},
		Instance: &memorystorepb.Instance{Name: name, ShardCount: 4}})
	if err != nil {
		t.Fatalf("UpdateInstance shard_count: %v", err)
	}
	if inst, err = waitValkeyOp(t, h, uop); err != nil || inst.GetShardCount() != 4 {
		t.Fatalf("UpdateInstance shard_count operation = %v, %v", inst, err)
	}
	rc.ReloadState(h.Context())
	if n := len(clusterMasters(t, h, rc)); n != 4 {
		t.Errorf("after 4 shards CLUSTER SLOTS names %d primaries, want 4", n)
	}
	clusterKeys(t, h, rc, "vk", 60, false)

	uop, err = c.UpdateInstance(h.Context(), &memorystorepb.UpdateInstanceRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"replica_count"}},
		Instance: &memorystorepb.Instance{Name: name, ReplicaCount: proto.Int32(0)}})
	if err != nil {
		t.Fatalf("UpdateInstance replica_count: %v", err)
	}
	if inst, err = waitValkeyOp(t, h, uop); err != nil || inst.GetReplicaCount() != 0 {
		t.Fatalf("UpdateInstance replica_count operation = %v, %v", inst, err)
	}
	nodes, err := rc.ClusterNodes(h.Context()).Result()
	if err != nil || strings.Count(strings.TrimSpace(nodes), "\n")+1 != 4 || strings.Contains(nodes, "slave") {
		t.Errorf("after 0 replicas CLUSTER NODES = %q, %v; want 4 primaries", nodes, err)
	}

	uop, err = c.UpdateInstance(h.Context(), &memorystorepb.UpdateInstanceRequest{
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"node_type", "engine_configs", "persistence_config"}},
		Instance: &memorystorepb.Instance{Name: name, NodeType: memorystorepb.Instance_STANDARD_SMALL,
			EngineConfigs: map[string]string{"maxmemory-policy": "allkeys-lru"},
			PersistenceConfig: &memorystorepb.PersistenceConfig{Mode: memorystorepb.PersistenceConfig_AOF,
				AofConfig: &memorystorepb.PersistenceConfig_AOFConfig{AppendFsync: memorystorepb.PersistenceConfig_AOFConfig_ALWAYS}}}})
	if err != nil {
		t.Fatalf("UpdateInstance node_type: %v", err)
	}
	if inst, err = uop.Wait(h.Context()); err != nil || inst.GetNodeType() != memorystorepb.Instance_STANDARD_SMALL ||
		inst.GetPersistenceConfig().GetMode() != memorystorepb.PersistenceConfig_AOF {
		t.Fatalf("UpdateInstance node_type operation = %v, %v", inst, err)
	}
	err = rc.ForEachShard(h.Context(), func(ctx2 context.Context, m *redis.Client) error {
		cfg, err := m.ConfigGet(ctx2, "*").Result()
		if err != nil {
			return err
		}
		if cfg["maxmemory-policy"] != "allkeys-lru" || cfg["maxmemory"] != strconv.FormatInt(int64(6.5*(1<<30)), 10) ||
			cfg["appendonly"] != "yes" || cfg["appendfsync"] != "always" {
			return fmt.Errorf("node %s: maxmemory %s, policy %s, appendonly %s, appendfsync %s", m.Options().Addr,
				cfg["maxmemory"], cfg["maxmemory-policy"], cfg["appendonly"], cfg["appendfsync"])
		}
		return nil
	})
	if err != nil {
		t.Errorf("after node_type: %v", err)
	}
	clusterKeys(t, h, rc, "vk", 60, false)

	dop, err := c.DeleteInstance(h.Context(), &memorystorepb.DeleteInstanceRequest{Name: name})
	if err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	if err := dop.Wait(h.Context()); err != nil {
		t.Fatalf("DeleteInstance operation: %v", err)
	}
	if _, err := c.GetInstance(h.Context(), &memorystorepb.GetInstanceRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetInstance after delete = %v, want NOT_FOUND", err)
	}
}

// covers: google.cloud.memorystore.v1.Memorystore/CreateInstance, google.cloud.memorystore.v1.Memorystore/UpdateInstance, google.cloud.memorystore.v1.Memorystore/DeleteInstance
//
// TestMemorystoreValkeyClusterDisabled (#1519): an instance in
// CLUSTER_DISABLED mode is a primary and its replica, plain Valkey servers:
// its psc_auto_connections, as asked, carry a PRIMARY and a READER
// connection, and it has no discovery endpoint; a key written to
// the primary with an ordinary client is read from the reader, which
// refuses writes (READONLY). Adding a replica starts one that syncs.
func TestMemorystoreValkeyClusterDisabled(t *testing.T) {
	h := New(t)
	c, project := valkeyAdminClient(t, h)
	parent := "projects/" + project + "/locations/us-central1"
	id := fmt.Sprintf("compat-vd-%d", time.Now().UnixNano()%1e8)
	name := parent + "/instances/" + id
	op, err := c.CreateInstance(h.Context(), &memorystorepb.CreateInstanceRequest{Parent: parent, InstanceId: id, Instance: &memorystorepb.Instance{
		Mode: memorystorepb.Instance_CLUSTER_DISABLED, ReplicaCount: proto.Int32(1), NodeType: memorystorepb.Instance_SHARED_CORE_NANO,
		PscAutoConnections: []*memorystorepb.PscAutoConnection{{ProjectId: project, Network: "projects/" + project + "/global/networks/default"}},
		PersistenceConfig: &memorystorepb.PersistenceConfig{Mode: memorystorepb.PersistenceConfig_RDB,
			RdbConfig: &memorystorepb.PersistenceConfig_RDBConfig{RdbSnapshotPeriod: memorystorepb.PersistenceConfig_RDBConfig_ONE_HOUR}},
	}})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(h.Context(), &memorystorepb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = op.Wait(h.Context())
		}
	})
	inst, err := op.Wait(h.Context())
	if err != nil {
		t.Fatalf("CreateInstance operation: %v", err)
	}
	if inst.GetMode() != memorystorepb.Instance_CLUSTER_DISABLED || inst.GetShardCount() != 1 || len(inst.GetDiscoveryEndpoints()) != 0 ||
		len(inst.GetPscAutoConnections()) != 2 || len(inst.GetEndpoints()) != 0 {
		t.Fatalf("created instance = %v", inst)
	}
	primary := redis.NewClient(&redis.Options{Addr: valkeyEndpoint(t, inst, memorystorepb.ConnectionType_CONNECTION_TYPE_PRIMARY)})
	defer primary.Close()
	reader := redis.NewClient(&redis.Options{Addr: valkeyEndpoint(t, inst, memorystorepb.ConnectionType_CONNECTION_TYPE_READER)})
	defer reader.Close()
	if err := primary.Set(h.Context(), "greeting", "hello", 0).Err(); err != nil {
		t.Fatalf("SET on the primary: %v", err)
	}
	if save, _ := primary.ConfigGet(h.Context(), "save").Result(); save["save"] != "3600 1" {
		t.Errorf("RDB persistence: save = %q, want \"3600 1\"", save["save"])
	}
	var v string
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if v, _ = reader.Get(h.Context(), "greeting").Result(); v == "hello" {
			break
		}
	}
	if v != "hello" {
		t.Errorf("GET on the reader = %q, want hello", v)
	}
	if err := reader.Set(h.Context(), "x", "y", 0).Err(); err == nil || !strings.Contains(err.Error(), "READONLY") {
		t.Errorf("SET on the reader = %v, want READONLY", err)
	}
	uop, err := c.UpdateInstance(h.Context(), &memorystorepb.UpdateInstanceRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"replica_count"}},
		Instance: &memorystorepb.Instance{Name: name, ReplicaCount: proto.Int32(2)}})
	if err != nil {
		t.Fatalf("UpdateInstance replica_count: %v", err)
	}
	if inst, err = waitValkeyOp(t, h, uop); err != nil || inst.GetReplicaCount() != 2 {
		t.Fatalf("UpdateInstance replica_count operation = %v, %v", inst, err)
	}
	info, err := primary.Info(h.Context(), "replication").Result()
	if err != nil || !strings.Contains(info, "connected_slaves:2") {
		t.Errorf("INFO replication on the primary = %q, %v; want 2 replicas", info, err)
	}
	if _, err := c.UpdateInstance(h.Context(), &memorystorepb.UpdateInstanceRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"shard_count"}},
		Instance: &memorystorepb.Instance{Name: name, ShardCount: 2}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("UpdateInstance of 2 shards in CLUSTER_DISABLED mode = %v, want INVALID_ARGUMENT", err)
	}
}

// covers: google.cloud.memorystore.v1.Memorystore/CreateInstance, google.cloud.memorystore.v1.Memorystore/GetCertificateAuthority
//
// TestMemorystoreValkeyRefusals (#1519): what an instance here cannot do is
// refused, creating nothing: TLS and token auth are UNIMPLEMENTED with the
// reason, as is an engine version with no image; shard_count 0 in CLUSTER
// mode, an engine config Memorystore does not take and a certificate
// authority of an unknown instance are refused.
func TestMemorystoreValkeyRefusals(t *testing.T) {
	h := New(t)
	c, project := valkeyAdminClient(t, h)
	parent := "projects/" + project + "/locations/us-central1"
	for _, tc := range []struct {
		what string
		in   *memorystorepb.Instance
		want codes.Code
	}{
		{"TLS", &memorystorepb.Instance{ShardCount: 1, TransitEncryptionMode: memorystorepb.Instance_SERVER_AUTHENTICATION}, codes.Unimplemented},
		{"token auth", &memorystorepb.Instance{ShardCount: 1, AuthorizationMode: memorystorepb.Instance_TOKEN_AUTH}, codes.Unimplemented},
		{"VALKEY_7_2", &memorystorepb.Instance{ShardCount: 1, EngineVersion: "VALKEY_7_2"}, codes.Unimplemented},
		{"0 shards", &memorystorepb.Instance{}, codes.InvalidArgument},
		{"an unknown config", &memorystorepb.Instance{ShardCount: 1, EngineConfigs: map[string]string{"appendonly": "yes"}}, codes.InvalidArgument},
		{"2 shards with cluster mode disabled", &memorystorepb.Instance{ShardCount: 2, Mode: memorystorepb.Instance_CLUSTER_DISABLED}, codes.InvalidArgument},
	} {
		_, err := c.CreateInstance(h.Context(), &memorystorepb.CreateInstanceRequest{Parent: parent, InstanceId: "compat-refused", Instance: tc.in})
		if status.Code(err) != tc.want {
			t.Errorf("CreateInstance with %s = %v, want %s", tc.what, err, tc.want)
		}
	}
	_, err := c.GetCertificateAuthority(h.Context(), &memorystorepb.GetCertificateAuthorityRequest{Name: parent + "/instances/compat-none/certificateAuthority"})
	if status.Code(err) != codes.NotFound {
		t.Errorf("GetCertificateAuthority of an unknown instance = %v, want NOT_FOUND", err)
	}
}
