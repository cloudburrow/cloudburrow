package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"google.golang.org/genproto/googleapis/type/dayofweek"
	"google.golang.org/genproto/googleapis/type/timeofday"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// The Memorystore instance page's maintenance (#1524): Edit's weekly
// maintenance window (maintenance_policy); the window, scheduled
// maintenance and its deadline in the summary; Reschedule maintenance
// (RescheduleMaintenance) while one is scheduled; and CloudBurrow's own
// Schedule maintenance and Cancel maintenance, which stand in for
// Memorystore scheduling an update.

// memorystoreWindowDays are Edit's window day choices.
var memorystoreWindowDays = []string{"NONE", "MONDAY", "TUESDAY", "WEDNESDAY", "THURSDAY", "FRIDAY", "SATURDAY", "SUNDAY"}

func memorystoreWindow(inst *redispb.Instance) *redispb.WeeklyMaintenanceWindow {
	if ws := inst.GetMaintenancePolicy().GetWeeklyMaintenanceWindow(); len(ws) > 0 {
		return ws[0]
	}
	return nil
}

func memorystoreWindowDay(inst *redispb.Instance) string {
	if w := memorystoreWindow(inst); w != nil {
		return w.GetDay().String()
	}
	return "NONE"
}

func memorystoreWindowHour(inst *redispb.Instance) string {
	if w := memorystoreWindow(inst); w != nil {
		return strconv.Itoa(int(w.GetStartTime().GetHours()))
	}
	return ""
}

// memorystorePolicy is Edit's window as a maintenance_policy; NONE is
// none.
func memorystorePolicy(day, hour string) (*redispb.MaintenancePolicy, error) {
	if day == "NONE" {
		return nil, nil
	}
	d, ok := dayofweek.DayOfWeek_value[day]
	if !ok || d == 0 {
		return nil, fmt.Errorf("maintenance window day %q is not a day of the week", day)
	}
	h, err := strconv.Atoi(hour)
	if err != nil || h < 0 || h > 23 {
		return nil, fmt.Errorf("maintenance window hour %q is not 0 to 23", hour)
	}
	return &redispb.MaintenancePolicy{WeeklyMaintenanceWindow: []*redispb.WeeklyMaintenanceWindow{
		{Day: dayofweek.DayOfWeek(d), StartTime: &timeofday.TimeOfDay{Hours: int32(h)}}}}, nil
}

// memorystoreMaintenanceSummary is the instance's window and event.
func memorystoreMaintenanceSummary(inst *redispb.Instance) []console.Property {
	var out []console.Property
	if w := memorystoreWindow(inst); w != nil {
		out = append(out, console.Property{Label: "Maintenance window (UTC)",
			Value: fmt.Sprintf("%s, %02d:%02d, 1 hour", w.GetDay(), w.GetStartTime().GetHours(), w.GetStartTime().GetMinutes())})
	}
	if m := inst.GetMaintenanceSchedule(); m != nil {
		out = append(out,
			console.Property{Label: "Scheduled maintenance", Value: m.GetStartTime().AsTime().UTC().Format(time.RFC3339) + " to " + m.GetEndTime().AsTime().UTC().Format(time.RFC3339)},
			console.Property{Label: "Reschedule deadline", Value: m.GetScheduleDeadlineTime().AsTime().UTC().Format(time.RFC3339)})
	}
	return out
}

// memorystoreMaintenanceActions are a created instance's maintenance
// actions.
func memorystoreMaintenanceActions(inst *redispb.Instance) []console.Action {
	acts := []console.Action{{ID: "schedule-maintenance", Label: "Schedule maintenance (CloudBurrow)", Fields: []console.Field{
		{Name: "startTime", Label: "Start time (UTC, RFC 3339)", Type: "text",
			Help: "CloudBurrow only, standing in for an update Memorystore schedules: empty for the first maintenance window a week away. At this time a Basic tier instance is unavailable while its server restarts, and a Standard tier instance fails over."}}}}
	if m := inst.GetMaintenanceSchedule(); m != nil {
		help := "Up to twice, until " + m.GetScheduleDeadlineTime().AsTime().UTC().Format(time.RFC3339) + "."
		if !m.GetCanReschedule() {
			help = "It was rescheduled twice already, or starts within the hour: only IMMEDIATE may still apply, if more than an hour remains."
		}
		acts = append(acts,
			console.Action{ID: "reschedule-maintenance", Label: "Reschedule maintenance", Fields: []console.Field{
				{Name: "rescheduleType", Label: "Reschedule", Type: "select", Required: true, Default: "NEXT_AVAILABLE_WINDOW",
					Options: []string{"IMMEDIATE", "NEXT_AVAILABLE_WINDOW", "SPECIFIC_TIME"},
					Help:    "IMMEDIATE applies the update now; NEXT_AVAILABLE_WINDOW defers it a week, to the next window; SPECIFIC_TIME to the time below, within a week of the original time. " + help},
				{Name: "scheduleTime", Label: "Specific time (UTC, RFC 3339)", Type: "text", Help: "With SPECIFIC_TIME only."}}},
			console.Action{ID: "cancel-maintenance", Label: "Cancel maintenance (CloudBurrow)",
				Confirm: "The scheduled maintenance event is removed, as if Memorystore had not scheduled it."})
	}
	return acts
}

// maintenanceAct performs the maintenance actions.
func (p memorystoreProvider) maintenanceAct(ctx context.Context, name, action string, v map[string]string) error {
	switch action {
	case "schedule-maintenance":
		var start time.Time
		if s := strings.TrimSpace(v["startTime"]); s != "" {
			t, err := time.Parse(time.RFC3339Nano, s)
			if err != nil {
				return fmt.Errorf("start time %q is not an RFC 3339 time", s)
			}
			start = t
		}
		_, err := p.api().ScheduleMaintenance(ctx, name, start)
		return err
	case "cancel-maintenance":
		_, err := p.api().CancelMaintenance(ctx, name)
		return err
	}
	typ, ok := redispb.RescheduleMaintenanceRequest_RescheduleType_value[v["rescheduleType"]]
	if !ok {
		return fmt.Errorf("reschedule %q is not IMMEDIATE, NEXT_AVAILABLE_WINDOW or SPECIFIC_TIME", v["rescheduleType"])
	}
	req := &redispb.RescheduleMaintenanceRequest{Name: name, RescheduleType: redispb.RescheduleMaintenanceRequest_RescheduleType(typ)}
	if s := strings.TrimSpace(v["scheduleTime"]); s != "" && req.RescheduleType == redispb.RescheduleMaintenanceRequest_SPECIFIC_TIME {
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return fmt.Errorf("specific time %q is not an RFC 3339 time", s)
		}
		req.ScheduleTime = timestamppb.New(t)
	}
	op, err := p.api().RescheduleMaintenance(ctx, req)
	if err != nil {
		return err
	}
	return p.wait(ctx, op.GetName())
}
