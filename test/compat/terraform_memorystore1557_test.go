//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"
	"time"

	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
)

// covers: google.cloud.memorystore.v1.Memorystore/CreateInstance, google.cloud.memorystore.v1.Memorystore/UpdateInstance, google.cloud.redis.cluster.v1.CloudRedisCluster/CreateCluster
//
// TestTerraformMemorystore1557 (#1557): with the official hashicorp/google
// provider through `cloudburrow terraform`, a google_memorystore_instance
// and a google_redis_cluster with kms_key (a key of this instance's Cloud
// KMS, made with the official client), a google_memorystore_instance at
// engine_version VALKEY_7_2, and a CLUSTER_DISABLED primary with a
// secondary (cross_instance_replication_config SECONDARY with its
// primary_instance) apply; the official clients read them back, and the
// plan is empty. engine_version changed to VALKEY_9_0 upgrades the instance
// in place; the plan is empty again, and destroy removes everything.
func TestTerraformMemorystore1557(t *testing.T) {
	h := New(t)
	rc, _ := redisClusterAdminClient(t, h)
	vc, _ := valkeyAdminClient(t, h)
	kmsClients(t, h) // skips without Cloud KMS
	m := newTFModule(t)
	key := memorystoreKey(t, h, m.project)
	sfx := time.Now().UnixNano() % 1e7
	kid, cid, eid := fmt.Sprintf("tf-vkms-%d", sfx), fmt.Sprintf("tf-ckms-%d", sfx), fmt.Sprintf("tf-ver-%d", sfx)
	pid, sid := fmt.Sprintf("tf-cirp-%d", sfx), fmt.Sprintf("tf-cirs-%d", sfx)
	parent := "projects/" + m.project + "/locations/us-central1"
	t.Cleanup(func() {
		for _, id := range []string{sid, pid, kid, eid} {
			if op, err := vc.DeleteInstance(h.Context(), &memorystorepb.DeleteInstanceRequest{Name: parent + "/instances/" + id}); err == nil {
				_ = op.Wait(h.Context())
			}
		}
		if op, err := rc.DeleteCluster(h.Context(), &clusterpb.DeleteClusterRequest{Name: parent + "/clusters/" + cid}); err == nil {
			_ = op.Wait(h.Context())
		}
	})
	instance := func(name, id, extra string) string {
		return fmt.Sprintf(`
resource "google_memorystore_instance" %q {
  project                     = %q
  location                    = "us-central1"
  instance_id                 = %q
  mode                        = "CLUSTER_DISABLED"
  shard_count                 = 1
  replica_count               = 0
  node_type                   = "SHARED_CORE_NANO"
  deletion_protection_enabled = false
  desired_auto_created_endpoints {
    network    = "projects/%s/global/networks/default"
    project_id = %q
  }
%s
}
`, name, m.project, id, m.project, m.project, extra)
	}
	module := func(version string) string {
		return instance("kms", kid, fmt.Sprintf("  kms_key = %q", key)) +
			instance("ver", eid, fmt.Sprintf("  engine_version = %q", version)) +
			instance("primary", pid, "") +
			instance("secondary", sid, `  cross_instance_replication_config {
    instance_role = "SECONDARY"
    primary_instance {
      instance = google_memorystore_instance.primary.id
    }
  }`) + fmt.Sprintf(`
resource "google_redis_cluster" "kms" {
  project                     = %q
  region                      = "us-central1"
  name                        = %q
  shard_count                 = 1
  replica_count               = 0
  node_type                   = "REDIS_SHARED_CORE_NANO"
  deletion_protection_enabled = false
  kms_key                     = %q
  psc_configs {
    network = "projects/%s/global/networks/default"
  }
}
`, m.project, cid, key, m.project)
	}
	get := func(id string) *memorystorepb.Instance {
		t.Helper()
		inst, err := vc.GetInstance(h.Context(), &memorystorepb.GetInstanceRequest{Name: parent + "/instances/" + id})
		if err != nil {
			t.Fatalf("GetInstance %s: %v", id, err)
		}
		return inst
	}
	m.write(module("VALKEY_7_2"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	if inst := get(kid); inst.GetKmsKey() != key || inst.GetEncryptionInfo().GetEncryptionType() != memorystorepb.EncryptionInfo_CUSTOMER_MANAGED_ENCRYPTION {
		t.Errorf("kms_key instance: %q, %v", inst.GetKmsKey(), inst.GetEncryptionInfo())
	}
	if cl, err := rc.GetCluster(h.Context(), &clusterpb.GetClusterRequest{Name: parent + "/clusters/" + cid}); err != nil || cl.GetKmsKey() != key {
		t.Errorf("kms_key cluster: %v, %v", cl.GetKmsKey(), err)
	}
	ver := get(eid)
	if ver.GetEngineVersion() != "VALKEY_7_2" {
		t.Errorf("engine_version = %q, want VALKEY_7_2", ver.GetEngineVersion())
	}
	if cfg := get(sid).GetCrossInstanceReplicationConfig(); cfg.GetInstanceRole() != memorystorepb.CrossInstanceReplicationConfig_SECONDARY ||
		!strings.HasSuffix(cfg.GetPrimaryInstance().GetInstance(), "/instances/"+pid) {
		t.Errorf("secondary's cross_instance_replication_config = %v", cfg)
	}
	if cfg := get(pid).GetCrossInstanceReplicationConfig(); cfg.GetInstanceRole() != memorystorepb.CrossInstanceReplicationConfig_PRIMARY {
		t.Errorf("primary's cross_instance_replication_config = %v", cfg)
	}
	m.planClean()

	m.write(module("VALKEY_9_0"))
	m.apply()
	if up := get(eid); up.GetEngineVersion() != "VALKEY_9_0" || up.GetUid() != ver.GetUid() {
		t.Errorf("after engine_version VALKEY_9_0: %q, uid %s (was %s): want an upgrade in place", up.GetEngineVersion(), up.GetUid(), ver.GetUid())
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
}
