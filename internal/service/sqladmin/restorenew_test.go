package sqladmin

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// jobsBackupDR resolves one Backup and DR backup to a run, and records the
// restore jobs logged (#1924).
type jobsBackupDR struct {
	run  string
	mu   sync.Mutex
	jobs []string
}

func (*jobsBackupDR) DataSourceInstance(string) (string, string, error) {
	return "", "", errors.New("no data source")
}

func (j *jobsBackupDR) BackupRunID(name string) (string, error) {
	if strings.HasSuffix(name, "/backups/b") {
		return j.run, nil
	}
	return "", errors.New("The Backup and DR backup " + name + " does not exist.")
}

func (j *jobsBackupDR) RestoreStarted(category, source, project, instance, at string) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.jobs = append(j.jobs, "start "+category+" "+project+"/"+instance)
	return "job-1"
}

func (j *jobsBackupDR) RestoreFinished(id string, err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.jobs = append(j.jobs, "end "+id+" "+map[bool]string{true: "FAILED", false: "SUCCESSFUL"}[err != nil])
}

func (j *jobsBackupDR) logged() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]string(nil), j.jobs...)
}

// TestRestoreBackupToANewInstance (#1922): a Backup and DR backup restored
// to an instance that does not exist makes it with the backup's settings
// and loads the backup into it; a major version change is refused, and a
// backup run restores only into an instance that exists. The restore is logged as a job (#1924).
func TestRestoreBackupToANewInstance(t *testing.T) {
	dir := t.TempDir()
	srv := NewServer("", newFake())
	srv.EnableInstanceWrites(newFakeProv(), filepath.Join(dir, "instances.json"))
	d := &fakeDumper{data: map[string]string{"pg": "v1"}}
	if err := srv.EnableBackups(d, filepath.Join(dir, "backups"), true); err != nil {
		t.Fatal(err)
	}
	run, done, err := srv.TakeBackupDRBackup("proj", "pg", "Backup and DR")
	if err != nil {
		t.Fatal(err)
	}
	<-done
	jobs := &jobsBackupDR{run: run}
	srv.SetBackupDR(jobs)
	ts := httptest.NewServer(srv)
	defer ts.Close()
	svc, err := sqladmin.NewService(context.Background(), option.WithEndpoint(ts.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	const name = "projects/proj/locations/us-central1/backupVaults/v/dataSources/ds/backups/b"
	refused := func(what string, req *sqladmin.InstancesRestoreBackupRequest, instance string, code int, want string) {
		t.Helper()
		_, err := svc.Instances.RestoreBackup("proj", instance, req).Do()
		var ge *googleapi.Error
		if !errors.As(err, &ge) || ge.Code != code || !strings.Contains(ge.Message, want) {
			t.Errorf("%s: %v, want %d with %q", what, err, code, want)
		}
	}
	refused("a backup run into a missing instance", &sqladmin.InstancesRestoreBackupRequest{
		RestoreBackupContext: &sqladmin.RestoreBackupContext{BackupRunId: 1, InstanceId: "pg"}}, "fresh", 404, "does not exist")
	refused("a major version change", &sqladmin.InstancesRestoreBackupRequest{BackupdrBackup: name,
		RestoreInstanceSettings: &sqladmin.DatabaseInstance{DatabaseVersion: "POSTGRES_14"}}, "fresh", 400, "major database version")
	refused("a bad name", &sqladmin.InstancesRestoreBackupRequest{BackupdrBackup: name}, "Fresh_", 400, "instance name")
	refused("an unknown backup", &sqladmin.InstancesRestoreBackupRequest{
		BackupdrBackup: strings.TrimSuffix(name, "b") + "nope"}, "fresh", 404, "does not exist")

	d.mu.Lock()
	d.data["pg"] = "v2"
	d.mu.Unlock()
	op, err := svc.Instances.RestoreBackup("proj", "fresh", &sqladmin.InstancesRestoreBackupRequest{BackupdrBackup: name,
		RestoreInstanceSettings: &sqladmin.DatabaseInstance{Region: "europe-west1",
			Settings: &sqladmin.Settings{Tier: "db-custom-2-7680"}}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if got := wait(t, svc, "proj", op); got.Error != nil || got.OperationType != "RESTORE_VOLUME" || d.get("fresh") != "v1" {
		t.Fatalf("restore to a new instance: %+v, data %q", got.Error, d.get("fresh"))
	}
	in, err := svc.Instances.Get("proj", "fresh").Do()
	if err != nil || in.State != "RUNNABLE" || in.Region != "europe-west1" || in.Settings.Tier != "db-custom-2-7680" ||
		in.DatabaseVersion != srv.engine("pg").DatabaseVersion() {
		t.Fatalf("the new instance: %v %+v", err, in)
	}
	if d.get("pg") != "v2" {
		t.Errorf("the source instance changed: %q", d.get("pg"))
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := jobs.logged()
		if len(got) == 2 {
			if got[0] != "start RESTORE proj/fresh" || got[1] != "end job-1 SUCCESSFUL" {
				t.Errorf("restore jobs: %q", got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("restore jobs: %q", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestMajorVersion(t *testing.T) {
	for v, want := range map[string]string{"MYSQL_8_0_31": "MYSQL_8_0", "MYSQL_8_0": "MYSQL_8_0", "POSTGRES_16": "POSTGRES_16",
		"SQLSERVER_2022_STANDARD": "SQLSERVER_2022", "MYSQL_8_4": "MYSQL_8_4"} {
		if got := majorVersion(v); got != want {
			t.Errorf("majorVersion(%s) = %s, want %s", v, got, want)
		}
	}
}
