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
// TestTerraformMemorystoreAutomatedBackups (#1549): with the official
// hashicorp/google provider through `cloudburrow terraform`, a
// google_redis_cluster and a google_memorystore_instance with an
// automated_backup_config (a daily start hour and a retention) apply; the
// official clients read the config back, ENABLED, with the cluster's backup
// collection; the second plan is empty, and destroy removes them.
func TestTerraformMemorystoreAutomatedBackups(t *testing.T) {
	h := New(t)
	rc, _ := redisClusterAdminClient(t, h)
	vc, _ := valkeyAdminClient(t, h)
	m := newTFModule(t)
	sfx := time.Now().UnixNano() % 1e8
	cid, vid := fmt.Sprintf("tf-clbk-%d", sfx), fmt.Sprintf("tf-vkbk-%d", sfx)
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
  deletion_protection_enabled = false
  psc_configs {
    network = "projects/%s/global/networks/default"
  }
  automated_backup_config {
    fixed_frequency_schedule {
      start_time {
        hours = 3
      }
    }
    retention = "604800s"
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
  deletion_protection_enabled = false
  desired_auto_created_endpoints {
    network    = "projects/%s/global/networks/default"
    project_id = %q
  }
  automated_backup_config {
    fixed_frequency_schedule {
      start_time {
        hours = 3
      }
    }
    retention = "604800s"
  }
}
`, m.project, cid, m.project, m.project, vid, m.project, m.project))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	cl, err := rc.GetCluster(h.Context(), &clusterpb.GetClusterRequest{Name: cname})
	if err != nil || cl.GetAutomatedBackupConfig().GetAutomatedBackupMode() != clusterpb.AutomatedBackupConfig_ENABLED ||
		cl.GetAutomatedBackupConfig().GetFixedFrequencySchedule().GetStartTime().GetHours() != 3 || cl.GetBackupCollection() == "" {
		t.Errorf("after apply, GetCluster = %v, %v", cl, err)
	}
	inst, err := vc.GetInstance(h.Context(), &memorystorepb.GetInstanceRequest{Name: vname})
	if err != nil || inst.GetAutomatedBackupConfig().GetAutomatedBackupMode() != memorystorepb.AutomatedBackupConfig_ENABLED ||
		inst.GetAutomatedBackupConfig().GetRetention().AsDuration() != 7*24*time.Hour {
		t.Errorf("after apply, GetInstance = %v, %v", inst, err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
}
