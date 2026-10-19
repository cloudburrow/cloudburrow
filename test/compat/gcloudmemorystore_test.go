//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"
	"time"

	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// covers: google.cloud.memorystore.v1.Memorystore/CreateInstance, google.cloud.memorystore.v1.Memorystore/GetInstance, google.cloud.memorystore.v1.Memorystore/ListInstances, google.cloud.memorystore.v1.Memorystore/UpdateInstance, google.cloud.memorystore.v1.Memorystore/DeleteInstance
//
// TestGcloudMemorystoreValkey (#1519): through the configuration
// `cloudburrow gcloud-setup` writes, the real gcloud memorystore instances
// creates an instance of one shard, describes and lists it, adds a replica
// and deletes it, over memorystore.googleapis.com's JSON under
// /memorystore on the Memorystore admin port, waiting on each operation.
// The official gRPC client reads each change back.
func TestGcloudMemorystoreValkey(t *testing.T) {
	h := New(t)
	c, _ := valkeyAdminClient(t, h) // skips without the admin API
	g := newGcloudSession(t, h)
	id := fmt.Sprintf("gvk-%d", time.Now().UnixNano()%1e8)
	p, loc := "--project="+g.project, "--location=us-central1"
	name := "projects/" + g.project + "/locations/us-central1/instances/" + id
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(h.Context(), &memorystorepb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = waitLRODone(h.Context(), op)
		}
	})
	g.must("memorystore", "instances", "create", id, p, loc, "--shard-count=1", "--replica-count=0",
		"--node-type=shared-core-nano", "--engine-configs=maxmemory-policy=allkeys-lru",
		"--psc-auto-connections=network=projects/"+g.project+"/global/networks/default,projectId="+g.project)
	inst, err := c.GetInstance(h.Context(), &memorystorepb.GetInstanceRequest{Name: name})
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if inst.GetState() != memorystorepb.Instance_ACTIVE || inst.GetShardCount() != 1 || inst.GetNodeType() != memorystorepb.Instance_SHARED_CORE_NANO ||
		inst.GetEngineConfigs()["maxmemory-policy"] != "allkeys-lru" || len(inst.GetDiscoveryEndpoints()) != 1 {
		t.Errorf("the created instance reads back as %v", inst)
	}
	port := inst.GetDiscoveryEndpoints()[0].GetPort()
	if got := g.must("memorystore", "instances", "describe", id, p, loc, "--format=value(state,discoveryEndpoints[0].port)"); !strings.Contains(got, fmt.Sprintf("ACTIVE\t%d", port)) {
		t.Errorf("describe = %q", got)
	}
	if got := g.must("memorystore", "instances", "list", p, loc, "--format=value(name)"); !strings.Contains(got, id) {
		t.Errorf("list = %q", got)
	}
	g.must("memorystore", "instances", "update", id, p, loc, "--replica-count=1")
	if inst, err := c.GetInstance(h.Context(), &memorystorepb.GetInstanceRequest{Name: name}); err != nil || inst.GetReplicaCount() != 1 {
		t.Errorf("after update, GetInstance = %v, %v", inst, err)
	}
	g.must("memorystore", "instances", "delete", id, p, loc, "--quiet")
	if _, err := c.GetInstance(h.Context(), &memorystorepb.GetInstanceRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetInstance after delete: %v; want NotFound", err)
	}
}
