//go:build compat

package compat

import (
	"context"
	"fmt"
	"testing"
	"time"

	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"github.com/redis/go-redis/v9"
)

// covers: google.cloud.redis.v1.CloudRedis/CreateInstance, google.cloud.redis.v1.CloudRedis/RescheduleMaintenance, google.cloud.redis.v1.CloudRedis/GetInstance
//
// TestMemorystoreDataAcrossServerRestart (#1911), in the memorystore
// shard, with cloud.google.com/go/redis/apiv1 and go-redis: each tier and
// persistence mode across a restart of its server, a maintenance run at
// once (IMMEDIATE), as Memorystore documents
// (https://docs.cloud.google.com/memorystore/docs/redis/about-rdb-snapshots,
// /ha-and-replicas):
//   - Basic tier, persistence DISABLED: the data is lost, and the server
//     keeps neither an append-only file nor a save point;
//   - Basic tier, RDB ONE_HOUR: the data comes back from the snapshot;
//   - Standard tier, persistence DISABLED: the maintenance fails over to
//     the replica, which has the data.
func TestMemorystoreDataAcrossServerRestart(t *testing.T) {
	h := New(t)
	c, project := memorystoreAdminClient(t, h)
	parent := "projects/" + project + "/locations/us-central1"
	for _, tc := range []struct {
		name string
		inst *redispb.Instance
		kept bool
	}{
		{"basic-none", &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1}, false},
		{"basic-rdb", &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1,
			PersistenceConfig: &redispb.PersistenceConfig{PersistenceMode: redispb.PersistenceConfig_RDB, RdbSnapshotPeriod: redispb.PersistenceConfig_ONE_HOUR}}, true},
		{"standard-none", &redispb.Instance{Tier: redispb.Instance_STANDARD_HA, MemorySizeGb: 1}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := h.For(t)
			ctx := memorystoreWait(t)
			id := fmt.Sprintf("p1911-%s-%d", tc.name, time.Now().UnixNano()%1e6)
			name := parent + "/instances/" + id
			t.Cleanup(func() {
				if op, err := c.DeleteInstance(context.Background(), &redispb.DeleteInstanceRequest{Name: name}); err == nil {
					_ = waitLRODone(context.Background(), op)
				}
			})
			op, err := c.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent, InstanceId: id, Instance: tc.inst})
			if err != nil {
				t.Fatal(err)
			}
			inst, err := waitLRO(ctx, op)
			if err != nil {
				t.Fatal(err)
			}
			rc := redisAt(inst, "")
			defer rc.Close()
			if err := rc.Set(ctx, "k", "before-restart", 0).Err(); err != nil {
				t.Fatal(err)
			}
			if tc.name == "basic-none" {
				cfg, err := rc.ConfigGet(ctx, "*").Result()
				if err != nil || cfg["appendonly"] != "no" || cfg["save"] != "" {
					t.Errorf("without persistence: appendonly %q, save %q, %v; want no and none", cfg["appendonly"], cfg["save"], err)
				}
			}
			scheduleRedisMaintenance(t, h, name, time.Time{})
			rop, err := c.RescheduleMaintenance(ctx, &redispb.RescheduleMaintenanceRequest{Name: name, RescheduleType: redispb.RescheduleMaintenanceRequest_IMMEDIATE})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := waitLRO(ctx, rop); err != nil {
				t.Fatal(err)
			}
			var after *redispb.Instance
			for end := time.Now().Add(4 * time.Minute); time.Now().Before(end); time.Sleep(500 * time.Millisecond) {
				if after, err = c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name}); err != nil {
					t.Fatal(err)
				}
				if after.GetState() == redispb.Instance_READY && after.GetMaintenanceSchedule() == nil {
					break
				}
			}
			if after.GetState() != redispb.Instance_READY || after.GetMaintenanceSchedule() != nil {
				t.Fatalf("after the maintenance: %v", after)
			}
			rc2 := redisAt(after, "")
			defer rc2.Close()
			var got string
			for end := time.Now().Add(time.Minute); ; time.Sleep(500 * time.Millisecond) {
				got, err = rc2.Get(ctx, "k").Result()
				if err == nil || err == redis.Nil || time.Now().After(end) {
					break
				}
			}
			switch {
			case tc.kept && (err != nil || got != "before-restart"):
				t.Errorf("after the restart GET = %q, %v; want the key kept", got, err)
			case !tc.kept && err != redis.Nil:
				t.Errorf("without persistence after the restart GET = %q, %v; want it lost (redis: nil)", got, err)
			}
		})
	}
}
