package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"google.golang.org/genproto/googleapis/type/dayofweek"
	"google.golang.org/genproto/googleapis/type/timeofday"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/redisadmin"
)

// The Redis Cluster and Memorystore for Valkey pages' maintenance (#1524):
// the weekly window (maintenance_policy) and the scheduled maintenance in
// the summary; Set maintenance window; Reschedule maintenance
// (RescheduleClusterMaintenance, RescheduleMaintenance) while one is
// scheduled; and CloudBurrow's own Schedule maintenance and Cancel
// maintenance, which stand in for Memorystore scheduling an update.

// clusterMaintenance is what the pages show of either API.
type clusterMaintenance struct {
	day        dayofweek.DayOfWeek
	hour       int32
	hasWindow  bool
	start, end *timestamppb.Timestamp
	what       string
}

func redisClusterMaintenance(c *clusterpb.Cluster) clusterMaintenance {
	m := clusterMaintenance{what: "cluster"}
	if ws := c.GetMaintenancePolicy().GetWeeklyMaintenanceWindow(); len(ws) > 0 {
		m.day, m.hour, m.hasWindow = ws[0].GetDay(), ws[0].GetStartTime().GetHours(), true
	}
	if s := c.GetMaintenanceSchedule(); s != nil {
		m.start, m.end = s.GetStartTime(), s.GetEndTime()
	}
	return m
}

func valkeyMaintenance(i *memorystorepb.Instance) clusterMaintenance {
	m := clusterMaintenance{what: "instance"}
	if ws := i.GetMaintenancePolicy().GetWeeklyMaintenanceWindow(); len(ws) > 0 {
		m.day, m.hour, m.hasWindow = ws[0].GetDay(), ws[0].GetStartTime().GetHours(), true
	}
	if s := i.GetMaintenanceSchedule(); s != nil && s.GetStartTime() != nil {
		m.start, m.end = s.GetStartTime(), s.GetEndTime()
	}
	return m
}

// summary is the window and event, as properties.
func (m clusterMaintenance) summary() []console.Property {
	var out []console.Property
	if m.hasWindow {
		out = append(out, console.Property{Label: "Maintenance window (UTC)", Value: fmt.Sprintf("%s, %02d:00, 1 hour", m.day, m.hour)})
	}
	if m.start != nil {
		out = append(out,
			console.Property{Label: "Scheduled maintenance", Value: m.start.AsTime().UTC().Format(time.RFC3339) + " to " + m.end.AsTime().UTC().Format(time.RFC3339)})
	}
	return out
}

// actions are the maintenance actions. Edit maintenance window is
// Google's dialog (#2099): "Next to the Maintenance window field, click
// Edit ... Select Custom. From the Day menu, select the day ... From the
// Time menu, select the hour ... Click Update cluster"
// (https://docs.cloud.google.com/memorystore/docs/cluster/find-and-set-maintenance-windows,
// read 2026-11-21); Default removes the window.
func (m clusterMaintenance) actions() []console.Action {
	window := memorystoreMaintenanceFields()
	for i := range window {
		window[i].Section = ""
		switch window[i].Name {
		case "maintenance":
			if m.hasWindow {
				window[i].Default = "CUSTOM"
			}
			window[i].Help = "Custom sets the weekly one-hour window (maintenance_policy) in which Memorystore schedules maintenance; Default removes it."
		case "maintenanceDay":
			if m.hasWindow {
				window[i].Default = m.day.String()
			}
		case "maintenanceHour":
			window[i].Label = "Time"
			if m.hasWindow {
				window[i].Default = strconv.Itoa(int(m.hour))
			}
		}
	}
	acts := []console.Action{
		{ID: "maintenance-window", Label: "Edit maintenance window", Title: "Edit maintenance window", Fields: window},
		{ID: "schedule-maintenance", Label: "Schedule maintenance (CloudBurrow)", Fields: []console.Field{
			{Name: "startTime", Label: "Start time (UTC, RFC 3339)", Type: "text",
				Help: "CloudBurrow only, standing in for an update Memorystore schedules: empty for the first maintenance window a week away. At this time every node is replaced, shard by shard: a new replica first, a coordinated failover for a primary, then the old node is removed."}}},
	}
	if m.start != nil {
		acts = append(acts,
			console.Action{ID: "reschedule-maintenance", Label: "Reschedule maintenance", Fields: []console.Field{
				{Name: "rescheduleType", Label: "Reschedule", Type: "select", Required: true, Default: "SPECIFIC_TIME",
					Options: []string{"IMMEDIATE", "SPECIFIC_TIME"},
					Help:    "IMMEDIATE applies the update now; SPECIFIC_TIME moves it to the time below, within 14 days of the originally scheduled time. Not within an hour of the scheduled start."},
				{Name: "scheduleTime", Label: "Specific time (UTC, RFC 3339)", Type: "text", Help: "With SPECIFIC_TIME only."}}},
			console.Action{ID: "cancel-maintenance", Label: "Cancel maintenance (CloudBurrow)",
				Confirm: "The scheduled maintenance event is removed, as if Memorystore had not scheduled it."})
	}
	return acts
}

