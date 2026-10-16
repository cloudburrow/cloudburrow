package backupdr

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeSQL struct {
	mu   sync.Mutex
	runs map[string]Run
	n    int
}

func (f *fakeSQL) Instance(project, instance string) (Instance, bool) {
	return Instance{DatabaseVersion: "POSTGRES_17", Tier: "db-custom-1-3840", Region: "us-central1"}, project == "p" && instance == "db"
}

func (f *fakeSQL) Backup(project, instance, description string) (string, <-chan struct{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	id := "run" + string(rune('0'+f.n))
	f.runs[id] = Run{Status: "SUCCESSFUL", EndTime: "2026-10-16T00:00:00Z"}
	ch := make(chan struct{})
	close(ch)
	return id, ch, nil
}

func (f *fakeSQL) Run(id string) (Run, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[id]
	return r, ok
}

func (f *fakeSQL) DeleteRun(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.runs, id)
	return nil
}

func call(t *testing.T, s *Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

const loc = "/v1/projects/p/locations/us-central1"

const planBody = `{"backupVault":"projects/p/locations/us-central1/backupVaults/v","resourceType":"sqladmin.googleapis.com/Instance",
  "backupRules":[{"ruleId":"daily","backupRetentionDays":7,"standardSchedule":{"recurrenceType":"DAILY","timeZone":"UTC",
  "backupWindow":{"startHourOfDay":1,"endHourOfDay":5}}}],"logRetentionDays":"7"}`

func TestCloudSQLBackupLifecycle(t *testing.T) {
	sql := &fakeSQL{runs: map[string]Run{}}
	state := filepath.Join(t.TempDir(), "backupdr.json")
	s := NewServer("", sql, state)
	if code, out := call(t, s, "POST", loc+"/backupVaults?backupVaultId=v", `{"backupMinimumEnforcedRetentionDuration":"86400s"}`); code != 200 || out["done"] != true {
		t.Fatalf("vault create: %d %v", code, out)
	}
	if code, _ := call(t, s, "POST", loc+"/backupVaults?backupVaultId=v", `{"backupMinimumEnforcedRetentionDuration":"86400s"}`); code != http.StatusConflict {
		t.Errorf("a second vault create: %d", code)
	}
	if code, out := call(t, s, "POST", loc+"/backupPlans?backupPlanId=gce", strings.Replace(planBody, "sqladmin.googleapis.com", "compute.googleapis.com", 1)); code != 501 ||
		!strings.Contains(out["error"].(map[string]any)["message"].(string), "Compute") {
		t.Errorf("a Compute Engine plan: %d %v", code, out)
	}
	if code, out := call(t, s, "POST", loc+"/backupPlans?backupPlanId=plan", planBody); code != 200 {
		t.Fatalf("plan create: %d %v", code, out)
	}
	if code, out := call(t, s, "GET", loc+"/backupPlans/plan", ""); code != 200 || out["logRetentionDays"] != "7" {
		t.Errorf("plan get: %d %v", code, out)
	}
	assoc := `{"backupPlan":"projects/p/locations/us-central1/backupPlans/plan","resource":"projects/p/instances/%s","resourceType":"sqladmin.googleapis.com/Instance"}`
	if code, _ := call(t, s, "POST", loc+"/backupPlanAssociations?backupPlanAssociationId=a", strings.Replace(assoc, "%s", "missing", 1)); code != 404 {
		t.Errorf("an association of a missing instance: %d", code)
	}
	if code, out := call(t, s, "POST", loc+"/backupPlanAssociations?backupPlanAssociationId=a", strings.Replace(assoc, "%s", "db", 1)); code != 200 {
		t.Fatalf("association create: %d %v", code, out)
	}
	_, a := call(t, s, "GET", loc+"/backupPlanAssociations/a", "")
	ds, _ := a["dataSource"].(string)
	if !strings.HasPrefix(ds, "projects/p/locations/us-central1/backupVaults/v/dataSources/") {
		t.Fatalf("association's dataSource: %v", a)
	}
	if p, i, err := s.DataSourceInstance(ds); err != nil || p != "p" || i != "db" {
		t.Errorf("DataSourceInstance: %s %s %v", p, i, err)
	}
	code, op := call(t, s, "POST", loc+"/backupPlanAssociations/a:triggerBackup", `{"ruleId":"daily"}`)
	if code != 200 {
		t.Fatalf("triggerBackup: %d %v", code, op)
	}
	name := op["name"].(string)
	deadline := time.Now().Add(5 * time.Second)
	for op["done"] != true && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		_, op = call(t, s, "GET", "/v1/"+name, "")
	}
	if op["done"] != true || op["error"] != nil {
		t.Fatalf("triggerBackup's operation: %v", op)
	}
	_, list := call(t, s, "GET", "/v1/"+ds+"/backups", "")
	bs, _ := list["backups"].([]any)
	if len(bs) != 1 || bs[0].(map[string]any)["state"] != "ACTIVE" || bs[0].(map[string]any)["backupType"] != "ON_DEMAND" {
		t.Fatalf("backups.list: %v", list)
	}
	bk := bs[0].(map[string]any)["name"].(string)
	if id, err := s.BackupRunID(bk); err != nil || id != "run1" {
		t.Errorf("BackupRunID: %s %v", id, err)
	}
	if code, out := call(t, s, "POST", "/v1/"+bk+":restore", `{"computeInstanceTargetEnvironment":{"project":"p","zone":"us-central1-a"}}`); code != 501 ||
		!strings.Contains(out["error"].(map[string]any)["message"].(string), "backupdrBackup") {
		t.Errorf("backups.restore: %d %v", code, out)
	}
	if code, _ := call(t, s, "DELETE", loc+"/backupPlans/plan", ""); code != 400 {
		t.Errorf("deleting a plan in use: %d", code)
	}
	if code, _ := call(t, s, "DELETE", loc+"/backupVaults/v", ""); code != 400 {
		t.Errorf("deleting a vault with a plan: %d", code)
	}

	// The records survive a restart.
	s2 := NewServer("", sql, state)
	if code, out := call(t, s2, "GET", "/v1/"+ds, ""); code != 200 || out["backupCount"] != "1" || out["configState"] != "ACTIVE" {
		t.Errorf("dataSources.get after a restart: %d %v", code, out)
	}

	for _, p := range []string{loc + "/backupPlanAssociations/a", loc + "/backupPlans/plan"} {
		if code, out := call(t, s, "DELETE", p, ""); code != 200 {
			t.Fatalf("delete %s: %d %v", p, code, out)
		}
	}
	if code, out := call(t, s, "GET", "/v1/"+ds, ""); code != 200 || out["configState"] != "PASSIVE" {
		t.Errorf("the data source after its association is deleted: %d %v", code, out)
	}
	if code, _ := call(t, s, "DELETE", loc+"/backupVaults/v", ""); code != 400 {
		t.Errorf("deleting a vault holding a data source: %d", code)
	}
	if code, _ := call(t, s, "DELETE", loc+"/backupVaults/v?force=true", ""); code != 200 {
		t.Errorf("force-deleting the vault: %d", code)
	}
	if _, ok := sql.Run("run1"); ok {
		t.Errorf("the vault's backup run survived the vault")
	}
	if code, _ := call(t, s, "GET", loc+"/managementServers", ""); code != 501 {
		t.Errorf("managementServers: %d", code)
	}
}

