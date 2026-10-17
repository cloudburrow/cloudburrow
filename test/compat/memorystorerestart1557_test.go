//go:build compat

package compat

import (
	"context"
	"fmt"
	"net"
	"os"
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
)

// The Redis clusters and Valkey instances TestMemorystoreClustersAcrossRestart
// leaves for the run after CI's stop and up (#1557).
const restartProbeKeys = 40

// restartProbe is one Redis cluster or Valkey instance of the probe:
// persisted says whether its keys come back.
type restartProbe struct {
	id        string
	persisted bool
	valkey    *memorystorepb.Instance
	redis     *clusterpb.Cluster
}

func restartProbes(project string) []restartProbe {
	network := "projects/" + project + "/global/networks/default"
	psc := []*memorystorepb.PscAutoConnection{{ProjectId: project, Network: network}}
	return []restartProbe{
		{id: "restart-probe-vk-aof", persisted: true, valkey: &memorystorepb.Instance{ShardCount: 2, ReplicaCount: proto.Int32(1),
			NodeType: memorystorepb.Instance_SHARED_CORE_NANO, PscAutoConnections: psc,
			PersistenceConfig: &memorystorepb.PersistenceConfig{Mode: memorystorepb.PersistenceConfig_AOF,
				AofConfig: &memorystorepb.PersistenceConfig_AOFConfig{AppendFsync: memorystorepb.PersistenceConfig_AOFConfig_EVERY_SEC}}}},
		{id: "restart-probe-vk-rdb", persisted: true, valkey: &memorystorepb.Instance{Mode: memorystorepb.Instance_CLUSTER_DISABLED,
			ReplicaCount: proto.Int32(1), NodeType: memorystorepb.Instance_SHARED_CORE_NANO, PscAutoConnections: psc,
			PersistenceConfig: &memorystorepb.PersistenceConfig{Mode: memorystorepb.PersistenceConfig_RDB,
				RdbConfig: &memorystorepb.PersistenceConfig_RDBConfig{RdbSnapshotPeriod: memorystorepb.PersistenceConfig_RDBConfig_ONE_HOUR}}}},
		{id: "restart-probe-vk-off", valkey: &memorystorepb.Instance{Mode: memorystorepb.Instance_CLUSTER_DISABLED,
			ReplicaCount: proto.Int32(0), NodeType: memorystorepb.Instance_SHARED_CORE_NANO, PscAutoConnections: psc}},
		{id: "restart-probe-cl-rdb", persisted: true, redis: &clusterpb.Cluster{ShardCount: proto.Int32(2), ReplicaCount: proto.Int32(0),
			NodeType: clusterpb.NodeType_REDIS_SHARED_CORE_NANO, AuthorizationMode: clusterpb.AuthorizationMode_AUTH_MODE_DISABLED,
			PscConfigs: []*clusterpb.PscConfig{{Network: network}},
			PersistenceConfig: &clusterpb.ClusterPersistenceConfig{Mode: clusterpb.ClusterPersistenceConfig_RDB,
				RdbConfig: &clusterpb.ClusterPersistenceConfig_RDBConfig{RdbSnapshotPeriod: clusterpb.ClusterPersistenceConfig_RDBConfig_ONE_HOUR}}}},
		{id: "restart-probe-cl-off", redis: &clusterpb.Cluster{ShardCount: proto.Int32(1), ReplicaCount: proto.Int32(0),
			NodeType: clusterpb.NodeType_REDIS_SHARED_CORE_NANO, AuthorizationMode: clusterpb.AuthorizationMode_AUTH_MODE_DISABLED,
			PscConfigs: []*clusterpb.PscConfig{{Network: network}}}},
	}
}

