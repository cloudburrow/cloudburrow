//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/logging/logadmin"
	backupdr "google.golang.org/api/backupdr/v1"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/structpb"
)

// TestBackupDRRestoreToANewInstance (#1922): a vaulted Cloud SQL backup is
// restored, through the official sqladmin/v1 client, into an instance that
// does not exist yet: instances.restoreBackup with backupdrBackup on the new
// instance's name creates it with the backup's settings (Google's "Restore
// to a new instance ... creates a new instance based on the configuration
// stored in your backup") and loads the backup's rows, which a database
// client reads through the official connector; the source is untouched.
// restoreInstanceSettings cannot change the major database version
// ("You can't change the instance's major database version"), and a backup
// run (restoreBackupContext) still restores only into an instance that
// exists; restoreInstanceSettings naming another instance and a backup
// that does not exist are refused. The new instance carries
// restoreInstanceSettings' labels over the backup's settings. Then
// `gcloud sql backups restore NAME --restore-instance NEW --region R`,
// Google's documented command, does the same, and Terraform's
// google_sql_database_instance with backupdr_backup brings the rows into
// the instance it makes. Last (#1924), the official Logging client reads
// the restore job in bdr_backup_restore_jobs, RUNNING then SUCCESSFUL with
// one jobId, its backupName and restoreResourceName, and, once the source
// instance is deleted, its failed on-demand backup job: FAILED, errorType
// NOT_FOUND, errorCode 5 and the error message.
// covers: sql.instances.restoreBackup, sql.instances.get, sql.instances.insert, sql.instances.delete, backupdr.projects.locations.backupPlanAssociations.triggerBackup, backupdr.projects.locations.backupVaults.dataSources.backups.list, google.logging.v2.LoggingServiceV2/ListLogEntries
func TestBackupDRRestoreToANewInstance(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvBackupDR) == "" {
		t.Skip("needs --services backupdr")
	}
	var c sqlBackupCase
	var bc *sqladmin.BackupConfiguration
	otherMajor := ""
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)
	switch {
	case os.Getenv(EnvCloudSQL) != "":
		h.Endpoint(EnvCloudSQL)
		c = sqlBackupCase{name: "postgres", version: "POSTGRES_17", instance: "bdrn-pg-" + suffix, created: true,
			table: "bdr_rows", dsn: func(_, conn string) string {
				return fmt.Sprintf("host=%s user=postgres dbname=postgres sslmode=disable", conn)
			}, register: pgRegister}
		bc, otherMajor = &sqladmin.BackupConfiguration{Enabled: true, StartTime: "03:00"}, "POSTGRES_16"
	case os.Getenv(EnvMySQL) != "":
		h.Endpoint(EnvMySQL)
		c = sqlBackupCase{name: "mysql", version: "MYSQL_8_4", instance: "bdrn-my-" + suffix, created: true,
			table: "bdr.bdr_rows", setup: "CREATE DATABASE IF NOT EXISTS bdr", dsn: func(drv, conn string) string {
				return fmt.Sprintf("root:Root-pass1@%s(%s)/mysql", drv, conn)
			}, register: mysqlRegister}
		bc, otherMajor = &sqladmin.BackupConfiguration{Enabled: true, StartTime: "03:00"}, "MYSQL_8_0"
	default:
		t.Skip("needs --services cloudsql or cloudsql-mysql")
	}
	bdr := backupDRService(t, h)
	wait := backupDRWaiter(t, bdr)
	svc := sqlAdminService(t, h)
	project := h.Project()
	loc := "projects/" + project + "/locations/us-central1"

	vaultName := loc + "/backupVaults/nvault-" + suffix
	wait(bdr.Projects.Locations.BackupVaults.Create(loc, &backupdr.BackupVault{BackupMinimumEnforcedRetentionDuration: "86400s"}).
		BackupVaultId("nvault-" + suffix).Do())
	defer func() {
		if op, err := bdr.Projects.Locations.BackupVaults.Delete(vaultName).Force(true).IgnoreBackupPlanReferences(true).Do(); err == nil {
			wait(op, nil)
		}
	}()
	planName := loc + "/backupPlans/nplan-" + suffix
	wait(bdr.Projects.Locations.BackupPlans.Create(loc, &backupdr.BackupPlan{BackupVault: vaultName,
		ResourceType: "sqladmin.googleapis.com/Instance", BackupRules: dailyRule()}).BackupPlanId("nplan-" + suffix).Do())
	defer func() {
		if op, err := bdr.Projects.Locations.BackupPlans.Delete(planName).Do(); err == nil {
			wait(op, nil)
		}
	}()

	deleteInstance := func(name string) {
		if d, err := svc.Instances.Delete(project, name).Do(); err == nil {
			waitSQLOp(t, svc, project, d)
		}
	}
	op, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: c.instance, DatabaseVersion: c.version,
		RootPassword: "Root-pass1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", BackupConfiguration: bc}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	defer deleteInstance(c.instance)
	waitSQLOp(t, svc, project, op)
	conn := project + ":us-central1:" + c.instance
	var stmts []string
	if c.setup != "" {
		stmts = append(stmts, c.setup)
	}
	stmts = append(stmts, "CREATE TABLE "+c.table+" (id INT PRIMARY KEY, name VARCHAR(20))",
		"INSERT INTO "+c.table+" VALUES (1, 'a'), (2, 'b')")
	backedUp := []string{"1=a", "2=b"}
	if got := sqlBackupRows(t, h, c, conn, stmts...); !reflect.DeepEqual(got, backedUp) {
		t.Fatalf("rows written: %v", got)
	}

	assocName := loc + "/backupPlanAssociations/nassoc-" + suffix
	wait(bdr.Projects.Locations.BackupPlanAssociations.Create(loc, &backupdr.BackupPlanAssociation{BackupPlan: planName,
		Resource: "projects/" + project + "/instances/" + c.instance, ResourceType: "sqladmin.googleapis.com/Instance"}).
		BackupPlanAssociationId("nassoc-" + suffix).Do())
	defer func() {
		if op, err := bdr.Projects.Locations.BackupPlanAssociations.Delete(assocName).Do(); err == nil {
			wait(op, nil)
		}
	}()
	assoc, err := bdr.Projects.Locations.BackupPlanAssociations.Get(assocName).Do()
	if err != nil {
		t.Fatal(err)
	}
	wait(bdr.Projects.Locations.BackupPlanAssociations.TriggerBackup(assocName, &backupdr.TriggerBackupRequest{RuleId: "daily"}).Do())
	backups, err := bdr.Projects.Locations.BackupVaults.DataSources.Backups.List(assoc.DataSource).Do()
	if err != nil || len(backups.Backups) != 1 || backups.Backups[0].State != "ACTIVE" {
		t.Fatalf("backups.list: %v %+v", err, backups)
	}
	bk := backups.Backups[0].Name
	after := []string{"1=a", "2=y"}
	if got := sqlBackupRows(t, h, c, conn, "UPDATE "+c.table+" SET name = 'y' WHERE id = 2"); !reflect.DeepEqual(got, after) {
		t.Fatalf("rows changed: %v", got)
	}

	// Refusals: a major version change, and a backup run into a missing
	// instance.
	_, err = svc.Instances.RestoreBackup(project, c.instance+"-x", &sqladmin.InstancesRestoreBackupRequest{BackupdrBackup: bk,
		RestoreInstanceSettings: &sqladmin.DatabaseInstance{DatabaseVersion: otherMajor}}).Do()
	wantBackupDRCode(t, "a restore that changes the major version", err, http.StatusBadRequest, "major database version")
	_, err = svc.Instances.RestoreBackup(project, c.instance+"-x", &sqladmin.InstancesRestoreBackupRequest{
		RestoreBackupContext: &sqladmin.RestoreBackupContext{BackupRunId: 1, InstanceId: c.instance}}).Do()
	wantBackupDRCode(t, "a backup run into a missing instance", err, http.StatusNotFound, "does not exist")
	if _, err := svc.Instances.Get(project, c.instance+"-x").Do(); err == nil {
		t.Errorf("a refused restore left the instance %s-x", c.instance)
	}

	_, err = svc.Instances.RestoreBackup(project, c.instance+"-x", &sqladmin.InstancesRestoreBackupRequest{BackupdrBackup: bk,
		RestoreInstanceSettings: &sqladmin.DatabaseInstance{Name: c.instance + "-y"}}).Do()
	wantBackupDRCode(t, "restoreInstanceSettings naming another instance", err, http.StatusBadRequest, "is not the instance restored to")
	_, err = svc.Instances.RestoreBackup(project, c.instance+"-x", &sqladmin.InstancesRestoreBackupRequest{BackupdrBackup: bk + "0"}).Do()
	wantBackupDRCode(t, "a backup that does not exist", err, http.StatusNotFound, "")

	// Restore to a new instance, through the client, with the backup's
	// settings and restoreInstanceSettings' labels over them.
	restoreStart := time.Now()
	target := c.instance + "-new"
	rop, err := svc.Instances.RestoreBackup(project, target, &sqladmin.InstancesRestoreBackupRequest{BackupdrBackup: bk,
		RestoreInstanceSettings: &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{UserLabels: map[string]string{"restored": "bdr"}}}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	defer deleteInstance(target)
	waitSQLOp(t, svc, project, rop)
	if rop, err = svc.Operations.Get(project, rop.Name).Do(); err != nil || rop.Error != nil || rop.OperationType != "RESTORE_VOLUME" {
		t.Fatalf("restoreBackup's operation: %v %+v", err, rop)
	}
	in, err := svc.Instances.Get(project, target).Do()
	if err != nil || in.State != "RUNNABLE" || in.DatabaseVersion != c.version || in.Settings == nil || in.Settings.Tier != "db-custom-1-3840" ||
		in.Settings.UserLabels["restored"] != "bdr" {
		t.Fatalf("the new instance: %v %+v", err, in)
	}
	if rows := sqlBackupRows(t, h, c, project+":us-central1:"+target); !reflect.DeepEqual(rows, backedUp) {
		t.Errorf("rows in the new instance: %v, want %v", rows, backedUp)
	}
	if rows := sqlBackupRows(t, h, c, conn); !reflect.DeepEqual(rows, after) {
		t.Errorf("rows in the source after the restore: %v, want %v", rows, after)
	}

	t.Run("gcloud", func(t *testing.T) {
		h := h.For(t)
		g := newGcloudSession(t, h)
		target := c.instance + "-g"
		// Into the test's project, where the client reads it and the
		// cleanup deletes it: gcloud's own default project is another.
		out, err := g.run(nil, "sql", "backups", "restore", bk, "--restore-instance", target, "--region", "us-central1",
			"--project", project, "--quiet")
		defer deleteInstance(target)
		if err != nil {
			t.Fatalf("gcloud sql backups restore: %v\n%s", err, out)
		}
		if !strings.Contains(out, "Restored") {
			t.Errorf("gcloud sql backups restore: %s", out)
		}
		if in, err := svc.Instances.Get(project, target).Do(); err != nil || in.State != "RUNNABLE" || in.Region != "us-central1" {
			t.Fatalf("the instance gcloud restored to: %v %+v", err, in)
		}
		if rows := sqlBackupRows(t, h, c, project+":us-central1:"+target); !reflect.DeepEqual(rows, backedUp) {
			t.Errorf("rows in the instance gcloud restored to: %v, want %v", rows, backedUp)
		}
	})

	// Terraform's google_sql_database_instance with backupdr_backup: the
	// provider (v8.5.0) inserts the instance with the resource's settings,
	// then restores the backup into it (instances.restoreBackup with
	// backupdrBackup), so the rows are the backup's.
	t.Run("terraform", func(t *testing.T) {
		h := h.For(t)
		m := newTFModule(t)
		target := c.instance + "-tf"
		defer deleteInstance(target)
		m.write(fmt.Sprintf(`
resource "google_sql_database_instance" "restored" {
  project             = %q
  name                = %q
  region              = "us-central1"
  database_version    = %q
  backupdr_backup     = %q
  deletion_protection = false
  settings {
    tier        = "db-custom-1-3840"
    user_labels = { restored = "terraform" }
  }
}
`, project, target, c.version, bk))
		m.must("init", "-input=false", "-no-color")
		m.apply()
		in, err := svc.Instances.Get(project, target).Do()
		if err != nil || in.State != "RUNNABLE" || in.Settings == nil || in.Settings.UserLabels["restored"] != "terraform" {
			t.Fatalf("the instance Terraform restored to: %v %+v", err, in)
		}
		if rows := sqlBackupRows(t, h, c, project+":us-central1:"+target); !reflect.DeepEqual(rows, backedUp) {
			t.Errorf("rows in the instance Terraform restored to: %v, want %v", rows, backedUp)
		}
		m.planClean()
		m.must("destroy", "-auto-approve", "-input=false", "-no-color")
		_, err = svc.Instances.Get(project, target).Do()
		wantHTTP(t, "instances.get after destroy", err, http.StatusNotFound)
	})

	// The restore job, and a failed backup job once the source instance is
	// gone, in Cloud Logging's bdr_backup_restore_jobs (#1924), read with
	// the official Logging client.
	t.Run("job logs", func(t *testing.T) {
		h := h.For(t)
		logAddr := shardEndpoint(h, envSQLLogging, EnvLogging)
		deleteInstance(c.instance)
		failStart := time.Now()
		_, err := bdr.Projects.Locations.BackupPlanAssociations.TriggerBackup(assocName, &backupdr.TriggerBackupRequest{RuleId: "daily"}).Do()
		if err == nil {
			t.Fatal("triggerBackup of a deleted instance succeeded")
		}
		jobs := func(since time.Time, match func(map[string]any) bool, enough int) []map[string]any {
			var out []map[string]any
			for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(300 * time.Millisecond) {
				out = out[:0]
				for _, p := range backupDRJobLogEntries(t, h, logAddr, project, "bdr_backup_restore_jobs", since) {
					if match(p) {
						out = append(out, p)
					}
				}
				if len(out) >= enough || time.Now().After(deadline) {
					return out
				}
			}
		}
		restores := jobs(restoreStart, func(p map[string]any) bool {
			return p["jobCategory"] == "RESTORE" && p["restoreResourceName"] == "projects/"+project+"/instances/"+target
		}, 2)
		status := map[string]string{}
		for _, p := range restores {
			s, _ := p["jobStatus"].(string)
			id, _ := p["jobId"].(string)
			status[s] = id
			if p["backupName"] != bk || p["dataSourceName"] != assoc.DataSource || p["sourceResourceName"] != "projects/"+project+"/instances/"+c.instance {
				t.Errorf("the restore job's entry: %v", p)
			}
		}
		if status["RUNNING"] == "" || status["RUNNING"] != status["SUCCESSFUL"] {
			t.Errorf("the restore to %s: %v; want one jobId RUNNING then SUCCESSFUL (%v)", target, status, restores)
		}
		failed := jobs(failStart, func(p map[string]any) bool {
			return p["jobStatus"] == "FAILED" && p["dataSourceName"] == assoc.DataSource
		}, 1)
		if len(failed) != 1 {
			t.Fatalf("failed backup jobs of %s: %v; want 1", assoc.DataSource, failed)
		}
		f := failed[0]
		if f["jobCategory"] != "ON_DEMAND_BACKUP" || f["backupRule"] != "daily" || f["errorType"] != "NOT_FOUND" || f["errorCode"] != float64(5) ||
			!strings.Contains(fmt.Sprint(f["errorMessage"]), c.instance) || f["endTime"] == nil {
			t.Errorf("the failed backup job's entry: %v", f)
		}
	})
}

