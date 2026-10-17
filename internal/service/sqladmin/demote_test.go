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

// TestDemote (#1524): demote makes a standalone MySQL instance a replica of
// a source representation instance, waiting for startExternalSync;
// demoteMaster checks the source first, and what it cannot do is refused
// naming why.
func TestDemote(t *testing.T) {
	dir := t.TempDir()
	prov := newFakeProv()
	srv := NewServer("", newFake(), namedFake{fake: newFake(), name: "my", version: "MYSQL_8_4"})
	srv.EnableInstanceWrites(prov, filepath.Join(dir, "instances.json"))
	ts := httptest.NewServer(srv)
	defer ts.Close()
	svc, err := sqladmin.NewService(context.Background(), option.WithEndpoint(ts.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close()
	for _, in := range []*sqladmin.DatabaseInstance{
		{Name: "ext", DatabaseVersion: "MYSQL_8_4", InstanceType: "ON_PREMISES_INSTANCE",
			OnPremisesConfiguration: &sqladmin.OnPremisesConfiguration{HostPort: dead, Username: "repl", Password: "secret"}},
		{Name: "solo", DatabaseVersion: "MYSQL_8_4", RootPassword: "pw", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}},
		{Name: "solo2", DatabaseVersion: "MYSQL_8_4", RootPassword: "pw", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}},
		{Name: "pgdb", DatabaseVersion: "POSTGRES_17", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}},
	} {
		op, err := svc.Instances.Insert("proj", in).Do()
		if err != nil {
			t.Fatal(err)
		}
		if got := wait(t, svc, "proj", op); got.Error != nil {
			t.Fatalf("create %s: %+v", in.Name, got.Error.Errors[0])
		}
	}
	master := func(c *sqladmin.DemoteMasterContext) error {
		_, err := svc.Instances.DemoteMaster("proj", "solo", &sqladmin.InstancesDemoteMasterRequest{DemoteMasterContext: c}).Do()
		return err
	}
	for _, c := range []struct {
		what string
		err  error
		want string
	}{
		{"no masterInstanceName", master(&sqladmin.DemoteMasterContext{}), "400"},
		{"skipReplicationSetup", master(&sqladmin.DemoteMasterContext{MasterInstanceName: "ext", SkipReplicationSetup: true}), "mysql.* stored procedures"},
		{"a master that is no source representation instance", master(&sqladmin.DemoteMasterContext{MasterInstanceName: "solo2"}), "400"},
		{"an unreachable source", master(&sqladmin.DemoteMasterContext{MasterInstanceName: "ext"}), "CONNECTION_FAILURE"},
		{"demoteMaster of PostgreSQL", func() error {
			_, err := svc.Instances.DemoteMaster("proj", "pgdb", &sqladmin.InstancesDemoteMasterRequest{
				DemoteMasterContext: &sqladmin.DemoteMasterContext{MasterInstanceName: "ext"}}).Do()
			return err
		}(), "400"},
		{"demote of PostgreSQL", func() error {
			_, err := svc.Instances.Demote("proj", "pgdb", &sqladmin.InstancesDemoteRequest{
				DemoteContext: &sqladmin.DemoteContext{SourceRepresentativeInstanceName: "ext"}}).Do()
			return err
		}(), "501"},
	} {
		if c.err == nil || !strings.Contains(c.err.Error(), c.want) {
			t.Errorf("demoteMaster with %s: %v; want %s", c.what, c.err, c.want)
		}
	}

	op, err := svc.Instances.Demote("proj", "solo", &sqladmin.InstancesDemoteRequest{
		DemoteContext: &sqladmin.DemoteContext{SourceRepresentativeInstanceName: "ext"}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if op.OperationType != "DEMOTE" {
		t.Errorf("operationType %s", op.OperationType)
	}
	if got := wait(t, svc, "proj", op); got.Error != nil {
		t.Fatalf("demote: %+v", got.Error.Errors[0])
	}
	got, err := svc.Instances.Get("proj", "solo").Do()
	if err != nil || got.MasterInstanceName != "proj:ext" || got.InstanceType != "READ_REPLICA_INSTANCE" {
		t.Fatalf("the demoted instance: %+v %v", got, err)
	}
	if s := prov.started["solo"]; s.External == nil || s.ExternalSyncStarted || s.External.Demoted || s.External.User != "repl" {
		t.Errorf("the demoted instance's spec: %+v %+v", s, s.External)
	}
	if src, err := svc.Instances.Get("proj", "ext").Do(); err != nil || len(src.ReplicaNames) != 1 || src.ReplicaNames[0] != "solo" {
		t.Errorf("the source's replicaNames: %+v %v", src, err)
	}
	if _, err := svc.Instances.Demote("proj", "solo", &sqladmin.InstancesDemoteRequest{
		DemoteContext: &sqladmin.DemoteContext{SourceRepresentativeInstanceName: "ext"}}).Do(); err == nil || !strings.Contains(err.Error(), "already a read replica") {
		t.Errorf("demote of a replica: %v", err)
	}
}
