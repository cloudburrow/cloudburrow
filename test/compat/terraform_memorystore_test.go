//go:build compat

package compat

import (
	"fmt"
	"net"
	"testing"
	"time"

	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// covers: google.cloud.memorystore.v1.Memorystore/CreateInstance, google.cloud.memorystore.v1.Memorystore/GetInstance, google.cloud.memorystore.v1.Memorystore/UpdateInstance, google.cloud.memorystore.v1.Memorystore/DeleteInstance
//
// TestTerraformMemorystoreInstance (#1519): a google_memorystore_instance
// of one shard applies with the official hashicorp/google provider through
// `cloudburrow terraform` (memorystore_custom_endpoint), a go-redis
// ClusterClient sets and gets keys at the discovery endpoint the provider
// reads, the second plan is empty, shard_count 2 and a label apply in place
// with the keys kept, the plan after it is empty, and destroy removes the
// instance.
func TestTerraformMemorystoreInstance(t *testing.T) {
	h := New(t)
	c, _ := valkeyAdminClient(t, h)
	m := newTFModule(t)
	id := fmt.Sprintf("tf-vk-%d", time.Now().UnixNano()%1e8)
	name := "projects/" + m.project + "/locations/us-central1/instances/" + id
	module := func(shards int, env string) string {
		return fmt.Sprintf(`
resource "google_memorystore_instance" "v" {
  project                     = %q
  location                    = "us-central1"
  instance_id                 = %q
  shard_count                 = %d
  replica_count               = 0
  node_type                   = "SHARED_CORE_NANO"
  engine_version              = "VALKEY_8_0"
  mode                        = "CLUSTER"
  authorization_mode          = "AUTH_DISABLED"
  transit_encryption_mode     = "TRANSIT_ENCRYPTION_DISABLED"
  deletion_protection_enabled = false
  engine_configs              = { "maxmemory-policy" = "allkeys-lru" }
  labels                      = { env = %q }
  desired_auto_created_endpoints {
    network    = "projects/%s/global/networks/default"
    project_id = %q
  }
  persistence_config {
    mode = "RDB"
    rdb_config {
      rdb_snapshot_period = "ONE_HOUR"
    }
  }
  zone_distribution_config {
    mode = "MULTI_ZONE"
  }
}
output "endpoint" {
  value = "${google_memorystore_instance.v.discovery_endpoints[0].address}:${google_memorystore_instance.v.discovery_endpoints[0].port}"
}
`, m.project, id, shards, env, m.project, m.project)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(h.Context(), &memorystorepb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = waitLRODone(h.Context(), op)
		}
	})
	m.write(module(1, "a"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	inst, err := c.GetInstance(h.Context(), &memorystorepb.GetInstanceRequest{Name: name})
	if err != nil {
		t.Fatalf("GetInstance after apply: %v", err)
	}
	if inst.GetShardCount() != 1 || inst.GetState() != memorystorepb.Instance_ACTIVE || inst.GetEngineConfigs()["maxmemory-policy"] != "allkeys-lru" ||
		inst.GetPersistenceConfig().GetMode() != memorystorepb.PersistenceConfig_RDB || len(inst.GetEndpoints()) != 1 {
		t.Errorf("after apply, the instance reads back as %v", inst)
	}
	addr := m.must("output", "-raw", "endpoint")
	if _, _, err := net.SplitHostPort(addr); err != nil {
		t.Fatalf("the provider's discovery endpoint %q: %v", addr, err)
	}
	rc := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{addr}})
	defer rc.Close()
	clusterKeys(t, h, rc, "tf", 20, true)
	m.planClean()

	m.write(module(2, "b"))
	m.apply()
	if inst, err = c.GetInstance(h.Context(), &memorystorepb.GetInstanceRequest{Name: name}); err != nil || inst.GetShardCount() != 2 || inst.GetLabels()["env"] != "b" {
		t.Errorf("after shard_count 2, GetInstance = %v, %v", inst, err)
	}
	rc.ReloadState(h.Context())
	clusterKeys(t, h, rc, "tf", 20, false)
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if _, err := c.GetInstance(h.Context(), &memorystorepb.GetInstanceRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetInstance after destroy = %v, want NOT_FOUND", err)
	}
}
