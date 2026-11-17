//go:build compat

package compat

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestCloudSQLBlueGreenDeployment (#1614): through the official Go client,
// as Cloud SQL documents blue-green deployments for MySQL
// (docs.cloud.google.com/sql/docs/mysql/about-blue-green-deployments): a
// MySQL 8.0 instance without binary logging, and one with a read replica,
// are refused; one with both is the blue instance of a deployment whose
// requestedConfig upgrades its green instance, BLUE-green-XXXXXXXX, to
// MySQL 8.4. The deployment is SWITCHOVER_READY once the green instance
// replicates; stopping its replication makes it SWITCHOVER_NOT_READY with
// errorDetail, and a switchover is refused until replication starts again.
// A row written on the blue instance before the switchover is on the green
// one; after it the blue instance's name, through the same connection
// name and the official connector, reaches MySQL 8.4 with every row and
// takes writes, the green name the old 8.0 server, a standalone read and
// write instance; the deployment is SWITCHOVER_COMPLETED, and delete with
// deleteOldSource deletes the old blue server, leaving the blue instance.
// covers: sql.blueGreenDeployments.create, sql.blueGreenDeployments.get, sql.blueGreenDeployments.list, sql.blueGreenDeployments.switchover, sql.blueGreenDeployments.delete, sql.instances.insert, sql.instances.get, sql.instances.stopReplica, sql.instances.startReplica, sql.operations.get, sql.connect.get, sql.connect.generateEphemeral
func TestCloudSQLBlueGreenDeployment(t *testing.T) {
	h := New(t)
	svc := sqlAdminService(t, h)
	if os.Getenv(EnvMySQL) == "" {
		t.Skip("needs --services cloudsql-mysql")
	}
	project := h.Project()
	e := sqlEngine{mysql: true}
	n := time.Now().UnixNano() % 1e6
	blue, plain := fmt.Sprintf("bg-blue-%d", n), fmt.Sprintf("bg-plain-%d", n)
	loc := "projects/" + project + "/locations/us-central1"
	create := func(id, src, version string) (*sqladmin.Operation, error) {
		d := &sqladmin.BlueGreenDeployment{SourceInstance: "projects/" + project + "/instances/" + src, Description: "upgrade " + src}
		if version != "" {
			d.RequestedConfig = &sqladmin.RequestedConfig{DatabaseVersion: version}
		}
		return svc.BlueGreenDeployments.Create(loc, d).BlueGreenDeploymentId(id).Do()
	}

	// Binary logging is required.
	createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: plain, DatabaseVersion: "MYSQL_8_0", RootPassword: "Root-pass1",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", BackupConfiguration: &sqladmin.BackupConfiguration{Enabled: true}}})
	_, err := create("bg-nolog", plain, "")
	wantCode(t, "a source without binary logging", err, 400, "binary logging")

	createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: blue, DatabaseVersion: "MYSQL_8_0", RootPassword: "Root-pass1",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840",
			BackupConfiguration: &sqladmin.BackupConfiguration{Enabled: true, BinaryLogEnabled: true}}})
	bdb := e.root(t, h, project+":us-central1:"+blue)
	for _, q := range []string{"CREATE DATABASE shop", "CREATE TABLE shop.orders (id int primary key, item varchar(40))",
		"INSERT INTO shop.orders VALUES (1, 'before the deployment')"} {
		q := q
		eventually(t, q, func() error { _, err := bdb.Exec(q); return err })
	}
	_, err = create("bg-pg", blue, "POSTGRES_17")
	wantCode(t, "an upgrade to another engine", err, 400, "MySQL 8.0 to 8.4")
	_, err = create("Bad_ID", blue, "")
	wantCode(t, "an invalid deployment ID", err, 400, "blueGreenDeploymentId")
	_, err = svc.BlueGreenDeployments.Create("projects/"+project+"/locations/europe-west1",
		&sqladmin.BlueGreenDeployment{SourceInstance: blue}).BlueGreenDeploymentId("bg-region").Do()
	wantCode(t, "a deployment in another region", err, 400, "region")

	id := fmt.Sprintf("bg-%d", n)
	name := loc + "/blueGreenDeployments/" + id
	op, err := create(id, blue, "MYSQL_8_4")
	if err != nil {
		t.Fatalf("create the deployment: %v", err)
	}
	if op.OperationType != "CREATE_BLUE_GREEN_DEPLOYMENT" {
		t.Errorf("create's operation type %s", op.OperationType)
	}
	if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
		t.Fatalf("create the deployment: %+v", op.Error.Errors[0])
	}
	d, err := svc.BlueGreenDeployments.Get(name).View("DETAILED").Do()
	if err != nil {
		t.Fatal(err)
	}
	green := strings.TrimPrefix(d.SwitchoverTargetInstance, "projects/"+project+"/instances/")
	if !strings.HasPrefix(green, blue+"-green-") || len(green) != len(blue)+15 || d.SourceInstance != "projects/"+project+"/instances/"+blue ||
		d.RequestedConfig == nil || d.RequestedConfig.DatabaseVersion != "MYSQL_8_4" || d.Description != "upgrade "+blue ||
		len(d.DeploymentMappings) != 1 || d.DeploymentMappings[0].State != "UPGRADED" {
		t.Fatalf("the deployment: %+v", d)
	}
	t.Cleanup(func() {
		if d, err := svc.Instances.Delete(project, green).Do(); err == nil {
			waitSQLOpDone(t, svc, project, d)
		}
	})
	if diffs := d.DeploymentMappings[0].Diffs; len(diffs) == 0 || diffs[0].Field != "database_version" || diffs[0].TargetValue != "MYSQL_8_4" {
		t.Errorf("the DETAILED view's diffs: %+v", diffs)
	}
	state := func(want, detail string) {
		t.Helper()
		var d *sqladmin.BlueGreenDeployment
		eventuallyTrue(t, "the deployment is "+want, func() bool {
			d, err = svc.BlueGreenDeployments.Get(name).Do()
			return err == nil && d.State == want && strings.Contains(d.ErrorDetail, detail)
		})
	}
	state("SWITCHOVER_READY", "")
	gi, err := svc.Instances.Get(project, green).Do()
	if err != nil {
		t.Fatal(err)
	}
	if gi.DatabaseVersion != "MYSQL_8_4" || gi.MasterInstanceName != project+":"+blue || gi.DeploymentInfo == nil ||
		gi.DeploymentInfo.DeploymentId != id || gi.DeploymentInfo.State != "PRE_SWITCHOVER" || gi.DeploymentInfo.Target == nil {
		t.Errorf("the green instance before the switchover: version %s master %s info %+v", gi.DatabaseVersion, gi.MasterInstanceName, gi.DeploymentInfo)
	}
	list, err := svc.BlueGreenDeployments.List(loc).Do()
	if err != nil || len(list.BlueGreenDeployments) == 0 {
		t.Fatalf("list: %v %+v", err, list)
	}

	// Broken replication: not ready, and no switchover.
	if op, err := svc.Instances.StopReplica(project, green).Do(); err != nil {
		t.Fatal(err)
	} else {
		waitSQLOpDone(t, svc, project, op)
	}
	state("SWITCHOVER_NOT_READY", "stopped")
	_, err = svc.BlueGreenDeployments.Switchover(name, &sqladmin.SwitchoverBlueGreenDeploymentRequest{}).Do()
	wantCode(t, "a switchover while not ready", err, 400, "SWITCHOVER_NOT_READY")
	if op, err := svc.Instances.StartReplica(project, green).Do(); err != nil {
		t.Fatal(err)
	} else {
		waitSQLOpDone(t, svc, project, op)
	}
	state("SWITCHOVER_READY", "")

	if _, err := bdb.Exec("INSERT INTO shop.orders VALUES (2, 'before the switchover')"); err != nil {
		t.Fatal(err)
	}
	gdb := e.root(t, h, project+":us-central1:"+green)
	eventually(t, "the blue row on the green instance", func() error {
		var item string
		return gdb.QueryRow("SELECT item FROM shop.orders WHERE id = 2").Scan(&item)
	})

	op, err = svc.BlueGreenDeployments.Switchover(name, &sqladmin.SwitchoverBlueGreenDeploymentRequest{}).Do()
	if err != nil {
		t.Fatalf("switchover: %v", err)
	}
	if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
		t.Fatalf("switchover: %+v", op.Error.Errors[0])
	}
	state("SWITCHOVER_COMPLETED", "")
	bi, err := svc.Instances.Get(project, blue).Do()
	if err != nil {
		t.Fatal(err)
	}
	if bi.DatabaseVersion != "MYSQL_8_4" || bi.MasterInstanceName != "" || bi.InstanceType != "CLOUD_SQL_INSTANCE" ||
		bi.ConnectionName != project+":us-central1:"+blue || bi.DeploymentInfo == nil || bi.DeploymentInfo.State != "POST_SWITCHOVER" {
		t.Errorf("the blue name after the switchover: version %s master %q type %s conn %s info %+v", bi.DatabaseVersion,
			bi.MasterInstanceName, bi.InstanceType, bi.ConnectionName, bi.DeploymentInfo)
	}
	gi, err = svc.Instances.Get(project, green).Do()
	if err != nil {
		t.Fatal(err)
	}
	if gi.DatabaseVersion != "MYSQL_8_0" || gi.MasterInstanceName != "" {
		t.Errorf("the green name after the switchover: version %s master %q", gi.DatabaseVersion, gi.MasterInstanceName)
	}
	// The same connection name now reaches the 8.4 server, with every row.
	nb := e.root(t, h, project+":us-central1:"+blue)
	eventually(t, "the blue name's server after the switchover", func() error {
		var v string
		var rows int
		if err := nb.QueryRow("SELECT version()").Scan(&v); err != nil {
			return err
		}
		if err := nb.QueryRow("SELECT count(*) FROM shop.orders").Scan(&rows); err != nil {
			return err
		}
		if !strings.HasPrefix(v, "8.4") || rows != 2 {
			return fmt.Errorf("version %s with %d rows, want 8.4 with 2", v, rows)
		}
		_, err := nb.Exec("INSERT INTO shop.orders VALUES (3, 'after the switchover')")
		return err
	})
	ng := e.root(t, h, project+":us-central1:"+green)
	eventually(t, "the old blue server under the green name", func() error {
		var v string
		if err := ng.QueryRow("SELECT version()").Scan(&v); err != nil {
			return err
		}
		if !strings.HasPrefix(v, "8.0") {
			return fmt.Errorf("version %s, want 8.0", v)
		}
		_, err := ng.Exec("INSERT INTO shop.orders VALUES (10, 'old blue takes writes')")
		return err
	})
	_, err = svc.BlueGreenDeployments.Switchover(name, &sqladmin.SwitchoverBlueGreenDeploymentRequest{}).Do()
	wantCode(t, "a second switchover", err, 400, "SWITCHOVER_COMPLETED")

	op, err = svc.BlueGreenDeployments.Delete(name).DeleteOldSource(true).Do()
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
		t.Fatalf("delete: %+v", op.Error.Errors[0])
	}
	_, err = svc.BlueGreenDeployments.Get(name).Do()
	wantCode(t, "the deleted deployment", err, 404, "does not exist")
	_, err = svc.Instances.Get(project, green).Do()
	wantCode(t, "the old blue server, deleted", err, 404, "does not exist")
	bi, err = svc.Instances.Get(project, blue).Do()
	if err != nil || bi.State != "RUNNABLE" || bi.DeploymentInfo != nil {
		t.Errorf("the blue instance after the delete: %v %+v", err, bi)
	}
	var item string
	if err := nb.QueryRow("SELECT item FROM shop.orders WHERE id = 3").Scan(&item); err != nil {
		t.Errorf("the blue instance's row after the delete: %v", err)
	}
}
