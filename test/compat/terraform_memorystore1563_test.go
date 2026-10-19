//go:build compat

package compat

import (
	"fmt"
	"testing"
	"time"

	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
)

// covers: google.cloud.memorystore.v1.Memorystore/CreateInstance, google.cloud.memorystore.v1.Memorystore/GetInstance, google.cloud.memorystore.v1.Memorystore/DeleteInstance
//
// TestTerraformMemorystore1563 (#1563): with the official hashicorp/google
// provider through `cloudburrow terraform`, a CLUSTER_DISABLED
// google_memorystore_instance with transit_encryption_mode
// SERVER_AUTHENTICATION and a secondary of it with the same
// (cross_instance_replication_config SECONDARY) apply; the official client
// reads them back, the plan is empty, and destroy removes them.
func TestTerraformMemorystore1563(t *testing.T) {
	h := New(t)
	vc, _ := valkeyAdminClient(t, h)
	m := newTFModule(t)
	sfx := time.Now().UnixNano() % 1e7
	pid, sid := fmt.Sprintf("tf-tlsp-%d", sfx), fmt.Sprintf("tf-tlss-%d", sfx)
	parent := "projects/" + m.project + "/locations/us-central1"
	t.Cleanup(func() {
		for _, id := range []string{sid, pid} {
			if op, err := vc.DeleteInstance(h.Context(), &memorystorepb.DeleteInstanceRequest{Name: parent + "/instances/" + id}); err == nil {
				_ = waitLRODone(h.Context(), op)
			}
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
  transit_encryption_mode     = "SERVER_AUTHENTICATION"
  deletion_protection_enabled = false
  desired_auto_created_endpoints {
    network    = "projects/%s/global/networks/default"
    project_id = %q
  }
%s
}
`, name, m.project, id, m.project, m.project, extra)
	}
	m.write(instance("primary", pid, "") + instance("secondary", sid, `  cross_instance_replication_config {
    instance_role = "SECONDARY"
    primary_instance {
      instance = google_memorystore_instance.primary.id
    }
  }`))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	sec, err := vc.GetInstance(h.Context(), &memorystorepb.GetInstanceRequest{Name: parent + "/instances/" + sid})
	if err != nil {
		t.Fatal(err)
	}
	if sec.GetTransitEncryptionMode() != memorystorepb.Instance_SERVER_AUTHENTICATION ||
		sec.GetCrossInstanceReplicationConfig().GetInstanceRole() != memorystorepb.CrossInstanceReplicationConfig_SECONDARY {
		t.Errorf("secondary: %v, %v", sec.GetTransitEncryptionMode(), sec.GetCrossInstanceReplicationConfig())
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
}
