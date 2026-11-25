package backupdr

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// fakeWorkload is an AlloyDB or Filestore service whose backups are
// strings, so a test reads back what a backup kept.
type fakeWorkload struct {
	mu        sync.Mutex
	data      map[string]string // by resource
	backups   map[string]string // by run ID
	protected map[string]int64  // by resource: the log retention
	block     chan struct{}     // a backup waits for it when set
}

func newFakeWorkload() *fakeWorkload {
	return &fakeWorkload{data: map[string]string{}, backups: map[string]string{}, protected: map[string]int64{}}
}

func (f *fakeWorkload) Resource(name string) (WorkloadResource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.data[name]; !ok {
		return WorkloadResource{}, notFound("%s does not exist.", name)
	}
	return WorkloadResource{Name: name, Props: map[string]any{"name": name, "clusterUid": "uid-1"},
		AssociationProps: map[string]any{"clusterUid": "uid-1"}}, nil
}

func (f *fakeWorkload) Protect(_ context.Context, name, _, _ string, logDays int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.protected[name] = logDays
	return nil
}

func (f *fakeWorkload) Unprotect(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.protected, name)
	return nil
}

func (f *fakeWorkload) Backup(_ context.Context, name, runID string) (int64, map[string]any, error) {
	f.mu.Lock()
	block := f.block
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.backups[runID] = f.data[name]
	return int64(len(f.data[name])), map[string]any{"databaseVersion": "POSTGRES_16"}, nil
}

func (f *fakeWorkload) DeleteBackup(_ context.Context, runID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.backups, runID)
	return nil
}

const alloyPlan = `{"backupVault":"projects/p/locations/us-central1/backupVaults/v","resourceType":"alloydb.googleapis.com/Cluster",
  "backupRules":[{"ruleId":"daily","backupRetentionDays":7,"standardSchedule":{"recurrenceType":"DAILY","timeZone":"UTC",
  "backupWindow":{"startHourOfDay":1,"endHourOfDay":7}}}],"logRetentionDays":"3"}`

