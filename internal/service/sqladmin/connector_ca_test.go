package sqladmin

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestCreatedInstanceHasAServerCA: Cloud SQL has a server CA for every
// instance from its creation, whatever happens to the local connector
// listener. An instance created while the listener's address is taken
// (another CloudBurrow instance holds 3307) still answers listServerCas and
// sslCerts, and keeps the same CA when the server is started again over the
// same state, as a restored instance. Before, both answered 503 "has no
// server CA yet", which the console's Connections > Security tab showed.
func TestCreatedInstanceHasAServerCA(t *testing.T) {
	ctx := context.Background()
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	dir := t.TempDir()
	state := filepath.Join(dir, "instances.json")

	start := func() *sqladmin.Service {
		t.Helper()
		srv := NewServer("127.0.0.1:0", newFake())
		if err := srv.EnableConnector(taken.Addr().String(), dir); err != nil {
			t.Fatal(err)
		}
		srv.EnableInstanceWrites(newFakeProv(), state)
		if err := srv.Start(ctx); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = srv.Stop(ctx) })
		if srv.conn.unavailable == "" {
			t.Fatal("the connector bound an address that is taken")
		}
		svc, err := sqladmin.NewService(ctx, option.WithEndpoint("http://"+srv.ln.Addr().String()+"/"), option.WithoutAuthentication())
		if err != nil {
			t.Fatal(err)
		}
		return svc
	}
	caOf := func(svc *sqladmin.Service) string {
		t.Helper()
		var cas *sqladmin.InstancesListServerCasResponse
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			if cas, err = svc.Instances.ListServerCas("proj", "orders").Do(); err == nil || time.Now().After(deadline) {
				break
			}
		}
		if err != nil || len(cas.Certs) == 0 || cas.ActiveVersion == "" {
			t.Fatalf("listServerCas of a created instance: %v %+v", err, cas)
		}
		if _, err := svc.SslCerts.List("proj", "orders").Do(); err != nil {
			t.Errorf("sslCerts.list of a created instance: %v", err)
		}
		return cas.ActiveVersion
	}

	svc := start()
	op, err := svc.Instances.Insert("proj", &sqladmin.DatabaseInstance{Name: "orders", DatabaseVersion: "POSTGRES_17",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if got := wait(t, svc, "proj", op); got.Error != nil {
		t.Fatalf("insert: %+v", got.Error)
	}
	first := caOf(svc)

	// Started again over the same state: the restored instance has its CA.
	if again := caOf(start()); again != first {
		t.Errorf("the restored instance's server CA is %s, want the one it was created with, %s", again, first)
	}
}