// covers: google.cloud.memorystore.v1.Memorystore/CreateInstance, google.cloud.memorystore.v1.Memorystore/GetInstance, google.cloud.redis.cluster.v1.CloudRedisCluster/CreateCluster, google.cloud.redis.cluster.v1.CloudRedisCluster/GetCluster
//
// TestMemorystoreClustersAcrossRestart (#1557): a Redis cluster's and a
// Valkey instance's data follow their persistence_config across a restart
// of CloudBurrow. Without CLOUDBURROW_TEST_MEMORYSTORE_EXPECT it creates a
// Valkey instance in CLUSTER mode with AOF, one CLUSTER_DISABLED with RDB
// and one without persistence, and Redis clusters with RDB and without,
// and writes keys to each; CI then stops and starts the instance (the same
// state directory) and runs it again. "present" (persistent mode) wants
// each one ACTIVE again with its keys when it has persistence, and empty
// without, as Memorystore loses a node's data without persistence; then
// deletes them. "absent" (ephemeral mode) wants none of them.
func TestMemorystoreClustersAcrossRestart(t *testing.T) {
	h := New(t)
	vc, project := valkeyAdminClient(t, h)
	rcc, _ := redisClusterAdminClient(t, h)
	parent := "projects/" + project + "/locations/us-central1"
	expect := os.Getenv(envMemorystoreExpect)
	for _, p := range restartProbes(project) {
		t.Run(p.id, func(t *testing.T) {
			ctx := h.Context()
			var addr string
			var get func() (string, error)
			var name string
			if p.valkey != nil {
				name = parent + "/instances/" + p.id
				get = func() (string, error) {
					inst, err := vc.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: name})
					if err != nil {
						return "", err
					}
					return inst.GetState().String(), nil
				}
			} else {
				name = parent + "/clusters/" + p.id
				get = func() (string, error) {
					cl, err := rcc.GetCluster(ctx, &clusterpb.GetClusterRequest{Name: name})
					if err != nil {
						return "", err
					}
					return cl.GetState().String(), nil
				}
			}
			_, err := get()
			switch expect {
			case "absent":
				if status.Code(err) != codes.NotFound {
					t.Fatalf("ephemeral mode after stop/up: %s = %v, want NOT_FOUND", name, err)
				}
				return
			case "":
				if status.Code(err) == codes.NotFound {
					createRestartProbe(t, ctx, vc, rcc, parent, p)
				} else if err != nil {
					t.Fatal(err)
				}
			case "present":
				if err != nil {
					t.Fatalf("persistent mode after stop/up: %s = %v, want it kept", name, err)
				}
				defer deleteRestartProbe(vc, rcc, name, p)
			default:
				t.Fatalf("%s must be present or absent, not %q", envMemorystoreExpect, expect)
			}
			// ACTIVE, formed again after a restart.
			deadline := time.Now().Add(5 * time.Minute)
			for {
				st, err := get()
				if err != nil {
					t.Fatal(err)
				}
				if st == "ACTIVE" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("%s is %s, not ACTIVE, after 5 minutes", name, st)
				}
				time.Sleep(time.Second)
			}
			if p.valkey != nil {
				inst, err := vc.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: name})
				if err != nil {
					t.Fatal(err)
				}
				kind := memorystorepb.ConnectionType_CONNECTION_TYPE_DISCOVERY
				if inst.GetMode() == memorystorepb.Instance_CLUSTER_DISABLED {
					kind = memorystorepb.ConnectionType_CONNECTION_TYPE_PRIMARY
				}
				addr = valkeyEndpoint(t, inst, kind)
				if want := p.valkey.GetPersistenceConfig(); want != nil && inst.GetPersistenceConfig().GetMode() != want.GetMode() {
					t.Errorf("after stop/up persistence_config = %v", inst.GetPersistenceConfig())
				}
			} else {
				cl, err := rcc.GetCluster(ctx, &clusterpb.GetClusterRequest{Name: name})
				if err != nil {
					t.Fatal(err)
				}
				ep := cl.GetDiscoveryEndpoints()[0]
				addr = net.JoinHostPort(ep.GetAddress(), strconv.Itoa(int(ep.GetPort())))
			}
			var rc redis.UniversalClient
			if p.valkey != nil && p.valkey.GetMode() == memorystorepb.Instance_CLUSTER_DISABLED {
				rc = redis.NewClient(&redis.Options{Addr: addr})
			} else {
				rc = redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{addr}})
			}
			defer rc.Close()
			if expect == "" {
				for i := 0; i < restartProbeKeys; i++ {
					if err := rc.Set(ctx, fmt.Sprintf("probe-%d", i), strconv.Itoa(i), 0).Err(); err != nil {
						t.Fatalf("SET probe-%d: %v", i, err)
					}
				}
				return
			}
			missing := 0
			for i := 0; i < restartProbeKeys; i++ {
				v, err := rc.Get(ctx, fmt.Sprintf("probe-%d", i)).Result()
				switch {
				case err == redis.Nil:
					missing++
				case err != nil:
					t.Fatalf("GET probe-%d: %v", i, err)
				case v != strconv.Itoa(i):
					t.Errorf("GET probe-%d = %q, want %d", i, v, i)
				}
			}
			if p.persisted && missing != 0 {
				t.Errorf("with persistence after stop/up: %d of %d keys missing, want every key back", missing, restartProbeKeys)
			}
			if !p.persisted && missing != restartProbeKeys {
				t.Errorf("without persistence after stop/up: %d of %d keys kept, want none", restartProbeKeys-missing, restartProbeKeys)
			}
		})
	}
}

func createRestartProbe(t *testing.T, ctx context.Context, vc *memorystore.Client, rcc *cluster.CloudRedisClusterClient, parent string, p restartProbe) {
	t.Helper()
	if p.valkey != nil {
		op, err := vc.CreateInstance(ctx, &memorystorepb.CreateInstanceRequest{Parent: parent, InstanceId: p.id, Instance: p.valkey})
		if err != nil {
			t.Fatalf("CreateInstance %s: %v", p.id, err)
		}
		if _, err := op.Wait(ctx); err != nil {
			t.Fatalf("CreateInstance %s operation: %v", p.id, err)
		}
		return
	}
	op, err := rcc.CreateCluster(ctx, &clusterpb.CreateClusterRequest{Parent: parent, ClusterId: p.id, Cluster: p.redis})
	if err != nil {
		t.Fatalf("CreateCluster %s: %v", p.id, err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("CreateCluster %s operation: %v", p.id, err)
	}
}

func deleteRestartProbe(vc *memorystore.Client, rcc *cluster.CloudRedisClusterClient, name string, p restartProbe) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if p.valkey != nil {
		if op, err := vc.DeleteInstance(ctx, &memorystorepb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = op.Wait(ctx)
		}
		return
	}
	if op, err := rcc.DeleteCluster(ctx, &clusterpb.DeleteClusterRequest{Name: name}); err == nil {
		_ = op.Wait(ctx)
	}
}
