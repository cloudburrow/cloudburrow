//go:build compat

package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"google.golang.org/genproto/googleapis/type/dayofweek"
	"google.golang.org/genproto/googleapis/type/timeofday"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// scheduleRedisMaintenance schedules a maintenance event on a created
// Memorystore instance through CloudBurrow's control endpoint (not a
// Memorystore API).
func scheduleRedisMaintenance(t *testing.T, h *Harness, name string, start time.Time) {
	t.Helper()
	body := `{}`
	if !start.IsZero() {
		body = `{"startTime":"` + start.UTC().Format(time.RFC3339) + `"}`
	}
	url := "http://" + h.Endpoint(EnvMemorystoreAdmin) + "/cloudburrow/v1/" + name + ":scheduleMaintenance"
	res, err := http.Post(url, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	if res.StatusCode != http.StatusOK || out["maintenanceSchedule"] == nil {
		t.Fatalf("scheduleMaintenance: %d %v", res.StatusCode, out)
	}
}

func wantRedisCode(t *testing.T, what string, err error, want codes.Code) {
	t.Helper()
	if status.Code(err) != want {
		t.Errorf("%s = %v; want %s", what, err, want)
	}
}

// covers: google.cloud.redis.v1.CloudRedis/RescheduleMaintenance, google.cloud.redis.v1.CloudRedis/GetInstance
//
// TestMemorystoreRescheduleMaintenance (#1524), in the memorystore shard,
// with cloud.google.com/go/redis/apiv1 and gcloud. A Basic tier instance
// with a Sunday 03:00 UTC maintenance window: with nothing scheduled,
// RescheduleMaintenance is FAILED_PRECONDITION; once an event is scheduled
// (CloudBurrow's control endpoint), maintenance_schedule has the first
// window a week away, an end_time an hour on, can_reschedule and a
// schedule_deadline_time a week after the original time.
// SPECIFIC_TIME past that deadline is INVALID_ARGUMENT; `gcloud redis
// instances reschedule-maintenance --reschedule-type=specific-time` moves
// it two days, NEXT_AVAILABLE_WINDOW then to the next Sunday window, the
// deadline, and a third reschedule is FAILED_PRECONDITION with
// can_reschedule false.
// IMMEDIATE then runs it: the server is stopped and started again, and
// the instance is READY with no maintenance_schedule and serves.
func TestMemorystoreRescheduleMaintenance(t *testing.T) {
	h := New(t)
	c, _ := memorystoreAdminClient(t, h)
	g := newGcloudSession(t, h)
	id := fmt.Sprintf("maint-%d", time.Now().UnixNano()%1e8)
	name := "projects/" + g.project + "/locations/us-central1/instances/" + id
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(context.Background(), &redispb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = waitLRODone(context.Background(), op)
		}
	})
	ctx := h.Context()
	op, err := c.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: "projects/" + g.project + "/locations/us-central1", InstanceId: id,
		Instance: &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1, MaintenancePolicy: &redispb.MaintenancePolicy{
			WeeklyMaintenanceWindow: []*redispb.WeeklyMaintenanceWindow{{Day: dayofweek.DayOfWeek_SUNDAY, StartTime: &timeofday.TimeOfDay{Hours: 3}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	resched := func(typ redispb.RescheduleMaintenanceRequest_RescheduleType, at time.Time) error {
		req := &redispb.RescheduleMaintenanceRequest{Name: name, RescheduleType: typ}
		if !at.IsZero() {
			req.ScheduleTime = timestamppb.New(at)
		}
		op, err := c.RescheduleMaintenance(h.Context(), req)
		if err != nil {
			return err
		}
		_, err = op.Wait(h.Context())
		return err
	}
	wantRedisCode(t, "RescheduleMaintenance with nothing scheduled", resched(redispb.RescheduleMaintenanceRequest_NEXT_AVAILABLE_WINDOW, time.Time{}), codes.FailedPrecondition)

	scheduleRedisMaintenance(t, h, name, time.Time{})
	inst, err := c.GetInstance(h.Context(), &redispb.GetInstanceRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	ms := inst.GetMaintenanceSchedule()
	orig := ms.GetStartTime().AsTime()
	if orig.Weekday() != time.Sunday || orig.Hour() != 3 || time.Until(orig) < 7*24*time.Hour ||
		!ms.GetEndTime().AsTime().Equal(orig.Add(time.Hour)) || !ms.GetCanReschedule() ||
		!ms.GetScheduleDeadlineTime().AsTime().Equal(orig.Add(7*24*time.Hour)) {
		t.Fatalf("maintenance_schedule = %v; want the first Sunday 03:00 UTC a week away, an hour long, a deadline a week on", ms)
	}
	wantRedisCode(t, "SPECIFIC_TIME past the deadline", resched(redispb.RescheduleMaintenanceRequest_SPECIFIC_TIME, orig.Add(8*24*time.Hour)), codes.InvalidArgument)

	at := orig.Add(2 * 24 * time.Hour)
	g.must("redis", "instances", "reschedule-maintenance", id, "--project="+g.project, "--region=us-central1",
		"--reschedule-type=specific-time", "--schedule-time="+at.Format(time.RFC3339), "--quiet")
	got := strings.TrimSpace(g.must("redis", "instances", "describe", id, "--project="+g.project, "--region=us-central1",
		"--format=value(maintenanceSchedule.startTime)"))
	if gt, err := time.Parse(time.RFC3339Nano, got); err != nil || !gt.Equal(at) {
		t.Errorf("after gcloud reschedule-maintenance: startTime %q, want %s", got, at.Format(time.RFC3339))
	}
	// The next window after a Tuesday 03:00 is the Sunday a week after the
	// original time: the deadline, still allowed.
	if err := resched(redispb.RescheduleMaintenanceRequest_NEXT_AVAILABLE_WINDOW, time.Time{}); err != nil {
		t.Fatal(err)
	}
	at2 := orig.Add(7 * 24 * time.Hour)
	inst, _ = c.GetInstance(h.Context(), &redispb.GetInstanceRequest{Name: name})
	if ms := inst.GetMaintenanceSchedule(); !ms.GetStartTime().AsTime().Equal(at2) || ms.GetCanReschedule() {
		t.Errorf("after two reschedules, maintenance_schedule = %v; want %s, can_reschedule false", ms, at2)
	}
	wantRedisCode(t, "a third reschedule", resched(redispb.RescheduleMaintenanceRequest_SPECIFIC_TIME, orig.Add(6*24*time.Hour)), codes.FailedPrecondition)

	rc := redisAt(inst, "")
	defer rc.Close()
	if err := rc.Set(h.Context(), "k", "v", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := resched(redispb.RescheduleMaintenanceRequest_IMMEDIATE, time.Time{}); err != nil {
		t.Fatalf("IMMEDIATE: %v", err)
	}
	sawMaintenance := false
	var after *redispb.Instance
	for end := time.Now().Add(4 * time.Minute); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		after, err = c.GetInstance(h.Context(), &redispb.GetInstanceRequest{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		sawMaintenance = sawMaintenance || after.GetState() == redispb.Instance_MAINTENANCE
		if after.GetState() == redispb.Instance_READY && after.GetMaintenanceSchedule() == nil {
			break
		}
	}
	if after.GetState() != redispb.Instance_READY || after.GetMaintenanceSchedule() != nil {
		t.Fatalf("after IMMEDIATE: %v", after)
	}
	if !sawMaintenance {
		t.Log("the instance's MAINTENANCE state passed between two polls")
	}
	rc2 := redisAt(after, "")
	defer rc2.Close()
	if err := rc2.Ping(h.Context()).Err(); err != nil {
		t.Errorf("after maintenance, PING = %v", err)
	}
}

// covers: google.cloud.redis.v1.CloudRedis/RescheduleMaintenance
//
// TestMemorystoreStandardTierMaintenanceFailsOver (#1524): a Standard tier
// instance's maintenance, run at once (IMMEDIATE), fails it over, as
// Memorystore documents: the primary moves to the other zone, and the
// endpoint keeps the data.
func TestMemorystoreStandardTierMaintenanceFailsOver(t *testing.T) {
	h := New(t)
	c, project := memorystoreAdminClient(t, h)
	id := fmt.Sprintf("maint-ha-%d", time.Now().UnixNano()%1e8)
	name := "projects/" + project + "/locations/us-central1/instances/" + id
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(context.Background(), &redispb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = waitLRODone(context.Background(), op)
		}
	})
	op, err := c.CreateInstance(h.Context(), &redispb.CreateInstanceRequest{Parent: "projects/" + project + "/locations/us-central1", InstanceId: id,
		Instance: &redispb.Instance{Tier: redispb.Instance_STANDARD_HA, MemorySizeGb: 1, LocationId: "us-central1-a"}})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := op.Wait(h.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx := h.Context()
	rc := redisAt(inst, "")
	defer rc.Close()
	if err := rc.Set(ctx, "k", "v", 0).Err(); err != nil {
		t.Fatal(err)
	}
	eventuallyTrue(t, "the replica has the key", func() bool {
		n, err := rc.Do(ctx, "WAIT", 1, 1000).Int()
		return err == nil && n >= 1
	})
	scheduleRedisMaintenance(t, h, name, time.Time{})
	rop, err := c.RescheduleMaintenance(h.Context(), &redispb.RescheduleMaintenanceRequest{Name: name, RescheduleType: redispb.RescheduleMaintenanceRequest_IMMEDIATE})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rop.Wait(h.Context()); err != nil {
		t.Fatal(err)
	}
	var after *redispb.Instance
	for end := time.Now().Add(2 * time.Minute); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		if after, err = c.GetInstance(h.Context(), &redispb.GetInstanceRequest{Name: name}); err != nil {
			t.Fatal(err)
		}
		if after.GetState() == redispb.Instance_READY && after.GetMaintenanceSchedule() == nil && after.GetCurrentLocationId() != inst.GetCurrentLocationId() {
			break
		}
	}
	if after.GetCurrentLocationId() == inst.GetCurrentLocationId() || after.GetState() != redispb.Instance_READY {
		t.Fatalf("after maintenance: primary in %s (was %s), %s; want a failover", after.GetCurrentLocationId(), inst.GetCurrentLocationId(), after.GetState())
	}
	rc2 := redisAt(after, "")
	defer rc2.Close()
	eventuallyTrue(t, "the endpoint serves the data after the failover", func() bool {
		v, err := rc2.Get(h.Context(), "k").Result()
		return err == nil && v == "v"
	})
}