// TestWorkloadBackups (#1524): AlloyDB and Filestore plans' documented
// limits, an association each (the service told to protect the resource),
// one backup job at a time, the backups' properties, their resolution for
// the services' restores, backups.restore's refusal, and the end of
// protection when the association is deleted. Cloud SQL is not served.
func TestWorkloadBackups(t *testing.T) {
	s := NewServer("", nil, "")
	adb, fs := newFakeWorkload(), newFakeWorkload()
	s.SetWorkload(AlloyDBResourceType, func() Workload { return adb })
	s.SetWorkload(FilestoreResourceType, func() Workload { return fs })
	const cluster = "projects/p/locations/us-central1/clusters/c"
	const inst = "projects/p/locations/us-central1-a/instances/f"
	adb.data[cluster] = "rows v1"
	fs.data[inst] = "files v1"

	if code, out := call(t, s, "POST", loc+"/backupVaults?backupVaultId=v", `{"backupMinimumEnforcedRetentionDuration":"86400s"}`); code != 200 {
		t.Fatalf("vault: %d %v", code, out)
	}
	fsPlan := strings.Replace(strings.Replace(alloyPlan, AlloyDBResourceType, FilestoreResourceType, 1), `,"logRetentionDays":"3"`, "", 1)
	for _, c := range []struct {
		what, body string
	}{
		{"log retention on a Filestore plan", strings.Replace(alloyPlan, AlloyDBResourceType, FilestoreResourceType, 1)},
		{"a five-hour window", strings.Replace(alloyPlan, `"endHourOfDay":7`, `"endHourOfDay":6`, 1)},
		{"a weekly AlloyDB rule kept 400 days", strings.Replace(strings.Replace(alloyPlan, `"DAILY"`, `"WEEKLY","daysOfWeek":["MONDAY"]`, 1), `"backupRetentionDays":7`, `"backupRetentionDays":400`, 1)},
		{"a daily rule kept 187 days", strings.Replace(alloyPlan, `"backupRetentionDays":7`, `"backupRetentionDays":187`, 1)},
	} {
		if code, out := call(t, s, "POST", loc+"/backupPlans?backupPlanId=bad", c.body); code != 400 {
			t.Errorf("%s: %d %v, want 400", c.what, code, out)
		}
	}
	hourly := strings.Replace(alloyPlan, `"recurrenceType":"DAILY"`, `"recurrenceType":"HOURLY","hourlyFrequency":1`, 1)
	if code, out := call(t, s, "POST", loc+"/backupPlans?backupPlanId=hourly", hourly); code != 200 {
		t.Errorf("an hourly frequency of 1 for AlloyDB: %d %v", code, out)
	}
	if code, out := call(t, s, "POST", loc+"/backupPlans?backupPlanId=adb", alloyPlan); code != 200 {
		t.Fatalf("AlloyDB plan: %d %v", code, out)
	}
	if code, out := call(t, s, "POST", loc+"/backupPlans?backupPlanId=fs", fsPlan); code != 200 {
		t.Fatalf("Filestore plan: %d %v", code, out)
	}
	if code, out := call(t, s, "POST", "/v1/projects/p/locations/us-east1/backupPlanAssociations?backupPlanAssociationId=x",
		`{"backupPlan":"projects/p/locations/us-central1/backupPlans/adb","resource":"`+cluster+`","resourceType":"`+AlloyDBResourceType+`"}`); code != 400 {
		t.Errorf("an association outside the cluster's region: %d %v", code, out)
	}
	if code, out := call(t, s, "POST", loc+"/backupPlanAssociations?backupPlanAssociationId=x",
		`{"backupPlan":"projects/p/locations/us-central1/backupPlans/adb","resource":"projects/p/locations/us-central1/clusters/none","resourceType":"`+AlloyDBResourceType+`"}`); code != 404 {
		t.Errorf("an association of a missing cluster: %d %v", code, out)
	}
	if code, out := call(t, s, "POST", loc+"/backupPlanAssociations?backupPlanAssociationId=sql",
		`{"backupPlan":"projects/p/locations/us-central1/backupPlans/adb","resource":"projects/p/instances/db","resourceType":"`+CloudSQLResourceType+`"}`); code != 400 && code != 412 {
		t.Errorf("a Cloud SQL association without Cloud SQL: %d %v", code, out)
	}
	for id, body := range map[string]string{
		"adb": `{"backupPlan":"projects/p/locations/us-central1/backupPlans/adb","resource":"` + cluster + `","resourceType":"` + AlloyDBResourceType + `"}`,
		"fs":  `{"backupPlan":"projects/p/locations/us-central1/backupPlans/fs","resource":"projects/p/zones/us-central1-a/instances/f","resourceType":"` + FilestoreResourceType + `"}`,
	} {
		if code, out := call(t, s, "POST", loc+"/backupPlanAssociations?backupPlanAssociationId="+id, body); code != 200 {
			t.Fatalf("association %s: %d %v", id, code, out)
		}
	}
	if adb.protected[cluster] != 3 {
		t.Errorf("the cluster's protection: %v, want log retention 3", adb.protected)
	}
	_, a := call(t, s, "GET", loc+"/backupPlanAssociations/adb", "")
	if p, _ := a["alloydbClusterBackupPlanAssociationProperties"].(map[string]any); p["clusterUid"] != "uid-1" {
		t.Errorf("the AlloyDB association: %v", a)
	}
	_, ds := call(t, s, "GET", "/v1/"+a["dataSource"].(string), "")
	if g, _ := ds["dataSourceGcpResource"].(map[string]any); g["gcpResourcename"] != "//alloydb.googleapis.com/"+cluster || g["alloyDbClusterDatasourceProperties"] == nil {
		t.Errorf("the AlloyDB data source: %v", ds)
	}

	// One backup job at a time.
	block := make(chan struct{})
	adb.mu.Lock()
	adb.block = block
	adb.mu.Unlock()
	code, op := call(t, s, "POST", loc+"/backupPlanAssociations/adb:triggerBackup", `{"ruleId":"daily"}`)
	if code != 200 {
		t.Fatalf("triggerBackup: %d %v", code, op)
	}
	if code, out := call(t, s, "POST", loc+"/backupPlanAssociations/adb:triggerBackup", `{"ruleId":"daily"}`); code != 400 ||
		!strings.Contains(out["error"].(map[string]any)["message"].(string), "Unable to queue the operation") {
		t.Errorf("a second backup job: %d %v", code, out)
	}
	adb.mu.Lock()
	adb.block = nil
	adb.mu.Unlock()
	close(block)
	if out := waitOp(t, s, op); out["error"] != nil {
		t.Fatalf("backup: %v", out["error"])
	}
	adb.mu.Lock()
	adb.data[cluster] = "rows v2"
	adb.mu.Unlock()
	_, list := call(t, s, "GET", "/v1/"+a["dataSource"].(string)+"/backups", "")
	bs, _ := list["backups"].([]any)
	if len(bs) != 1 {
		t.Fatalf("backups: %v", list)
	}
	b := bs[0].(map[string]any)
	if p, _ := b["alloyDbBackupProperties"].(map[string]any); b["state"] != "ACTIVE" || p["storedBytes"] != "7" || p["databaseVersion"] != "POSTGRES_16" {
		t.Errorf("the AlloyDB backup: %v", b)
	}
	runID, src, err := s.AlloyDBBackup(b["name"].(string))
	if err != nil || src != cluster || adb.backups[runID] != "rows v1" {
		t.Errorf("AlloyDBBackup = %q, %q, %v; want the run holding rows v1, of %s", runID, src, err, cluster)
	}
	if _, err := s.FilestoreBackup(b["name"].(string)); err == nil {
		t.Errorf("FilestoreBackup of an AlloyDB backup: no error")
	}
	if c, err := s.AlloyDBDataSource(a["dataSource"].(string)); err != nil || c != cluster {
		t.Errorf("AlloyDBDataSource = %q, %v", c, err)
	}
	if code, out := call(t, s, "POST", "/v1/"+b["name"].(string)+":restore", `{}`); code != 400 ||
		!strings.Contains(out["error"].(map[string]any)["message"].(string), "backupdrBackupSource") {
		t.Errorf("backups.restore of an AlloyDB backup: %d %v", code, out)
	}

	// Filestore.
	code, op = call(t, s, "POST", loc+"/backupPlanAssociations/fs:triggerBackup", `{"ruleId":"daily"}`)
	if code != 200 {
		t.Fatalf("Filestore triggerBackup: %d %v", code, op)
	}
	if out := waitOp(t, s, op); out["error"] != nil {
		t.Fatalf("Filestore backup: %v", out["error"])
	}
	_, fa := call(t, s, "GET", loc+"/backupPlanAssociations/fs", "")
	if fa["resource"] != inst {
		t.Errorf("the Filestore association's resource: %v, want %s", fa["resource"], inst)
	}
	_, list = call(t, s, "GET", "/v1/"+fa["dataSource"].(string)+"/backups", "")
	fb := list["backups"].([]any)[0].(map[string]any)
	if p, _ := fb["filestoreInstanceBackupProperties"].(map[string]any); p["sourceInstance"] != inst {
		t.Errorf("the Filestore backup: %v", fb)
	}
	if id, err := s.FilestoreBackup(fb["name"].(string)); err != nil || fs.backups[id] != "files v1" {
		t.Errorf("FilestoreBackup = %q, %v", id, err)
	}

	// A plan in another vault is refused; deleting the association ends
	// the protection and keeps the backups.
	if code, out := call(t, s, "POST", loc+"/backupVaults?backupVaultId=w", `{"backupMinimumEnforcedRetentionDuration":"86400s"}`); code != 200 {
		t.Fatalf("vault w: %d %v", code, out)
	}
	if code, out := call(t, s, "POST", loc+"/backupPlans?backupPlanId=other", strings.Replace(alloyPlan, "backupVaults/v", "backupVaults/w", 1)); code != 200 {
		t.Fatalf("plan other: %d %v", code, out)
	}
	if code, out := call(t, s, "PATCH", loc+"/backupPlanAssociations/adb?updateMask=backupPlan",
		`{"backupPlan":"projects/p/locations/us-central1/backupPlans/other"}`); code != 400 {
		t.Errorf("a plan in another vault: %d %v", code, out)
	}
	if code, out := call(t, s, "DELETE", loc+"/backupPlanAssociations/adb", ""); code != 200 {
		t.Fatalf("delete association: %d %v", code, out)
	}
	if _, ok := adb.protected[cluster]; ok {
		t.Errorf("the cluster is still protected after its association is deleted")
	}
	if _, _, err := s.AlloyDBBackup(b["name"].(string)); err != nil {
		t.Errorf("the backup after the association is deleted: %v", err)
	}
	var se interface{ GoogleStatus() string }
	if _, err := s.FilestoreBackup(loc[4:] + "/backupVaults/v/dataSources/x/backups/y"); !errors.As(err, &se) || se.GoogleStatus() != "NOT_FOUND" {
		t.Errorf("a missing backup: %v", err)
	}
}
