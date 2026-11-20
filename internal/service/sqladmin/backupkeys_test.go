package sqladmin

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestSQLBackupLocations (#1956): a backup is in the multi-region nearest
// its instance by default, in settings.backupConfiguration.location once
// it is set, and in the location an on-demand backup names; a location
// that is no region or multi-region is 400.
func TestSQLBackupLocations(t *testing.T) {
	dir := t.TempDir()
	srv := NewServer("", newFake())
	srv.EnableInstanceWrites(newFakeProv(), filepath.Join(dir, "instances.json"))
	if err := srv.EnableBackups(&fakeDumper{data: map[string]string{}}, filepath.Join(dir, "backups"), true); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	svc, err := sqladmin.NewService(context.Background(), option.WithEndpoint(ts.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	op, err := svc.Instances.Insert("proj", &sqladmin.DatabaseInstance{Name: "loc", DatabaseVersion: "POSTGRES_17",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	wait(t, svc, "proj", op)
	run := func(want string, r *sqladmin.BackupRun) {
		t.Helper()
		op, err := svc.BackupRuns.Insert("proj", "loc", r).Do()
		if err != nil {
			t.Fatal(err)
		}
		if got := wait(t, svc, "proj", op); got.Error != nil {
			t.Fatalf("backup: %+v", got.Error.Errors[0])
		}
		list, err := svc.BackupRuns.List("proj", "loc").Do()
		if err != nil || len(list.Items) == 0 {
			t.Fatalf("list: %v", err)
		}
		if got := list.Items[0].Location; got != want {
			t.Errorf("backup location %q, want %q", got, want)
		}
	}
	run("us", &sqladmin.BackupRun{})
	run("europe-west1", &sqladmin.BackupRun{Location: "europe-west1"})
	if _, err := svc.BackupRuns.Insert("proj", "loc", &sqladmin.BackupRun{Location: "moon"}).Do(); err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("a backup in moon: %v, want 400", err)
	}

	op, err = svc.Instances.Patch("proj", "loc", &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{
		BackupConfiguration: &sqladmin.BackupConfiguration{Enabled: true, Location: "asia-northeast1"}}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	wait(t, svc, "proj", op)
	in, err := svc.Instances.Get("proj", "loc").Do()
	if err != nil || in.Settings.BackupConfiguration.Location != "asia-northeast1" {
		t.Fatalf("backupConfiguration after the patch: %v %+v", err, in.Settings.BackupConfiguration)
	}
	run("asia-northeast1", &sqladmin.BackupRun{})
	if _, err := svc.Instances.Patch("proj", "loc", &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{
		BackupConfiguration: &sqladmin.BackupConfiguration{Enabled: true, Location: "Nowhere"}}}).Do(); err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("backupConfiguration.location Nowhere: %v, want 400", err)
	}

	// The project-level Backups API takes and answers a location too.
	op, err = svc.Backups.CreateBackup("projects/proj", &sqladmin.Backup{Instance: "loc", Location: "eu"}).Do()
	if err != nil {
		t.Fatal(err)
	}
	wait(t, svc, "proj", op)
	list, err := svc.Backups.ListBackups("projects/proj").Do()
	if err != nil || len(list.Backups) == 0 || list.Backups[0].Location != "eu" {
		t.Fatalf("backups.create in eu: %v %+v", err, list)
	}
}

// TestSQLBackupReencryption (#1956): a CMEK instance's backups are wrapped
// with its key's primary version when taken; reencrypt alone leaves them
// on it, backupReencryptionConfig wraps the backups of its type again (at
// most backupLimit, newest first), and a backup whose version is disabled
// cannot be restored.
func TestSQLBackupReencryption(t *testing.T) {
	dir := t.TempDir()
	srv := NewServer("", newFake())
	if err := srv.EnableConnector("127.0.0.1:0", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	srv.EnableInstanceWrites(newFakeProv(), filepath.Join(dir, "instances.json"))
	if err := srv.EnableBackups(&fakeDumper{data: map[string]string{}}, filepath.Join(dir, "backups"), true); err != nil {
		t.Fatal(err)
	}
	k := &versionedKMS{primary: 1, disabled: map[int]bool{}}
	srv.UseKMS(k)
	ts := httptest.NewServer(srv)
	defer ts.Close()
	svc, err := sqladmin.NewService(context.Background(), option.WithEndpoint(ts.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	op, err := svc.Instances.Insert("proj", &sqladmin.DatabaseInstance{Name: "enc", DatabaseVersion: "POSTGRES_17",
		Settings:                    &sqladmin.Settings{Tier: "db-custom-1-3840"},
		DiskEncryptionConfiguration: &sqladmin.DiskEncryptionConfiguration{KmsKeyName: testKey}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	wait(t, svc, "proj", op)
	primary := func(n int) {
		k.mu.Lock()
		k.primary = n
		k.mu.Unlock()
	}
	backup := func() {
		t.Helper()
		op, err := svc.BackupRuns.Insert("proj", "enc", &sqladmin.BackupRun{}).Do()
		if err != nil {
			t.Fatal(err)
		}
		if got := wait(t, svc, "proj", op); got.Error != nil {
			t.Fatalf("backup: %+v", got.Error.Errors[0])
		}
	}
	versions := func() []string {
		t.Helper()
		list, err := svc.BackupRuns.List("proj", "enc").Do()
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range list.Items {
			if r.DiskEncryptionConfiguration == nil || r.DiskEncryptionConfiguration.KmsKeyName != testKey || r.DiskEncryptionStatus == nil {
				t.Fatalf("backup run %d has no key: %+v", r.Id, r)
			}
			v := r.DiskEncryptionStatus.KmsKeyVersionName
			out = append(out, v[strings.LastIndexByte(v, '/')+1:])
		}
		return out
	}
	backup()
	// "Backups created after a key rotation use the new primary key
	// version", though the instance is still on version 1.
	primary(2)
	backup()
	backup()
	if got := strings.Join(versions(), ","); got != "2,2,1" {
		t.Fatalf("backup versions %s, want 2,2,1", got)
	}
	list, _ := svc.Backups.ListBackups("projects/proj").Do()
	if b := list.Backups[0]; b.KmsKey != testKey || !strings.HasSuffix(b.KmsKeyVersion, "/cryptoKeyVersions/2") {
		t.Errorf("the backup's kmsKey %q kmsKeyVersion %q", b.KmsKey, b.KmsKeyVersion)
	}

	primary(3)
	op, err = svc.Instances.Reencrypt("proj", "enc", &sqladmin.InstancesReencryptRequest{}).Do()
	if err != nil {
		t.Fatal(err)
	}
	wait(t, svc, "proj", op)
	if got := strings.Join(versions(), ","); got != "2,2,1" {
		t.Fatalf("after reencrypt without backupReencryptionConfig: %s, want 2,2,1", got)
	}
	if _, err := svc.Instances.Reencrypt("proj", "enc", &sqladmin.InstancesReencryptRequest{
		BackupReencryptionConfig: &sqladmin.BackupReencryptionConfig{BackupType: "AUTOMATED"}}).Do(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(versions(), ","); got != "2,2,1" {
		t.Errorf("AUTOMATED re-encryption changed the on-demand backups: %s", got)
	}
	if _, err := svc.Instances.Reencrypt("proj", "enc", &sqladmin.InstancesReencryptRequest{
		BackupReencryptionConfig: &sqladmin.BackupReencryptionConfig{BackupType: "ON_DEMAND", BackupLimit: 2}}).Do(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(versions(), ","); got != "3,3,1" {
		t.Fatalf("after ON_DEMAND re-encryption of 2: %s, want 3,3,1", got)
	}
	if _, err := svc.Instances.Reencrypt("proj", "enc", &sqladmin.InstancesReencryptRequest{
		BackupReencryptionConfig: &sqladmin.BackupReencryptionConfig{BackupType: "WEEKLY"}}).Do(); err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("backupType WEEKLY: %v, want 400", err)
	}

	// With version 1 disabled, the oldest backup cannot be restored, nor
	// re-encrypted; the newer ones can.
	k.mu.Lock()
	k.disabled[1] = true
	k.mu.Unlock()
	runs, _ := svc.BackupRuns.List("proj", "enc").Do()
	oldest, newest := runs.Items[2].Id, runs.Items[0].Id
	_, err = svc.Instances.RestoreBackup("proj", "enc", &sqladmin.InstancesRestoreBackupRequest{
		RestoreBackupContext: &sqladmin.RestoreBackupContext{BackupRunId: oldest, InstanceId: "enc"}}).Do()
	if err == nil || !strings.Contains(err.Error(), "cryptoKeyVersions/1") {
		t.Errorf("restore of a backup whose version is disabled: %v", err)
	}
	if _, err := svc.Instances.Reencrypt("proj", "enc", &sqladmin.InstancesReencryptRequest{
		BackupReencryptionConfig: &sqladmin.BackupReencryptionConfig{BackupType: "ON_DEMAND"}}).Do(); err == nil {
		t.Error("re-encrypting a backup whose version is disabled succeeded")
	}
	if got := strings.Join(versions(), ","); got != "3,3,1" {
		t.Errorf("a refused re-encryption changed backups: %s", got)
	}
	op, err = svc.Instances.RestoreBackup("proj", "enc", &sqladmin.InstancesRestoreBackupRequest{
		RestoreBackupContext: &sqladmin.RestoreBackupContext{BackupRunId: newest, InstanceId: "enc"}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if got := wait(t, svc, "proj", op); got.Error != nil {
		t.Fatalf("restore of the re-encrypted backup: %+v", got.Error.Errors[0])
	}
}
