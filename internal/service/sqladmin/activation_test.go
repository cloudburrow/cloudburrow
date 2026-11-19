package sqladmin

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestStopAndStartAnInstance is #1875: activationPolicy NEVER stops the
// server, keeps the instance RUNNABLE in the API, refuses what needs the
// server, and ALWAYS starts it again.
func TestStopAndStartAnInstance(t *testing.T) {
	prov := newFakeProv()
	srv := NewServer("", newFake())
	srv.EnableInstanceWrites(prov, "")
	ts := httptest.NewServer(srv)
	defer ts.Close()
	svc, err := sqladmin.NewService(context.Background(), option.WithEndpoint(ts.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	op, err := svc.Instances.Insert("proj", &sqladmin.DatabaseInstance{Name: "stops", DatabaseVersion: "POSTGRES_17",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	wait(t, svc, "proj", op)
	for _, bad := range []string{"ON_DEMAND", "SOMETIMES"} {
		if _, err := svc.Instances.Patch("proj", "stops", &sqladmin.DatabaseInstance{
			Settings: &sqladmin.Settings{ActivationPolicy: bad}}).Do(); err == nil || !strings.Contains(err.Error(), "400") {
			t.Errorf("activationPolicy %s: %v, want 400", bad, err)
		}
	}
	op, err = svc.Instances.Patch("proj", "stops", &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{ActivationPolicy: "NEVER"}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if got := wait(t, svc, "proj", op); got.Error != nil || got.OperationType != "UPDATE" {
		t.Fatalf("stop: %+v", got)
	}
	if !prov.stopped["stops"] {
		t.Error("the server was not stopped")
	}
	in, err := svc.Instances.Get("proj", "stops").Do()
	if err != nil {
		t.Fatal(err)
	}
	if in.State != "RUNNABLE" || in.Settings.ActivationPolicy != "NEVER" {
		t.Errorf("stopped instance: state %s policy %s", in.State, in.Settings.ActivationPolicy)
	}
	notRunning := func(what string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "instance is not running") {
			t.Errorf("%s on a stopped instance: %v, want 400 not running", what, err)
		}
	}
	_, err = svc.Users.List("proj", "stops").Do()
	notRunning("users.list", err)
	_, err = svc.Databases.Insert("proj", "stops", &sqladmin.Database{Name: "d"}).Do()
	notRunning("databases.insert", err)
	_, err = svc.Instances.Restart("proj", "stops").Do()
	notRunning("restart", err)
	_, err = svc.BackupRuns.Insert("proj", "stops", &sqladmin.BackupRun{}).Do()
	notRunning("backupRuns.insert", err)
	_, err = svc.Instances.Insert("proj", &sqladmin.DatabaseInstance{Name: "stops-r", MasterInstanceName: "stops"}).Do()
	notRunning("a replica of a stopped primary", err)
	// The record is still read and changed.
	if _, err := svc.Instances.ListServerCas("proj", "stops").Do(); err != nil && strings.Contains(err.Error(), "not running") {
		t.Errorf("listServerCas of a stopped instance: %v", err)
	}
	op, err = svc.Instances.Patch("proj", "stops", &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{
		UserLabels: map[string]string{"k": "v"}}}).Do()
	if err != nil {
		t.Fatalf("label patch of a stopped instance: %v", err)
	}
	wait(t, svc, "proj", op)
	if names := prov.startedNames(); len(names) != 0 {
		t.Errorf("a label patch started the stopped instance: %v", names)
	}
	op, err = svc.Instances.Patch("proj", "stops", &sqladmin.DatabaseInstance{Settings: &sqladmin.Settings{ActivationPolicy: "ALWAYS"}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if got := wait(t, svc, "proj", op); got.Error != nil || got.OperationType != "UPDATE" {
		t.Fatalf("start: %+v", got)
	}
	if names := prov.startedNames(); len(names) != 1 {
		t.Errorf("start: started %v", names)
	}
	in, _ = svc.Instances.Get("proj", "stops").Do()
	if in.Settings.ActivationPolicy != "ALWAYS" || in.Settings.UserLabels["k"] != "v" {
		t.Errorf("started instance: %+v", in.Settings)
	}
	if _, err := svc.Users.List("proj", "stops").Do(); err != nil {
		t.Errorf("users.list once started: %v", err)
	}
	// A read replica cannot be stopped.
	op, err = svc.Instances.Insert("proj", &sqladmin.DatabaseInstance{Name: "stops-r", MasterInstanceName: "stops",
		Settings: &sqladmin.Settings{ActivationPolicy: "NEVER"}}).Do()
	if err == nil || !strings.Contains(err.Error(), "read replica") {
		t.Errorf("a stopped replica: %v %+v, want 400", err, op)
	}
	// An instance created stopped is initialised, then stopped.
	op, err = svc.Instances.Insert("proj", &sqladmin.DatabaseInstance{Name: "cold", DatabaseVersion: "POSTGRES_17",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", ActivationPolicy: "NEVER"}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if got := wait(t, svc, "proj", op); got.Error != nil || !prov.stopped["cold"] {
		t.Errorf("created stopped: %+v stopped=%v", got.Error, prov.stopped["cold"])
	}
}
