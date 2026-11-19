//go:build browser

package browser

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	redisadmin "cloud.google.com/go/redis/apiv1"
	"cloud.google.com/go/redis/apiv1/redispb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestMemorystoreMaintenanceThroughThePage (#1524), in the memorystore
// shard. On a created Basic tier instance's page: Edit's maintenance
// window sets a Sunday 03:00 UTC maintenance_policy; Schedule maintenance
// (CloudBurrow) with no time schedules the first window a week away, which
// GetInstance's maintenance_schedule reads; Reschedule maintenance with
// SPECIFIC_TIME moves it two days; Cancel maintenance, confirmed, removes
// it; a second event, rescheduled IMMEDIATE, runs: the instance is READY
// again with no maintenance_schedule.
func TestMemorystoreMaintenanceThroughThePage(t *testing.T) {
	needService(t, "memorystore")
	addr := strings.TrimSpace(os.Getenv(envMemorystoreAdmin))
	project := strings.TrimSpace(os.Getenv(envMemorystoreProject))
	if addr == "" || project == "" {
		t.Skipf("%s and %s are not set: the maintenance is read back through the admin API itself", envMemorystoreAdmin, envMemorystoreProject)
	}
	ctx := context.Background()
	c, err := redisadmin.NewCloudRedisClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	id := fmt.Sprintf("browser-maint-%d", time.Now().UnixNano()%1e8)
	name := "projects/" + project + "/locations/us-central1/instances/" + id
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(context.Background(), &redispb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	op, err := c.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: "projects/" + project + "/locations/us-central1", InstanceId: id,
		Instance: &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	get := func(what string, ok func(*redispb.Instance) bool) *redispb.Instance {
		t.Helper()
		var inst *redispb.Instance
		for end := time.Now().Add(3 * time.Minute); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
			if inst, err = c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name}); err == nil && ok(inst) {
				return inst
			}
		}
		t.Fatalf("instance %s: %v, %v", what, inst, err)
		return nil
	}

	p := open(t)
	page := "/memorystore/" + id + "?project=" + project
	p.navigate(page)
	p.pageAction("Edit", map[string]string{"maintenanceDay": "SUNDAY", "maintenanceHour": "3"}, "")
	get("with a Sunday 03:00 window", func(in *redispb.Instance) bool {
		ws := in.GetMaintenancePolicy().GetWeeklyMaintenanceWindow()
		return len(ws) == 1 && ws[0].GetDay().String() == "SUNDAY" && ws[0].GetStartTime().GetHours() == 3
	})
	p.navigate(page)
	p.pageAction("Schedule maintenance (CloudBurrow)", map[string]string{"startTime": ""}, "")
	inst := get("with the event scheduled", func(in *redispb.Instance) bool { return in.GetMaintenanceSchedule() != nil })
	orig := inst.GetMaintenanceSchedule().GetStartTime().AsTime()
	if orig.Weekday() != time.Sunday || orig.Hour() != 3 {
		t.Errorf("scheduled for %s; want a Sunday 03:00 UTC", orig)
	}
	p.navigate(page)
	// The summary shows the event and its deadline.
	p.waitFor(`document.querySelector("#view").textContent.includes("Scheduled maintenance") &&
		document.querySelector("#view").textContent.includes("Reschedule deadline") &&
		document.querySelector("#view").textContent.includes("SUNDAY, 03:00")`)
	at := orig.Add(48 * time.Hour)
	p.pageAction("Reschedule maintenance", map[string]string{"rescheduleType": "SPECIFIC_TIME", "scheduleTime": at.Format(time.RFC3339)}, "")
	get("with the event two days on", func(in *redispb.Instance) bool {
		return in.GetMaintenanceSchedule().GetStartTime().AsTime().Equal(at)
	})
	p.navigate(page)
	p.clickAction("Cancel maintenance (CloudBurrow)", id)
	get("with the event cancelled", func(in *redispb.Instance) bool { return in.GetMaintenanceSchedule() == nil })

	p.navigate(page)
	p.pageAction("Schedule maintenance (CloudBurrow)", map[string]string{"startTime": ""}, "")
	get("with a second event", func(in *redispb.Instance) bool { return in.GetMaintenanceSchedule() != nil })
	p.navigate(page)
	p.pageAction("Reschedule maintenance", map[string]string{"rescheduleType": "IMMEDIATE"}, "")
	get("after the maintenance ran", func(in *redispb.Instance) bool {
		return in.GetState() == redispb.Instance_READY && in.GetMaintenanceSchedule() == nil
	})
}
