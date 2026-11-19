package redisadmin

import (
	"time"

	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"google.golang.org/genproto/googleapis/type/timeofday"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// The maintenance policy of a Redis cluster and of a Valkey instance
// (#1909), checked as the protos document it: weekly_maintenance_window
// is "Minimum 1" and "the maximum number ... is expected to be one"; each
// window has a day (not DAY_OF_WEEK_UNSPECIFIED) and a UTC start_time, a
// time of day (hours 0 to 23), as Memorystore for Redis's is (maintenance.go).
// create_time and update_time are output only: when the policy was first
// set and last changed. Neither API has deny periods: the protos carry no
// such field, so there are none to keep.

// checkWindows checks n windows, day(i) and start(i) of each.
func checkWindows(n int, day func(int) int32, start func(int) *timeofday.TimeOfDay) error {
	if n == 0 {
		return apierror.InvalidArgument("maintenance_policy.weekly_maintenance_window needs a window: minimum 1")
	}
	if n > 1 {
		return apierror.InvalidArgument("maintenance_policy.weekly_maintenance_window holds at most one window, not %d", n)
	}
	for i := 0; i < n; i++ {
		if d := day(i); d < 1 || d > 7 {
			return apierror.InvalidArgument("maintenance_policy.weekly_maintenance_window.day is required: MONDAY to SUNDAY")
		}
		st := start(i)
		if st == nil || st.GetHours() < 0 || st.GetHours() > 23 || st.GetMinutes() < 0 || st.GetMinutes() > 59 ||
			st.GetSeconds() < 0 || st.GetSeconds() > 59 || st.GetNanos() < 0 || st.GetNanos() > 999999999 {
			return apierror.InvalidArgument("maintenance_policy.weekly_maintenance_window.start_time must be a time of day, hours 0 to 23")
		}
	}
	return nil
}

// clusterMaintenancePolicy is the policy a Redis cluster keeps of p, set at now and
// first set at created (zero: now); nil keeps none.
func clusterMaintenancePolicy(p *clusterpb.ClusterMaintenancePolicy, created, now time.Time) (*clusterpb.ClusterMaintenancePolicy, error) {
	if p == nil {
		return nil, nil
	}
	ws := p.GetWeeklyMaintenanceWindow()
	if err := checkWindows(len(ws), func(i int) int32 { return int32(ws[i].GetDay()) }, func(i int) *timeofday.TimeOfDay { return ws[i].GetStartTime() }); err != nil {
		return nil, err
	}
	if created.IsZero() {
		created = now
	}
	out := proto.Clone(p).(*clusterpb.ClusterMaintenancePolicy)
	out.CreateTime, out.UpdateTime = timestamppb.New(created), timestamppb.New(now)
	return out, nil
}

// valkeyMaintenancePolicy is clusterMaintenancePolicy of a Valkey instance's policy.
func valkeyMaintenancePolicy(p *memorystorepb.MaintenancePolicy, created, now time.Time) (*memorystorepb.MaintenancePolicy, error) {
	if p == nil {
		return nil, nil
	}
	ws := p.GetWeeklyMaintenanceWindow()
	if err := checkWindows(len(ws), func(i int) int32 { return int32(ws[i].GetDay()) }, func(i int) *timeofday.TimeOfDay { return ws[i].GetStartTime() }); err != nil {
		return nil, err
	}
	if created.IsZero() {
		created = now
	}
	out := proto.Clone(p).(*memorystorepb.MaintenancePolicy)
	out.CreateTime, out.UpdateTime = timestamppb.New(created), timestamppb.New(now)
	return out, nil
}
