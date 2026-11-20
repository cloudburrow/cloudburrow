package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/backupdr"
)

// TestConsoleBackupDRGooglePages (#1844): Google's Backup and DR pages over
// the in-process service. Create a backup vault (days, access restriction),
// Create Backup plan (two rules from the Backup rules group, Database Logs),
// Schedule backup on Vaulted backups (no association ID asked for), the
// resource's page with Create on-demand backup, Change backup plan, Restore
// (Overwrite the source instance, sent to Cloud SQL) and Remove backup
// plan; Backup plans' and Backup plan associations' pages; Jobs and the
// Overview counting what was done.
func TestConsoleBackupDRGooglePages(t *testing.T) {
	srv := backupdr.NewServer("127.0.0.1:0", &fakeBackupDRSQL{runs: map[string]bool{}}, "")
	var restores []string
	sql := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/instances") {
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{
				map[string]any{"name": "pg", "region": "us-central1", "databaseVersion": "POSTGRES_17"},
				map[string]any{"name": "pg-copy", "region": "us-central1", "databaseVersion": "POSTGRES_16"}}})
			return
		}
		b, _ := io.ReadAll(r.Body)
		restores = append(restores, r.URL.Path+" "+string(b))
		_ = json.NewEncoder(w).Encode(map[string]any{"kind": "sql#operation", "status": "PENDING"})
	})
	api := backupDRProvider{bdr: srv, sql: sql}
	vaults, plans, vaulted, assocs, jobs, overview := api, bdrPlansProvider{api: api}, bdrVaultedProvider{api: api},
		bdrAssocsProvider{api: api}, bdrJobsProvider{api: api}, bdrOverviewProvider{api: api}
	ctx := context.Background()
	const project = "bdr-google"

	// Create a backup vault: Google's limits are checked.
	if _, err := vaults.Create(ctx, project, map[string]string{"id": "vault-1", "region": "us-central1", "retentionDays": "40000"}); err == nil ||
		!strings.Contains(err.Error(), "99 years") {
		t.Errorf("a retention past 99 years: %v", err)
	}
	if _, err := vaults.Create(ctx, project, map[string]string{"id": "Vault", "region": "us-central1", "retentionDays": "1"}); err == nil {
		t.Errorf("an upper-case vault name was accepted")
	}
	for _, id := range []string{"vault-1", "vault-2"} {
		if _, err := vaults.Create(ctx, project, map[string]string{"id": id, "locationType": "region", "region": "us-central1",
			"retentionDays": "2", "access": "WITHIN_PROJECT", "encryption": "google", "labels": `{"tier":"gold"}`}); err != nil {
			t.Fatal(err)
		}
	}
	vd, _ := vaults.Detail(ctx, project, []string{"vault-1"})
	got := map[string]string{}
	for _, p := range vd.Summary {
		got[p.Label] = p.Value
	}
	if got["Enforced retention"] != "Minimum for backups: 2 days" || got["Access restriction"] != "Restrict access to current project" ||
		got["Location"] != "us-central1 (Iowa)" || got["Encryption type"] != "Google-managed encryption key" || got["Retention lock status"] != "Unlocked" {
		t.Errorf("the vault's details: %v", got)
	}
	if err := vaults.ActAt(ctx, project, []string{"vault-1"}, "editretention", map[string]string{"retentionDays": "3"}); err != nil {
		t.Fatal(err)
	}
	if v, _ := api.get(ctx, "projects/"+project+"/locations/us-central1/backupVaults/vault-1"); str(v, "backupMinimumEnforcedRetentionDuration") != "259200s" {
		t.Errorf("after Edit minimum enforced retention: %v", v)
	}

	// Create Backup plan, with its form's checks.
	rules := console.FormatGroup([]map[string]string{
		{"ruleId": "daily", "recurrence": "DAILY", "start": "1", "end": "5", "retention": "7"},
		{"ruleId": "hourly", "recurrence": "HOURLY", "hourly": "6", "start": "0", "end": "24", "retention": "3"}})
	plan := func(id, vault string, rules string) map[string]string {
		return map[string]string{"resourceType": backupdr.CloudSQLResourceType, "id": id, "description": "gold", "region": "us-central1",
			"vault": "projects/" + project + "/locations/us-central1/backupVaults/" + vault, "rules": rules, "logs": "true", "logDays": "7"}
	}
	bad := console.FormatGroup([]map[string]string{{"ruleId": "h", "recurrence": "HOURLY", "hourly": "2", "start": "0", "end": "24", "retention": "3"}})
	if _, err := plans.Create(ctx, project, plan("plan-x", "vault-1", bad)); err == nil || !strings.Contains(err.Error(), "6 to 23 hours") {
		t.Errorf("an hourly rule every 2 hours: %v", err)
	}
	if _, err := plans.Create(ctx, project, map[string]string{"id": "plan-x", "region": "us-east1", "rules": rules,
		"vault": "projects/" + project + "/locations/us-central1/backupVaults/vault-1"}); err == nil || !strings.Contains(err.Error(), "plan's region") {
		t.Errorf("a vault in another region: %v", err)
	}
	for _, step := range [][2]string{{"plan-1", "vault-1"}, {"plan-2", "vault-2"}} {
		if _, err := plans.Create(ctx, project, plan(step[0], step[1], rules)); err != nil {
			t.Fatal(err)
		}
	}
	pl, _ := plans.List(ctx, project)
	if len(pl.Items) != 2 || pl.Items[0].Fields["Backup rules"] != "2" || pl.Items[0].Fields["Resource type"] != "Cloud SQL" {
		t.Errorf("Backup plans: %+v", pl.Items)
	}
	pd, _ := plans.Detail(ctx, project, []string{"plan-1"})
	if r := pd.Sections[0].Listing.Items; len(r) != 2 || r[0].Fields["Backup window"] != "01:00 to 05:00" || r[1].Fields["Recurrence"] != "Hourly, every 6 hours" {
		t.Errorf("the plan's rules: %+v", r)
	}

	// Schedule backup: an instance and a plan of its region.
	if f := vaulted.CreateFormFor(ctx, project); len(f[2].Options) != 2 || len(f[3].Options) != 2 {
		t.Errorf("Schedule backup's resources and plans: %+v %+v", f[2], f[3])
	}
	if _, err := vaulted.Create(ctx, project, map[string]string{"resourceType": backupdr.CloudSQLResourceType, "region": "us-east1",
		"instance": "pg", "plan": "projects/" + project + "/locations/us-central1/backupPlans/plan-1"}); err == nil {
		t.Errorf("an instance scheduled in another region")
	}
	if _, err := vaulted.Create(ctx, project, map[string]string{"resourceType": backupdr.CloudSQLResourceType, "region": "us-central1",
		"instance": "pg", "plan": "projects/" + project + "/locations/us-central1/backupPlans/plan-1"}); err != nil {
		t.Fatal(err)
	}
	vl, _ := vaulted.List(ctx, project)
	if len(vl.Items) != 1 || vl.Items[0].Name != "pg" || vl.Items[0].Fields["Backup plan"] != "plan-1" || len(vl.Items[0].Opens) != 2 {
		t.Fatalf("Vaulted backups: %+v", vl.Items)
	}
	path := vl.Items[0].Opens
	if acts := bdrActionIDs(vaulted.DetailActions(ctx, project, path)); acts != "ondemand,changeplan,removeplan,pitr" {
		t.Errorf("a resource's actions before a backup: %s", acts)
	}
	if err := vaulted.ActAt(ctx, project, path, "ondemand", map[string]string{"rule": "daily"}); err != nil {
		t.Fatal(err)
	}
	var backup string
	for deadline := time.Now().Add(5 * time.Second); backup == "" && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		d, _ := vaulted.Detail(ctx, project, path)
		if b := d.Sections[0].Listing.Items; len(b) == 1 && b[0].Status == "Succeeded" {
			backup = b[0].Name
			if ids := bdrActionIDs(b[0].Actions); ids != "restore,backupdelete" {
				t.Errorf("a backup row's actions: %s", ids)
			}
		}
	}
	if backup == "" {
		t.Fatal("the on-demand backup never succeeded")
	}
	acts := vaulted.DetailActions(ctx, project, path)
	var restore console.Action
	for _, a := range acts {
		if a.ID == "restore" {
			restore = a
		}
	}
	if restore.ID == "" || len(restore.Fields[1].UnavailableOptions) != 0 || strings.Join(restore.Fields[2].Options, ",") != "pg-copy" ||
		restore.Fields[3].Name != "newinstance" || restore.Fields[3].ShowWhen != "destination=new" {
		t.Errorf("Restore: %+v; want every destination offered, pg-copy, another Postgres instance, to overwrite, and a new instance's ID", restore)
	}
	if err := vaulted.ActAt(ctx, project, append(append([]string{}, path...), backup), "restore", map[string]string{"destination": "source"}); err != nil {
		t.Fatal(err)
	}
	if err := vaulted.ActAt(ctx, project, path, "restore", map[string]string{"backup": backup, "destination": "new"}); err == nil {
		t.Errorf("Restore to a new instance without its ID was accepted")
	}
	if err := vaulted.ActAt(ctx, project, path, "restore", map[string]string{"backup": backup, "destination": "new", "newinstance": "pg-restored"}); err != nil {
		t.Fatal(err)
	}
	if len(restores) != 2 || !strings.Contains(restores[0], "/instances/pg/restoreBackup") || !strings.Contains(restores[0], "/backups/"+backup) ||
		!strings.Contains(restores[1], "/instances/pg-restored/restoreBackup") || !strings.Contains(restores[1], "/backups/"+backup) {
		t.Errorf("Cloud SQL restores: %q", restores)
	}

	// Jobs and the Overview.
	jl, _ := jobs.List(ctx, project)
	if len(jl.Items) != 1 || jl.Items[0].Fields["Job status"] != "Succeeded" || jl.Items[0].Fields["Job category"] != "On-demand backup" ||
		len(jl.Tabs) != 2 || len(jl.Tabs[1].Groups) < 3 || jl.Tabs[0].Groups[0].Heading != "Current failures" {
		t.Errorf("Jobs: %+v, tabs %+v", jl.Items, jl.Tabs)
	}
	// Google's time ranges (#1924): an unknown one is refused, 1h still
	// holds the job just taken.
	if bad, _ := jobs.ListFiltered(ctx, project, "3d"); bad.ServerFilter == nil || bad.ServerFilter.Error == "" {
		t.Errorf("time range 3d: %+v", bad.ServerFilter)
	}
	if hour, _ := jobs.ListFiltered(ctx, project, "1h"); len(hour.Items) != 1 {
		t.Errorf("time range 1h: %d jobs", len(hour.Items))
	}
	ov, _ := overview.List(ctx, project)
	if ov.Summary[0].Properties[0].Value != "1" || ov.Summary[0].Properties[2].Value != "2" || ov.Summary[1].Properties[0].Value != "1" {
		t.Errorf("Overview: %+v", ov.Summary)
	}

	// Change backup plan to plan-2 (vault-2), then Remove backup plan.
	al, _ := assocs.List(ctx, project)
	if len(al.Items) != 1 {
		t.Fatalf("associations: %+v", al.Items)
	}
	if err := vaulted.ActAt(ctx, project, path, "changeplan", map[string]string{"plan": "projects/" + project + "/locations/us-central1/backupPlans/plan-2"}); err != nil {
		t.Fatal(err)
	}
	ad, _ := assocs.Detail(ctx, project, []string{al.Items[0].Name})
	if ad.Summary[4].Value != "plan-2" {
		t.Errorf("the association after Change backup plan: %+v", ad.Summary)
	}
	if err := assocs.ActAt(ctx, project, []string{al.Items[0].Name}, "delete", map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if acts := bdrActionIDs(vaulted.DetailActions(ctx, project, path)); acts != "restore,pitr" {
		t.Errorf("a resource's actions with no plan: %s", acts)
	}
}