func TestScheduleIsDue(t *testing.T) {
	daily := map[string]any{"recurrenceType": "DAILY", "timeZone": "UTC",
		"backupWindow": map[string]any{"startHourOfDay": 1.0, "endHourOfDay": 5.0}}
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	for _, c := range []struct {
		sch       map[string]any
		last, now string
		want      bool
		what      string
	}{
		{daily, "", "2026-10-16T02:00:00Z", true, "first in the window"},
		{daily, "", "2026-10-16T06:00:00Z", false, "outside the window"},
		{daily, "2026-10-16T01:00:00Z", "2026-10-16T02:00:00Z", false, "already taken today"},
		{daily, "2026-10-15T01:00:00Z", "2026-10-16T02:00:00Z", true, "taken yesterday"},
		{map[string]any{"recurrenceType": "HOURLY", "hourlyFrequency": 6.0, "timeZone": "UTC"}, "2026-10-16T00:00:00Z", "2026-10-16T05:00:00Z", false, "hourly too soon"},
		{map[string]any{"recurrenceType": "HOURLY", "hourlyFrequency": 6.0, "timeZone": "UTC"}, "2026-10-16T00:00:00Z", "2026-10-16T06:00:00Z", true, "hourly due"},
		{map[string]any{"recurrenceType": "WEEKLY", "daysOfWeek": []any{"FRIDAY"}, "timeZone": "UTC"}, "", "2026-10-16T06:00:00Z", true, "weekly on its day"},
		{map[string]any{"recurrenceType": "WEEKLY", "daysOfWeek": []any{"MONDAY"}, "timeZone": "UTC"}, "", "2026-10-16T06:00:00Z", false, "weekly on another day"},
		{map[string]any{"recurrenceType": "MONTHLY", "daysOfMonth": []any{16.0}, "timeZone": "UTC"}, "", "2026-10-16T06:00:00Z", true, "monthly on its day"},
	} {
		var last time.Time
		if c.last != "" {
			last = at(c.last)
		}
		if got := isDue(c.sch, last, at(c.now)); got != c.want {
			t.Errorf("%s: isDue = %v", c.what, got)
		}
	}
}
