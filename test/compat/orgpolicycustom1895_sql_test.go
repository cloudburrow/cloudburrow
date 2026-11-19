//go:build compat

package compat

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/googleapi"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// covers: sql.instances.insert, sql.instances.patch, sql.backupRuns.insert
//
// TestOrgPolicyCustomConstraintsCloudSQL (#1895), with the official Cloud
// SQL Admin client (google.golang.org/api/sqladmin/v1): a DENY on
// sqladmin.googleapis.com/Instance, resource.settings.dataDiskSizeGb >
// 100, CREATE and UPDATE, refuses an insert of a 200 GB instance, 412
// "Operation denied by custom org policies: [...]"; a 20 GB one is
// created, and a patch to 500 GB refused, a patch of its labels made; a
// DENY on sqladmin.googleapis.com/BackupRun's description refuses an
// on-demand backup.
func TestOrgPolicyCustomConstraintsCloudSQL(t *testing.T) {
	h := New(t)
	shardEndpoint(h, envSQLResourceManager, EnvResourceManager)
	svc := sqlAdminService(t, h)
	c := orgPolicyClients(t, h)["grpc"]
	_, _, pc := foldersClientsAt(t, h, rmOptionsAt(h.Endpoint(EnvResourceManager)))
	project := orgPolicyProject(t, h, pc, localOrg, "ocs-")
	disk := customName(h, "sqldisk")
	ddesc := "Cloud SQL disks of 100 GB at most."
	adhoc := customName(h, "sqladhoc")
	adesc := "No ad hoc backups."
	createCustom(t, h, c, disk, "sqladmin.googleapis.com/Instance", "resource.settings.dataDiskSizeGb > 100", ccDeny, ddesc, ccCreate, ccUpdate)
	createCustom(t, h, c, adhoc, "sqladmin.googleapis.com/BackupRun", "resource.description.startsWith('adhoc')", ccDeny, adesc, ccCreate)
	setOrgPolicy(t, h, c, "projects/"+project, disk, opEnforce())
	setOrgPolicy(t, h, c, "projects/"+project, adhoc, opEnforce())
	want412 := func(what string, err error, msg string) {
		t.Helper()
		var ge *googleapi.Error
		if !errors.As(err, &ge) || ge.Code != 412 || ge.Message != msg {
			t.Errorf("%s = %v, want 412 %s", what, err, msg)
		}
	}
	name := fmt.Sprintf("cc-pg-%d", time.Now().UnixNano()%1e6)
	_, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: name + "-big", DatabaseVersion: "POSTGRES_17",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", DataDiskSizeGb: 200}}).Do()
	want412("a 200 GB instance", err, customMessage(disk, ddesc))
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("an instance that is created needs --services cloudsql")
	}
	op, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "POSTGRES_17",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", DataDiskSizeGb: 20}}).Do()
	if err != nil {
		t.Fatalf("a 20 GB instance: %v", err)
	}
	defer func() {
		if d, err := svc.Instances.Delete(project, name).Do(); err == nil {
			waitSQLOp(t, svc, project, d)
		}
	}()
	waitSQLOp(t, svc, project, op)
	_, err = svc.Instances.Patch(project, name, &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{DataDiskSizeGb: 500}}).Do()
	want412("a patch to 500 GB", err, customMessage(disk, ddesc))
	op, err = svc.Instances.Patch(project, name, &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{UserLabels: map[string]string{"team": "a"}}}).Do()
	if err != nil {
		t.Errorf("a patch of the labels: %v", err)
	} else {
		waitSQLOp(t, svc, project, op)
	}
	_, err = svc.BackupRuns.Insert(project, name, &sqladmin.BackupRun{Description: "adhoc 1"}).Do()
	var ge *googleapi.Error
	if errors.As(err, &ge) && ge.Code == 501 && strings.Contains(ge.Message, "backup") {
		return // backups are not enabled on this instance
	}
	want412("an ad hoc backup", err, customMessage(adhoc, adesc))
}
