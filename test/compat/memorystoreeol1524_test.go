//go:build compat

package compat

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// covers: google.cloud.redis.v1.CloudRedis/CreateInstance, google.cloud.redis.v1.CloudRedis/UpgradeInstance, google.cloud.redis.v1.CloudRedis/FailoverInstance
//
// TestMemorystoreEndOfLifeVersions (#1524), with cloud.google.com/go/redis/
// apiv1 and go-redis: REDIS_3_2 runs Redis 3.2 (INFO redis_version
// 3.2.12), and UpgradeInstance takes it to REDIS_4_0 (4.0.14) and
// REDIS_5_0 (5.0.14) keeping its keys and AUTH string; a STANDARD_HA
// REDIS_4_0 instance replicates (SLAVEOF, the only name 4.0 has) and fails
// over keeping its keys. What Memorystore documents for later versions only
// is refused: allkeys-lfu and activedefrag before 4.0, the stream configs
// before 5.0 and read replicas before 5.0 (INVALID_ARGUMENT); in-transit
// encryption before 6.0 is 501 (#1768).
func TestMemorystoreEndOfLifeVersions(t *testing.T) {
	h := New(t)
	c, project := memorystoreAdminClient(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	parent := "projects/" + project + "/locations/us-central1"
	stamp := time.Now().UnixNano() % 1e8
	for _, tc := range []struct {
		desc string
		in   *redispb.Instance
		want codes.Code
	}{
		{"allkeys-lfu on 3.2", &redispb.Instance{RedisVersion: "REDIS_3_2", RedisConfigs: map[string]string{"maxmemory-policy": "allkeys-lfu"}}, codes.InvalidArgument},
		{"activedefrag on 3.2", &redispb.Instance{RedisVersion: "REDIS_3_2", RedisConfigs: map[string]string{"activedefrag": "yes"}}, codes.InvalidArgument},
		{"stream-node-max-bytes on 4.0", &redispb.Instance{RedisVersion: "REDIS_4_0", RedisConfigs: map[string]string{"stream-node-max-bytes": "8192"}}, codes.InvalidArgument},
		{"read replicas on 4.0", &redispb.Instance{RedisVersion: "REDIS_4_0", Tier: redispb.Instance_STANDARD_HA, MemorySizeGb: 5,
			ReadReplicasMode: redispb.Instance_READ_REPLICAS_ENABLED}, codes.InvalidArgument},
		{"in-transit encryption on 5.0", &redispb.Instance{RedisVersion: "REDIS_5_0", TransitEncryptionMode: redispb.Instance_SERVER_AUTHENTICATION}, codes.Unimplemented},
	} {
		if tc.in.Tier == redispb.Instance_TIER_UNSPECIFIED {
			tc.in.Tier, tc.in.MemorySizeGb = redispb.Instance_BASIC, 1
		}
		if _, err := c.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent, InstanceId: fmt.Sprintf("eol-refused-%d", stamp), Instance: tc.in}); status.Code(err) != tc.want {
			t.Errorf("%s: %v, want %s", tc.desc, err, tc.want)
		}
	}

	version := func(inst *redispb.Instance, pw string) string {
		rc := redisAt(inst, pw)
		defer rc.Close()
		info, err := rc.Info(ctx, "server").Result()
		if err != nil {
			t.Fatalf("INFO on %s: %v", inst.GetRedisVersion(), err)
		}
		for _, l := range strings.Split(info, "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(l), "redis_version:"); ok {
				return v
			}
		}
		return ""
	}
	create := func(id string, in *redispb.Instance) (string, *redispb.Instance, string) {
		name := parent + "/instances/" + id
		op, err := c.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent, InstanceId: id, Instance: in})
		if err != nil {
			t.Fatalf("CreateInstance %s: %v", in.GetRedisVersion(), err)
		}
		t.Cleanup(func() {
			if op, err := c.DeleteInstance(context.Background(), &redispb.DeleteInstanceRequest{Name: name}); err == nil {
				_ = waitLRODone(context.Background(), op)
			}
		})
		inst, err := waitLRO(ctx, op)
		if err != nil {
			t.Fatalf("CreateInstance %s operation: %v", in.GetRedisVersion(), err)
		}
		if inst.GetRedisVersion() != in.GetRedisVersion() || inst.GetState() != redispb.Instance_READY {
			t.Errorf("created: %s %s, want %s READY", inst.GetRedisVersion(), inst.GetState(), in.GetRedisVersion())
		}
		auth, err := c.GetInstanceAuthString(ctx, &redispb.GetInstanceAuthStringRequest{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		return name, inst, auth.GetAuthString()
	}

	name, inst, pw := create(fmt.Sprintf("eol32-%d", stamp), &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1, AuthEnabled: true,
		RedisVersion: "REDIS_3_2", RedisConfigs: map[string]string{"maxmemory-policy": "allkeys-lru"}})
	if v := version(inst, pw); v != "3.2.12" {
		t.Errorf("REDIS_3_2 server reports %s, want 3.2.12", v)
	}
	rc := redisAt(inst, pw)
	if err := rc.Set(ctx, "k", "v", 0).Err(); err != nil {
		t.Fatalf("SET on 3.2: %v", err)
	}
	if p, err := rc.ConfigGet(ctx, "maxmemory-policy").Result(); err != nil || p["maxmemory-policy"] != "allkeys-lru" {
		t.Errorf("CONFIG GET maxmemory-policy = %v, %v", p, err)
	}
	rc.Close()
	for _, step := range []struct{ version, want string }{{"REDIS_4_0", "4.0.14"}, {"REDIS_5_0", "5.0.14"}} {
		uop, err := c.UpgradeInstance(ctx, &redispb.UpgradeInstanceRequest{Name: name, RedisVersion: step.version})
		if err != nil {
			t.Fatalf("UpgradeInstance %s: %v", step.version, err)
		}
		if inst, err = waitLRO(ctx, uop); err != nil {
			t.Fatalf("UpgradeInstance %s operation: %v", step.version, err)
		}
		if inst.GetRedisVersion() != step.version {
			t.Errorf("after the upgrade: %s", inst.GetRedisVersion())
		}
		if v := version(inst, pw); v != step.want {
			t.Errorf("%s server reports %s, want %s", step.version, v, step.want)
		}
		rc := redisAt(inst, pw)
		if got, err := rc.Get(ctx, "k").Result(); err != nil || got != "v" {
			t.Errorf("on %s GET k = %q, %v; want the key kept", step.version, got, err)
		}
		rc.Close()
	}

	haName, ha, hapw := create(fmt.Sprintf("eol40ha-%d", stamp), &redispb.Instance{Tier: redispb.Instance_STANDARD_HA, MemorySizeGb: 1, AuthEnabled: true,
		RedisVersion: "REDIS_4_0", LocationId: "us-central1-a", AlternativeLocationId: "us-central1-c"})
	if v := version(ha, hapw); v != "4.0.14" {
		t.Errorf("REDIS_4_0 server reports %s", v)
	}
	primary := redisAt(ha, hapw)
	if err := primary.Set(ctx, "h", "1", 0).Err(); err != nil {
		t.Fatalf("SET on the 4.0 primary: %v", err)
	}
	eventuallyTrue(t, "the 4.0 replica has the write (WAIT 1)", func() bool {
		n, err := primary.Do(ctx, "WAIT", 1, 1000).Int()
		return err == nil && n >= 1
	})
	primary.Close()
	fop, err := c.FailoverInstance(ctx, &redispb.FailoverInstanceRequest{Name: haName, DataProtectionMode: redispb.FailoverInstanceRequest_LIMITED_DATA_LOSS})
	if err != nil {
		t.Fatalf("FailoverInstance: %v", err)
	}
	after, err := waitLRO(ctx, fop)
	if err != nil {
		t.Fatalf("FailoverInstance operation: %v", err)
	}
	if after.GetCurrentLocationId() == "us-central1-a" || after.GetState() != redispb.Instance_READY {
		t.Errorf("after failover: current_location_id %s, state %s", after.GetCurrentLocationId(), after.GetState())
	}
	next := redisAt(after, hapw)
	defer next.Close()
	if r := role(ctx, t, next); r != "master" {
		t.Errorf("after failover host:port reaches a %s", r)
	}
	if v, err := next.Get(ctx, "h").Result(); err != nil || v != "1" {
		t.Errorf("GET h on the new 4.0 primary = %q, %v", v, err)
	}
	if err := next.Set(ctx, "h2", "2", 0).Err(); err != nil {
		t.Errorf("SET on the new 4.0 primary: %v", err)
	}
}
