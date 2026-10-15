package sqladmin

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// fakeDumper keeps each instance's "data" as a string, dumped to a file.
type fakeDumper struct {
	mu   sync.Mutex
	data map[string]string
}

func (f *fakeDumper) Dump(_ context.Context, t DumpTarget, dir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return os.WriteFile(filepath.Join(dir, "data"), []byte(f.data[t.Instance]), 0o600)
}

func (f *fakeDumper) Load(_ context.Context, t DumpTarget, dir string) error {
	b, err := os.ReadFile(filepath.Join(dir, "data"))
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data[t.Instance] = string(b)
	return nil
}

func (f *fakeDumper) get(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.data[name]
}

func TestBackupsRestoreAndClone(t *testing.T) {
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
	op, err := svc.BackupRuns.Insert("proj", "pg", &sqladmin.BackupRun{Description: "before"}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if got := wait(t, svc, "proj", op); got.Error != nil || got.OperationType != "BACKUP_VOLUME" {
		t.Fatalf("backup: %+v", got)
	}
	list, err := svc.BackupRuns.List("proj", "pg").Do()
	if err != nil || len(list.Items) != 1 || list.Items[0].Status != "SUCCESSFUL" || list.Items[0].Description != "before" {
		t.Fatalf("list: %v %+v", err, list)
	}
	id := list.Items[0].Id
	if got, err := svc.BackupRuns.Get("proj", "pg", id).Do(); err != nil || got.Type != "ON_DEMAND" {
		t.Fatalf("get: %v %+v", err, got)
	}
	d.mu.Lock()
	d.data["pg"] = "v2"
	d.mu.Unlock()
	op, err = svc.Instances.RestoreBackup("proj", "pg", &sqladmin.InstancesRestoreBackupRequest{
		RestoreBackupContext: &sqladmin.RestoreBackupContext{BackupRunId: id, InstanceId: "pg"}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if got := wait(t, svc, "proj", op); got.Error != nil || d.get("pg") != "v1" {
		t.Fatalf("restore: %+v, data %q", got.Error, d.get("pg"))
	}
	if _, err := svc.Instances.RestoreBackup("proj", "pg", &sqladmin.InstancesRestoreBackupRequest{
		RestoreBackupContext: &sqladmin.RestoreBackupContext{BackupRunId: 42}}).Do(); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("restore of a missing run: %v, want 404", err)
	}

	op, err = svc.Instances.Clone("proj", "pg", &sqladmin.InstancesCloneRequest{CloneContext: &sqladmin.CloneContext{
		DestinationInstanceName: "pg-copy"}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if got := wait(t, svc, "proj", op); got.Error != nil || got.OperationType != "CLONE" || d.get("pg-copy") != "v1" {
		t.Fatalf("clone: %+v, data %q", got.Error, d.get("pg-copy"))
	}
	if in, err := svc.Instances.Get("proj", "pg-copy").Do(); err != nil || in.State != "RUNNABLE" {
		t.Fatalf("clone get: %v %+v", err, in)
	}
	if _, err := svc.Instances.Clone("proj", "pg", &sqladmin.InstancesCloneRequest{CloneContext: &sqladmin.CloneContext{
		DestinationInstanceName: "pg-pit", PointInTime: "2026-01-01T00:00:00Z"}}).Do(); err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("point-in-time clone of a server that keeps no log: %v, want 400", err)
	}

	// backupConfiguration is recorded and an automated backup is taken at
	// its startTime.
	at := time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC)
	op, err = svc.Instances.Patch("proj", "pg-copy", &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{
		BackupConfiguration: &sqladmin.BackupConfiguration{Enabled: true, StartTime: "03:04"}}}).Do()
	if err != nil || op.Status != "DONE" {
		t.Fatalf("patch: %v %+v", err, op)
	}
	in, _ := svc.Instances.Get("proj", "pg-copy").Do()
	if bc := in.Settings.BackupConfiguration; !bc.Enabled || bc.StartTime != "03:04" {
		t.Errorf("backupConfiguration: %+v", bc)
	}
	srv.automatedTick(at)
	srv.automatedTick(at) // once a day
	deadline := time.Now().Add(2 * time.Second)
	for {
		l, _ := svc.BackupRuns.List("proj", "pg-copy").Do()
		if len(l.Items) == 1 && l.Items[0].Type == "AUTOMATED" && l.Items[0].Status == "SUCCESSFUL" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("automated backups: %+v", l.Items)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := svc.Instances.Patch("proj", "pg-copy", &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{
		BackupConfiguration: &sqladmin.BackupConfiguration{PointInTimeRecoveryEnabled: true}}}).Do(); err != nil {
		t.Errorf("enabling point-in-time recovery: %v", err)
	}
	// No base has been taken yet: a clone to a time is refused.
	if _, err := svc.Instances.Clone("proj", "pg-copy", &sqladmin.InstancesCloneRequest{CloneContext: &sqladmin.CloneContext{
		DestinationInstanceName: "pg-pit", PointInTime: time.Now().UTC().Format(time.RFC3339)}}).Do(); err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("a clone to a time with no base: %v, want 400", err)
	}

	// Records survive a restart of the API; delete removes the dump.
	srv2 := NewServer("", newFake())
	if err := srv2.EnableBackups(d, filepath.Join(dir, "backups"), true); err != nil {
		t.Fatal(err)
	}
	if n := len(srv2.backups.runs); n != 2 {
		t.Errorf("records kept: %d, want 2", n)
	}
	op, err = svc.BackupRuns.Delete("proj", "pg", id).Do()
	if err != nil || op.OperationType != "DELETE_BACKUP" {
		t.Fatalf("delete: %v %+v", err, op)
	}
	if _, err := os.Stat(filepath.Join(dir, "backups", strconv.FormatInt(id, 10))); !os.IsNotExist(err) {
		t.Errorf("the deleted backup's dump is still there: %v", err)
	}
	if _, err := svc.BackupRuns.Get("proj", "pg", id).Do(); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("get of a deleted run: %v, want 404", err)
	}
}

// The fake keeps no log: it has no base to recover from.
func (d *fakeDumper) PITRBase(context.Context, DumpTarget) error { return nil }
func (d *fakeDumper) RecoveryWindow(context.Context, DumpTarget) (time.Time, time.Time, bool, error) {
	return time.Time{}, time.Time{}, false, nil
}
func (d *fakeDumper) CopyAt(context.Context, DumpTarget, DumpTarget, time.Time, string) error {
	return errors.New("the fake keeps no log")
}
func (d *fakeDumper) Prune(context.Context, DumpTarget, time.Time) error { return nil }
