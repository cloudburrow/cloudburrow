//go:build compat

package compat

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	backupdr "google.golang.org/api/backupdr/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// EnvBackupDR is the host address of the Backup and DR Service API (#1433).
const EnvBackupDR = "CLOUDBURROW_TEST_BACKUPDR"

func backupDRService(t *testing.T, h *Harness) *backupdr.Service {
	t.Helper()
	svc, err := backupdr.NewService(h.Context(), option.WithEndpoint("http://"+h.Endpoint(EnvBackupDR)+"/"),
		option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// backupDRWaiter returns a function that polls a Backup and DR operation
// until it is done, and fails on its error (or the call's).
func backupDRWaiter(t *testing.T, svc *backupdr.Service) func(*backupdr.Operation, error) *backupdr.Operation {
	return func(op *backupdr.Operation, err error) *backupdr.Operation {
		t.Helper()
		return waitBackupDROp(t, svc, op, err)
	}
}

func waitBackupDROp(t *testing.T, svc *backupdr.Service, op *backupdr.Operation, err error) *backupdr.Operation {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Minute)
	for !op.Done {
		if time.Now().After(deadline) {
			t.Fatalf("operation %s is not done", op.Name)
		}
		time.Sleep(500 * time.Millisecond)
		if op, err = svc.Projects.Locations.Operations.Get(op.Name).Do(); err != nil {
			t.Fatal(err)
		}
	}
	if op.Error != nil {
		t.Fatalf("operation %s: %d %s", op.Name, op.Error.Code, op.Error.Message)
	}
	return op
}

func wantBackupDRCode(t *testing.T, what string, err error, code int, contains string) {
	t.Helper()
	var ge *googleapi.Error
	if !errors.As(err, &ge) || ge.Code != code || !strings.Contains(ge.Message, contains) {
		t.Errorf("%s: %v; want HTTP %d naming %q", what, err, code, contains)
	}
}

// TestBackupDRCloudSQLRestore (#1433, #1425): through the official
// backupdr/v1 and sqladmin/v1 clients, a backup vault, a backup plan with a
// rule and an association of a created Cloud SQL instance with point-in-time
// recovery make a data source in the vault; triggerBackup takes a backup of
// the instance's rows. More rows are written, the server's time noted
// between them, and:
//   - instances.restoreBackup with backupdrBackup brings the source back to
//     the backup's rows;
//   - instances.pointInTimeRestore with the data source creates a target
//     holding the rows of the noted time.
//
// Both are read with a database client through the official connector.
// backups.restore (Compute Engine and disk targets) and a plan for a
// Compute Engine instance are refused with the reason.
// covers: backupdr.projects.locations.backupVaults.create, backupdr.projects.locations.backupVaults.get, backupdr.projects.locations.backupVaults.list, backupdr.projects.locations.backupVaults.delete, backupdr.projects.locations.backupPlans.create, backupdr.projects.locations.backupPlans.get, backupdr.projects.locations.backupPlans.list, backupdr.projects.locations.backupPlans.delete, backupdr.projects.locations.backupPlanAssociations.create, backupdr.projects.locations.backupPlanAssociations.get, backupdr.projects.locations.backupPlanAssociations.list, backupdr.projects.locations.backupPlanAssociations.delete, backupdr.projects.locations.backupPlanAssociations.triggerBackup, backupdr.projects.locations.backupVaults.dataSources.list, backupdr.projects.locations.backupVaults.dataSources.get, backupdr.projects.locations.backupVaults.dataSources.backups.list, backupdr.projects.locations.backupVaults.dataSources.backups.get, backupdr.projects.locations.backupVaults.dataSources.backups.delete, backupdr.projects.locations.backupVaults.dataSources.backups.restore (unimplemented), backupdr.projects.locations.operations.get, backupdr.projects.locations.operations.list, backupdr.projects.locations.list, backupdr.projects.locations.get, sql.instances.restoreBackup, sql.instances.pointInTimeRestore, sql.instances.insert, sql.instances.get, sql.operations.get
func TestBackupDRCloudSQLRestore(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvBackupDR) == "" {
		t.Skip("needs --services backupdr")
	}
	if os.Getenv(EnvCloudSQL) == "" && os.Getenv(EnvMySQL) == "" {
		t.Skip("needs --services cloudsql or cloudsql-mysql")
	}
	bdr := backupDRService(t, h)
	wait := backupDRWaiter(t, bdr)
	svc := sqlAdminService(t, h)
	project := h.Project()
	loc := "projects/" + project + "/locations/us-central1"
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e6)

	locs, err := bdr.Projects.Locations.List("projects/" + project).Do()
	if err != nil || len(locs.Locations) == 0 {
		t.Fatalf("locations.list: %v %+v", err, locs)
	}
	if l, err := bdr.Projects.Locations.Get(loc).Do(); err != nil || l.LocationId != "us-central1" {
		t.Fatalf("locations.get: %v %+v", err, l)
	}

	vaultID := "vault-" + suffix
	wait(bdr.Projects.Locations.BackupVaults.Create(loc, &backupdr.BackupVault{
		BackupMinimumEnforcedRetentionDuration: "86400s", Description: "compat"}).BackupVaultId(vaultID).Do())
	vaultName := loc + "/backupVaults/" + vaultID
	defer func() {
		if op, err := bdr.Projects.Locations.BackupVaults.Delete(vaultName).Force(true).Do(); err == nil {
			wait(op, nil)
		}
	}()
	if v, err := bdr.Projects.Locations.BackupVaults.Get(vaultName).Do(); err != nil || v.State != "ACTIVE" {
		t.Fatalf("backupVaults.get: %v %+v", err, v)
	}
	if l, err := bdr.Projects.Locations.BackupVaults.List(loc).Do(); err != nil || !hasVault(l.BackupVaults, vaultName) {
		t.Errorf("backupVaults.list: %v", err)
	}
	if ops, err := bdr.Projects.Locations.Operations.List(loc).Do(); err != nil || len(ops.Operations) == 0 {
		t.Errorf("operations.list: %v", err)
	}

	_, err = bdr.Projects.Locations.BackupPlans.Create(loc, &backupdr.BackupPlan{BackupVault: vaultName,
		ResourceType: "compute.googleapis.com/Instance", BackupRules: dailyRule()}).BackupPlanId("gce-" + suffix).Do()
	wantBackupDRCode(t, "a plan for a Compute Engine instance", err, http.StatusNotImplemented, "Compute")

	planID := "plan-" + suffix
	wait(bdr.Projects.Locations.BackupPlans.Create(loc, &backupdr.BackupPlan{BackupVault: vaultName,
		ResourceType: "sqladmin.googleapis.com/Instance", BackupRules: dailyRule(), LogRetentionDays: 7}).BackupPlanId(planID).Do())
	planName := loc + "/backupPlans/" + planID
	defer func() {
		if op, err := bdr.Projects.Locations.BackupPlans.Delete(planName).Do(); err == nil {
			wait(op, nil)
		}
	}()
	if p, err := bdr.Projects.Locations.BackupPlans.Get(planName).Do(); err != nil || len(p.BackupRules) != 1 || p.BackupRules[0].RuleId != "daily" {
		t.Fatalf("backupPlans.get: %v %+v", err, p)
	}
	if l, err := bdr.Projects.Locations.BackupPlans.List(loc).Do(); err != nil || len(l.BackupPlans) == 0 {
		t.Errorf("backupPlans.list: %v", err)
	}

	type restoreCase struct {
		c  sqlBackupCase
		bc *sqladmin.BackupConfiguration
	}
	var cases []restoreCase
	if os.Getenv(EnvCloudSQL) != "" {
		cases = append(cases, restoreCase{sqlBackupCase{name: "postgres", version: "POSTGRES_17", instance: "bdr-pg-" + suffix, created: true,
			table: "bdr_rows", dsn: func(_, conn string) string {
				return fmt.Sprintf("host=%s user=postgres dbname=postgres sslmode=disable", conn)
			}, register: pgRegister}, &sqladmin.BackupConfiguration{Enabled: true, StartTime: "03:00", PointInTimeRecoveryEnabled: true}})
	}
	if os.Getenv(EnvMySQL) != "" {
		cases = append(cases, restoreCase{sqlBackupCase{name: "mysql", version: "MYSQL_8_4", instance: "bdr-my-" + suffix, created: true,
			table: "bdr.bdr_rows", setup: "CREATE DATABASE IF NOT EXISTS bdr", dsn: func(drv, conn string) string {
				return fmt.Sprintf("root:Root-pass1@%s(%s)/mysql", drv, conn)
			}, register: mysqlRegister}, &sqladmin.BackupConfiguration{Enabled: true, StartTime: "03:00", BinaryLogEnabled: true}})
	}
	for _, rc := range cases {
		t.Run(rc.c.name, func(t *testing.T) {
			h := h.For(t)
			backupDRRestoreCase(t, h, bdr, svc, loc, vaultName, planName, rc.c, rc.bc)
		})
	}
}

