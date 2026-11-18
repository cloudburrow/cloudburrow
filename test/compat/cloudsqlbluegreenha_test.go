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

// TestCloudSQLBlueGreenDeploymentHA (#1614): through the official Go
// client, a blue-green deployment of a high-availability (REGIONAL) MySQL
// 8.0 instance. The green instance is "a clone of the blue environment"
// (docs.cloud.google.com/sql/docs/mysql/about-blue-green-deployments): it
// is REGIONAL too, with a primary and a secondary zone, and its primary
// server, replicating from the blue one, has its standby replicating from
// it semi-synchronously (Rpl_semi_sync_source_status ON with one client).
// After the switchover the blue name, through the same connection name,
// reaches the green server, REGIONAL, with every row; an instances.failover
// of it moves its zones, and the new primary has every row and takes
// writes, so the green standby had them. The green name, the old blue
// server, is REGIONAL still and fails over too.
// covers: sql.blueGreenDeployments.create, sql.blueGreenDeployments.get, sql.blueGreenDeployments.switchover, sql.blueGreenDeployments.delete, sql.instances.insert, sql.instances.get, sql.instances.failover, sql.operations.get, sql.connect.get, sql.connect.generateEphemeral
func TestCloudSQLBlueGreenDeploymentHA(t *testing.T) {
	h := New(t)
	svc := sqlAdminService(t, h)
	if os.Getenv(EnvMySQL) == "" {
		t.Skip("needs --services cloudsql-mysql")
	}
	project := h.Project()
	e := sqlEngine{mysql: true}
	n := time.Now().UnixNano() % 1e6
	blue := fmt.Sprintf("bgha-%d", n)
	loc := "projects/" + project + "/locations/us-central1"
	createSQLInstance(t, svc, project, &sqladmin.DatabaseInstance{Name: blue, DatabaseVersion: "MYSQL_8_0", RootPassword: "Root-pass1",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", AvailabilityType: "REGIONAL",
			BackupConfiguration: &sqladmin.BackupConfiguration{Enabled: true, BinaryLogEnabled: true}}})
	bdb := e.root(t, h, project+":us-central1:"+blue)
	for _, q := range []string{"CREATE DATABASE shop", "CREATE TABLE shop.orders (id int primary key, item varchar(40))",
		"INSERT INTO shop.orders VALUES (1, 'before the deployment')"} {
		q := q
		eventually(t, q, func() error { _, err := bdb.Exec(q); return err })
	}

	id := fmt.Sprintf("bgha-%d", n)
	name := loc + "/blueGreenDeployments/" + id
	op, err := svc.BlueGreenDeployments.Create(loc, &sqladmin.BlueGreenDeployment{SourceInstance: "projects/" + project + "/instances/" + blue}).
		BlueGreenDeploymentId(id).Do()
	if err != nil {
		t.Fatalf("create the deployment of a REGIONAL instance: %v", err)
	}
	if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
		t.Fatalf("create the deployment: %+v", op.Error.Errors[0])
	}
	d, err := svc.BlueGreenDeployments.Get(name).Do()
	if err != nil {
		t.Fatal(err)
	}
	green := strings.TrimPrefix(d.SwitchoverTargetInstance, "projects/"+project+"/instances/")
	t.Cleanup(func() {
		if d, err := svc.Instances.Delete(project, green).Do(); err == nil {
			waitSQLOpDone(t, svc, project, d)
		}
	})
	eventuallyTrue(t, "the deployment is SWITCHOVER_READY", func() bool {
		d, err = svc.BlueGreenDeployments.Get(name).Do()
		return err == nil && d.State == "SWITCHOVER_READY"
	})
	regional := func(inst, what string) *sqladmin.DatabaseInstance {
		t.Helper()
		in, err := svc.Instances.Get(project, inst).Do()
		if err != nil {
			t.Fatal(err)
		}
		if in.Settings == nil || in.Settings.AvailabilityType != "REGIONAL" || in.GceZone == "" || in.SecondaryGceZone == "" ||
			in.GceZone == in.SecondaryGceZone || in.State != "RUNNABLE" {
			t.Errorf("%s %s: settings %+v zones %q %q state %s", what, inst, in.Settings, in.GceZone, in.SecondaryGceZone, in.State)
		}
		return in
	}
	gi := regional(green, "the green instance")
	if gi.MasterInstanceName != project+":"+blue {
		t.Errorf("the green instance's master %q", gi.MasterInstanceName)
	}
	gdb := e.root(t, h, project+":us-central1:"+green)
	eventually(t, "the green primary's standby is semi-synchronous", func() error {
		var status, clients string
		if err := gdb.QueryRow("SELECT VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME = 'Rpl_semi_sync_source_status'").
			Scan(&status); err != nil {
			return err
		}
		if err := gdb.QueryRow("SELECT VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME = 'Rpl_semi_sync_source_clients'").
			Scan(&clients); err != nil {
			return err
		}
		if status != "ON" || clients != "1" {
			return fmt.Errorf("semi-synchronous status %s with %s clients, want ON with 1", status, clients)
		}
		return nil
	})
	if _, err := bdb.Exec("INSERT INTO shop.orders VALUES (2, 'before the switchover')"); err != nil {
		t.Fatal(err)
	}
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
	failover := func(inst string, rows int) {
		t.Helper()
		in := regional(inst, "before its failover")
		op, err := svc.Instances.Failover(project, inst, &sqladmin.InstancesFailoverRequest{
			FailoverContext: &sqladmin.FailoverContext{SettingsVersion: in.Settings.SettingsVersion}}).Do()
		if err != nil {
			t.Fatalf("failover of %s: %v", inst, err)
		}
		if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
			t.Fatalf("failover of %s: %+v", inst, op.Error.Errors[0])
		}
		after := regional(inst, "after its failover")
		if after.GceZone != in.SecondaryGceZone || after.SecondaryGceZone != in.GceZone {
			t.Errorf("%s's zones after its failover: %s and %s, were %s and %s", inst, after.GceZone, after.SecondaryGceZone,
				in.GceZone, in.SecondaryGceZone)
		}
		db := e.root(t, h, project+":us-central1:"+inst)
		eventually(t, "the new primary of "+inst+" has every row and takes writes", func() error {
			var got int
			if err := db.QueryRow("SELECT count(*) FROM shop.orders").Scan(&got); err != nil {
				return err
			}
			if got != rows {
				return fmt.Errorf("%d rows, want %d", got, rows)
			}
			_, err := db.Exec(fmt.Sprintf("INSERT INTO shop.orders VALUES (%d, 'after the failover')", 100+rows))
			return err
		})
	}
	bi := regional(blue, "the blue name after the switchover")
	if bi.MasterInstanceName != "" || bi.InstanceType != "CLOUD_SQL_INSTANCE" {
		t.Errorf("the blue name after the switchover: master %q type %s", bi.MasterInstanceName, bi.InstanceType)
	}
	nb := e.root(t, h, project+":us-central1:"+blue)
	eventually(t, "a write on the blue name's new server", func() error {
		_, err := nb.Exec("INSERT INTO shop.orders VALUES (3, 'after the switchover')")
		return err
	})
	failover(blue, 3)
	failover(green, 2)

	op, err = svc.BlueGreenDeployments.Delete(name).DeleteOldSource(true).Do()
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if op = waitSQLOpDone(t, svc, project, op); op.Error != nil {
		t.Fatalf("delete: %+v", op.Error.Errors[0])
	}
}
