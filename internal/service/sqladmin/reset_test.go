package sqladmin

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"

	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// A project's reset (#1541) deletes its created instances, with their
// servers, its backups, client certificates and operations, and leaves
// another project's alone.
func TestResetProjectDropsOnlyThatProject(t *testing.T) {
	dir := t.TempDir()
	prov := newFakeProv()
	srv := NewServer("", newFake())
	if err := srv.EnableConnector("127.0.0.1:0", dir); err != nil {
		t.Fatal(err)
	}
	srv.EnableInstanceWrites(prov, filepath.Join(dir, "instances.json"))
	if err := srv.EnableBackups(&fakeDumper{data: map[string]string{}}, filepath.Join(dir, "backups"), true); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	svc, err := sqladmin.NewService(context.Background(), option.WithEndpoint(ts.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"gone", "kept"} {
		op, err := svc.Instances.Insert(p, &sqladmin.DatabaseInstance{Name: "db-" + p, DatabaseVersion: "POSTGRES_17",
			Settings: &sqladmin.Settings{Tier: "db-f1-micro"}}).Do()
		if err != nil {
			t.Fatal(err)
		}
		wait(t, svc, p, op)
		op, err = svc.BackupRuns.Insert(p, "db-"+p, &sqladmin.BackupRun{}).Do()
		if err != nil {
			t.Fatal(err)
		}
		wait(t, svc, p, op)
		// A client certificate on the server `up` runs, every project's.
		if _, err := svc.SslCerts.Insert(p, "pg", &sqladmin.SslCertsInsertRequest{CommonName: "c-" + p}).Do(); err != nil {
			t.Fatal(err)
		}
	}

	if err := srv.ResetProject(context.Background(), "gone"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(prov.removed, "db-gone") || slices.Contains(prov.removed, "db-kept") {
		t.Errorf("servers removed: %v, want db-gone only", prov.removed)
	}
	check := func(p string, want int) {
		t.Helper()
		list, err := svc.Instances.List(p).Do()
		if err != nil {
			t.Fatal(err)
		}
		created := 0
		for _, i := range list.Items {
			if i.Name == "db-"+p {
				created++
			}
		}
		if created != want {
			t.Errorf("%s: %d created instances, want %d", p, created, want)
		}
		bs, err := svc.Backups.ListBackups("projects/" + p).Do()
		if err != nil {
			t.Fatal(err)
		}
		if len(bs.Backups) != want {
			t.Errorf("%s: %d backups, want %d", p, len(bs.Backups), want)
		}
		certs, err := svc.SslCerts.List(p, "pg").Do()
		if err != nil {
			t.Fatal(err)
		}
		if len(certs.Items) != want {
			t.Errorf("%s: %d client certificates, want %d", p, len(certs.Items), want)
		}
		ops, err := svc.Operations.List(p).Do()
		if err != nil {
			t.Fatal(err)
		}
		if (len(ops.Items) > 0) != (want > 0) {
			t.Errorf("%s: %d operations", p, len(ops.Items))
		}
	}
	check("gone", 0)
	check("kept", 1)

	// The name is free again in the project.
	op, err := svc.Instances.Insert("gone", &sqladmin.DatabaseInstance{Name: "db-gone", DatabaseVersion: "POSTGRES_17",
		Settings: &sqladmin.Settings{Tier: "db-f1-micro"}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	if got := wait(t, svc, "gone", op); got.Error != nil {
		t.Fatalf("insert again: %+v", got.Error)
	}

	// Every project's.
	if err := srv.ResetProject(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	check("gone", 0)
	check("kept", 0)
}
