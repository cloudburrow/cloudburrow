//go:build compat

package compat

import (
	"fmt"
	"testing"
	"time"

	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
)

// covers: google.cloud.redis.cluster.v1.CloudRedisCluster/CreateCluster, google.cloud.memorystore.v1.Memorystore/CreateInstance
//
// TestTerraformMemorystoreInTransitEncryption (#1549): with the official
// hashicorp/google provider through `cloudburrow terraform`, a
// google_redis_cluster with transit_encryption_mode
// TRANSIT_ENCRYPTION_MODE_SERVER_AUTHENTICATION and AUTH_MODE_IAM_AUTH, and
// a google_memorystore_instance with SERVER_AUTHENTICATION and IAM_AUTH,
// apply; the official clients read both modes back, the second plan is
// empty, and destroy removes them.
func TestTerraformMemorystoreInTransitEncryption(t *testing.T) {
	h := New(t)
	rc, _ := redisClusterAdminClient(t, h)
	vc, _ := valkeyAdminClient(t, h)
	m := newTFModule(t)
	sfx := time.Now().UnixNano() % 1e8
	cid, vid := fmt.Sprintf("tf-cltls-%d", sfx), fmt.Sprintf("tf-vktls-%d", sfx)
	cname := "projects/" + m.project + "/locations/us-central1/clusters/" + cid
	vname := "projects/" + m.project + "/locations/us-central1/instances/" + vid
	t.Cleanup(func() {
		if op, err := rc.DeleteCluster(h.Context(), &clusterpb.DeleteClusterRequest{Name: cname}); err == nil {
			_ = op.Wait(h.Context())
		}
		if op, err := vc.DeleteInstance(h.Context(), &memorystorepb.DeleteInstanceRequest{Name: vname}); err == nil {
			_ = op.Wait(h.Context())
		}
	})
	m.write(fmt.Sprintf(`
resource "google_redis_cluster" "c" {
  project                     = %q
  region                      = "us-central1"
  name                        = %q
  shard_count                 = 1
  replica_count               = 0
  node_type                   = "REDIS_SHARED_CORE_NANO"
  authorization_mode          = "AUTH_MODE_IAM_AUTH"
  transit_encryption_mode     = "TRANSIT_ENCRYPTION_MODE_SERVER_AUTHENTICATION"
  deletion_protection_enabled = false
  psc_configs {
    network = "projects/%s/global/networks/default"
  }
}
resource "google_memorystore_instance" "v" {
  project                     = %q
  location                    = "us-central1"
  instance_id                 = %q
  shard_count                 = 1
  replica_count               = 0
  node_type                   = "SHARED_CORE_NANO"
  engine_version              = "VALKEY_8_0"
  mode                        = "CLUSTER"
  authorization_mode          = "IAM_AUTH"
  transit_encryption_mode     = "SERVER_AUTHENTICATION"
  deletion_protection_enabled = false
  desired_auto_created_endpoints {
    network    = "projects/%s/global/networks/default"
    project_id = %q
  }
}
`, m.project, cid, m.project, m.project, vid, m.project, m.project))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	cl, err := rc.GetCluster(h.Context(), &clusterpb.GetClusterRequest{Name: cname})
	if err != nil || cl.GetTransitEncryptionMode() != clusterpb.TransitEncryptionMode_TRANSIT_ENCRYPTION_MODE_SERVER_AUTHENTICATION ||
		cl.GetAuthorizationMode() != clusterpb.AuthorizationMode_AUTH_MODE_IAM_AUTH {
		t.Errorf("after apply, GetCluster = %v, %v", cl, err)
	}
	inst, err := vc.GetInstance(h.Context(), &memorystorepb.GetInstanceRequest{Name: vname})
	if err != nil || inst.GetTransitEncryptionMode() != memorystorepb.Instance_SERVER_AUTHENTICATION ||
		inst.GetAuthorizationMode() != memorystorepb.Instance_IAM_AUTH {
		t.Errorf("after apply, GetInstance = %v, %v", inst, err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
}