// backupDRRestoreCase enrolls a new instance of c's engine in the plan,
// backs it up, and restores it both ways.
func backupDRRestoreCase(t *testing.T, h *Harness, bdr *backupdr.Service, svc *sqladmin.Service, loc, vaultName, planName string,
	c sqlBackupCase, bc *sqladmin.BackupConfiguration) {
	wait := backupDRWaiter(t, bdr)
	project := h.Project()
	assocID := "assoc-" + c.instance
	op, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: c.instance, DatabaseVersion: c.version,
		RootPassword: "Root-pass1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", BackupConfiguration: bc}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if d, err := svc.Instances.Delete(project, c.instance).Do(); err == nil {
			waitSQLOp(t, svc, project, d)
		}
	}()
	waitSQLOp(t, svc, project, op)
	waitRecoverable(t, svc, project, c.instance)
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

	aop := wait(bdr.Projects.Locations.BackupPlanAssociations.Create(loc, &backupdr.BackupPlanAssociation{
		BackupPlan: planName, Resource: "projects/" + project + "/instances/" + c.instance,
		ResourceType: "sqladmin.googleapis.com/Instance"}).BackupPlanAssociationId(assocID).Do())
	if aop.Response == nil {
		t.Errorf("the association's operation has no response")
	}
	assocName := loc + "/backupPlanAssociations/" + assocID
	defer func() {
		if op, err := bdr.Projects.Locations.BackupPlanAssociations.Delete(assocName).Do(); err == nil {
			wait(op, nil)
		}
	}()
	assoc, err := bdr.Projects.Locations.BackupPlanAssociations.Get(assocName).Do()
	if err != nil || assoc.DataSource == "" || !strings.HasPrefix(assoc.DataSource, vaultName+"/dataSources/") {
		t.Fatalf("backupPlanAssociations.get: %v %+v", err, assoc)
	}
	if l, err := bdr.Projects.Locations.BackupPlanAssociations.List(loc).Do(); err != nil || len(l.BackupPlanAssociations) == 0 {
		t.Errorf("backupPlanAssociations.list: %v", err)
	}
	ds, err := bdr.Projects.Locations.BackupVaults.DataSources.Get(assoc.DataSource).Do()
	if err != nil || ds.DataSourceGcpResource == nil || ds.DataSourceGcpResource.CloudSqlInstanceDatasourceProperties == nil ||
		ds.DataSourceGcpResource.CloudSqlInstanceDatasourceProperties.Name != "projects/"+project+"/instances/"+c.instance {
		t.Fatalf("dataSources.get: %v %+v", err, ds)
	}
	if l, err := bdr.Projects.Locations.BackupVaults.DataSources.List(vaultName).Do(); err != nil || !hasDataSource(l.DataSources, assoc.DataSource) {
		t.Errorf("dataSources.list: %v %+v", err, l)
	}

	wait(bdr.Projects.Locations.BackupPlanAssociations.TriggerBackup(assocName,
		&backupdr.TriggerBackupRequest{RuleId: "daily"}).Do())
	backups, err := bdr.Projects.Locations.BackupVaults.DataSources.Backups.List(assoc.DataSource).Do()
	if err != nil || len(backups.Backups) != 1 {
		t.Fatalf("backups.list: %v %+v", err, backups)
	}
	bk, err := bdr.Projects.Locations.BackupVaults.DataSources.Backups.Get(backups.Backups[0].Name).Do()
	if err != nil || bk.State != "ACTIVE" || bk.CloudSqlInstanceBackupProperties == nil || bk.ConsistencyTime == "" {
		t.Fatalf("backups.get: %v %+v", err, bk)
	}
	// The vault's backup is Backup and DR's: Cloud SQL's backupRuns do not list it.
	if runs, err := svc.BackupRuns.List(project, c.instance).Do(); err != nil || len(runs.Items) != 0 {
		t.Errorf("backupRuns.list shows the Backup and DR backup: %v %+v", err, runs)
	}

	_, err = bdr.Projects.Locations.BackupVaults.DataSources.Backups.Restore(bk.Name, &backupdr.RestoreBackupRequest{
		ComputeInstanceTargetEnvironment: &backupdr.ComputeInstanceTargetEnvironment{Project: project, Zone: "us-central1-a"}}).Do()
	wantBackupDRCode(t, "backups.restore into a Compute Engine instance", err, http.StatusNotImplemented, "backupdrBackup")

	if got := sqlBackupRows(t, h, c, conn, "UPDATE "+c.table+" SET name = 'y' WHERE id = 2",
		"INSERT INTO "+c.table+" VALUES (3, 'c')"); !reflect.DeepEqual(got, []string{"1=a", "2=y", "3=c"}) {
		t.Fatalf("rows changed: %v", got)
	}
	at := sqlServerTime(t, h, c, conn)
	atRows := []string{"1=a", "2=y", "3=c"}
	if got := sqlBackupRows(t, h, c, conn, "DELETE FROM "+c.table+" WHERE id = 1"); !reflect.DeepEqual(got, []string{"2=y", "3=c"}) {
		t.Fatalf("rows deleted: %v", got)
	}

	// instances.pointInTimeRestore from the data source, to the noted time.
	target := c.instance + "-pit"
	pop, err := svc.Instances.PointInTimeRestore("projects/"+project, &sqladmin.PointInTimeRestoreContext{
		Datasource: assoc.DataSource, TargetInstance: target, PointInTime: at.Format(time.RFC3339Nano)}).Do()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if d, err := svc.Instances.Delete(project, target).Do(); err == nil {
			waitSQLOp(t, svc, project, d)
		}
	}()
	waitSQLOp(t, svc, project, pop)
	if pop, err = svc.Operations.Get(project, pop.Name).Do(); err != nil || pop.Error != nil {
		t.Fatalf("pointInTimeRestore's operation: %v %+v", err, pop.Error)
	}
	if got, err := svc.Instances.Get(project, target).Do(); err != nil || got.State != "RUNNABLE" {
		t.Fatalf("the restored target: %v %+v", err, got)
	}
	if rows := sqlBackupRows(t, h, c, project+":us-central1:"+target); !reflect.DeepEqual(rows, atRows) {
		t.Errorf("rows in the target restored to %s: %v, want %v", at.Format(time.RFC3339Nano), rows, atRows)
	}

	// instances.restoreBackup with backupdrBackup, into the source.
	rop, err := svc.Instances.RestoreBackup(project, c.instance, &sqladmin.InstancesRestoreBackupRequest{BackupdrBackup: bk.Name}).Do()
	if err != nil {
		t.Fatal(err)
	}
	waitSQLOp(t, svc, project, rop)
	if rop, err = svc.Operations.Get(project, rop.Name).Do(); err != nil || rop.Error != nil {
		t.Fatalf("restoreBackup's operation: %v %+v", err, rop.Error)
	}
	if rows := sqlBackupRows(t, h, c, conn); !reflect.DeepEqual(rows, backedUp) {
		t.Errorf("rows after restoring the Backup and DR backup: %v, want %v", rows, backedUp)
	}

	// The plan cannot go while an association applies it.
	_, err = bdr.Projects.Locations.BackupPlans.Delete(planName).Do()
	wantBackupDRCode(t, "deleting a plan in use", err, http.StatusBadRequest, assocName)
	wait(bdr.Projects.Locations.BackupVaults.DataSources.Backups.Delete(bk.Name).Do())
	if _, err := bdr.Projects.Locations.BackupVaults.DataSources.Backups.Get(bk.Name).Do(); err == nil {
		t.Errorf("backups.get after delete: no error")
	}
}

func hasVault(vs []*backupdr.BackupVault, name string) bool {
	for _, v := range vs {
		if v.Name == name {
			return true
		}
	}
	return false
}

func dailyRule() []*backupdr.BackupRule {
	return []*backupdr.BackupRule{{RuleId: "daily", BackupRetentionDays: 7, StandardSchedule: &backupdr.StandardSchedule{
		RecurrenceType: "DAILY", TimeZone: "UTC", BackupWindow: &backupdr.BackupWindow{StartHourOfDay: 1, EndHourOfDay: 5}}}}
}

func hasDataSource(ds []*backupdr.DataSource, name string) bool {
	for _, d := range ds {
		if d.Name == name {
			return true
		}
	}
	return false
}
