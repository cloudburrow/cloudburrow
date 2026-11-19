//go:build compat

package compat

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	redispb "cloud.google.com/go/redis/apiv1/redispb"
	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/service/redisadmin"
)

// deleteServerPod deletes the pod of a Memorystore server, app=<server>,
// in this instance's cluster only.
func deleteServerPod(ctx context.Context, t *testing.T, kubeconfig, app string) {
	t.Helper()
	ns, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig, "get", "pods", "-A", "-l", "app="+app,
		"-o", "jsonpath={.items[0].metadata.namespace}").Output()
	if err != nil || len(ns) == 0 {
		t.Fatalf("find the pod of %s: %v", app, err)
	}
	if out, err := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfig, "-n", string(ns), "delete", "pod", "-l", "app="+app, "--wait=false").CombinedOutput(); err != nil {
		t.Fatalf("kubectl delete pod: %v: %s", err, out)
	}
}

func failoverKubeconfig(t *testing.T) string {
	t.Helper()
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	if kubeconfig == "" {
		t.Skip("CLOUDBURROW_TEST_KUBECONFIG is required to delete the primary's pod")
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is required to delete the primary's pod")
	}
	return kubeconfig
}

// clusterAfterPrimaryLoss is the check of a one-shard, one-replica
// cluster whose primary's pod is deleted: discovery reads the discovery
// endpoint and the state from the admin API.
func clusterAfterPrimaryLoss(t *testing.T, ctx context.Context, kubeconfig, base string, discovery func() (string, bool)) {
	addr, _ := discovery()
	rc := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{addr}})
	if err := rc.Set(ctx, "k", "before", 0).Err(); err != nil {
		rc.Close()
		t.Fatalf("SET: %v", err)
	}
	direct := redis.NewClient(&redis.Options{Addr: addr})
	eventuallyTrue(t, "the replica has the key", func() bool {
		n, err := direct.Do(ctx, "WAIT", 1, 1000).Int()
		return err == nil && n >= 1
	})
	direct.Close()
	rc.Close()

	// Shard 0's primary is node 0.
	deleteServerPod(ctx, t, kubeconfig, base+"-0")
	deleted := time.Now()
	var moved string
	for end := time.Now().Add(2 * time.Minute); ; time.Sleep(500 * time.Millisecond) {
		a, active := discovery()
		if a != addr && active {
			c := redis.NewClient(&redis.Options{Addr: a})
			r := role(ctx, t, c)
			c.Close()
			if r == "master" {
				moved = a
				break
			}
		}
		if time.Now().After(end) {
			t.Fatalf("the discovery endpoint did not move to a promoted replica within 2 minutes of deleting the primary's pod (still %s)", a)
		}
	}
	t.Logf("failed over %v after the pod was deleted", time.Since(deleted).Round(time.Second))
	rc2 := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{moved}})
	defer rc2.Close()
	if v, err := rc2.Get(ctx, "k").Result(); err != nil || v != "before" {
		t.Errorf("GET k after the failover = %q, %v; want before", v, err)
	}
	if err := rc2.Set(ctx, "k2", "after", 0).Err(); err != nil {
		t.Fatalf("SET after the failover: %v", err)
	}
	// The deleted node comes back and rejoins as a replica: the shard keeps
	// its replica, and the cluster knows no failed node.
	primary := redis.NewClient(&redis.Options{Addr: moved})
	defer primary.Close()
	var nodes string
	for end := time.Now().Add(4 * time.Minute); ; time.Sleep(time.Second) {
		var err error
		nodes, err = primary.ClusterNodes(ctx).Result()
		n, werr := primary.Do(ctx, "WAIT", 1, 1000).Int()
		lines := strings.Split(strings.TrimSpace(nodes), "\n")
		if err == nil && werr == nil && n >= 1 && len(lines) == 2 && !strings.Contains(nodes, "fail") && strings.Contains(nodes, "slave") {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("the deleted node did not rejoin as a replica within 4 minutes: CLUSTER NODES %q, %v; WAIT %d, %v", nodes, err, n, werr)
		}
	}
	if info, err := primary.ClusterInfo(ctx).Result(); err != nil || !strings.Contains(info, "cluster_state:ok") || !strings.Contains(info, "cluster_known_nodes:2") {
		t.Errorf("CLUSTER INFO after the rejoin = %q, %v", info, err)
	}
}

