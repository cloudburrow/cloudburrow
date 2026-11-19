//go:build browser

package browser

import (
	"fmt"
	"testing"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestCloudSQLMaintenanceThroughThePage (#1524), in the sql shard. On a
// created PostgreSQL instance's page: Schedule maintenance (CloudBurrow)
// at a time three days on makes the scheduledMaintenance instances.get
// reads; Reschedule maintenance with NEXT_AVAILABLE_WINDOW moves it a week
// (no maintenance window is set); Cancel maintenance, confirmed, removes
// it; a second event, rescheduled IMMEDIATE, runs: the instance is
// RUNNABLE again with no scheduledMaintenance, after a MAINTENANCE
// operation.
func TestCloudSQLMaintenanceThroughThePage(t *testing.T) {
	needService(t, "cloudsql-instances")
	svc := sqlAdminSDK(t)
	project := servedProject(t)
	name := fmt.Sprintf("browser-maint-%d", time.Now().UnixNano()%1e6)
	createSQL(t, svc, project, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "POSTGRES_17", RootPassword: "Root-pass1",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}})
	start := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Hour)

	p := open(t)
	page := "/cloudsql-instances/" + name + "?project=" + project
	p.navigate(page)
	p.pageAction("Schedule maintenance (CloudBurrow)", map[string]string{"startTime": start.Format(time.RFC3339)}, "")
	waitSQLInstance(t, svc, project, name, "with the event scheduled", func(in *sqladmin.DatabaseInstance) bool {
		return in.ScheduledMaintenance != nil && in.ScheduledMaintenance.StartTime == start.Format(time.RFC3339Nano)
	})
	p.navigate(page)
	p.pageAction("Reschedule maintenance", map[string]string{"rescheduleType": "NEXT_AVAILABLE_WINDOW"}, "")
	next := start.Add(7 * 24 * time.Hour)
	waitSQLInstance(t, svc, project, name, "with the event a week on", func(in *sqladmin.DatabaseInstance) bool {
		return in.ScheduledMaintenance != nil && in.ScheduledMaintenance.StartTime == next.Format(time.RFC3339Nano)
	})
	p.navigate(page)
	p.clickAction("Cancel maintenance (CloudBurrow)", name)
	waitSQLInstance(t, svc, project, name, "with the event cancelled", func(in *sqladmin.DatabaseInstance) bool {
		return in.ScheduledMaintenance == nil
	})

	p.navigate(page)
	p.pageAction("Schedule maintenance (CloudBurrow)", map[string]string{"startTime": start.Format(time.RFC3339)}, "")
	waitSQLInstance(t, svc, project, name, "with a second event", func(in *sqladmin.DatabaseInstance) bool {
		return in.ScheduledMaintenance != nil
	})
	p.navigate(page)
	p.pageAction("Reschedule maintenance", map[string]string{"rescheduleType": "IMMEDIATE"}, "")
	waitSQLInstance(t, svc, project, name, "after the maintenance ran", func(in *sqladmin.DatabaseInstance) bool {
		return in.State == "RUNNABLE" && in.ScheduledMaintenance == nil
	})
	ops, err := svc.Operations.List(project).Instance(name).Do()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, o := range ops.Items {
		found = found || (o.OperationType == "MAINTENANCE" && o.Status == "DONE" && o.Error == nil)
	}
	if !found {
		t.Errorf("no finished MAINTENANCE operation in %d operations", len(ops.Items))
	}
}
