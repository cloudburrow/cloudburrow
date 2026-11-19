//go:build compat

package compat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1"
)

// scheduleSQLMaintenance schedules a maintenance event on a created
// instance through CloudBurrow's control endpoint (not a Cloud SQL API).
func scheduleSQLMaintenance(t *testing.T, h *Harness, project, name string, start time.Time) {
	t.Helper()
	body := `{}`
	if !start.IsZero() {
		body = `{"startTime":"` + start.UTC().Format(time.RFC3339) + `"}`
	}
	url := "http://" + h.Endpoint(EnvSQLAdmin) + "/cloudburrow/v1/projects/" + project + "/instances/" + name + ":scheduleMaintenance"
	res, err := http.Post(url, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	if res.StatusCode != http.StatusOK || out["scheduledMaintenance"] == nil {
		t.Fatalf("scheduleMaintenance: %d %v", res.StatusCode, out)
	}
}

// TestCloudSQLRescheduleMaintenance (#1524), in the sql shard. Through the
// official Go client (google.golang.org/api/sqladmin/v1): an instance with
// no scheduled maintenance refuses rescheduleMaintenance 400; with an event
// scheduled (CloudBurrow's control endpoint), instances.get reports
// scheduledMaintenance with canReschedule and a 28-day
// scheduleDeadlineTime; NEXT_AVAILABLE_WINDOW moves it to the following
// Sunday 03:00 UTC window, a RESCHEDULE_MAINTENANCE operation; SPECIFIC_TIME
// past the deadline, or before the current start, is 400; `gcloud sql
// reschedule-maintenance --reschedule-type=SPECIFIC_TIME` moves it; IMMEDIATE runs it: a
// MAINTENANCE operation restarts the server, after which the instance is
// RUNNABLE with no scheduledMaintenance, and serves again.
// covers: sql.projects.instances.rescheduleMaintenance, sql.instances.get, sql.operations.list
func TestCloudSQLRescheduleMaintenance(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	svc := sqlAdminService(t, h)
	g := newGcloudSession(t, h)
	project := h.Project()
	name := fmt.Sprintf("maint-%d", time.Now().UnixNano()%1e6)
	op, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "POSTGRES_17", RootPassword: "Root-pass1",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", MaintenanceWindow: &sqladmin.MaintenanceWindow{Day: 7, Hour: 3, UpdateTrack: "stable"}}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if d, err := svc.Instances.Delete(project, name).Do(); err == nil {
			waitSQLOp(t, svc, project, d)
		}
	})
	waitSQLOp(t, svc, project, op)
	resched := func(typ, at string) (*sqladmin.Operation, error) {
		return svc.Projects.Instances.RescheduleMaintenance(project, name, &sqladmin.SqlInstancesRescheduleMaintenanceRequestBody{
			Reschedule: &sqladmin.Reschedule{RescheduleType: typ, ScheduleTime: at}}).Do()
	}
	_, err = resched("NEXT_AVAILABLE_WINDOW", "")
	wantHTTP(t, "rescheduleMaintenance with no event", err, 400)

	scheduleSQLMaintenance(t, h, project, name, time.Time{})
	in, err := svc.Instances.Get(project, name).Do()
	if err != nil || in.ScheduledMaintenance == nil || !in.ScheduledMaintenance.CanReschedule {
		t.Fatalf("instances.get: %+v, %v", in.ScheduledMaintenance, err)
	}
	start, _ := time.Parse(time.RFC3339Nano, in.ScheduledMaintenance.StartTime)
	deadline, _ := time.Parse(time.RFC3339Nano, in.ScheduledMaintenance.ScheduleDeadlineTime)
	if start.Weekday() != time.Sunday || start.Hour() != 3 || deadline.Sub(start) != 28*24*time.Hour {
		t.Fatalf("scheduledMaintenance %+v: want a Sunday 03:00 UTC start and a deadline 28 days on", in.ScheduledMaintenance)
	}

	op, err = resched("NEXT_AVAILABLE_WINDOW", "")
	if err != nil || op.OperationType != "RESCHEDULE_MAINTENANCE" {
		t.Fatalf("NEXT_AVAILABLE_WINDOW: %+v, %v", op, err)
	}
	in, _ = svc.Instances.Get(project, name).Do()
	if got, _ := time.Parse(time.RFC3339Nano, in.ScheduledMaintenance.StartTime); !got.Equal(start.Add(7 * 24 * time.Hour)) {
		t.Errorf("after NEXT_AVAILABLE_WINDOW: %s, want %s", got, start.Add(7*24*time.Hour))
	}
	_, err = resched("SPECIFIC_TIME", deadline.Add(time.Hour).Format(time.RFC3339))
	wantHTTP(t, "SPECIFIC_TIME past the deadline", err, 400)
	_, err = resched("SPECIFIC_TIME", start.Format(time.RFC3339))
	wantHTTP(t, "SPECIFIC_TIME before the current start", err, 400)

	// gcloud checks the time is after the current start itself.
	at := start.Add(9 * 24 * time.Hour)
	g.must("sql", "reschedule-maintenance", name, "--project", project, "--reschedule-type=SPECIFIC_TIME",
		"--schedule-time="+at.Format(time.RFC3339), "--quiet")
	got := strings.TrimSpace(g.must("sql", "instances", "describe", name, "--project", project, "--format=value(scheduledMaintenance.startTime)"))
	if gt, err := time.Parse(time.RFC3339Nano, got); err != nil || !gt.Equal(at) {
		t.Errorf("after gcloud sql reschedule-maintenance: startTime %q, want %s", got, at.Format(time.RFC3339))
	}

	if _, err := resched("IMMEDIATE", ""); err != nil {
		t.Fatalf("IMMEDIATE: %v", err)
	}
	var mop *sqladmin.Operation
	for end := time.Now().Add(5 * time.Minute); time.Now().Before(end) && mop == nil; time.Sleep(time.Second) {
		ops, err := svc.Operations.List(project).Instance(name).Do()
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range ops.Items {
			if o.OperationType == "MAINTENANCE" && o.Status == "DONE" {
				mop = o
			}
		}
	}
	if mop == nil || mop.Error != nil {
		t.Fatalf("no finished MAINTENANCE operation: %+v", mop)
	}
	in, err = svc.Instances.Get(project, name).Do()
	if err != nil || in.State != "RUNNABLE" || in.ScheduledMaintenance != nil {
		t.Fatalf("after maintenance: %+v, %v", in, err)
	}
	if _, err := svc.Databases.List(project, name).Do(); err != nil {
		t.Errorf("databases.list after maintenance: %v", err)
	}
}
