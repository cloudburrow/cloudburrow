package sqladmin

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

func TestProjectBackupsAndFinalBackup(t *testing.T) {
	dir := t.TempDir()
	srv := NewServer("", newFake())
	srv.EnableInstanceWrites(newFakeProv(), filepath.Join(dir, "instances.json"))
	d := &fakeDumper{data: map[string]string{"pg": "v1"}}
	if err := srv.EnableBackups(d, filepath.Join(dir, "backups"), true); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	svc, err := sqladmin.NewService(context.Background(), option.WithEndpoint(ts.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	op, err := svc.Backups.CreateBackup("projects/proj", &sqladmin.Backup{Instance: "pg", Description: "first", TtlDays: 2}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if got := wait(t, svc, "proj", op); got.Error != nil {
		t.Fatalf("create: %+v", got.Error)
	}
	list, err := svc.Backups.ListBackups("projects/proj").Filter(`instance="pg"`).Do()
	if err != nil || len(list.Backups) != 1 {
		t.Fatalf("list: %v %+v", err, list)
	}
	b := list.Backups[0]
	if b.State != "SUCCESSFUL" || b.Type != "ON_DEMAND" || b.Description != "first" || b.ExpiryTime == "" || !strings.HasPrefix(b.Name, "projects/proj/backups/") {
		t.Fatalf("backup: %+v", b)
	}
	if _, err := svc.Backups.UpdateBackup(b.Name, &sqladmin.Backup{TtlDays: 5}).UpdateMask("ttl_days").Do(); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("ttl of an ON_DEMAND backup: %v, want 400", err)
	}
	if _, err := svc.Backups.UpdateBackup(b.Name, &sqladmin.Backup{Description: "renamed"}).UpdateMask("description").Do(); err != nil {
		t.Fatal(err)
	}
	if got, err := svc.Backups.GetBackup(b.Name).Do(); err != nil || got.Description != "renamed" {
		t.Fatalf("get: %v %+v", err, got)
	}
	// The same backup is the instance's backup run.
	runs, err := svc.BackupRuns.List("proj", "pg").Do()
	if err != nil || len(runs.Items) != 1 || !strings.HasSuffix(b.Name, "/"+strconv.FormatInt(runs.Items[0].Id, 10)) {
		t.Fatalf("runs: %v %+v", err, runs)
	}
	// It expires on the retention clock.
	if _, err := srv.Advance(3 * 24 * time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Backups.GetBackup(b.Name).Do(); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("an expired backup: %v, want 404", err)
	}

	// A created instance deleted with a final backup keeps it.
	op, err = svc.Instances.Insert("proj", &sqladmin.DatabaseInstance{Name: "orders", DatabaseVersion: "POSTGRES_17",
		Settings: &sqladmin.Settings{Tier: "db-custom-2-7680"}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	wait(t, svc, "proj", op)
	d.mu.Lock()
	d.data["orders"] = "final data"
	d.mu.Unlock()
	op, err = svc.Instances.Delete("proj", "orders").EnableFinalBackup(true).FinalBackupDescription("last").Do()
	if err != nil {
		t.Fatal(err)
	}
	if got := wait(t, svc, "proj", op); got.Error != nil {
		t.Fatalf("delete: %+v", got.Error)
	}
	list, err = svc.Backups.ListBackups("projects/proj").Filter("type=FINAL").Do()
	if err != nil || len(list.Backups) != 1 {
		t.Fatalf("final: %v %+v", err, list)
	}
	f := list.Backups[0]
	if f.Instance != "orders" || f.Description != "last" || f.InstanceDeletionTime == "" || f.ExpiryTime == "" {
		t.Fatalf("final backup: %+v", f)
	}
	if _, err := svc.Backups.UpdateBackup(f.Name, &sqladmin.Backup{TtlDays: 10}).UpdateMask("ttl_days").Do(); err != nil {
		t.Fatalf("a final backup's ttl: %v", err)
	}
	// It restores into another instance by its name.
	d.mu.Lock()
	d.data["pg"] = "other"
	d.mu.Unlock()
	op, err = svc.Instances.RestoreBackup("proj", "pg", &sqladmin.InstancesRestoreBackupRequest{Backup: f.Name}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if got := wait(t, svc, "proj", op); got.Error != nil || d.get("pg") != "final data" {
		t.Fatalf("restore: %+v %q", got.Error, d.get("pg"))
	}
	op, err = svc.Backups.DeleteBackup(f.Name).Do()
	if err != nil || op.OperationType != "DELETE_BACKUP" {
		t.Fatalf("delete backup: %v %+v", err, op)
	}
	if _, err := svc.Backups.GetBackup(f.Name).Do(); err == nil {
		t.Fatal("a deleted backup is still served")
	}
}

func TestRefusedVerbsSayWhy(t *testing.T) {
	srv := NewServer("", newFake())
	ts := httptest.NewServer(srv)
	defer ts.Close()
	svc, err := sqladmin.NewService(context.Background(), option.WithEndpoint(ts.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	for what, call := range map[string]func() error{
		"Certificate Authority Service": func() error { _, err := svc.Instances.ListServerCertificates("proj", "pg").Do(); return err },
		"no Google-managed maintenance": func() error {
			_, err := svc.Projects.Instances.RescheduleMaintenance("proj", "pg", &sqladmin.SqlInstancesRescheduleMaintenanceRequestBody{}).Do()
			return err
		},
		"SQL Server": func() error { _, err := svc.Instances.AcquireSsrsLease("proj", "pg", nil).Do(); return err },
		"Google-managed networking": func() error {
			_, err := svc.BlueGreenDeployments.List("projects/proj/locations/us-central1").Do()
			return err
		},
		"Cloud DNS": func() error { _, err := svc.Connect.Resolve("us-central1", "x.example.com").Do(); return err },
	} {
		if err := call(); err == nil || !strings.Contains(err.Error(), "501") || !strings.Contains(err.Error(), what) {
			t.Errorf("%s: %v", what, err)
		}
	}
}
