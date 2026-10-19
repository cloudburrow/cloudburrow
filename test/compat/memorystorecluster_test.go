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

	cluster "cloud.google.com/go/redis/cluster/apiv1"
	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
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

func redisClusterAdminClient(t *testing.T, h *Harness) (*cluster.CloudRedisClusterClient, string) {
	t.Helper()
	endpoint := h.Endpoint(EnvMemorystoreAdmin)
	project := strings.TrimSpace(os.Getenv(EnvMemorystoreProject))
	if project == "" {
		t.Skipf("%s is not set", EnvMemorystoreProject)
	}
	c, err := cluster.NewCloudRedisClusterClient(h.Context(), option.WithEndpoint(endpoint),
		option.WithoutAuthentication(), option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, project
}

// waitClusterOp polls a cluster operation until it is done, each poll
// under a fresh harness context: a reshard of a real cluster can take
// longer than one context allows, as Google's takes minutes.
func waitClusterOp(t *testing.T, h *Harness, op *cluster.UpdateClusterOperation) (*clusterpb.Cluster, error) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Minute); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		cl, err := op.Poll(h.Context())
		if err != nil || op.Done() {
			return cl, err
		}
	}
	return nil, fmt.Errorf("operation %s is still running after 5 minutes", op.Name())
}

// memorystoreWait is a context for waiting on one Memorystore operation: a
// cluster's or instance's creation, starting its nodes and forming it, can
// outlast h.Context()'s minute on a loaded machine (#1568).
func memorystoreWait(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// primariesOnly reads CLUSTER NODES until it names n primaries and no
// replica, for up to 30 seconds: the client's view of the cluster can still
// hold the replicas just removed, and a reload from them came back with no
// nodes at all ("redis: cluster has no nodes").
func primariesOnly(h *Harness, rc *redis.ClusterClient, n int) (string, error) {
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(time.Second) {
		rc.ReloadState(h.Context())
		nodes, err := rc.ClusterNodes(h.Context()).Result()
		if err == nil && strings.Count(strings.TrimSpace(nodes), "\n")+1 == n && !strings.Contains(nodes, "slave") {
			return nodes, nil
		}
		if time.Now().After(deadline) {
			if err == nil {
				err = fmt.Errorf("not %d primaries alone", n)
			}
			return nodes, err
		}
	}
}

// clusterKeys writes n keys through a cluster client, spread over every
// slot range, and reads them back.
func clusterKeys(t *testing.T, h *Harness, rc *redis.ClusterClient, prefix string, n int, write bool) {
	t.Helper()
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("%s-%d", prefix, i)
		if write {
			if err := rc.Set(h.Context(), k, strconv.Itoa(i), 0).Err(); err != nil {
				t.Fatalf("SET %s: %v", k, err)
			}
		}
		if v, err := rc.Get(h.Context(), k).Result(); err != nil || v != strconv.Itoa(i) {
			t.Fatalf("GET %s = %q, %v", k, v, err)
		}
	}
}

// clusterMasters is the primaries CLUSTER SHARDS names that hold slots, by
// address.
func clusterMasters(t *testing.T, h *Harness, rc *redis.ClusterClient) map[string]bool {
	t.Helper()
	slots, err := rc.ClusterSlots(h.Context()).Result()
	if err != nil {
		t.Fatalf("CLUSTER SLOTS: %v", err)
	}
	out := map[string]bool{}
	covered := 0
	for _, s := range slots {
		covered += s.End - s.Start + 1
		if len(s.Nodes) > 0 {
			out[s.Nodes[0].Addr] = true
		}
	}
	if covered != 16384 {
		t.Errorf("CLUSTER SLOTS covers %d slots, want 16384", covered)
	}
	return out
}

