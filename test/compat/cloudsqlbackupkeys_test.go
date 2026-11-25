//go:build compat

package compat

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	kmspb "cloud.google.com/go/kms/apiv1/kmspb"
	sqladmin "google.golang.org/api/sqladmin/v1"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestCloudSQLBackupLocationsAndReencryption (#1956): through the official
// Go client, a CMEK PostgreSQL instance's backups are in the multi-region
// nearest it by default ("us"), in the location an on-demand backup names,
// and in settings.backupConfiguration.location once it is set; a backup
// records its key and the key's primary version when taken. reencrypt
// alone leaves the backups on their version; with that version disabled a
// backup on it cannot be restored nor re-encrypted; with it enabled,
// backupReencryptionConfig (ON_DEMAND) moves them to the primary version,
// after which, with the old version disabled again, the oldest backup
// restores and its rows read back through the official connector.
// gcloud sql backups create --location and gcloud sql instances patch
// --backup-location set the same.
// covers: sql.backupRuns.insert, sql.backupRuns.list, sql.Backups.CreateBackup, sql.Backups.ListBackups, sql.instances.patch, sql.instances.reencrypt, sql.instances.restoreBackup
func TestCloudSQLBackupLocationsAndReencryption(t *testing.T) {
	h := New(t)
	svc := sqlAdminService(t, h)
	shardEndpoint(h, envCMEKKMS, EnvKMS)
	kc := kmsClients(t, h)["grpc"]
	if os.Getenv(EnvCloudSQL) == "" || os.Getenv(EnvKMS) == "" {
		t.Skip("needs --services cloudsql,kms")
	}
	ctx := h.Context()
	project := h.Project()
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	key, version1 := sqlKey(t, h, "bkenc-"+suffix)
	name := "bkenc-" + suffix
	c := sqlBackupCase{name: name, version: "POSTGRES_17", instance: name, created: true, table: "bk_rows",
		dsn: func(_, conn string) string {
			return fmt.Sprintf("host=%s user=postgres password=%s dbname=postgres sslmode=disable", conn, pgRootPassword)
		}, register: pgRegister}
	op, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "POSTGRES_17",
		RootPassword: "Root-pass1",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840",
			BackupConfiguration: &sqladmin.BackupConfiguration{Enabled: true, StartTime: "03:00"}},
		DiskEncryptionConfiguration: &sqladmin.DiskEncryptionConfiguration{KmsKeyName: key}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		version1(kmspb.CryptoKeyVersion_ENABLED)
		if op, err := svc.Instances.Delete(project, name).Do(); err == nil {
			waitSQLOp(t, svc, project, op)
		}
	}()
	waitSQLOp(t, svc, project, op)
	conn := project + ":us-central1:" + name
	sqlBackupExec(t, h, c, conn, "CREATE TABLE bk_rows (id INT PRIMARY KEY, name TEXT)", "INSERT INTO bk_rows VALUES (1, 'first')")

	backup := func(loc string) {
		t.Helper()
		op, err := svc.BackupRuns.Insert(project, name, &sqladmin.BackupRun{Location: loc}).Do()
		if err != nil {
			t.Fatal(err)
		}
		if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
			t.Fatalf("backup in %q: %s", loc, op.Error.Errors[0].Message)
		}
	}
	runs := func() []*sqladmin.BackupRun {
		t.Helper()
		list, err := svc.BackupRuns.List(project, name).Do()
		if err != nil {
			t.Fatal(err)
		}
		return list.Items
	}
	versions := func() string {
		t.Helper()
		var out []string
		for _, r := range runs() {
			if r.DiskEncryptionConfiguration == nil || r.DiskEncryptionConfiguration.KmsKeyName != key || r.DiskEncryptionStatus == nil {
				t.Fatalf("backup run %d has no key: %+v", r.Id, r)
			}
			v := r.DiskEncryptionStatus.KmsKeyVersionName
			out = append(out, v[strings.LastIndexByte(v, '/')+1:])
		}
		return strings.Join(out, ",")
	}

	backup("")
	backup("europe-west1")
	if rs := runs(); len(rs) != 2 || rs[0].Location != "europe-west1" || rs[1].Location != "us" {
		t.Fatalf("backup locations: %+v", rs)
	}
	_, err = svc.BackupRuns.Insert(project, name, &sqladmin.BackupRun{Location: "not a region"}).Do()
	wantSQLRefusal(t, "a backup in an invalid location", err, "not a region")
	op, err = svc.Instances.Patch(project, name, &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{
		BackupConfiguration: &sqladmin.BackupConfiguration{Enabled: true, StartTime: "03:00", Location: "asia-northeast1"}}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOp(t, svc, project, op)
	if in, err := svc.Instances.Get(project, name).Do(); err != nil || in.Settings.BackupConfiguration.Location != "asia-northeast1" {
		t.Fatalf("backupConfiguration.location after the patch: %v", err)
	}
	op, err = svc.Backups.CreateBackup("projects/"+project, &sqladmin.Backup{Instance: name, Description: "configured"}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOp(t, svc, project, op)
	list, err := svc.Backups.ListBackups("projects/" + project).Filter(`instance="` + name + `"`).Do()
	if err != nil || len(list.Backups) != 3 {
		t.Fatalf("backups: %v %+v", err, list)
	}
	if b := list.Backups[0]; b.Location != "asia-northeast1" || b.KmsKey != key || b.KmsKeyVersion != key+"/cryptoKeyVersions/1" {
		t.Errorf("the configured backup: location %q kmsKey %q kmsKeyVersion %q", b.Location, b.KmsKey, b.KmsKeyVersion)
	}
	if got := versions(); got != "1,1,1" {
		t.Fatalf("backup versions %s, want 1,1,1", got)
	}

	// A new primary version: the instance is re-encrypted alone, and its
	// backups stay on version 1.
	v2, err := kc.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: key})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kc.UpdateCryptoKeyPrimaryVersion(ctx, &kmspb.UpdateCryptoKeyPrimaryVersionRequest{Name: key,
		CryptoKeyVersionId: v2.GetName()[len(key+"/cryptoKeyVersions/"):]}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = kc.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{
			CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: v2.GetName(), State: kmspb.CryptoKeyVersion_ENABLED},
			UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"state"}}})
	}()
	op, err = svc.Instances.Reencrypt(project, name, &sqladmin.InstancesReencryptRequest{}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOp(t, svc, project, op)
	if got := versions(); got != "1,1,1" {
		t.Fatalf("after reencrypt without backupReencryptionConfig: %s, want 1,1,1", got)
	}
	oldest := runs()[2].Id

	// With version 1 disabled, a backup on it is neither restored nor
	// re-encrypted.
	version1(kmspb.CryptoKeyVersion_DISABLED)
	_, err = svc.Instances.RestoreBackup(project, name, &sqladmin.InstancesRestoreBackupRequest{
		RestoreBackupContext: &sqladmin.RestoreBackupContext{BackupRunId: oldest, InstanceId: name, Project: project}}).Do()
	wantSQLRefusal(t, "restore of a backup whose key version is disabled", err, "cryptoKeyVersions/1")
	_, err = svc.Instances.Reencrypt(project, name, &sqladmin.InstancesReencryptRequest{
		BackupReencryptionConfig: &sqladmin.BackupReencryptionConfig{BackupType: "ON_DEMAND"}}).Do()
	wantSQLRefusal(t, "re-encryption of backups on a disabled version", err, key)
	version1(kmspb.CryptoKeyVersion_ENABLED)

	_, err = svc.Instances.Reencrypt(project, name, &sqladmin.InstancesReencryptRequest{
		BackupReencryptionConfig: &sqladmin.BackupReencryptionConfig{BackupType: "DAILY"}}).Do()
	wantCode(t, "backupType DAILY", err, 400, "backupType")
	op, err = svc.Instances.Reencrypt(project, name, &sqladmin.InstancesReencryptRequest{
		BackupReencryptionConfig: &sqladmin.BackupReencryptionConfig{BackupType: "ON_DEMAND", BackupLimit: 10}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOp(t, svc, project, op)
	if got := versions(); got != "2,2,2" {
		t.Fatalf("after re-encrypting the on-demand backups: %s, want 2,2,2", got)
	}

	// Version 1 is no longer needed: the oldest backup restores.
	version1(kmspb.CryptoKeyVersion_DISABLED)
	sqlBackupExec(t, h, c, conn, "UPDATE bk_rows SET name = 'changed'")
	op, err = svc.Instances.RestoreBackup(project, name, &sqladmin.InstancesRestoreBackupRequest{
		RestoreBackupContext: &sqladmin.RestoreBackupContext{BackupRunId: oldest, InstanceId: name, Project: project}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
		t.Fatalf("restore of the re-encrypted backup: %s", op.Error.Errors[0].Message)
	}
	if got := strings.Join(sqlBackupRows(t, h, c, conn), ","); got != "1=first" {
		t.Errorf("rows after the restore: %q, want 1=first", got)
	}

	// gcloud sql backups create --location and instances patch
	// --backup-location, through the configuration `cloudburrow
	// gcloud-setup` writes.
	g := newGcloudSession(t, h)
	g.must("sql", "backups", "create", "--instance", name, "--location", "eu", "--project", project, "--quiet")
	if out := g.must("sql", "backups", "list", "--instance", name, "--project", project, "--format=value(location)"); !strings.Contains(out, "eu") {
		t.Errorf("gcloud sql backups list: %s, want a backup in eu", out)
	}
	g.must("sql", "instances", "patch", name, "--backup-location", "europe-west2", "--project", project, "--quiet")
	if out := g.must("sql", "instances", "describe", name, "--project", project,
		"--format=value(settings.backupConfiguration.location)"); strings.TrimSpace(out) != "europe-west2" {
		t.Errorf("after gcloud sql instances patch --backup-location: %q", out)
	}
}

// TestTerraformSQLBackupLocation (#1956): a google_sql_database_instance
// whose backup_configuration names a location applies, reads back through
// the official Go client, plans clean, moves to another location in place,
// and its next backup is taken there.
// covers: sql.instances.insert, sql.instances.patch, sql.backupRuns.insert
func TestTerraformSQLBackupLocation(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	svc := sqlAdminService(t, h)
	m := newTFModule(t)
	name := fmt.Sprintf("tf-bkloc-%d", time.Now().UnixNano()%1e6)
	module := func(loc string) string {
		return fmt.Sprintf(`
resource "google_sql_database_instance" "i" {
  project             = %[1]q
  name                = %[2]q
  region              = "us-central1"
  database_version    = "POSTGRES_17"
  root_password       = "Root-pass1"
  deletion_protection = false
  settings {
    tier = "db-custom-1-3840"
    backup_configuration {
      enabled    = true
      start_time = "03:00"
      location   = %[3]q
    }
  }
}
`, m.project, name, loc)
	}
	check := func(want string) {
		t.Helper()
		in, err := svc.Instances.Get(m.project, name).Do()
		if err != nil || in.Settings.BackupConfiguration == nil || in.Settings.BackupConfiguration.Location != want {
			t.Fatalf("backupConfiguration: %v, want location %s", err, want)
		}
	}
	m.write(module("europe-west1"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	defer m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	check("europe-west1")
	m.planClean()

	m.write(module("eu"))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 1 changed, 0 destroyed") {
		t.Errorf("the location was not changed in place:\n%s", lastLines(out, 20))
	}
	check("eu")
	m.planClean()
	op, err := svc.BackupRuns.Insert(m.project, name, &sqladmin.BackupRun{}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if op = waitSQLOpDone(t, svc, m.project, op); op.Error != nil {
		t.Fatalf("backup: %s", op.Error.Errors[0].Message)
	}
	runs, err := svc.BackupRuns.List(m.project, name).Do()
	if err != nil || len(runs.Items) == 0 || runs.Items[0].Location != "eu" {
		t.Errorf("the backup after the change: %v %+v", err, runs)
	}
}
