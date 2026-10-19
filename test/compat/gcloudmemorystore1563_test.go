//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"
	"time"

	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
)

// covers: google.cloud.memorystore.v1.Memorystore/CreateInstance, google.cloud.memorystore.v1.Memorystore/UpdateInstance, google.cloud.memorystore.v1.Memorystore/GetInstance
//
// TestGcloudMemorystore1563 (#1563): with gcloud through gcloud-setup
// (egress to Google refused), a secondary with
// --transit-encryption-mode=server-authentication of a primary with it is
// created with --cross-instance-replication-config-role=secondary
// --primary-instance, described as SECONDARY with SERVER_AUTHENTICATION,
// and detached with --cross-instance-replication-config-role=none.
func TestGcloudMemorystore1563(t *testing.T) {
	h := New(t)
	vc, _ := valkeyAdminClient(t, h) // skips without the admin API
	g := newGcloudSession(t, h)
	sfx := time.Now().UnixNano() % 1e7
	p := "--project=" + g.project
	loc := "--location=us-central1"
	parent := "projects/" + g.project + "/locations/us-central1"
	psc := "--psc-auto-connections=network=projects/" + g.project + "/global/networks/default,projectId=" + g.project
	small := []string{p, loc, "--mode=cluster-disabled", "--replica-count=0", "--node-type=shared-core-nano", psc,
		"--transit-encryption-mode=server-authentication"}
	pid, sid := fmt.Sprintf("gtlsp-%d", sfx), fmt.Sprintf("gtlss-%d", sfx)
	for _, id := range []string{pid, sid} {
		name := parent + "/instances/" + id
		t.Cleanup(func() {
			for i := 0; i < 3; i++ {
				if op, err := vc.DeleteInstance(h.Context(), &memorystorepb.DeleteInstanceRequest{Name: name}); err == nil {
					_ = waitLRODone(h.Context(), op)
					return
				}
				time.Sleep(2 * time.Second)
			}
		})
	}
	g.must(append([]string{"memorystore", "instances", "create", pid}, small...)...)
	g.must(append([]string{"memorystore", "instances", "create", sid, "--cross-instance-replication-config-role=secondary",
		"--primary-instance=" + parent + "/instances/" + pid}, small...)...)
	sec, err := vc.GetInstance(h.Context(), &memorystorepb.GetInstanceRequest{Name: parent + "/instances/" + sid})
	if err != nil {
		t.Fatal(err)
	}
	if sec.GetTransitEncryptionMode() != memorystorepb.Instance_SERVER_AUTHENTICATION ||
		sec.GetCrossInstanceReplicationConfig().GetInstanceRole() != memorystorepb.CrossInstanceReplicationConfig_SECONDARY {
		t.Errorf("secondary created with gcloud: %v, %v", sec.GetTransitEncryptionMode(), sec.GetCrossInstanceReplicationConfig())
	}
	if got := g.must("memorystore", "instances", "describe", sid, p, loc,
		"--format=value(transitEncryptionMode,crossInstanceReplicationConfig.instanceRole)"); !strings.Contains(got, "SERVER_AUTHENTICATION") || !strings.Contains(got, "SECONDARY") {
		t.Errorf("describe of the secondary = %q", got)
	}
	g.must("memorystore", "instances", "update", sid, p, loc, "--cross-instance-replication-config-role=none")
	if got, err := vc.GetInstance(h.Context(), &memorystorepb.GetInstanceRequest{Name: parent + "/instances/" + sid}); err != nil || got.GetCrossInstanceReplicationConfig() != nil {
		t.Errorf("after the detach: %v, %v", got.GetCrossInstanceReplicationConfig(), err)
	}
}
