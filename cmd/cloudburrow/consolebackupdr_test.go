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

// fakeBackupDRSQL is a Cloud SQL with one instance whose backups are taken
// at once.
type fakeBackupDRSQL struct{ runs map[string]bool }

func (f *fakeBackupDRSQL) Instance(project, instance string) (backupdr.Instance, bool) {
	return backupdr.Instance{DatabaseVersion: "POSTGRES_17", Tier: "db-custom-1-3840", Region: "us-central1"}, instance == "pg"
}

func (f *fakeBackupDRSQL) Backup(project, instance, description string) (string, <-chan struct{}, error) {
	id := "run-1"
	f.runs[id] = true
	done := make(chan struct{})
	close(done)
	return id, done, nil
}

func (f *fakeBackupDRSQL) Run(id string) (backupdr.Run, bool) {
	return backupdr.Run{Status: "SUCCESSFUL", EndTime: "2026-10-16T12:00:00Z"}, f.runs[id]
}

func (f *fakeBackupDRSQL) DeleteRun(id string) error { delete(f.runs, id); return nil }

// The Backup and DR page (#1433): Create backup vault, a vault's Create
// backup plan, Associate an instance and Trigger backup through the Backup
// and DR Service, the data source and its backup on their pages, and
// Restore a backup and Point-in-time restore sent to the Cloud SQL Admin
// API with the backup's and the data source's names.
func TestConsoleBackupDRVaultPlanAssociationRestore(t *testing.T) {
	srv := backupdr.NewServer("127.0.0.1:0", &fakeBackupDRSQL{runs: map[string]bool{}}, "")
	var sqlCalls []string
	sql := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sqlCalls = append(sqlCalls, r.URL.Path+" "+string(b))
		_ = json.NewEncoder(w).Encode(map[string]any{"kind": "sql#operation", "status": "PENDING"})
	})
	p := backupDRProvider{bdr: srv, sql: sql}
	ctx := context.Background()
	const project = "bdr-console"

	if _, err := p.Create(ctx, project, map[string]string{"id": "vault-1", "location": "us-central1", "retention": "86400s"}); err != nil {
		t.Fatal(err)
	}
	l, err := p.List(ctx, project)
	if err != nil || len(l.Items) != 1 || l.Items[0].Name != "vault-1" || l.Items[0].Fields["Location"] != "us-central1" {
		t.Fatalf("vaults: %v %+v", err, l)
	}
	if acts := bdrActionIDs(p.DetailActions(ctx, project, []string{"vault-1"})); acts != "createplan,delete" {
		t.Errorf("a new vault's actions: %s", acts)
	}
	for _, step := range []struct {
		action string
		values map[string]string
	}{
		{"createplan", map[string]string{"id": "plan-1", "rule": "daily", "recurrence": "DAILY", "retention": "7", "logdays": "7"}},
		{"associate", map[string]string{"id": "assoc-1", "plan": "plan-1", "instance": "pg"}},
		{"trigger", map[string]string{"association": "assoc-1", "rule": "daily"}},
	} {
		if err := p.ActAt(ctx, project, []string{"vault-1"}, step.action, step.values); err != nil {
			t.Fatalf("%s: %v", step.action, err)
		}
	}
	if err := p.ActAt(ctx, project, []string{"vault-1"}, "associate", map[string]string{"id": "assoc-2", "plan": "plan-1", "instance": "nope"}); err == nil ||
		!strings.Contains(err.Error(), "does not exist") {
		t.Errorf("associating a missing instance: %v", err)
	}

	d, err := p.Detail(ctx, project, []string{"vault-1"})
	if err != nil || len(d.Sections) != 3 {
		t.Fatalf("vault page: %v %+v", err, d)
	}
	ds := d.Sections[0].Listing.Items
	if len(ds) != 1 || ds[0].Fields["Instance"] != "projects/"+project+"/instances/pg" {
		t.Fatalf("data sources: %+v", ds)
	}
	if plans := d.Sections[1].Listing.Items; len(plans) != 1 || plans[0].Fields["Rules"] != "daily: DAILY, 7 days" {
		t.Errorf("plans: %+v", plans)
	}
	if as := d.Sections[2].Listing.Items; len(as) != 1 || as[0].Fields["Resource"] != "projects/"+project+"/instances/pg" {
		t.Errorf("associations: %+v", as)
	}

	path := []string{"vault-1", ds[0].Name}
	var backup string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		dd, err := p.Detail(ctx, project, path)
		if err != nil {
			t.Fatal(err)
		}
		if b := dd.Sections[0].Listing.Items; len(b) == 1 && b[0].Status == "ACTIVE" {
			backup = b[0].Name
			break
		}
	}
	if backup == "" {
		dd, _ := p.Detail(ctx, project, path)
		t.Fatalf("the triggered backup never became ACTIVE on the data source's page: %+v", dd)
	}
	if acts := bdrActionIDs(p.DetailActions(ctx, project, path)); acts != "restore,pitr" {
		t.Errorf("data source actions: %s", acts)
	}
	if err := p.ActAt(ctx, project, path, "restore", map[string]string{"backup": backup, "target": "pg-restored"}); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, project, path, "pitr", map[string]string{"target": "pg-pit", "time": "2026-10-16T12:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	dsName := "projects/" + project + "/locations/us-central1/backupVaults/vault-1/dataSources/" + ds[0].Name
	if len(sqlCalls) != 2 ||
		!strings.HasPrefix(sqlCalls[0], "/v1/projects/"+project+"/instances/pg-restored/restoreBackup ") ||
		!strings.Contains(sqlCalls[0], `"backupdrBackup":"`+dsName+"/backups/"+backup+`"`) ||
		!strings.HasPrefix(sqlCalls[1], "/v1/projects/"+project+":pointInTimeRestore ") ||
		!strings.Contains(sqlCalls[1], `"datasource":"`+dsName+`"`) {
		t.Errorf("Cloud SQL calls: %q", sqlCalls)
	}
}

func bdrActionIDs(acts []console.Action) string {
	var out []string
	for _, a := range acts {
		out = append(out, a.ID)
	}
	return strings.Join(out, ",")
}
