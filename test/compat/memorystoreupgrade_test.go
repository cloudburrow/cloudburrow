//go:build compat

package compat

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// covers: google.cloud.redis.v1.CloudRedis/CreateInstance, google.cloud.redis.v1.CloudRedis/UpgradeInstance
//
// TestMemorystoreVersionsAndUpgrade (#1386), with cloud.google.com/go/redis/
// apiv1: a REDIS_6_X instance runs Redis 6.2 (INFO redis_version 6.2.x);
// UpgradeInstance to REDIS_7_0 runs Redis 7.0 and to REDIS_7_2 Valkey
// (which reports 7.2.4), each keeping the instance's keys and AUTH; a
// downgrade is INVALID_ARGUMENT, and REDIS_5_0 is 501.
func TestMemorystoreVersionsAndUpgrade(t *testing.T) {
	h := New(t)
	c, project := memorystoreAdminClient(t, h)
	// Each version's image is pulled into the cluster the first time it
	// runs, which can take longer than the harness's own deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	parent := "projects/" + project + "/locations/us-central1"
	id := fmt.Sprintf("upgrade-%d", time.Now().UnixNano()%1e8)
	name := parent + "/instances/" + id
	if _, err := c.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent, InstanceId: id + "x",
		Instance: &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1, RedisVersion: "REDIS_5_0"}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("REDIS_5_0 = %v, want UNIMPLEMENTED", err)
	}
	op, err := c.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent, InstanceId: id,
		Instance: &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1, AuthEnabled: true, RedisVersion: "REDIS_6_X"}})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(context.Background(), &redispb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = waitLRODone(context.Background(), op)
		}
	})
	inst, err := waitLRO(ctx, op)
	if err != nil {
		t.Fatalf("CreateInstance operation: %v", err)
	}
	if inst.GetRedisVersion() != "REDIS_6_X" {
		t.Errorf("redis_version = %s", inst.GetRedisVersion())
	}
	auth, err := c.GetInstanceAuthString(ctx, &redispb.GetInstanceAuthStringRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	serverVersion := func(inst *redispb.Instance) string {
		rc := redisAt(inst, auth.GetAuthString())
		defer rc.Close()
		info, err := rc.Info(ctx, "server").Result()
		if err != nil {
			t.Fatalf("INFO: %v", err)
		}
		for _, l := range strings.Split(info, "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(l), "redis_version:"); ok {
				if got, err := rc.Get(ctx, "k").Result(); inst.GetRedisVersion() != "REDIS_6_X" && (err != nil || got != "v") {
					t.Errorf("on %s GET k = %q, %v; want the key kept", inst.GetRedisVersion(), got, err)
				}
				return v
			}
		}
		return ""
	}
	if v := serverVersion(inst); !strings.HasPrefix(v, "6.2.") {
		t.Errorf("REDIS_6_X server reports %s", v)
	}
	rc := redisAt(inst, auth.GetAuthString())
	rc.Set(ctx, "k", "v", 0)
	rc.Close()
	for _, step := range []struct{ version, prefix string }{{"REDIS_7_0", "7.0."}, {"REDIS_7_2", "7.2."}} {
		uop, err := c.UpgradeInstance(ctx, &redispb.UpgradeInstanceRequest{Name: name, RedisVersion: step.version})
		if err != nil {
			t.Fatalf("UpgradeInstance %s: %v", step.version, err)
		}
		inst, err = waitLRO(ctx, uop)
		if err != nil {
			t.Fatalf("UpgradeInstance %s operation: %v", step.version, err)
		}
		if inst.GetRedisVersion() != step.version || inst.GetState() != redispb.Instance_READY {
			t.Errorf("after the upgrade: %s %s", inst.GetRedisVersion(), inst.GetState())
		}
		if v := serverVersion(inst); !strings.HasPrefix(v, step.prefix) {
			t.Errorf("%s server reports %s", step.version, v)
		}
	}
	if _, err := c.UpgradeInstance(ctx, &redispb.UpgradeInstanceRequest{Name: name, RedisVersion: "REDIS_7_0"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("downgrade = %v, want INVALID_ARGUMENT", err)
	}
}

// redisAt is a client of inst's server, with password.
func redisAt(inst *redispb.Instance, password string) *redis.Client {
	return redis.NewClient(&redis.Options{Addr: net.JoinHostPort(inst.GetHost(), strconv.Itoa(int(inst.GetPort()))), Password: password})
}
