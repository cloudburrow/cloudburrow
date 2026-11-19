package redisadmin

import (
	"context"
	"sync"
	"testing"
	"time"

	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"google.golang.org/genproto/googleapis/type/dayofweek"
	"google.golang.org/genproto/googleapis/type/timeofday"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// clock is a settable time.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

func TestPolicyWindow(t *testing.T) {
	kept, err := maintenancePolicy(&redispb.MaintenancePolicy{WeeklyMaintenanceWindow: []*redispb.WeeklyMaintenanceWindow{
		{Day: dayofweek.DayOfWeek_SUNDAY, StartTime: &timeofday.TimeOfDay{Hours: 3}}}}, time.Now(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// 2026-11-18 is a Wednesday.
	at := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	if w, _ := policyWindow(kept, at); !w.Equal(time.Date(2026, 11, 22, 3, 0, 0, 0, time.UTC)) {
		t.Errorf("next Sunday 03:00 after %s = %s", at, w)
	}
	if got := firstStart(kept, at); !got.Equal(time.Date(2026, 11, 29, 3, 0, 0, 0, time.UTC)) {
		t.Errorf("first start = %s; want the first window a week away", got)
	}
	if got := firstStart("", at.Add(time.Minute)); !got.Equal(at.Add(7*24*time.Hour + time.Hour)) {
		t.Errorf("first start with no policy = %s; want a week on, to the hour", got)
	}
}

func TestRescheduleMaintenanceRules(t *testing.T) {
	s, p, _ := newCreating(t, store.NewMemory())
	clk := &clock{t: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}
	s.created.now = clk.now
	ctx := context.Background()
	op, err := s.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent, InstanceId: "cache",
		Instance: &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1}})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, s, op)
	name := parent + "/instances/cache"
	re := func(typ redispb.RescheduleMaintenanceRequest_RescheduleType, at *time.Time) error {
		req := &redispb.RescheduleMaintenanceRequest{Name: name, RescheduleType: typ}
		if at != nil {
			req.ScheduleTime = timestamppb.New(*at)
		}
		op, err := s.RescheduleMaintenance(ctx, req)
		if err == nil && !op.GetDone() {
			t.Errorf("reschedule operation not done: %v", op)
		}
		return err
	}
	if err := re(redispb.RescheduleMaintenanceRequest_NEXT_AVAILABLE_WINDOW, nil); code(err) != codes.FailedPrecondition {
		t.Errorf("with nothing scheduled = %v; want FAILED_PRECONDITION", err)
	}
	if err := re(redispb.RescheduleMaintenanceRequest_SPECIFIC_TIME, nil); code(err) != codes.InvalidArgument {
		t.Errorf("SPECIFIC_TIME with no time = %v; want INVALID_ARGUMENT", err)
	}
	inst, err := s.ScheduleMaintenance(ctx, name, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	orig := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	ms := inst.GetMaintenanceSchedule()
	if !ms.GetStartTime().AsTime().Equal(orig) || !ms.GetEndTime().AsTime().Equal(orig.Add(time.Hour)) || !ms.GetCanReschedule() ||
		!ms.GetScheduleDeadlineTime().AsTime().Equal(orig.Add(7*24*time.Hour)) {
		t.Fatalf("maintenance_schedule = %v", ms)
	}
	late := orig.Add(7*24*time.Hour + time.Minute)
	if err := re(redispb.RescheduleMaintenanceRequest_SPECIFIC_TIME, &late); code(err) != codes.InvalidArgument {
		t.Errorf("past the deadline = %v; want INVALID_ARGUMENT", err)
	}
	early := orig.Add(-time.Minute)
	if err := re(redispb.RescheduleMaintenanceRequest_SPECIFIC_TIME, &early); code(err) != codes.InvalidArgument {
		t.Errorf("before the original time = %v; want INVALID_ARGUMENT", err)
	}
	at := orig.Add(2 * 24 * time.Hour)
	if err := re(redispb.RescheduleMaintenanceRequest_SPECIFIC_TIME, &at); err != nil {
		t.Fatal(err)
	}
	if err := re(redispb.RescheduleMaintenanceRequest_NEXT_AVAILABLE_WINDOW, nil); code(err) != codes.FailedPrecondition {
		t.Errorf("a week after %s is past the deadline: %v; want FAILED_PRECONDITION", at, err)
	}
	at2 := orig.Add(3 * 24 * time.Hour)
	if err := re(redispb.RescheduleMaintenanceRequest_SPECIFIC_TIME, &at2); err != nil {
		t.Fatal(err)
	}
	inst, _ = s.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
	if ms := inst.GetMaintenanceSchedule(); !ms.GetStartTime().AsTime().Equal(at2) || ms.GetCanReschedule() {
		t.Errorf("after two reschedules = %v; want %s and can_reschedule false", ms, at2)
	}
	at3 := orig.Add(4 * 24 * time.Hour)
	if err := re(redispb.RescheduleMaintenanceRequest_SPECIFIC_TIME, &at3); code(err) != codes.FailedPrecondition {
		t.Errorf("a third reschedule = %v; want FAILED_PRECONDITION", err)
	}
	clk.set(at2.Add(-30 * time.Minute))
	if err := re(redispb.RescheduleMaintenanceRequest_IMMEDIATE, nil); code(err) != codes.FailedPrecondition {
		t.Errorf("less than an hour before = %v; want FAILED_PRECONDITION", err)
	}

	// At its start the Basic tier server is started again.
	before := len(p.running)
	clk.set(at2)
	s.created.maintenanceDue()
	deadline := time.Now().Add(5 * time.Second)
	for {
		inst, _ = s.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
		if inst.GetState() == redispb.Instance_READY && inst.GetMaintenanceSchedule() == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after maintenance: %v", inst)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(p.running) != before {
		t.Errorf("servers = %d; want %d", len(p.running), before)
	}

	// IMMEDIATE runs it at once; cancel removes one.
	if _, err := s.ScheduleMaintenance(ctx, name, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if inst, _ := s.CancelMaintenance(ctx, name); inst.GetMaintenanceSchedule() != nil {
		t.Errorf("cancelled, maintenance_schedule = %v", inst.GetMaintenanceSchedule())
	}
	if _, err := s.ScheduleMaintenance(ctx, name, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := re(redispb.RescheduleMaintenanceRequest_IMMEDIATE, nil); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		inst, _ = s.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
		if inst.GetMaintenanceSchedule() == nil && inst.GetState() == redispb.Instance_READY {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("IMMEDIATE did not run: %v", inst)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := s.ScheduleMaintenance(ctx, "projects/my-proj/locations/us-central1/instances/nope", time.Time{}); code(err) != codes.NotFound {
		t.Errorf("unknown instance = %v; want NOT_FOUND", err)
	}
}

func TestStandardTierMaintenanceFailsOver(t *testing.T) {
	s, _ := newHA(t)
	inst := createHA(t, s, "ha", &redispb.Instance{Tier: redispb.Instance_STANDARD_HA, MemorySizeGb: 1})
	name := inst.GetName()
	ctx := context.Background()
	s.created.mu.Lock()
	was := s.created.byID[name].Primary
	s.created.mu.Unlock()
	if _, err := s.ScheduleMaintenance(ctx, name, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	s.created.maintenanceDue()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.created.mu.Lock()
		r := s.created.byID[name]
		now, state := r.Primary, r.state
		_, busy := s.created.busy[name]
		s.created.mu.Unlock()
		if !busy && state == redispb.Instance_READY && now != was {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("maintenance did not fail over: primary node-%d (was node-%d), %s", now, was, state)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
