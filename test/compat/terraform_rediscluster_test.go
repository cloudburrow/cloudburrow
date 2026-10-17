//go:build compat

package compat

import (
	"fmt"
	"net"
	"testing"
	"time"

	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// covers: google.cloud.redis.cluster.v1.CloudRedisCluster/CreateCluster, google.cloud.redis.cluster.v1.CloudRedisCluster/GetCluster, google.cloud.redis.cluster.v1.CloudRedisCluster/UpdateCluster, google.cloud.redis.cluster.v1.CloudRedisCluster/DeleteCluster
//
// TestTerraformRedisCluster (#1519): a google_redis_cluster of one shard
// applies with the official hashicorp/google provider through `cloudburrow
// terraform` (redis_custom_endpoint), a go-redis ClusterClient sets and
// gets keys at the discovery endpoint the provider reads, the second plan
// is empty, shard_count 2 applies in place with the keys kept, the plan
// after it is empty, and destroy removes the cluster.
func TestTerraformRedisCluster(t *testing.T) {
	h := New(t)
	c, _ := redisClusterAdminClient(t, h)
	m := newTFModule(t)
	id := fmt.Sprintf("tf-cl-%d", time.Now().UnixNano()%1e8)
	name := "projects/" + m.project + "/locations/us-central1/clusters/" + id
	module := func(shards int) string {
		return fmt.Sprintf(`
resource "google_redis_cluster" "c" {
  project                     = %q
  region                      = "us-central1"
  name                        = %q
  shard_count                 = %d
  replica_count               = 0
  node_type                   = "REDIS_SHARED_CORE_NANO"
  authorization_mode          = "AUTH_MODE_DISABLED"
  transit_encryption_mode     = "TRANSIT_ENCRYPTION_MODE_DISABLED"
  deletion_protection_enabled = false
  redis_configs               = { "maxmemory-policy" = "allkeys-lru" }
  psc_configs {
    network = "projects/%s/global/networks/default"
  }
  zone_distribution_config {
    mode = "MULTI_ZONE"
  }
}
output "endpoint" {
  value = "${google_redis_cluster.c.discovery_endpoints[0].address}:${google_redis_cluster.c.discovery_endpoints[0].port}"
}
`, m.project, id, shards, m.project)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteCluster(h.Context(), &clusterpb.DeleteClusterRequest{Name: name}); err == nil {
			_ = op.Wait(h.Context())
		}
	})
	m.write(module(1))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	cl, err := c.GetCluster(h.Context(), &clusterpb.GetClusterRequest{Name: name})
	if err != nil {
		t.Fatalf("GetCluster after apply: %v", err)
	}
	if cl.GetShardCount() != 1 || cl.GetState() != clusterpb.Cluster_ACTIVE || cl.GetRedisConfigs()["maxmemory-policy"] != "allkeys-lru" {
		t.Errorf("after apply, the cluster reads back as %v", cl)
	}
	addr := m.must("output", "-raw", "endpoint")
	if _, _, err := net.SplitHostPort(addr); err != nil {
		t.Fatalf("the provider's discovery endpoint %q: %v", addr, err)
	}
	rc := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{addr}})
	defer rc.Close()
	clusterKeys(t, h, rc, "tf", 20, true)
	m.planClean()

	m.write(module(2))
	m.apply()
	if cl, err = c.GetCluster(h.Context(), &clusterpb.GetClusterRequest{Name: name}); err != nil || cl.GetShardCount() != 2 {
		t.Errorf("after shard_count 2, GetCluster = %v, %v", cl, err)
	}
	rc.ReloadState(h.Context())
	clusterKeys(t, h, rc, "tf", 20, false)
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := c.GetCluster(h.Context(), &clusterpb.GetClusterRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetCluster after destroy = %v, want NOT_FOUND", err)
	}
}