// covers: google.cloud.redis.cluster.v1.CloudRedisCluster/CreateCluster, google.cloud.redis.cluster.v1.CloudRedisCluster/GetCluster, google.cloud.redis.cluster.v1.CloudRedisCluster/ListClusters, google.cloud.redis.cluster.v1.CloudRedisCluster/UpdateCluster, google.cloud.redis.cluster.v1.CloudRedisCluster/DeleteCluster
//
// TestMemorystoreRedisCluster (#1519), with cloud.google.com/go/redis/
// cluster/apiv1: CreateCluster's operation completes with an ACTIVE
// cluster of 3 shards of 1 replica, whose discovery endpoint a go-redis
// ClusterClient starts from, setting and getting keys in every shard; its
// psc_configs come back with a PSC connection. UpdateCluster to 4 shards
// moves slots, keys and all, to a fourth primary; to 0 replicas forgets the
// replicas; to node_type and redis_configs sets maxmemory and the policy on
// every node. DeleteCluster removes it.
func TestMemorystoreRedisCluster(t *testing.T) {
	// Forming the nodes, replacing them for a new node type and deleting
	// them each outlast h.Context()'s minute on a loaded runner; the
	// reshapes poll for five minutes (CI run 39038657629).
	ops, cancelOps := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancelOps()
	h := New(t)
	c, project := redisClusterAdminClient(t, h)
	parent := "projects/" + project + "/locations/us-central1"
	id := fmt.Sprintf("compat-cl-%d", time.Now().UnixNano()%1e8)
	name := parent + "/clusters/" + id

	op, err := c.CreateCluster(h.Context(), &clusterpb.CreateClusterRequest{Parent: parent, ClusterId: id, Cluster: &clusterpb.Cluster{
		ShardCount: proto.Int32(3), ReplicaCount: proto.Int32(1), NodeType: clusterpb.NodeType_REDIS_SHARED_CORE_NANO,
		AuthorizationMode: clusterpb.AuthorizationMode_AUTH_MODE_DISABLED,
		PscConfigs:        []*clusterpb.PscConfig{{Network: "projects/" + project + "/global/networks/default"}},
	}})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteCluster(h.Context(), &clusterpb.DeleteClusterRequest{Name: name}); err == nil {
			_ = waitLRODone(h.Context(), op)
		}
	})
	cl, err := waitLRO(ops, op)
	if err != nil {
		t.Fatalf("CreateCluster operation: %v", err)
	}
	if cl.GetName() != name || cl.GetState() != clusterpb.Cluster_ACTIVE || cl.GetShardCount() != 3 || cl.GetReplicaCount() != 1 ||
		len(cl.GetDiscoveryEndpoints()) != 1 || len(cl.GetPscConnections()) != 1 || cl.GetSizeGb() != 5 {
		t.Fatalf("created cluster = %v", cl)
	}
	ep := cl.GetDiscoveryEndpoints()[0]
	addr := net.JoinHostPort(ep.GetAddress(), strconv.Itoa(int(ep.GetPort())))
	rc := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{addr}})
	defer rc.Close()
	if n := len(clusterMasters(t, h, rc)); n != 3 {
		t.Errorf("CLUSTER SLOTS names %d primaries, want 3", n)
	}
	clusterKeys(t, h, rc, "k", 60, true)

	got, err := c.GetCluster(h.Context(), &clusterpb.GetClusterRequest{Name: name})
	if err != nil || got.GetUid() != cl.GetUid() {
		t.Errorf("GetCluster = %v, %v", got, err)
	}
	listed := false
	it := c.ListClusters(h.Context(), &clusterpb.ListClustersRequest{Parent: parent})
	for {
		l, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListClusters: %v", err)
		}
		listed = listed || l.GetName() == name
	}
	if !listed {
		t.Errorf("ListClusters does not list %s", name)
	}

	uop, err := c.UpdateCluster(h.Context(), &clusterpb.UpdateClusterRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"shard_count"}},
		Cluster: &clusterpb.Cluster{Name: name, ShardCount: proto.Int32(4)}})
	if err != nil {
		t.Fatalf("UpdateCluster shard_count: %v", err)
	}
	if cl, err = waitClusterOp(t, h, uop); err != nil || cl.GetShardCount() != 4 {
		t.Fatalf("UpdateCluster shard_count operation = %v, %v", cl, err)
	}
	rc.ReloadState(h.Context())
	if n := len(clusterMasters(t, h, rc)); n != 4 {
		t.Errorf("after 4 shards CLUSTER SLOTS names %d primaries, want 4", n)
	}
	clusterKeys(t, h, rc, "k", 60, false)

	uop, err = c.UpdateCluster(h.Context(), &clusterpb.UpdateClusterRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"replica_count"}},
		Cluster: &clusterpb.Cluster{Name: name, ReplicaCount: proto.Int32(0)}})
	if err != nil {
		t.Fatalf("UpdateCluster replica_count: %v", err)
	}
	if cl, err = waitClusterOp(t, h, uop); err != nil || cl.GetReplicaCount() != 0 {
		t.Fatalf("UpdateCluster replica_count operation = %v, %v", cl, err)
	}
	if nodes, err := primariesOnly(h, rc, 4); err != nil {
		t.Errorf("after 0 replicas CLUSTER NODES = %q, %v; want 4 primaries", nodes, err)
	}

	uop, err = c.UpdateCluster(h.Context(), &clusterpb.UpdateClusterRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"node_type", "redis_configs"}},
		Cluster: &clusterpb.Cluster{Name: name, NodeType: clusterpb.NodeType_REDIS_STANDARD_SMALL,
			RedisConfigs: map[string]string{"maxmemory-policy": "allkeys-lru"}}})
	if err != nil {
		t.Fatalf("UpdateCluster node_type: %v", err)
	}
	if cl, err = waitLRO(ops, uop); err != nil || cl.GetNodeType() != clusterpb.NodeType_REDIS_STANDARD_SMALL || cl.GetSizeGb() != 26 {
		t.Fatalf("UpdateCluster node_type operation = %v, %v", cl, err)
	}
	err = rc.ForEachShard(h.Context(), func(ctx2 context.Context, m *redis.Client) error {
		cfg, err := m.ConfigGet(ctx2, "maxmemory*").Result()
		if err != nil {
			return err
		}
		if cfg["maxmemory-policy"] != "allkeys-lru" || cfg["maxmemory"] != strconv.FormatInt(int64(6.5*(1<<30)), 10) {
			return fmt.Errorf("node %s: maxmemory %s, policy %s", m.Options().Addr, cfg["maxmemory"], cfg["maxmemory-policy"])
		}
		return nil
	})
	if err != nil {
		t.Errorf("after node_type: %v", err)
	}
	clusterKeys(t, h, rc, "k", 60, false)

	dop, err := c.DeleteCluster(h.Context(), &clusterpb.DeleteClusterRequest{Name: name})
	if err != nil {
		t.Fatalf("DeleteCluster: %v", err)
	}
	if err := waitLRODone(ops, dop); err != nil {
		t.Fatalf("DeleteCluster operation: %v", err)
	}
	if _, err := c.GetCluster(h.Context(), &clusterpb.GetClusterRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetCluster after delete = %v, want NOT_FOUND", err)
	}
}