// backupDRJobLogEntries are the jsonPayloads of project's Backup and DR job
// log entries in log (bdr_backup_restore_jobs or bdr_backup_plan_jobs)
// since since, read through the official Logging client at logAddr.
func backupDRJobLogEntries(t *testing.T, h *Harness, logAddr, project, log string, since time.Time) []map[string]any {
	t.Helper()
	if logAddr == "" {
		t.Fatal("no Cloud Logging endpoint")
	}
	ctx := h.Context()
	ac, err := logadmin.NewClient(ctx, "projects/"+project, option.WithEndpoint(logAddr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer ac.Close()
	filter := `logName = "projects/` + project + `/logs/backupdr.googleapis.com%2F` + log + `" AND timestamp >= "` +
		since.Add(-time.Minute).UTC().Format(time.RFC3339) + `"`
	var out []map[string]any
	it := ac.Entries(ctx, logadmin.Filter(filter))
	for {
		e, err := it.Next()
		if err == iterator.Done {
			return out
		}
		if err != nil {
			t.Fatalf("entries of %s: %v", log, err)
		}
		switch v := e.Payload.(type) {
		case map[string]any:
			out = append(out, v)
		case *structpb.Struct:
			out = append(out, v.AsMap())
		default:
			t.Fatalf("%s: payload %T", log, e.Payload)
		}
	}
}
