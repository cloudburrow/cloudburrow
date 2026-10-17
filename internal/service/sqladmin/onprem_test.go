package sqladmin

import (
	"context"
	"net"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestSourceRepresentationInstance (#1501): an instance describing a MySQL
// server outside Cloud SQL, what google_sql_source_representation_instance
// inserts, is kept as a record, answered without its password, and runs no
// server; a replica of it runs idle until startExternalSync. What is not
// built stays 501 with why.
func TestSourceRepresentationInstance(t *testing.T) {
	dir := t.TempDir()
	prov := newFakeProv()
	srv := NewServer("", newFake(), namedFake{fake: newFake(), name: "my", version: "MYSQL_8_4"})
	if err := srv.EnableConnector("127.0.0.1:0", dir); err != nil {
		t.Fatal(err)
	}
	srv.EnableInstanceWrites(prov, filepath.Join(dir, "instances.json"))
	ts := httptest.NewServer(srv)
	defer ts.Close()
	svc, err := sqladmin.NewService(context.Background(), option.WithEndpoint(ts.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	// A port nothing listens on: the source is unreachable.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close()

	for _, c := range []struct {
		in   *sqladmin.DatabaseInstance
		want string
	}{
		{&sqladmin.DatabaseInstance{Name: "pgext", DatabaseVersion: "POSTGRES_17", InstanceType: "ON_PREMISES_INSTANCE",
			OnPremisesConfiguration: &sqladmin.OnPremisesConfiguration{HostPort: "10.0.0.5:5432", Username: "repl", DumpFilePath: "gs://b/d.sql"}}, "400"},
		{&sqladmin.DatabaseInstance{Name: "dump", DatabaseVersion: "MYSQL_8_4",
			OnPremisesConfiguration: &sqladmin.OnPremisesConfiguration{HostPort: "10.0.0.5:3306", DumpFilePath: "b/d.sql"}}, "400"},
		{&sqladmin.DatabaseInstance{Name: "bad", DatabaseVersion: "MYSQL_8_4",
			OnPremisesConfiguration: &sqladmin.OnPremisesConfiguration{HostPort: "10.0.0.5"}}, "400"},
	} {
		if _, err := svc.Instances.Insert("proj", c.in).Do(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("insert of %s: %v; want %s", c.in.Name, err, c.want)
		}
	}

	op, err := svc.Instances.Insert("proj", &sqladmin.DatabaseInstance{Name: "ext", DatabaseVersion: "MYSQL_8_4",
		InstanceType: "ON_PREMISES_INSTANCE", Region: "us-central1",
		OnPremisesConfiguration: &sqladmin.OnPremisesConfiguration{HostPort: dead, Username: "repl", Password: "secret"}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if got := wait(t, svc, "proj", op); got.Error != nil {
		t.Fatalf("create: %+v", got.Error.Errors[0])
	}
	got, err := svc.Instances.Get("proj", "ext").Do()
	if err != nil {
		t.Fatal(err)
	}
	if got.InstanceType != "ON_PREMISES_INSTANCE" || got.OnPremisesConfiguration == nil ||
		got.OnPremisesConfiguration.HostPort != dead || got.OnPremisesConfiguration.Username != "repl" ||
		got.OnPremisesConfiguration.Password != "" || got.State != "RUNNABLE" {
		t.Errorf("source representation instance: %+v %+v", got, got.OnPremisesConfiguration)
	}
	if len(prov.started) != 0 {
		t.Errorf("a server was started for a source representation instance: %v", prov.started)
	}
	if _, err := svc.Instances.Restart("proj", "ext").Do(); err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("restart of a source representation instance: %v; want 400", err)
	}

	op, err = svc.Instances.Insert("proj", &sqladmin.DatabaseInstance{Name: "rep", MasterInstanceName: "ext",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if got := wait(t, svc, "proj", op); got.Error != nil {
		t.Fatalf("replica: %+v", got.Error.Errors[0])
	}
	r, err := svc.Instances.Get("proj", "rep").Do()
	if err != nil {
		t.Fatal(err)
	}
	if r.InstanceType != "READ_REPLICA_INSTANCE" || r.MasterInstanceName != "proj:ext" || r.DatabaseVersion != "MYSQL_8_4" ||
		r.ReplicaConfiguration == nil || r.ReplicaConfiguration.MysqlReplicaConfiguration == nil ||
		r.ReplicaConfiguration.MysqlReplicaConfiguration.Username != "repl" {
		t.Errorf("replica: %+v", r)
	}
	sp := prov.started["rep"]
	if sp.External == nil || sp.External.Port == 0 || sp.External.User != "repl" || sp.External.Password != "secret" || sp.ExternalSyncStarted {
		t.Errorf("replica's spec: %+v", sp.External)
	}
	if s, _ := svc.Instances.Get("proj", "ext").Do(); s == nil || len(s.ReplicaNames) != 1 {
		t.Errorf("source's replicaNames: %+v", s)
	}

	v, err := svc.Projects.Instances.VerifyExternalSyncSettings("proj", "rep",
		&sqladmin.SqlInstancesVerifyExternalSyncSettingsRequest{SyncMode: "ONLINE"}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Errors) == 0 || v.Errors[0].Type != "CONNECTION_FAILURE" {
		t.Errorf("verify of an unreachable source: %+v", v.Errors)
	}
	if _, err := svc.Projects.Instances.StartExternalSync("proj", "rep",
		&sqladmin.SqlInstancesStartExternalSyncRequest{SyncMode: "ONLINE"}).Do(); err == nil || !strings.Contains(err.Error(), "CONNECTION_FAILURE") {
		t.Errorf("start sync from an unreachable source: %v; want its verification", err)
	}
	if _, err := svc.Projects.Instances.StartExternalSync("proj", "rep",
		&sqladmin.SqlInstancesStartExternalSyncRequest{SyncMode: "OFFLINE"}).Do(); err == nil || !strings.Contains(err.Error(), "CONNECTION_FAILURE") {
		t.Errorf("offline sync from an unreachable source: %v; want its verification", err)
	}
	if _, err := svc.Projects.Instances.StartExternalSync("proj", "my",
		&sqladmin.SqlInstancesStartExternalSyncRequest{SkipVerification: true}).Do(); err == nil {
		t.Error("start sync on a server `up` runs succeeded")
	}
	// A PostgreSQL source (#1524): its replica is a PostgreSQL server
	// with the source's selected databases, verified the same way.
	op, err = svc.Instances.Insert("proj", &sqladmin.DatabaseInstance{Name: "pgext", DatabaseVersion: "POSTGRES_16",
		InstanceType: "ON_PREMISES_INSTANCE", OnPremisesConfiguration: &sqladmin.OnPremisesConfiguration{HostPort: dead,
			Username: "repl", Password: "secret", SelectedObjects: []*sqladmin.SelectedObjects{{Database: "shop"}}}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	wait(t, svc, "proj", op)
	if _, err := svc.Instances.Insert("proj", &sqladmin.DatabaseInstance{Name: "pgbad", MasterInstanceName: "pgext",
		DatabaseVersion: "MYSQL_8_4"}).Do(); err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("a MySQL replica of a PostgreSQL source: %v; want 400", err)
	}
	op, err = svc.Instances.Insert("proj", &sqladmin.DatabaseInstance{Name: "pgrep", MasterInstanceName: "pgext", DatabaseVersion: "POSTGRES_17"}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if got := wait(t, svc, "proj", op); got.Error != nil {
		t.Fatalf("PostgreSQL replica: %+v", got.Error.Errors[0])
	}
	if sp := prov.started["pgrep"]; sp.External == nil || len(sp.External.Databases) != 1 || sp.External.Databases[0] != "shop" || sp.DatabaseVersion != "POSTGRES_17" {
		t.Errorf("PostgreSQL replica's spec: %+v", sp.External)
	}
	if pr, err := svc.Instances.Get("proj", "pgrep").Do(); err != nil || pr.ReplicaConfiguration == nil || pr.ReplicaConfiguration.MysqlReplicaConfiguration != nil {
		t.Errorf("PostgreSQL replica: %+v %v", pr, err)
	}
	if v, err := svc.Projects.Instances.VerifyExternalSyncSettings("proj", "pgrep",
		&sqladmin.SqlInstancesVerifyExternalSyncSettingsRequest{}).Do(); err != nil || len(v.Errors) == 0 || v.Errors[0].Type != "CONNECTION_FAILURE" {
		t.Errorf("verify of an unreachable PostgreSQL source: %+v %v", v, err)
	}

	op, err = svc.Projects.Instances.StartExternalSync("proj", "rep",
		&sqladmin.SqlInstancesStartExternalSyncRequest{SyncMode: "ONLINE", SkipVerification: true}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if got := wait(t, svc, "proj", op); got.Error != nil || got.OperationType != "START_EXTERNAL_SYNC" {
		t.Fatalf("start sync: %+v", got)
	}
	prov.mu.Lock()
	started := prov.started["rep"].ExternalSyncStarted
	prov.mu.Unlock()
	if !started {
		t.Error("startExternalSync did not start the replica's replication")
	}

	if _, err := svc.Instances.Delete("proj", "ext").Do(); err == nil || !strings.Contains(err.Error(), "replicas") {
		t.Errorf("delete of a source with a replica: %v", err)
	}
	// promoteReplica, which needs a real server, is in the compat test.
	op, err = svc.Instances.Delete("proj", "rep").Do()
	if err != nil {
		t.Fatal(err)
	}
	wait(t, svc, "proj", op)
	op, err = svc.Instances.Delete("proj", "ext").Do()
	if err != nil {
		t.Fatal(err)
	}
	wait(t, svc, "proj", op)
	if _, err := svc.Instances.Get("proj", "ext").Do(); err == nil {
		t.Error("a deleted source representation instance exists")
	}
}

// TestSourceRepresentationInstancesPersist: the records are kept with the
// created instances' and brought back by a new server.
func TestSourceRepresentationInstancesPersist(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "instances.json")
	srv := NewServer("127.0.0.1:0", namedFake{fake: newFake(), name: "my", version: "MYSQL_8_4"})
	srv.EnableInstanceWrites(newFakeProv(), state)
	ts := httptest.NewServer(srv)
	svc, _ := sqladmin.NewService(context.Background(), option.WithEndpoint(ts.URL+"/"), option.WithoutAuthentication())
	if _, err := svc.Instances.Insert("proj", &sqladmin.DatabaseInstance{Name: "ext", DatabaseVersion: "MYSQL_8_0",
		OnPremisesConfiguration: &sqladmin.OnPremisesConfiguration{HostPort: "192.0.2.1:3306", Username: "u"}}).Do(); err != nil {
		t.Fatal(err)
	}
	ts.Close()
	srv2 := NewServer("127.0.0.1:0", namedFake{fake: newFake(), name: "my", version: "MYSQL_8_4"})
	srv2.EnableInstanceWrites(newFakeProv(), state)
	if err := srv2.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer srv2.Stop(context.Background())
	svc2, _ := sqladmin.NewService(context.Background(), option.WithEndpoint("http://"+srv2.Addr()+"/"), option.WithoutAuthentication())
	l, err := svc2.Instances.List("proj").Do()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, in := range l.Items {
		found = found || (in.Name == "ext" && in.InstanceType == "ON_PREMISES_INSTANCE")
	}
	if !found {
		t.Errorf("the source representation instance did not persist: %d items", len(l.Items))
	}
}
