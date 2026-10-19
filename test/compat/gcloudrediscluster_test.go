//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"
	"time"

	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// covers: google.cloud.redis.cluster.v1.CloudRedisCluster/CreateCluster, google.cloud.redis.cluster.v1.CloudRedisCluster/GetCluster, google.cloud.redis.cluster.v1.CloudRedisCluster/ListClusters, google.cloud.redis.cluster.v1.CloudRedisCluster/UpdateCluster, google.cloud.redis.cluster.v1.CloudRedisCluster/DeleteCluster
//
// TestGcloudRedisCluster (#1519): through the configuration `cloudburrow
// gcloud-setup` writes, the real gcloud creates a cluster of one shard,
// describes and lists it, adds a replica and deletes it, over the JSON
// API on the Memorystore admin port, waiting on each operation. The
// official gRPC client reads each change back.
func TestGcloudRedisCluster(t *testing.T) {
	h := New(t)
	c, _ := redisClusterAdminClient(t, h) // skips without the admin API
	g := newGcloudSession(t, h)
	id := fmt.Sprintf("gcl-%d", time.Now().UnixNano()%1e8)
	p, region := "--project="+g.project, "--region=us-central1"
	name := "projects/" + g.project + "/locations/us-central1/clusters/" + id
	t.Cleanup(func() {
		if op, err := c.DeleteCluster(h.Context(), &clusterpb.DeleteClusterRequest{Name: name}); err == nil {
			_ = waitLRODone(h.Context(), op)
		}
	})
	g.must("redis", "clusters", "create", id, p, region, "--shard-count=1", "--replica-count=0",
		"--node-type=redis-shared-core-nano", "--network=projects/"+g.project+"/global/networks/default")
	cl, err := c.GetCluster(h.Context(), &clusterpb.GetClusterRequest{Name: name})
	if err != nil {
		t.Fatalf("GetCluster: %v", err)
	}
	if cl.GetState() != clusterpb.Cluster_ACTIVE || cl.GetShardCount() != 1 || cl.GetNodeType() != clusterpb.NodeType_REDIS_SHARED_CORE_NANO {
		t.Errorf("the created cluster reads back as %v", cl)
	}
	ep := cl.GetDiscoveryEndpoints()[0]
	if got := g.must("redis", "clusters", "describe", id, p, region, "--format=value(state,discoveryEndpoints[0].port)"); !strings.Contains(got, fmt.Sprintf("ACTIVE\t%d", ep.GetPort())) {
		t.Errorf("describe = %q", got)
	}
	if got := g.must("redis", "clusters", "list", p, region, "--format=value(name)"); !strings.Contains(got, id) {
		t.Errorf("list = %q", got)
	}
	g.must("redis", "clusters", "update", id, p, region, "--replica-count=1")
	if cl, err := c.GetCluster(h.Context(), &clusterpb.GetClusterRequest{Name: name}); err != nil || cl.GetReplicaCount() != 1 {
		t.Errorf("after update, GetCluster = %v, %v", cl, err)
	}
	g.must("redis", "clusters", "delete", id, p, region, "--quiet")
	if _, err := c.GetCluster(h.Context(), &clusterpb.GetClusterRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetCluster after delete: %v; want NotFound", err)
	}
}
