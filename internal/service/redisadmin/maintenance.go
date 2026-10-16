package redisadmin

import (
	"context"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"google.golang.org/genproto/googleapis/type/timeofday"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// The maintenance policy (#1386) is stored and read back as Memorystore
// documents it: at most one weekly window, a day and a UTC start hour,
// one hour long, with its create and update times. CloudBurrow never
// schedules maintenance, so an instance has no maintenance_schedule and
// RescheduleMaintenance is FAILED_PRECONDITION: there is nothing to move.

// maintenancePolicy checks p and returns it as kept: output-only fields
// set, created at created (now for a new policy) and updated now.
func maintenancePolicy(p *redispb.MaintenancePolicy, created, now time.Time) (string, error) {
	ws := p.GetWeeklyMaintenanceWindow()
	if len(ws) > 1 {
		return "", apierror.InvalidArgument("maintenance_policy.weekly_maintenance_window holds at most one window, not %d", len(ws))
	}
	if len(p.GetDescription()) > 512 {
		return "", apierror.InvalidArgument("maintenance_policy.description is at most 512 characters")
	}
	out := &redispb.MaintenancePolicy{Description: p.GetDescription(), CreateTime: timestamppb.New(created), UpdateTime: timestamppb.New(now)}
	for _, w := range ws {
		if w.GetDay() == 0 {
			return "", apierror.InvalidArgument("maintenance_policy.weekly_maintenance_window.day is required")
		}
		st := w.GetStartTime()
		if st == nil || st.GetHours() < 0 || st.GetHours() > 23 || st.GetMinutes() < 0 || st.GetMinutes() > 59 ||
			st.GetSeconds() < 0 || st.GetSeconds() > 59 || st.GetNanos() < 0 || st.GetNanos() > 999999999 {
			return "", apierror.InvalidArgument("maintenance_policy.weekly_maintenance_window.start_time must be a time of day, hours 0 to 23")
		}
		out.WeeklyMaintenanceWindow = append(out.WeeklyMaintenanceWindow, &redispb.WeeklyMaintenanceWindow{
			Day: w.GetDay(), StartTime: proto.Clone(st).(*timeofday.TimeOfDay), Duration: durationpb.New(time.Hour)})
	}
	b, err := protojson.Marshal(out)
	return string(b), err
}

func renderPolicy(kept string) *redispb.MaintenancePolicy {
	if kept == "" {
		return nil
	}
	p := &redispb.MaintenancePolicy{}
	if protojson.Unmarshal([]byte(kept), p) != nil {
		return nil
	}
	return p
}

// policyCreated is when kept was first set, or now.
func policyCreated(kept string, now time.Time) time.Time {
	if p := renderPolicy(kept); p != nil && p.GetCreateTime() != nil {
		return p.GetCreateTime().AsTime()
	}
	return now
}

// RescheduleMaintenance is FAILED_PRECONDITION for every instance: no
// maintenance is ever scheduled here, so there is none to reschedule.
func (s *Server) RescheduleMaintenance(ctx context.Context, req *redispb.RescheduleMaintenanceRequest) (*longrunningpb.Operation, error) {
	if _, err := s.find(req.GetName()); err != nil {
		return nil, err
	}
	switch req.GetRescheduleType() {
	case redispb.RescheduleMaintenanceRequest_IMMEDIATE, redispb.RescheduleMaintenanceRequest_NEXT_AVAILABLE_WINDOW:
	case redispb.RescheduleMaintenanceRequest_SPECIFIC_TIME:
		if req.GetScheduleTime() == nil {
			return nil, apierror.InvalidArgument("schedule_time is required with reschedule_type SPECIFIC_TIME")
		}
	default:
		return nil, apierror.InvalidArgument("reschedule_type is required: IMMEDIATE, NEXT_AVAILABLE_WINDOW or SPECIFIC_TIME")
	}
	return nil, apierror.FailedPrecondition("instance %s has no scheduled maintenance to reschedule: CloudBurrow schedules none", req.GetName())
}
