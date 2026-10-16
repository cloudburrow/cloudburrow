package sqladmin

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestSourceRepresentationInstanceRefused (#1304): an instance describing a
// server outside Cloud SQL, what google_sql_source_representation_instance
// inserts, is 501 with why, and creates nothing; it is not run as a created
// instance of its databaseVersion.
func TestSourceRepresentationInstanceRefused(t *testing.T) {
	dir := t.TempDir()
	srv := NewServer("", newFake())
	if err := srv.EnableConnector("127.0.0.1:0", dir); err != nil {
		t.Fatal(err)
	}
	srv.EnableInstanceWrites(newFakeProv(), filepath.Join(dir, "instances.json"))
	ts := httptest.NewServer(srv)
	defer ts.Close()
	svc, err := sqladmin.NewService(context.Background(), option.WithEndpoint(ts.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []*sqladmin.DatabaseInstance{
		{Name: "ext", DatabaseVersion: "POSTGRES_17", InstanceType: "ON_PREMISES_INSTANCE", Region: "us-central1",
			OnPremisesConfiguration: &sqladmin.OnPremisesConfiguration{HostPort: "10.0.0.5:5432", Username: "repl"}},
		{Name: "ext", DatabaseVersion: "POSTGRES_17", OnPremisesConfiguration: &sqladmin.OnPremisesConfiguration{HostPort: "10.0.0.5:5432"}},
	} {
		if _, err := svc.Instances.Insert("proj", in).Do(); err == nil || !strings.Contains(err.Error(), "501") ||
			!strings.Contains(err.Error(), "outside") {
			t.Errorf("insert of %s %+v: %v; want 501 saying why", in.InstanceType, in.OnPremisesConfiguration, err)
		}
	}
	if _, err := svc.Instances.Get("proj", "ext").Do(); err == nil {
		t.Error("a refused source representation instance exists")
	}
}