// covers: google.cloud.redis.cluster.v1.CloudRedisCluster/CreateCluster, google.cloud.redis.cluster.v1.CloudRedisCluster/GetCluster
//
// TestRedisClusterAutomaticFailover (#1910), with cloud.google.com/go/
// redis/cluster/apiv1 and go-redis: a cluster of one shard and one
// replica, whose primary's pod is deleted (kubectl, this instance's own
// pod). Valkey's own cluster failover never promotes a replica of a
// one-shard cluster (it needs a majority of the primaries' votes), so this
// is CloudBurrow's: the replica is promoted, discovery_endpoints moves to
// it, the cluster stays ACTIVE and serves the key written before; the
// deleted node comes back with a new identity and rejoins as a replica,
// so the shard keeps its replica (Google: automatic failover, the node
// replaced, https://docs.cloud.google.com/memorystore/docs/cluster/ha-and-replicas).
func TestRedisClusterAutomaticFailover(t *testing.T) {
	h := New(t)
	c, project := redisClusterAdminClient(t, h)
	kubeconfig := failoverKubeconfig(t)
	ctx := memorystoreWait(t)
	parent := "projects/" + project + "/locations/us-central1"
	id := fmt.Sprintf("fo-%d", time.Now().UnixNano()%1e8)
	name := parent + "/clusters/" + id
	op, err := c.CreateCluster(ctx, &clusterpb.CreateClusterRequest{Parent: parent, ClusterId: id, Cluster: &clusterpb.Cluster{
		ShardCount: proto.Int32(1), ReplicaCount: proto.Int32(1), NodeType: clusterpb.NodeType_REDIS_SHARED_CORE_NANO,
		AuthorizationMode: clusterpb.AuthorizationMode_AUTH_MODE_DISABLED,
		PscConfigs:        []*clusterpb.PscConfig{{Network: "projects/" + project + "/global/networks/default"}},
	}})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteCluster(context.Background(), &clusterpb.DeleteClusterRequest{Name: name}); err == nil {
			_ = waitLRODone(context.Background(), op)
		}
	})
	if _, err := waitLRO(ctx, op); err != nil {
		t.Fatalf("CreateCluster operation: %v", err)
	}
	clusterAfterPrimaryLoss(t, ctx, kubeconfig, redisadmin.ValkeyServerBase(name), func() (string, bool) {
		cl, err := c.GetCluster(ctx, &clusterpb.GetClusterRequest{Name: name})
		if err != nil || len(cl.GetDiscoveryEndpoints()) == 0 {
			t.Fatalf("GetCluster: %v %v", cl, err)
		}
		ep := cl.GetDiscoveryEndpoints()[0]
		return net.JoinHostPort(ep.GetAddress(), strconv.Itoa(int(ep.GetPort()))), cl.GetState() == clusterpb.Cluster_ACTIVE
	})
}

// covers: google.cloud.memorystore.v1.Memorystore/CreateInstance, google.cloud.memorystore.v1.Memorystore/GetInstance
//
// TestValkeyClusterModeAutomaticFailover (#1910) is
// TestRedisClusterAutomaticFailover for a Memorystore for Valkey instance
// in CLUSTER mode, with cloud.google.com/go/memorystore/apiv1: its
// DISCOVERY endpoint moves to the promoted replica, the instance stays
// ACTIVE, and the deleted node rejoins as a replica.
func TestValkeyClusterModeAutomaticFailover(t *testing.T) {
	h := New(t)
	c, project := valkeyAdminClient(t, h)
	kubeconfig := failoverKubeconfig(t)
	ctx := memorystoreWait(t)
	parent := "projects/" + project + "/locations/us-central1"
	id := fmt.Sprintf("vfo-%d", time.Now().UnixNano()%1e8)
	name := parent + "/instances/" + id
	op, err := c.CreateInstance(ctx, &memorystorepb.CreateInstanceRequest{Parent: parent, InstanceId: id, Instance: &memorystorepb.Instance{
		ShardCount: 1, ReplicaCount: proto.Int32(1), NodeType: memorystorepb.Instance_SHARED_CORE_NANO,
		Endpoints: []*memorystorepb.Instance_InstanceEndpoint{{Connections: []*memorystorepb.Instance_ConnectionDetail{{
			Connection: &memorystorepb.Instance_ConnectionDetail_PscAutoConnection{PscAutoConnection: &memorystorepb.PscAutoConnection{
				ProjectId: project, Network: "projects/" + project + "/global/networks/default"}}}}}},
	}})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(context.Background(), &memorystorepb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = waitLRODone(context.Background(), op)
		}
	})
	if _, err := waitLRO(ctx, op); err != nil {
		t.Fatalf("CreateInstance operation: %v", err)
	}
	clusterAfterPrimaryLoss(t, ctx, kubeconfig, redisadmin.ValkeyServerBase(name), func() (string, bool) {
		inst, err := c.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: name})
		if err != nil {
			t.Fatalf("GetInstance: %v", err)
		}
		return valkeyEndpoint(t, inst, memorystorepb.ConnectionType_CONNECTION_TYPE_DISCOVERY), inst.GetState() == memorystorepb.Instance_ACTIVE
	})
}

