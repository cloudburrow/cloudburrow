//go:build compat

package compat

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/cloudsqlconn/postgres/pgxv5"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestCloudSQLStopAndStart (#1875): through the official Go client a
// PostgreSQL instance is stopped with settings.activationPolicy NEVER and
// started with ALWAYS, as Google documents it
// (docs.cloud.google.com/sql/docs/mysql/start-stop-restart-instance). The
// change is an UPDATE operation; a stopped instance stays RUNNABLE in the
// API with activationPolicy NEVER, does not answer the connector, and
// refuses users.list and restart ("Invalid request since instance is not
// running."); started again, it has the row written before. Then `gcloud sql
// instances patch --activation-policy=never` and `=always` stop and start it
// the same way (gcloud's describe shows it STOPPED, as gcloud renders a
// RUNNABLE instance whose policy is NEVER), and ON_DEMAND, deprecated, is refused.
// covers: sql.instances.insert, sql.instances.patch, sql.instances.get, sql.instances.delete, sql.instances.restart, sql.users.list, sql.operations.get, sql.connect.get
func TestCloudSQLStopAndStart(t *testing.T) {
	h := New(t)
	if os.Getenv(EnvCloudSQL) == "" {
		t.Skip("needs --services cloudsql")
	}
	svc := sqlAdminService(t, h)
	g := newGcloudSession(t, h)
	project := g.project
	name := fmt.Sprintf("stopstart-%d", time.Now().UnixNano()%1e6)
	op, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "POSTGRES_17",
		Region: "us-central1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if d, err := svc.Instances.Delete(project, name).Do(); err == nil {
			waitSQLOp(t, svc, project, d)
		}
	}()
	waitSQLOp(t, svc, project, op)
	conn := project + ":us-central1:" + name
	// query runs the statements through the official connector, with a
	// short deadline, and answers the last one's one value.
	query := func(qs ...string) (string, error) {
		drv := "stopstart-" + fmt.Sprint(time.Now().UnixNano())
		cleanup, err := pgxv5.RegisterDriver(drv, connectorOptions(h)...)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		db, err := sql.Open(drv, fmt.Sprintf("host=%s user=postgres dbname=postgres sslmode=disable", conn))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		ctx, cancel := context.WithTimeout(h.Context(), 30*time.Second)
		defer cancel()
		last := len(qs) - 1
		for _, q := range qs[:last] {
			if _, err := db.ExecContext(ctx, q); err != nil {
				return "", err
			}
		}
		var out string
		err = db.QueryRowContext(ctx, qs[last]).Scan(&out)
		return out, err
	}
	if _, err := query("CREATE TABLE kept (v text)", "INSERT INTO kept VALUES ('before-stop')", "SELECT v FROM kept"); err != nil {
		t.Fatalf("write the row: %v", err)
	}
	patch := func(policy string) {
		t.Helper()
		op, err := svc.Instances.Patch(project, name, &sqladmin.DatabaseInstance{
			Settings: &sqladmin.Settings{ActivationPolicy: policy}}).Do()
		if err != nil {
			t.Fatalf("activationPolicy %s: %v", policy, err)
		}
		if op.OperationType != "UPDATE" {
			t.Errorf("activationPolicy %s: operation %s, want UPDATE", policy, op.OperationType)
		}
		waitSQLOp(t, svc, project, op)
	}
	stopped := func(how string) {
		t.Helper()
		in, err := svc.Instances.Get(project, name).Do()
		if err != nil {
			t.Fatal(err)
		}
		if in.State != "RUNNABLE" || in.Settings.ActivationPolicy != "NEVER" {
			t.Errorf("%s: state %s activationPolicy %s, want RUNNABLE and NEVER", how, in.State, in.Settings.ActivationPolicy)
		}
		if v, err := query("SELECT 1"); err == nil {
			t.Errorf("%s: the stopped instance answered the connector: %s", how, v)
		}
		_, err = svc.Users.List(project, name).Do()
		wantHTTP(t, how+": users.list of a stopped instance", err, 400)
		if err == nil || !strings.Contains(err.Error(), "instance is not running") {
			t.Errorf("%s: users.list: %v, want Invalid request since instance is not running", how, err)
		}
		_, err = svc.Instances.Restart(project, name).Do()
		wantHTTP(t, how+": restart of a stopped instance", err, 400)
	}
	started := func(how string) {
		t.Helper()
		in, err := svc.Instances.Get(project, name).Do()
		if err != nil || in.State != "RUNNABLE" || in.Settings.ActivationPolicy != "ALWAYS" {
			t.Fatalf("%s: %+v %v", how, in, err)
		}
		if v, err := query("SELECT v FROM kept"); err != nil || v != "before-stop" {
			t.Errorf("%s: the row written before the stop: %q %v", how, v, err)
		}
		if _, err := svc.Users.List(project, name).Do(); err != nil {
			t.Errorf("%s: users.list: %v", how, err)
		}
	}

	patch("NEVER")
	stopped("stopped through the Go client")
	patch("ALWAYS")
	started("started through the Go client")

	_, err = svc.Instances.Patch(project, name, &sqladmin.DatabaseInstance{
		Settings: &sqladmin.Settings{ActivationPolicy: "ON_DEMAND"}}).Do()
	wantHTTP(t, "activationPolicy ON_DEMAND", err, 400)

	g.must("sql", "instances", "patch", name, "--project", g.project, "--activation-policy=never", "--quiet")
	if out := g.must("sql", "instances", "describe", name, "--project", g.project,
		"--format=value(state,settings.activationPolicy)"); strings.Join(strings.Fields(out), " ") != "STOPPED NEVER" {
		// gcloud shows a RUNNABLE instance whose policy is NEVER as STOPPED.
		t.Errorf("gcloud describe of the stopped instance: %q", out)
	}
	stopped("stopped by gcloud")
	g.must("sql", "instances", "patch", name, "--project", g.project, "--activation-policy=always", "--quiet")
	started("started by gcloud")
}
