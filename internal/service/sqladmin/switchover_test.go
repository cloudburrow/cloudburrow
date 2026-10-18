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

// TestSwitchoverRefusals (#1614): what a DR replica's designation and
// instances.switchover refuse before any server is touched.
func TestSwitchoverRefusals(t *testing.T) {
	prov := newFakeProv()
	srv := NewServer("", newFake(), namedFake{fake: newFake(), name: "my", version: "MYSQL_8_4"})
	srv.EnableInstanceWrites(prov, filepath.Join(t.TempDir(), "instances.json"))
	ts := httptest.NewServer(srv)
	defer ts.Close()
	svc, err := sqladmin.NewService(context.Background(), option.WithEndpoint(ts.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []*sqladmin.DatabaseInstance{
		{Name: "plus", DatabaseVersion: "MYSQL_8_4", Settings: &sqladmin.Settings{Tier: "db-perf-optimized-N-2", Edition: "ENTERPRISE_PLUS"}},
		{Name: "ent", DatabaseVersion: "MYSQL_8_0", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}},
		{Name: "pgi", DatabaseVersion: "POSTGRES_17", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", Edition: "ENTERPRISE_PLUS"}},
	} {
		op, err := svc.Instances.Insert("proj", in).Do()
		if err != nil {
			t.Fatal(err)
		}
		if got := wait(t, svc, "proj", op); got.Error != nil {
			t.Fatalf("create %s: %+v", in.Name, got.Error.Errors[0])
		}
	}
	designate := func(primary, dr string) error {
		_, err := svc.Instances.Patch("proj", primary, &sqladmin.DatabaseInstance{
			ReplicationCluster: &sqladmin.ReplicationCluster{FailoverDrReplicaName: dr}}).Do()
		return err
	}
	_, insertErr := svc.Instances.Insert("proj", &sqladmin.DatabaseInstance{Name: "new", DatabaseVersion: "MYSQL_8_4",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}, ReplicationCluster: &sqladmin.ReplicationCluster{FailoverDrReplicaName: "plus"}}).Do()
	_, soloErr := svc.Instances.Switchover("proj", "plus").Do()
	_, pgErr := svc.Instances.Switchover("proj", "pgi").Do()
	_, timeoutErr := svc.Instances.Switchover("proj", "plus").DbTimeout("90000s").Do()
	for _, c := range []struct {
		what string
		err  error
		want string
	}{
		{"a new instance's DR replica", insertErr, "400"},
		{"a DR replica that is not a replica", designate("plus", "ent"), "not a read replica of plus"},
		{"a DR replica of another project", designate("plus", "other:ent"), "primary's project"},
		{"an Enterprise primary's DR replica", designate("ent", "plus"), "Enterprise Plus"},
		{"a PostgreSQL primary's DR replica", designate("pgi", "x"), "501"},
		{"switchover of a primary", soloErr, "not a read replica"},
		{"switchover of a PostgreSQL instance", pgErr, "501"},
		{"dbTimeout over 24 hours", timeoutErr, "24 hours"},
	} {
		if c.err == nil || !strings.Contains(c.err.Error(), c.want) {
			t.Errorf("%s: %v, want %s", c.what, c.err, c.want)
		}
	}
	// Removing a designation there is not is no error.
	if err := designate("plus", ""); err != nil {
		t.Errorf("clear the DR replica: %v", err)
	}
	in, err := svc.Instances.Get("proj", "plus").Do()
	if err != nil || in.ReplicationCluster != nil {
		t.Errorf("a primary without a DR replica: %+v, %v", in.ReplicationCluster, err)
	}
}
