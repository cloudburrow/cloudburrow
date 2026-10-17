package backupdr

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// A project's reset (#1542) deletes its vaults, data sources, backups and
// their runs, plans, associations, trials and operations, on disk too, and
// leaves another project's alone.
func TestResetProjectDropsOnlyThatProject(t *testing.T) {
	sql := &fakeSQL{runs: map[string]Run{}}
	state := filepath.Join(t.TempDir(), "backupdr.json")
	s := NewServer("", sql, state)
	const other = "/v1/projects/q/locations/us-central1"
	for _, c := range [][2]string{
		{loc + "/backupVaults?backupVaultId=v", `{"backupMinimumEnforcedRetentionDuration":"86400s"}`},
		{other + "/backupVaults?backupVaultId=v", `{"backupMinimumEnforcedRetentionDuration":"86400s"}`},
		{loc + "/backupPlans?backupPlanId=plan", planBody},
		{loc + "/backupPlanAssociations?backupPlanAssociationId=a", `{"backupPlan":"projects/p/locations/us-central1/backupPlans/plan",` +
			`"resource":"projects/p/instances/db","resourceType":"sqladmin.googleapis.com/Instance"}`},
		{"/v1/projects/p/locations/us/trial:subscribe", `{}`},
	} {
		if code, out := call(t, s, "POST", c[0], c[1]); code != 200 {
			t.Fatalf("POST %s: %d %v", c[0], code, out)
		}
	}
	_, op := call(t, s, "POST", loc+"/backupPlanAssociations/a:triggerBackup", `{"ruleId":"daily"}`)
	name, _ := op["name"].(string)
	for deadline := time.Now().Add(5 * time.Second); op["done"] != true && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
		_, op = call(t, s, "GET", "/v1/"+name, "")
	}
	if _, ok := sql.Run("run1"); !ok || op["done"] != true {
		t.Fatalf("the backup was not taken: %v", op)
	}

	if err := s.ResetProject(context.Background(), "p"); err != nil {
		t.Fatal(err)
	}
	if _, ok := sql.Run("run1"); ok {
		t.Error("the project's backup run survived its reset")
	}
	for _, srv := range []*Server{s, NewServer("", sql, state)} {
		for _, p := range []string{loc + "/backupVaults/v", loc + "/backupPlans/plan", loc + "/backupPlanAssociations/a", "/v1/" + name} {
			if code, _ := call(t, srv, "GET", p, ""); code != 404 {
				t.Errorf("GET %s after the reset: %d, want 404", p, code)
			}
		}
		if _, out := call(t, srv, "GET", loc+"/operations", ""); len(out["operations"].([]any)) != 0 {
			t.Errorf("operations after the reset: %v", out)
		}
		if _, out := call(t, srv, "GET", "/v1/projects/p/locations/us/trial", ""); out["state"] != "ELIGIBLE" {
			t.Errorf("the trial after the reset: %v", out)
		}
		if code, _ := call(t, srv, "GET", other+"/backupVaults/v", ""); code != 200 {
			t.Errorf("another project's vault after the reset: %d", code)
		}
	}
	// The IDs are free again.
	if code, out := call(t, s, "POST", loc+"/backupVaults?backupVaultId=v", `{"backupMinimumEnforcedRetentionDuration":"86400s"}`); code != 200 {
		t.Fatalf("vault create again: %d %v", code, out)
	}

	if err := s.ResetProject(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{loc + "/backupVaults/v", other + "/backupVaults/v"} {
		if code, _ := call(t, s, "GET", p, ""); code != 404 {
			t.Errorf("GET %s after resetting every project: %d, want 404", p, code)
		}
	}
}
