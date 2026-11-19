package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
	sqladminapi "google.golang.org/api/sqladmin/v1"
)

// The Cloud SQL page's maintenance (#1524): a created instance's
// Scheduled maintenance and its schedule deadline in the summary;
// Reschedule maintenance (instances.rescheduleMaintenance) while one is
// scheduled; and CloudBurrow's own Schedule maintenance and Cancel
// maintenance, which stand in for Google scheduling an update, through
// the control endpoint sqladmin.MaintenanceControlPrefix.

// maintenanceSummary is the instance's maintenance window and event.
func maintenanceSummary(in *sqladminapi.DatabaseInstance) []console.Property {
	var out []console.Property
	if st := in.Settings; st != nil && st.MaintenanceWindow != nil && (st.MaintenanceWindow.Day != 0 || st.MaintenanceWindow.Hour != 0) {
		day := "Any day"
		if d := st.MaintenanceWindow.Day; d >= 1 && d <= 7 {
			day = time.Weekday(d % 7).String()
		}
		out = append(out, console.Property{Label: "Maintenance window (UTC)", Value: fmt.Sprintf("%s, %02d:00", day, st.MaintenanceWindow.Hour)})
	}
	if m := in.ScheduledMaintenance; m != nil {
		out = append(out, console.Property{Label: "Scheduled maintenance", Value: m.StartTime},
			console.Property{Label: "Reschedule deadline", Value: m.ScheduleDeadlineTime})
	}
	return out
}

// maintenanceActions are the maintenance actions a created instance's
// page offers.
func maintenanceActions(in *sqladminapi.DatabaseInstance) []console.Action {
	if upSQLInstance(in.Name) || in.InstanceType == "ON_PREMISES_INSTANCE" {
		return nil
	}
	acts := []console.Action{{ID: "schedule-maintenance", Label: "Schedule maintenance (CloudBurrow)", Fields: []console.Field{
		{Name: "startTime", Label: "Start time (UTC, RFC 3339)", Type: "text",
			Help: "CloudBurrow only, standing in for an update Google schedules: empty for the first maintenance window a week away. At this time the instance restarts in a MAINTENANCE operation, unless a deny maintenance period holds it or the instance is stopped."}}}}
	if m := in.ScheduledMaintenance; m != nil {
		help := "Until " + m.ScheduleDeadlineTime + "."
		if !m.CanReschedule {
			help = "It starts within 24 hours, so Cloud SQL no longer reschedules it."
		}
		acts = append(acts,
			console.Action{ID: "reschedule-maintenance", Label: "Reschedule maintenance", Fields: []console.Field{
				{Name: "rescheduleType", Label: "Reschedule", Type: "select", Required: true, Default: "NEXT_AVAILABLE_WINDOW",
					Options: []string{"IMMEDIATE", "NEXT_AVAILABLE_WINDOW", "SPECIFIC_TIME"},
					Help:    "IMMEDIATE applies the update now; NEXT_AVAILABLE_WINDOW moves it to the next maintenance window; SPECIFIC_TIME to the time below. " + help},
				{Name: "scheduleTime", Label: "Specific time (UTC, RFC 3339)", Type: "text", Help: "With SPECIFIC_TIME only."}}},
			console.Action{ID: "cancel-maintenance", Label: "Cancel maintenance (CloudBurrow)",
				Confirm: "The scheduled maintenance event is removed, as if Google had not scheduled it."})
	}
	return acts
}

// maintenanceAct performs the maintenance actions; ok is false for any
// other action.
func (p cloudSQLInstancesProvider) maintenanceAct(ctx context.Context, svc *sqladminapi.Service, project, name, action string, v map[string]string) (bool, error) {
	switch action {
	case "reschedule-maintenance":
		_, err := svc.Projects.Instances.RescheduleMaintenance(project, name, &sqladminapi.SqlInstancesRescheduleMaintenanceRequestBody{
			Reschedule: &sqladminapi.Reschedule{RescheduleType: v["rescheduleType"], ScheduleTime: strings.TrimSpace(v["scheduleTime"])}}).Context(ctx).Do()
		if err != nil {
			return true, sqlAPIError(err)
		}
		return true, nil
	case "schedule-maintenance", "cancel-maintenance":
		verb := "scheduleMaintenance"
		body := map[string]string{}
		if action == "cancel-maintenance" {
			verb = "cancelMaintenance"
		} else if s := strings.TrimSpace(v["startTime"]); s != "" {
			body["startTime"] = s
		}
		b, _ := json.Marshal(body)
		url := "http://" + p.endpoint + sqladmin.MaintenanceControlPrefix + project + "/instances/" + name + ":" + verb
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
		if err != nil {
			return true, err
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return true, err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			var e struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			_ = json.NewDecoder(res.Body).Decode(&e)
			return true, fmt.Errorf("%s: %s", verb, e.Error.Message)
		}
		return true, nil
	}
	return false, nil
}