// clusterMaintenanceWindow reads Edit maintenance window's day and hour:
// none for Default (or the older form's NONE).
func clusterMaintenanceWindow(v map[string]string) (dayofweek.DayOfWeek, int32, bool, error) {
	day := v["maintenanceDay"]
	if day == "NONE" || day == "" || v["maintenance"] == "DEFAULT" {
		return 0, 0, false, nil
	}
	d, ok := dayofweek.DayOfWeek_value[day]
	if !ok || d == 0 {
		return 0, 0, false, fmt.Errorf("maintenance window day %q is not a day of the week", day)
	}
	h, err := strconv.Atoi(strings.TrimSpace(v["maintenanceHour"]))
	if err != nil || h < 0 || h > 23 {
		return 0, 0, false, fmt.Errorf("maintenance window hour %q is not 0 to 23", v["maintenanceHour"])
	}
	return dayofweek.DayOfWeek(d), int32(h), true, nil
}

func maintenanceTime(field, s string) (time.Time, error) {
	if s = strings.TrimSpace(s); s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s %q is not an RFC 3339 time", field, s)
	}
	return t, nil
}

// clusterMaintenanceAct runs a maintenance action on a Redis cluster;
// ok is false for another action.
func clusterMaintenanceAct(ctx context.Context, api *redisadmin.ClusterServer, srv *redisadmin.Server, name, action string, v map[string]string) (ok bool, err error) {
	var op interface{ GetName() string }
	switch action {
	case "schedule-maintenance", "cancel-maintenance":
		start, err := maintenanceTime("start time", v["startTime"])
		if err != nil {
			return true, err
		}
		_, err = srv.ControlClusterMaintenance(name, start, action == "cancel-maintenance")
		return true, err
	case "maintenance-window":
		day, hour, has, err := clusterMaintenanceWindow(v)
		if err != nil {
			return true, err
		}
		in := &clusterpb.Cluster{Name: name}
		if has {
			in.MaintenancePolicy = &clusterpb.ClusterMaintenancePolicy{WeeklyMaintenanceWindow: []*clusterpb.ClusterWeeklyMaintenanceWindow{
				{Day: day, StartTime: &timeofday.TimeOfDay{Hours: hour}}}}
		}
		op, err = api.UpdateCluster(ctx, &clusterpb.UpdateClusterRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"maintenance_policy"}}, Cluster: in})
		if err != nil {
			return true, err
		}
	case "reschedule-maintenance":
		typ, known := clusterpb.RescheduleClusterMaintenanceRequest_RescheduleType_value[v["rescheduleType"]]
		if !known || typ == 0 {
			return true, fmt.Errorf("reschedule %q is not IMMEDIATE or SPECIFIC_TIME", v["rescheduleType"])
		}
		req := &clusterpb.RescheduleClusterMaintenanceRequest{Name: name, RescheduleType: clusterpb.RescheduleClusterMaintenanceRequest_RescheduleType(typ)}
		if req.RescheduleType == clusterpb.RescheduleClusterMaintenanceRequest_SPECIFIC_TIME {
			t, err := maintenanceTime("specific time", v["scheduleTime"])
			if err != nil {
				return true, err
			}
			if !t.IsZero() {
				req.ScheduleTime = timestamppb.New(t)
			}
		}
		if op, err = api.RescheduleClusterMaintenance(ctx, req); err != nil {
			return true, err
		}
	default:
		return false, nil
	}
	return true, waitRedisOp(ctx, srv, op.GetName())
}

// valkeyMaintenanceAct runs a maintenance action on a Valkey instance;
// ok is false for another action.
func valkeyMaintenanceAct(ctx context.Context, api *redisadmin.MemorystoreServer, srv *redisadmin.Server, name, action string, v map[string]string) (ok bool, err error) {
	var op interface{ GetName() string }
	switch action {
	case "schedule-maintenance", "cancel-maintenance":
		start, err := maintenanceTime("start time", v["startTime"])
		if err != nil {
			return true, err
		}
		_, err = srv.ControlClusterMaintenance(name, start, action == "cancel-maintenance")
		return true, err
	case "maintenance-window":
		day, hour, has, err := clusterMaintenanceWindow(v)
		if err != nil {
			return true, err
		}
		in := &memorystorepb.Instance{Name: name}
		if has {
			in.MaintenancePolicy = &memorystorepb.MaintenancePolicy{WeeklyMaintenanceWindow: []*memorystorepb.WeeklyMaintenanceWindow{
				{Day: day, StartTime: &timeofday.TimeOfDay{Hours: hour}}}}
		}
		op, err = api.UpdateInstance(ctx, &memorystorepb.UpdateInstanceRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"maintenance_policy"}}, Instance: in})
		if err != nil {
			return true, err
		}
	case "reschedule-maintenance":
		typ, known := memorystorepb.RescheduleMaintenanceRequest_RescheduleType_value[v["rescheduleType"]]
		if !known || typ == 0 {
			return true, fmt.Errorf("reschedule %q is not IMMEDIATE or SPECIFIC_TIME", v["rescheduleType"])
		}
		req := &memorystorepb.RescheduleMaintenanceRequest{Name: name, RescheduleType: memorystorepb.RescheduleMaintenanceRequest_RescheduleType(typ)}
		if req.RescheduleType == memorystorepb.RescheduleMaintenanceRequest_SPECIFIC_TIME {
			t, err := maintenanceTime("specific time", v["scheduleTime"])
			if err != nil {
				return true, err
			}
			if !t.IsZero() {
				req.ScheduleTime = timestamppb.New(t)
			}
		}
		if op, err = api.RescheduleMaintenance(ctx, req); err != nil {
			return true, err
		}
	default:
		return false, nil
	}
	return true, waitRedisOp(ctx, srv, op.GetName())
}
