//go:build compat

package compat

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestCloudSQLProjectBackups (#1462): the project-level Backups API over
// the PostgreSQL server `up` runs, through the official Go client
// (sqladmin/v1) and gcloud: CreateBackup takes the same backup a backup
// run is, ListBackups filters by instance, UpdateBackup changes its
// description (and refuses a retention change, which only a FINAL backup
// takes), instances.restoreBackup restores it by name, the rows changed
// after it are gone, and DeleteBackup deletes it.
// covers: sql.Backups.CreateBackup, sql.Backups.ListBackups, sql.Backups.GetBackup, sql.Backups.UpdateBackup, sql.Backups.DeleteBackup, sql.backupRuns.list, sql.instances.restoreBackup, sql.operations.get
func TestCloudSQLProjectBackups(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	svc := sqlAdminService(t, h)
	project := h.Project()
	c := pgConnect(t, h, "cloudburrow")
	for _, s := range []string{"DROP TABLE IF EXISTS pbk_rows", "CREATE TABLE pbk_rows (id INT PRIMARY KEY)", "INSERT INTO pbk_rows VALUES (1), (2)"} {
		if _, err := c.Exec(h.Context(), s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	defer func() {
		c2 := pgConnect(t, h, "cloudburrow")
		_, _ = c2.Exec(context.Background(), "DROP TABLE IF EXISTS pbk_rows")
	}()
	op, err := svc.Backups.CreateBackup("projects/"+project, &sqladmin.Backup{Instance: "postgres", Description: "project-level"}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOp(t, svc, project, op)
	list, err := svc.Backups.ListBackups("projects/" + project).Filter(`instance="postgres"`).Do()
	if err != nil {
		t.Fatal(err)
	}
	var b *sqladmin.Backup
	for _, x := range list.Backups {
		if x.Description == "project-level" {
			b = x
		}
		if x.Instance != "postgres" {
			t.Errorf("filtered list has %s's backup %s", x.Instance, x.Name)
		}
	}
	if b == nil || b.State != "SUCCESSFUL" || b.Type != "ON_DEMAND" || b.BackupInterval == nil || b.BackupInterval.EndTime == "" {
		t.Fatalf("ListBackups: %+v", list.Backups)
	}
	if !strings.HasPrefix(b.Name, "projects/"+project+"/backups/") {
		t.Fatalf("backup name %q", b.Name)
	}
	id := b.Name[strings.LastIndex(b.Name, "/")+1:]
	runs, err := svc.BackupRuns.List(project, "postgres").Do()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range runs.Items {
		found = found || fmt.Sprint(r.Id) == id
	}
	if !found {
		t.Errorf("backup %s is not among postgres's backup runs", id)
	}
	if _, err := svc.Backups.UpdateBackup(b.Name, &sqladmin.Backup{TtlDays: 3}).UpdateMask("ttl_days").Do(); err == nil {
		t.Error("UpdateBackup changed an ON_DEMAND backup's retention")
	} else {
		wantCode(t, "UpdateBackup ttl_days", err, 400, "FINAL")
	}
	op, err = svc.Backups.UpdateBackup(b.Name, &sqladmin.Backup{Description: "renamed"}).UpdateMask("description").Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOp(t, svc, project, op)
	if got, err := svc.Backups.GetBackup(b.Name).Do(); err != nil || got.Description != "renamed" {
		t.Fatalf("GetBackup: %v %+v", err, got)
	}

	g := newGcloudSession(t, h)
	p := "--project=" + project
	if out := g.must("sql", "backups", "list", p, "--format=value(name)"); !strings.Contains(out, b.Name) {
		t.Errorf("gcloud sql backups list (project-level): %q", out)
	}
	if out := g.must("sql", "backups", "describe", b.Name, p, "--format=value(description)"); strings.TrimSpace(out) != "renamed" {
		t.Errorf("gcloud sql backups describe: %q", out)
	}

	if _, err := c.Exec(h.Context(), "INSERT INTO pbk_rows VALUES (3)"); err != nil {
		t.Fatal(err)
	}
	op, err = svc.Instances.RestoreBackup(project, "postgres", &sqladmin.InstancesRestoreBackupRequest{Backup: b.Name}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOp(t, svc, project, op)
	c3 := pgConnect(t, h, "cloudburrow")
	var n int
	if err := c3.QueryRow(h.Context(), "SELECT count(*) FROM pbk_rows").Scan(&n); err != nil || n != 2 {
		t.Errorf("rows after restoring by the backup's name: %d, %v, want 2", n, err)
	}
	op, err = svc.Backups.DeleteBackup(b.Name).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOp(t, svc, project, op)
	if _, err := svc.Backups.GetBackup(b.Name).Do(); err == nil {
		t.Error("GetBackup found a deleted backup")
	} else {
		wantCode(t, "GetBackup after DeleteBackup", err, 404, "does not exist")
	}
}

// TestCloudSQLDataAPIAndFinalBackup (#1462), on a created PostgreSQL
// instance with the Data API allowed: instances.executeSql runs
// statements as a database user whose password is a Secret Manager
// secret version (in a temporary table: since PostgreSQL 15 a user other
// than the database's owner cannot create in its public schema), through the official client and `gcloud sql instances
// execute-sql`, with a row limit and SQL errors in its status;
// preCheckMajorVersionUpgrade checks an upgrade, also through gcloud;
// truncateLog truncates MySQL's general log; and instances.delete with
// enableFinalBackup leaves a FINAL backup that outlives the instance and
// whose retention UpdateBackup changes.
// covers: sql.instances.executeSql, sql.instances.preCheckMajorVersionUpgrade, sql.instances.truncateLog, sql.instances.insert, sql.instances.delete, sql.users.insert, sql.Backups.ListBackups, sql.Backups.UpdateBackup, sql.Backups.DeleteBackup, sql.operations.get
func TestCloudSQLDataAPIAndFinalBackup(t *testing.T) {
	h := New(t)
	shardEndpoint(h, envCloudSQLSecrets, EnvSecrets)
	if os.Getenv(EnvCloudSQL) == "" || os.Getenv(EnvSecrets) == "" {
		t.Skip("needs --services cloudsql,secretmanager")
	}
	sqlShardSecrets(t, h) // the password's secret version
	svc := sqlAdminService(t, h)
	project := h.Project()
	name := fmt.Sprintf("data-pg-%d", time.Now().UnixNano()%1e6)
	op, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "POSTGRES_17",
		RootPassword: "Root-pass1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", DataApiAccess: "ALLOW_DATA_API"}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	deleted := false
	defer func() {
		if !deleted {
			if d, err := svc.Instances.Delete(project, name).Do(); err == nil {
				waitSQLOp(t, svc, project, d)
			}
		}
	}()
	waitSQLOp(t, svc, project, op)
	op, err = svc.Users.Insert(project, name, &sqladmin.User{Name: "dataapi", Password: "Data-pass1"}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOp(t, svc, project, op)

	sm := secretsClient(t, h)
	ctx := h.Context()
	parent := "projects/" + project
	secretID := name + "-pw"
	if _, err := sm.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{Parent: parent, SecretId: secretID,
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{
			Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}}}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = sm.DeleteSecret(context.Background(), &secretmanagerpb.DeleteSecretRequest{Name: parent + "/secrets/" + secretID})
	}()
	ver, err := sm.AddSecretVersion(ctx, &secretmanagerpb.AddSecretVersionRequest{Parent: parent + "/secrets/" + secretID,
		Payload: &secretmanagerpb.SecretPayload{Data: []byte("Data-pass1")}})
	if err != nil {
		t.Fatal(err)
	}
	run := func(stmt string, limit int64, mode string) (*sqladmin.SqlInstancesExecuteSqlResponse, error) {
		return svc.Instances.ExecuteSql(project, name, &sqladmin.ExecuteSqlPayload{User: "dataapi", Database: "postgres",
			PasswordSecretVersion: ver.Name, SqlStatement: stmt, RowLimit: limit, PartialResultMode: mode}).Do()
	}
	res, err := run("CREATE TEMP TABLE dapi (id INT, name TEXT); INSERT INTO dapi VALUES (1, 'a'), (2, NULL); SELECT id, name FROM dapi ORDER BY id", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != nil || len(res.Results) != 3 {
		t.Fatalf("executeSql: status %+v, %d results", res.Status, len(res.Results))
	}
	sel := res.Results[2]
	if len(sel.Columns) != 2 || sel.Columns[0].Name != "id" || len(sel.Rows) != 2 ||
		sel.Rows[0].Values[1].Value != "a" || !sel.Rows[1].Values[1].NullValue || sel.Rows[1].Values[0].Value != "2" {
		t.Errorf("SELECT result: %+v", sel)
	}
	if _, err := run("SELECT generate_series(1, 3)", 1, ""); err == nil {
		t.Error("executeSql returned a partial result without ALLOW_PARTIAL_RESULT")
	} else {
		wantCode(t, "executeSql over rowLimit", err, 400, "rowLimit")
	}
	if res, err := run("SELECT generate_series(1, 3)", 1, "ALLOW_PARTIAL_RESULT"); err != nil || len(res.Results) != 1 ||
		!res.Results[0].PartialResult || len(res.Results[0].Rows) != 1 {
		t.Errorf("a partial result: %v %+v", err, res)
	}
	if res, err := run("SELECT * FROM no_such_table", 0, ""); err != nil || res.Status == nil || !strings.Contains(res.Status.Message, "no_such_table") {
		t.Errorf("an SQL error: %v %+v", err, res)
	}
	if _, err := svc.Instances.ExecuteSql(project, name, &sqladmin.ExecuteSqlPayload{Database: "postgres", AutoIamAuthn: true,
		SqlStatement: "SELECT 1"}).Do(); err == nil {
		t.Error("executeSql served IAM database authentication")
	} else {
		wantCode(t, "executeSql with autoIamAuthn", err, 501, "ADR-0004")
	}
	if _, err := svc.Instances.ExecuteSql(project, "postgres", &sqladmin.ExecuteSqlPayload{User: "cloudburrow", Database: "cloudburrow",
		PasswordSecretVersion: ver.Name, SqlStatement: "SELECT 1"}).Do(); err == nil {
		t.Error("executeSql ran on an instance whose Data API is not allowed")
	} else {
		wantCode(t, "executeSql on the up server", err, 412, "dataApiAccess")
	}

	op, err = svc.Instances.PreCheckMajorVersionUpgrade(project, name, &sqladmin.InstancesPreCheckMajorVersionUpgradeRequest{
		PreCheckMajorVersionUpgradeContext: &sqladmin.PreCheckMajorVersionUpgradeContext{TargetDatabaseVersion: "POSTGRES_18"}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOp(t, svc, project, op)
	if got, err := svc.Operations.Get(project, op.Name).Do(); err != nil || got.OperationType != "PRE_CHECK_MAJOR_VERSION_UPGRADE" ||
		got.PreCheckMajorVersionUpgradeContext == nil || got.PreCheckMajorVersionUpgradeContext.TargetDatabaseVersion != "POSTGRES_18" ||
		len(got.PreCheckMajorVersionUpgradeContext.PreCheckResponse) != 0 {
		t.Errorf("pre-check: %v %+v", err, got)
	}
	if _, err := svc.Instances.PreCheckMajorVersionUpgrade(project, name, &sqladmin.InstancesPreCheckMajorVersionUpgradeRequest{
		PreCheckMajorVersionUpgradeContext: &sqladmin.PreCheckMajorVersionUpgradeContext{TargetDatabaseVersion: "POSTGRES_16"}}).Do(); err == nil {
		t.Error("pre-check of a downgrade passed")
	} else {
		wantCode(t, "pre-check of a downgrade", err, 400, "not a major version upgrade")
	}

	g := newGcloudSession(t, h)
	p := "--project=" + project
	out := g.must("sql", "instances", "execute-sql", name, p, "--database=postgres", "--user=dataapi",
		"--password-secret-version="+ver.Name, "--sql=SELECT count(*) AS n FROM (VALUES (1), (2)) AS v", "--format=json")
	if !strings.Contains(out, `"2"`) {
		t.Errorf("gcloud sql instances execute-sql: %s", out)
	}
	g.must("sql", "instances", "pre-check-major-version-upgrade", name, p, "--target-database-version=POSTGRES_18")

	if os.Getenv(EnvMySQL) != "" {
		op, err := svc.Instances.TruncateLog(project, "mysql", &sqladmin.InstancesTruncateLogRequest{
			TruncateLogContext: &sqladmin.TruncateLogContext{LogType: "MYSQL_GENERAL_TABLE"}}).Do()
		if err != nil {
			t.Fatal(err)
		}
		waitSQLOp(t, svc, project, op)
		if op.OperationType != "TRUNCATE_LOG" {
			t.Errorf("truncateLog's operation: %+v", op)
		}
	}
	if _, err := svc.Instances.TruncateLog(project, name, &sqladmin.InstancesTruncateLogRequest{
		TruncateLogContext: &sqladmin.TruncateLogContext{LogType: "MYSQL_GENERAL_TABLE"}}).Do(); err == nil {
		t.Error("truncateLog ran on PostgreSQL")
	} else {
		wantCode(t, "truncateLog on PostgreSQL", err, 400, "only for MySQL")
	}

	// The final backup.
	op, err = svc.Instances.Delete(project, name).EnableFinalBackup(true).FinalBackupDescription("final-" + name).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOp(t, svc, project, op)
	deleted = true
	list, err := svc.Backups.ListBackups("projects/" + project).Filter("type=FINAL").Do()
	if err != nil {
		t.Fatal(err)
	}
	var fb *sqladmin.Backup
	for _, b := range list.Backups {
		if b.Instance == name {
			fb = b
		}
	}
	if fb == nil || fb.Description != "final-"+name || fb.InstanceDeletionTime == "" || fb.ExpiryTime == "" || fb.State != "SUCCESSFUL" {
		t.Fatalf("final backup: %+v", list.Backups)
	}
	op, err = svc.Backups.UpdateBackup(fb.Name, &sqladmin.Backup{TtlDays: 7}).UpdateMask("ttl_days").Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOp(t, svc, project, op)
	op, err = svc.Backups.DeleteBackup(fb.Name).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOp(t, svc, project, op)
}

// TestCloudSQLRefusedMethods (#1462): what needs Compute Engine,
// Google-managed networking, another service or another engine is refused
// 501 through the official client, naming what is missing.
// covers: sql.blueGreenDeployments.create (unimplemented), sql.blueGreenDeployments.delete (unimplemented), sql.blueGreenDeployments.get (unimplemented), sql.blueGreenDeployments.list (unimplemented), sql.blueGreenDeployments.switchover (unimplemented), sql.connect.resolve (unimplemented), sql.instances.ListEntraIdCertificates (unimplemented), sql.instances.addEntraIdCertificate (unimplemented), sql.instances.RotateEntraIdCertificate (unimplemented), sql.instances.ListServerCertificates (unimplemented), sql.instances.addServerCertificate (unimplemented), sql.instances.RotateServerCertificate (unimplemented), sql.instances.acquireSsrsLease (unimplemented), sql.instances.releaseSsrsLease (unimplemented), sql.instances.switchover (unimplemented), sql.instances.reencrypt (unimplemented), sql.projects.instances.rescheduleMaintenance (unimplemented)
func TestCloudSQLRefusedMethods(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	svc := sqlAdminService(t, h)
	p, i := h.Project(), "postgres"
	loc := "projects/" + p + "/locations/us-central1"
	bg := loc + "/blueGreenDeployments/bg"
	for _, c := range []struct {
		what, why string
		call      func() error
	}{
		{"blueGreenDeployments.create", "Google-managed networking", func() error {
			_, err := svc.BlueGreenDeployments.Create(loc, &sqladmin.BlueGreenDeployment{}).Do()
			return err
		}},
		{"blueGreenDeployments.delete", "Google-managed networking", func() error { _, err := svc.BlueGreenDeployments.Delete(bg).Do(); return err }},
		{"blueGreenDeployments.get", "Google-managed networking", func() error { _, err := svc.BlueGreenDeployments.Get(bg).Do(); return err }},
		{"blueGreenDeployments.list", "Google-managed networking", func() error { _, err := svc.BlueGreenDeployments.List(loc).Do(); return err }},
		{"blueGreenDeployments.switchover", "Google-managed networking", func() error {
			_, err := svc.BlueGreenDeployments.Switchover(bg, &sqladmin.SwitchoverBlueGreenDeploymentRequest{}).Do()
			return err
		}},
		{"connect.resolve", "Cloud DNS", func() error { _, err := svc.Connect.Resolve("us-central1", "db.example.com").Do(); return err }},
		{"ListEntraIdCertificates", "SQL Server", func() error { _, err := svc.Instances.ListEntraIdCertificates(p, i).Do(); return err }},
		{"addEntraIdCertificate", "SQL Server", func() error { _, err := svc.Instances.AddEntraIdCertificate(p, i).Do(); return err }},
		{"RotateEntraIdCertificate", "SQL Server", func() error {
			_, err := svc.Instances.RotateEntraIdCertificate(p, i, &sqladmin.InstancesRotateEntraIdCertificateRequest{}).Do()
			return err
		}},
		{"ListServerCertificates", "Certificate Authority Service", func() error { _, err := svc.Instances.ListServerCertificates(p, i).Do(); return err }},
		{"addServerCertificate", "Certificate Authority Service", func() error { _, err := svc.Instances.AddServerCertificate(p, i).Do(); return err }},
		{"RotateServerCertificate", "Certificate Authority Service", func() error {
			_, err := svc.Instances.RotateServerCertificate(p, i, &sqladmin.InstancesRotateServerCertificateRequest{}).Do()
			return err
		}},
		{"acquireSsrsLease", "SQL Server", func() error {
			_, err := svc.Instances.AcquireSsrsLease(p, i, &sqladmin.InstancesAcquireSsrsLeaseRequest{}).Do()
			return err
		}},
		{"releaseSsrsLease", "SQL Server", func() error { _, err := svc.Instances.ReleaseSsrsLease(p, i).Do(); return err }},
		{"switchover", "second region", func() error { _, err := svc.Instances.Switchover(p, i).Do(); return err }},
		{"reencrypt", "Cloud KMS", func() error {
			_, err := svc.Instances.Reencrypt(p, i, &sqladmin.InstancesReencryptRequest{}).Do()
			return err
		}},
		{"rescheduleMaintenance", "maintenance", func() error {
			_, err := svc.Projects.Instances.RescheduleMaintenance(p, i, &sqladmin.SqlInstancesRescheduleMaintenanceRequestBody{}).Do()
			return err
		}},
	} {
		wantCode(t, c.what, c.call(), 501, c.why)
	}
}