// covers: google.cloud.redis.cluster.v1.CloudRedisCluster/CreateCluster
//
// TestMemorystoreRedisClusterRefusals (#1519): what a cluster here cannot
// do is refused, creating nothing: a customer-managed key that Cloud KMS
// cannot use is FAILED_PRECONDITION (#1557), one outside the cluster's
// region INVALID_ARGUMENT, a shard_count of 0 INVALID_ARGUMENT.
func TestMemorystoreRedisClusterRefusals(t *testing.T) {
	h := New(t)
	c, project := redisClusterAdminClient(t, h)
	parent := "projects/" + project + "/locations/us-central1"
	_, err := c.CreateCluster(h.Context(), &clusterpb.CreateClusterRequest{Parent: parent, ClusterId: "compat-kms", Cluster: &clusterpb.Cluster{
		ShardCount: proto.Int32(1), KmsKey: proto.String("projects/p/locations/us-central1/keyRings/r/cryptoKeys/k")}})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("CreateCluster with a kms_key Cloud KMS cannot use = %v, want FAILED_PRECONDITION", err)
	}
	_, err = c.CreateCluster(h.Context(), &clusterpb.CreateClusterRequest{Parent: parent, ClusterId: "compat-kms-region", Cluster: &clusterpb.Cluster{
		ShardCount: proto.Int32(1), KmsKey: proto.String("projects/p/locations/europe-west1/keyRings/r/cryptoKeys/k")}})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateCluster with a kms_key of another region = %v, want INVALID_ARGUMENT", err)
	}
	_, err = c.CreateCluster(h.Context(), &clusterpb.CreateClusterRequest{Parent: parent, ClusterId: "compat-zero", Cluster: &clusterpb.Cluster{
		ShardCount: proto.Int32(0)}})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateCluster of 0 shards = %v, want INVALID_ARGUMENT", err)
	}
}
