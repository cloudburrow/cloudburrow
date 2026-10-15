//go:build compat

package compat

import (
	"fmt"
	"strings"
	"testing"
	"time"

	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// covers: google.cloud.redis.v1.CloudRedis/CreateInstance, google.cloud.redis.v1.CloudRedis/GetInstance, google.cloud.redis.v1.CloudRedis/ListInstances, google.cloud.redis.v1.CloudRedis/UpdateInstance, google.cloud.redis.v1.CloudRedis/DeleteInstance, google.cloud.redis.v1.CloudRedis/GetInstanceAuthString, google.longrunning.Operations/GetOperation
//
// TestGcloudRedis (#1302): through the configuration `cloudburrow
// gcloud-setup` writes and nothing else, the real gcloud creates a Basic
// instance with AUTH, describes and lists it, reads its AUTH string,
// resizes it and deletes it, over the Memorystore admin API's JSON on its
// port, waiting on each operation as gcloud does. The official gRPC
// client reads each change back.
func TestGcloudRedis(t *testing.T) {
	h := New(t)
	c, _ := memorystoreAdminClient(t, h) // skips without the admin API
	g := newGcloudSession(t, h)
	ctx := h.Context()
	id := fmt.Sprintf("gcloud-%d", time.Now().UnixNano()%1e8)
	p, region := "--project="+g.project, "--region=us-central1"
	name := "projects/" + g.project + "/locations/us-central1/instances/" + id
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(h.Context(), &redispb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = op.Wait(h.Context())
		}
	})
	g.must("redis", "instances", "create", id, p, region, "--size=1", "--tier=basic", "--enable-auth",
		"--redis-config=maxmemory-policy=allkeys-lfu", "--labels=env=dev")
	inst, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if inst.GetState() != redispb.Instance_READY || !inst.GetAuthEnabled() || inst.GetRedisConfigs()["maxmemory-policy"] != "allkeys-lfu" ||
		inst.GetLabels()["env"] != "dev" {
		t.Errorf("the created instance reads back as %v", inst)
	}
	if got := g.must("redis", "instances", "describe", id, p, region, "--format=value(state,host,port,memorySizeGb)"); !strings.Contains(got, fmt.Sprintf("READY\t%s\t%d\t1", inst.GetHost(), inst.GetPort())) {
		t.Errorf("describe = %q", got)
	}
	if got := g.must("redis", "instances", "list", p, region, "--format=value(name)"); !strings.Contains(got, id) {
		t.Errorf("list = %q", got)
	}
	auth, err := c.GetInstanceAuthString(ctx, &redispb.GetInstanceAuthStringRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if got := g.must("redis", "instances", "get-auth-string", id, p, region); !strings.Contains(got, auth.GetAuthString()) {
		t.Error("get-auth-string does not print the instance's AUTH string")
	}
	g.must("redis", "instances", "update", id, p, region, "--size=2", "--update-labels=env=prod")
	if inst, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name}); err != nil || inst.GetMemorySizeGb() != 2 || inst.GetLabels()["env"] != "prod" {
		t.Errorf("after update, GetInstance = %v, %v", inst, err)
	}
	g.must("redis", "instances", "delete", id, p, region, "--quiet")
	if _, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetInstance after delete: %v; want NotFound", err)
	}
}