// covers: google.cloud.redis.v1.CloudRedis/CreateInstance, google.cloud.redis.v1.CloudRedis/GetInstance
//
// TestMemorystoreAutomaticFailoverReadEndpoint (#1910), with cloud.google.com/
// go/redis/apiv1 and go-redis: a Standard tier instance with read
// replicas (5 GB, two replicas) whose primary's pod is deleted fails over
// on its own, and its read endpoint keeps serving reads of the key written
// before, at the same address, on every new connection.
func TestMemorystoreAutomaticFailoverReadEndpoint(t *testing.T) {
	h := New(t)
	c, project := memorystoreAdminClient(t, h)
	kubeconfig := failoverKubeconfig(t)
	ctx := memorystoreWait(t)
	parent := "projects/" + project + "/locations/us-central1"
	id := fmt.Sprintf("rfo-%d", time.Now().UnixNano()%1e8)
	name := parent + "/instances/" + id
	op, err := c.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent, InstanceId: id, Instance: &redispb.Instance{
		Tier: redispb.Instance_STANDARD_HA, MemorySizeGb: 5, LocationId: "us-central1-a",
		ReadReplicasMode: redispb.Instance_READ_REPLICAS_ENABLED, ReplicaCount: 2}})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(context.Background(), &redispb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = waitLRODone(context.Background(), op)
		}
	})
	inst, err := waitLRO(ctx, op)
	if err != nil {
		t.Fatalf("CreateInstance operation: %v", err)
	}
	read := net.JoinHostPort(inst.GetReadEndpoint(), strconv.Itoa(int(inst.GetReadEndpointPort())))
	if inst.GetReadEndpoint() == "" {
		t.Fatalf("no read endpoint: %v", inst)
	}
	rc := redisAt(inst, "")
	defer rc.Close()
	if err := rc.Set(ctx, "k", "before", 0).Err(); err != nil {
		t.Fatal(err)
	}
	eventuallyTrue(t, "both replicas have the key", func() bool {
		n, err := rc.Do(ctx, "WAIT", 2, 1000).Int()
		return err == nil && n >= 2
	})
	deleteServerPod(ctx, t, kubeconfig, redisadmin.ServerName(name)+"-n0")
	var after *redispb.Instance
	for end := time.Now().Add(3 * time.Minute); ; time.Sleep(250 * time.Millisecond) {
		after, err = c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
		if err == nil && after.GetCurrentLocationId() != "us-central1-a" && after.GetState() == redispb.Instance_READY {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("no automatic failover within 3 minutes: %v, %v", after, err)
		}
	}
	if got := net.JoinHostPort(after.GetReadEndpoint(), strconv.Itoa(int(after.GetReadEndpointPort()))); got != read {
		t.Errorf("the read endpoint moved: %s, was %s", got, read)
	}
	// Every one of six new connections in a row reads the key: the read
	// endpoint carries none to a node that is not serving.
	ok := 0
	for end := time.Now().Add(2 * time.Minute); ok < 6; time.Sleep(200 * time.Millisecond) {
		c := redis.NewClient(&redis.Options{Addr: read, MaxRetries: -1, PoolSize: 1})
		v, err := c.Get(ctx, "k").Result()
		c.Close()
		if err == nil && v == "before" {
			ok++
			continue
		}
		ok = 0
		if time.Now().After(end) {
			t.Fatalf("the read endpoint after the failover: GET = %q, %v", v, err)
		}
	}
}
