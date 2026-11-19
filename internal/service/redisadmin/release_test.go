package redisadmin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	redispb "cloud.google.com/go/redis/apiv1/redispb"
	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"google.golang.org/genproto/googleapis/type/dayofweek"
	"google.golang.org/genproto/googleapis/type/timeofday"
	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestClusterAndValkeyMaintenancePolicy (#1909): one window, a day and a
// time of day; create_time and update_time set.
func TestClusterAndValkeyMaintenancePolicy(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	for _, p := range []*clusterpb.ClusterMaintenancePolicy{
		{},
		{WeeklyMaintenanceWindow: []*clusterpb.ClusterWeeklyMaintenanceWindow{{StartTime: &timeofday.TimeOfDay{Hours: 3}}}},
		{WeeklyMaintenanceWindow: []*clusterpb.ClusterWeeklyMaintenanceWindow{{Day: dayofweek.DayOfWeek_MONDAY}}},
		{WeeklyMaintenanceWindow: []*clusterpb.ClusterWeeklyMaintenanceWindow{{Day: dayofweek.DayOfWeek_MONDAY, StartTime: &timeofday.TimeOfDay{Hours: 24}}}},
		{WeeklyMaintenanceWindow: []*clusterpb.ClusterWeeklyMaintenanceWindow{
			{Day: dayofweek.DayOfWeek_MONDAY, StartTime: &timeofday.TimeOfDay{}}, {Day: dayofweek.DayOfWeek_TUESDAY, StartTime: &timeofday.TimeOfDay{}}}},
	} {
		if _, err := clusterMaintenancePolicy(p, time.Time{}, now); code(err) != codes.InvalidArgument {
			t.Errorf("%v = %v; want INVALID_ARGUMENT", p, err)
		}
	}
	created := now.Add(-time.Hour)
	out, err := valkeyMaintenancePolicy(&memorystorepb.MaintenancePolicy{WeeklyMaintenanceWindow: []*memorystorepb.WeeklyMaintenanceWindow{
		{Day: dayofweek.DayOfWeek_SUNDAY, StartTime: &timeofday.TimeOfDay{Hours: 3}}}}, created, now)
	if err != nil || !out.GetCreateTime().AsTime().Equal(created) || !out.GetUpdateTime().AsTime().Equal(now) {
		t.Errorf("policy %v, %v", out, err)
	}
	if out, err := clusterMaintenancePolicy(nil, created, now); out != nil || err != nil {
		t.Errorf("no policy = %v, %v", out, err)
	}
}

// TestReleaseUpdate (#1909): a release schedules maintenance on every
// instance with none, in its window a week away, and once the clock
// reaches it the update is applied.
func TestReleaseUpdate(t *testing.T) {
	s, _, _ := newCreating(t, store.NewMemory())
	clk := &clock{t: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)} // a Wednesday
	s.created.now = clk.now
	ctx := context.Background()
	for _, id := range []string{"windowed", "unwindowed"} {
		in := &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: 1}
		if id == "windowed" {
			in.MaintenancePolicy = &redispb.MaintenancePolicy{WeeklyMaintenanceWindow: []*redispb.WeeklyMaintenanceWindow{
				{Day: dayofweek.DayOfWeek_SUNDAY, StartTime: &timeofday.TimeOfDay{Hours: 3}}}}
		}
		op, err := s.CreateInstance(ctx, &redispb.CreateInstanceRequest{Parent: parent, InstanceId: id, Instance: in})
		if err != nil {
			t.Fatal(err)
		}
		wait(t, s, op)
	}
	project, region, _ := strings.Cut(strings.TrimPrefix(parent, "projects/"), "/locations/")
	rec := httptest.NewRecorder()
	s.ServeMaintenanceControl(rec, httptest.NewRequest(http.MethodPost, MaintenanceControlPath+"projects/"+project+"/locations/-:releaseUpdate", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "/instances/windowed") || !strings.Contains(rec.Body.String(), "/instances/unwindowed") {
		t.Fatalf("release = %d %s", rec.Code, rec.Body)
	}
	windowed, _ := s.GetInstance(ctx, &redispb.GetInstanceRequest{Name: parent + "/instances/windowed"})
	at := time.Date(2026, 11, 29, 3, 0, 0, 0, time.UTC) // the first Sunday 03:00 a week away
	if got := windowed.GetMaintenanceSchedule().GetStartTime().AsTime(); !got.Equal(at) {
		t.Errorf("windowed start %s; want %s", got, at)
	}
	unwindowed, _ := s.GetInstance(ctx, &redispb.GetInstanceRequest{Name: parent + "/instances/unwindowed"})
	if got := unwindowed.GetMaintenanceSchedule().GetStartTime().AsTime(); !got.Equal(clk.now().Add(7 * 24 * time.Hour)) {
		t.Errorf("unwindowed start %s", got)
	}
	// A second release keeps what is scheduled.
	if names, err := s.ReleaseUpdate(ctx, project, region); err != nil || len(names) != 0 {
		t.Errorf("second release = %v, %v", names, err)
	}
	// Before the windowed instance's start only the unwindowed one's
	// (a week on) is due.
	clk.set(at.Add(-time.Minute))
	s.created.maintenanceDue()
	waitApplied := func(id string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			inst, _ := s.GetInstance(ctx, &redispb.GetInstanceRequest{Name: parent + "/instances/" + id})
			if inst.GetState() == redispb.Instance_READY && inst.GetMaintenanceSchedule() == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s at its start: %v", id, inst)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitApplied("unwindowed")
	if inst, _ := s.GetInstance(ctx, &redispb.GetInstanceRequest{Name: parent + "/instances/windowed"}); inst.GetMaintenanceSchedule() == nil {
		t.Error("the windowed instance's maintenance ran before its window")
	}
	clk.set(at)
	s.created.maintenanceDue()
	waitApplied("windowed")
}
