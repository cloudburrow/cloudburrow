package sqladmin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

func scheduleMaint(t *testing.T, url, project, name, body string) map[string]any {
	t.Helper()
	res, err := http.Post(url+MaintenanceControlPrefix+project+"/instances/"+name+":scheduleMaintenance", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("scheduleMaintenance %s: %d %v", body, res.StatusCode, out)
	}
	return out
}

// TestRescheduleMaintenance (#1524): with no event, rescheduleMaintenance
// is refused; an event scheduled through the control endpoint is
// scheduledMaintenance, moved by NEXT_AVAILABLE_WINDOW to the next window
// and by SPECIFIC_TIME within the deadline, refused past it and within 24
// hours of its start, and IMMEDIATE runs it: a MAINTENANCE operation
// restarts the server and scheduledMaintenance is gone.
func TestRescheduleMaintenance(t *testing.T) {
	prov := newFakeProv()
	srv := NewServer("", newFake())
	srv.EnableInstanceWrites(prov, "")
	ts := httptest.NewServer(srv)
	defer ts.Close()
	svc, err := sqladmin.NewService(context.Background(), option.WithEndpoint(ts.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	op, err := svc.Instances.Insert("proj", &sqladmin.DatabaseInstance{Name: "pgm", DatabaseVersion: "POSTGRES_17",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", MaintenanceWindow: &sqladmin.MaintenanceWindow{Day: 7, Hour: 3}}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	wait(t, svc, "proj", op)
	resched := func(typ, at string) (*sqladmin.Operation, error) {
		return svc.Projects.Instances.RescheduleMaintenance("proj", "pgm", &sqladmin.SqlInstancesRescheduleMaintenanceRequestBody{
			Reschedule: &sqladmin.Reschedule{RescheduleType: typ, ScheduleTime: at}}).Do()
	}
	for _, tc := range []struct{ typ, at, want string }{
		{"", "", "rescheduleType is required"},
		{"SOON", "", "400"},
		{"SPECIFIC_TIME", "", "scheduleTime is required"},
		{"SPECIFIC_TIME", "tomorrow", "Invalid time format"},
		{"IMMEDIATE", "", "does not have any scheduled maintenance"},
	} {
		if _, err := resched(tc.typ, tc.at); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s %s: %v, want %s", tc.typ, tc.at, err, tc.want)
		}
	}

	// The default event: the first Sunday 03:00 UTC window a week away.
	out := scheduleMaint(t, ts.URL, "proj", "pgm", `{}`)
	sm := out["scheduledMaintenance"].(map[string]any)
	start, _ := time.Parse(time.RFC3339Nano, sm["startTime"].(string))
	if start.Weekday() != time.Sunday || start.Hour() != 3 || start.Before(time.Now().Add(7*24*time.Hour)) || sm["canReschedule"] != true {
		t.Fatalf("default event %v", sm)
	}
	in, err := svc.Instances.Get("proj", "pgm").Do()
	if err != nil || in.ScheduledMaintenance == nil || in.ScheduledMaintenance.StartTime != sm["startTime"] ||
		!in.ScheduledMaintenance.CanReschedule {
		t.Fatalf("instances.get: %+v %v", in.ScheduledMaintenance, err)
	}
	deadline, _ := time.Parse(time.RFC3339Nano, in.ScheduledMaintenance.ScheduleDeadlineTime)
	if deadline.Sub(start) != 28*24*time.Hour {
		t.Errorf("deadline %s for start %s, want 28 days on", deadline, start)
	}

	if op, err := resched("NEXT_AVAILABLE_WINDOW", ""); err != nil || op.OperationType != "RESCHEDULE_MAINTENANCE" || op.Status != "DONE" {
		t.Fatalf("NEXT_AVAILABLE_WINDOW: %+v %v", op, err)
	}
	in, _ = svc.Instances.Get("proj", "pgm").Do()
	if got, _ := time.Parse(time.RFC3339Nano, in.ScheduledMaintenance.StartTime); !got.Equal(start.Add(7 * 24 * time.Hour)) {
		t.Errorf("after NEXT_AVAILABLE_WINDOW the start is %s, want %s", got, start.Add(7*24*time.Hour))
	}
	if _, err := resched("SPECIFIC_TIME", deadline.Add(time.Hour).Format(time.RFC3339)); err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Errorf("past the deadline: %v", err)
	}
	if _, err := resched("SPECIFIC_TIME", start.Format(time.RFC3339)); err == nil || !strings.Contains(err.Error(), "after original scheduled time") {
		t.Errorf("before the current start: %v", err)
	}
	later := start.Add(10 * 24 * time.Hour)
	if _, err := resched("SPECIFIC_TIME", later.Format(time.RFC3339)); err != nil {
		t.Fatalf("SPECIFIC_TIME: %v", err)
	}
	in, _ = svc.Instances.Get("proj", "pgm").Do()
	if in.ScheduledMaintenance.StartTime != later.Format(time.RFC3339Nano) {
		t.Errorf("after SPECIFIC_TIME %s: %+v", later, in.ScheduledMaintenance)
	}
	soon := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	scheduleMaint(t, ts.URL, "proj", "pgm", `{"startTime":"`+soon.Format(time.RFC3339)+`"}`)
	if in, _ = svc.Instances.Get("proj", "pgm").Do(); in.ScheduledMaintenance.CanReschedule {
		t.Errorf("an event 2 hours away: %+v", in.ScheduledMaintenance)
	}
	if _, err := resched("IMMEDIATE", ""); err == nil || !strings.Contains(err.Error(), "24 hours") {
		t.Errorf("within 24 hours: %v", err)
	}

	scheduleMaint(t, ts.URL, "proj", "pgm", `{"startTime":"`+time.Now().Add(72*time.Hour).UTC().Format(time.RFC3339)+`"}`)
	if _, err := resched("IMMEDIATE", ""); err != nil {
		t.Fatalf("IMMEDIATE: %v", err)
	}
	var mop *sqladmin.Operation
	for i := 0; i < 200 && mop == nil; i++ {
		ops, _ := svc.Operations.List("proj").Instance("pgm").Do()
		for _, o := range ops.Items {
			if o.OperationType == "MAINTENANCE" && o.Status == "DONE" {
				mop = o
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if mop == nil || mop.Error != nil {
		t.Fatalf("no MAINTENANCE operation: %+v", mop)
	}
	prov.mu.Lock()
	restarts := prov.restarts
	prov.mu.Unlock()
	in, _ = svc.Instances.Get("proj", "pgm").Do()
	if restarts != 1 || in.ScheduledMaintenance != nil || in.State != "RUNNABLE" {
		t.Errorf("after maintenance: %d restarts, %+v, %s", restarts, in.ScheduledMaintenance, in.State)
	}
}

// A deny maintenance period skips an event; a stopped instance skips it.
func TestMaintenanceSkipped(t *testing.T) {
	now := time.Date(2026, 12, 25, 3, 0, 0, 0, time.UTC)
	spec := Spec{Settings: map[string]any{"denyMaintenancePeriods": []any{
		map[string]any{"startDate": "12-20", "endDate": "01-05", "time": "00:00:00"}}}}
	if !denied(spec, now) || !denied(spec, now.AddDate(0, 0, 10)) || denied(spec, now.AddDate(0, 0, 12)) {
		t.Error("a recurring period across the new year")
	}
	spec.Settings["denyMaintenancePeriods"] = []any{map[string]any{"startDate": "2026-12-20", "endDate": "2027-01-05", "time": "00:00:00"}}
	if !denied(spec, now) || denied(spec, now.AddDate(1, 0, 0)) {
		t.Error("a dated period")
	}
	for _, bad := range []string{
		`{"denyMaintenancePeriods":[{"startDate":"2026-01-01","endDate":"2026-06-01"}]}`,
		`{"denyMaintenancePeriods":[{"startDate":"2026-01-01","endDate":"2026-01-02"},{"startDate":"2026-03-01","endDate":"2026-03-02"}]}`,
		`{"denyMaintenancePeriods":[{"startDate":"2026-01-01","endDate":"01-02"}]}`,
		`{"maintenanceWindow":{"day":8}}`, `{"maintenanceWindow":{"hour":24}}`, `{"maintenanceWindow":{"updateTrack":"week9"}}`,
	} {
		var st map[string]any
		_ = json.Unmarshal([]byte(bad), &st)
		if err := checkMaintenanceSettings(st); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}

	prov := newFakeProv()
	srv := NewServer("", newFake())
	srv.EnableInstanceWrites(prov, "")
	ts := httptest.NewServer(srv)
	defer ts.Close()
	svc, _ := sqladmin.NewService(context.Background(), option.WithEndpoint(ts.URL+"/"), option.WithoutAuthentication())
	today := time.Now().UTC()
	op, err := svc.Instances.Insert("proj", &sqladmin.DatabaseInstance{Name: "pgd", DatabaseVersion: "POSTGRES_17",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", DenyMaintenancePeriods: []*sqladmin.DenyMaintenancePeriod{{
			StartDate: today.AddDate(0, 0, -1).Format("2006-01-02"), EndDate: today.AddDate(0, 0, 5).Format("2006-01-02"), Time: "00:00:00"}}}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	wait(t, svc, "proj", op)
	scheduleMaint(t, ts.URL, "proj", "pgd", `{"startTime":"`+today.Add(-time.Minute).Format(time.RFC3339)+`"}`)
	for i := 0; i < 200; i++ {
		if in, _ := svc.Instances.Get("proj", "pgd").Do(); in.ScheduledMaintenance == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	prov.mu.Lock()
	defer prov.mu.Unlock()
	if in, _ := svc.Instances.Get("proj", "pgd").Do(); in.ScheduledMaintenance != nil || prov.restarts != 0 {
		t.Errorf("an event in a deny period: %+v, %d restarts", in.ScheduledMaintenance, prov.restarts)
	}
}
