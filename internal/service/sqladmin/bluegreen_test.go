package sqladmin

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestBlueGreenRefusals (#1614): a deployment of a PostgreSQL instance
// ("MySQL only"), of an instance that does not exist, with a description
// over 255 characters or an invalid ID is refused; get of an unknown
// deployment is 404 and list of none is empty. Spec.Server names the
// server, its own name by default.
func TestBlueGreenRefusals(t *testing.T) {
	srv := NewServer("", newFake())
	srv.EnableInstanceWrites(newFakeProv(), "")
	ts := httptest.NewServer(srv)
	defer ts.Close()
	svc, err := sqladmin.NewService(context.Background(), option.WithEndpoint(ts.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	op, err := svc.Instances.Insert("proj", &sqladmin.DatabaseInstance{Name: "pgi", DatabaseVersion: "POSTGRES_17",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	wait(t, svc, "proj", op)
	loc := "projects/proj/locations/" + Region
	for want, d := range map[string]struct {
		id string
		bg *sqladmin.BlueGreenDeployment
	}{
		"MySQL only":            {"bg1", &sqladmin.BlueGreenDeployment{SourceInstance: "projects/proj/instances/pgi"}},
		"404":                   {"bg2", &sqladmin.BlueGreenDeployment{SourceInstance: "nope"}},
		"255 characters":        {"bg3", &sqladmin.BlueGreenDeployment{SourceInstance: "pgi", Description: strings.Repeat("x", 256)}},
		"blueGreenDeploymentId": {"Bad_ID", &sqladmin.BlueGreenDeployment{SourceInstance: "pgi"}},
		"sourceInstance":        {"bg4", &sqladmin.BlueGreenDeployment{}},
	} {
		if _, err := svc.BlueGreenDeployments.Create(loc, d.bg).BlueGreenDeploymentId(d.id).Do(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("create %s: %v, want %q", d.id, err, want)
		}
	}
	if _, err := svc.BlueGreenDeployments.Get(loc + "/blueGreenDeployments/none").Do(); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("get of none: %v", err)
	}
	if l, err := svc.BlueGreenDeployments.List(loc).Do(); err != nil || len(l.BlueGreenDeployments) != 0 {
		t.Errorf("list: %v %+v", err, l)
	}
	if got := srv.ServerOf("pgi"); got != "pgi" {
		t.Errorf("ServerOf(pgi) = %q", got)
	}
	srv.noteServer(Spec{Name: "pgi", Server: "other"})
	if got := srv.ServerOf("pgi"); got != "other" {
		t.Errorf("ServerOf after a switchover = %q", got)
	}
}
