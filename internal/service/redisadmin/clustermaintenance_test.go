package redisadmin

import (
	"context"
	"testing"
	"time"

	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	"google.golang.org/genproto/googleapis/type/dayofweek"
	"google.golang.org/genproto/googleapis/type/timeofday"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestValkeyMaintenanceRules (#1524): a Valkey instance's event starts in
// the first window a week away and lasts an hour; SPECIFIC_TIME moves it
// within 14 days of the original time, any number of times, never within
// the hour before it; IMMEDIATE replaces every node, create-before-destroy,
// the primary handed over to a new node, and the primary endpoint follows.
func TestValkeyMaintenanceRules(t *testing.T) {
	oldEvery := healthEvery
	healthEvery = time.Hour
	t.Cleanup(func() { healthEvery = oldEvery })
	s, _ := newHA(t)
	f := &vkNodes{t: t, nodes: map[string]*vkNode{}, byAddr: map[string]*vkNode{}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s.EnableValkey(ctx, f, func(addr, _, _ string) ClusterNode { return vkConn{f, addr} }, nil)
	ms := NewMemorystoreServer(s)
	c := s.valkey
	clk := &clock{t: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}
	c.now = clk.now
	name := haParent + "/instances/vk-maint"
	if _, err := ms.CreateInstance(ctx, &memorystorepb.CreateInstanceRequest{Parent: haParent, InstanceId: "vk-maint", Instance: &memorystorepb.Instance{
		Mode: memorystorepb.Instance_CLUSTER_DISABLED, ReplicaCount: proto.Int32(1), NodeType: memorystorepb.Instance_SHARED_CORE_NANO,
		PscAutoConnections: []*memorystorepb.PscAutoConnection{{ProjectId: "test-project", Network: "projects/test-project/global/networks/default"}},
		MaintenancePolicy: &memorystorepb.MaintenancePolicy{WeeklyMaintenanceWindow: []*memorystorepb.WeeklyMaintenanceWindow{
			{Day: dayofweek.DayOfWeek_SUNDAY, StartTime: &timeofday.TimeOfDay{Hours: 3}}}},
	}}); err != nil {
		t.Fatal(err)
	}
	var inst *memorystorepb.Instance
	for i := 0; i < 500; i++ {
		if inst, _ = ms.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: name}); inst.GetState() == memorystorepb.Instance_ACTIVE {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if inst.GetState() != memorystorepb.Instance_ACTIVE {
		t.Fatalf("instance %v", inst)
	}
	resched := func(typ memorystorepb.RescheduleMaintenanceRequest_RescheduleType, at time.Time) error {
		req := &memorystorepb.RescheduleMaintenanceRequest{Name: name, RescheduleType: typ}
		if !at.IsZero() {
			req.ScheduleTime = timestamppb.New(at)
		}
		_, err := ms.RescheduleMaintenance(ctx, req)
		return err
	}
	want := func(what string, err error, code codes.Code) {
		t.Helper()
		if status.Code(err) != code {
			t.Errorf("%s = %v; want %s", what, err, code)
		}
	}
	want("with nothing scheduled", resched(memorystorepb.RescheduleMaintenanceRequest_IMMEDIATE, time.Time{}), codes.FailedPrecondition)
	want("no type", resched(0, time.Time{}), codes.InvalidArgument)
	want("SPECIFIC_TIME with no time", resched(memorystorepb.RescheduleMaintenanceRequest_SPECIFIC_TIME, time.Time{}), codes.InvalidArgument)

	if _, err := s.ControlClusterMaintenance(name, time.Time{}, false); err != nil {
		t.Fatal(err)
	}
	inst, _ = ms.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: name})
	orig := time.Date(2026, 11, 29, 3, 0, 0, 0, time.UTC)
	if ms := inst.GetMaintenanceSchedule(); !ms.GetStartTime().AsTime().Equal(orig) || !ms.GetEndTime().AsTime().Equal(orig.Add(time.Hour)) {
		t.Fatalf("maintenance_schedule = %v; want %s for an hour", ms, orig)
	}
	want("15 days on", resched(memorystorepb.RescheduleMaintenanceRequest_SPECIFIC_TIME, orig.Add(15*24*time.Hour)), codes.InvalidArgument)
	want("before the original time", resched(memorystorepb.RescheduleMaintenanceRequest_SPECIFIC_TIME, orig.Add(-time.Hour)), codes.InvalidArgument)
	for i, d := range []time.Duration{24 * time.Hour, 3 * 24 * time.Hour, 14 * 24 * time.Hour} {
		if err := resched(memorystorepb.RescheduleMaintenanceRequest_SPECIFIC_TIME, orig.Add(d)); err != nil {
			t.Fatalf("reschedule %d: %v", i+1, err)
		}
	}
	clk.set(orig.Add(14*24*time.Hour - 30*time.Minute))
	want("within the hour", resched(memorystorepb.RescheduleMaintenanceRequest_IMMEDIATE, time.Time{}), codes.FailedPrecondition)
	clk.set(orig)

	c.mu.Lock()
	before := map[string]bool{}
	for _, n := range c.byName[name].Nodes {
		before[n.Server] = true
	}
	c.mu.Unlock()
	if err := resched(memorystorepb.RescheduleMaintenanceRequest_IMMEDIATE, time.Time{}); err != nil {
		t.Fatal(err)
	}
	done := false
	for i := 0; i < 1000 && !done; i++ {
		time.Sleep(10 * time.Millisecond)
		c.mu.Lock()
		_, busy := c.busy[name]
		r := c.byName[name]
		done = !busy && r.Scheduled == nil && len(r.Nodes) == 2
		for _, n := range r.Nodes {
			done = done && !before[n.Server]
		}
		c.mu.Unlock()
	}
	if !done {
		c.mu.Lock()
		t.Fatalf("after maintenance, nodes %v (were %v)", c.byName[name].Nodes, before)
	}
	inst, _ = ms.GetInstance(ctx, &memorystorepb.GetInstanceRequest{Name: name})
	if inst.GetMaintenanceSchedule() != nil || inst.GetState() != memorystorepb.Instance_ACTIVE {
		t.Errorf("after maintenance: %v", inst)
	}
	c.mu.Lock()
	primary := c.byName[name].firstPrimary().Server
	c.mu.Unlock()
	ph, pp := vkEndpoint(t, inst, memorystorepb.ConnectionType_CONNECTION_TYPE_PRIMARY)
	if got := greet(t, ph, pp); got != primary {
		t.Errorf("the primary endpoint reaches %s, want the new primary %s", got, primary)
	}
}
