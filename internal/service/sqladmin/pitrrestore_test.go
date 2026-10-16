package sqladmin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestPointInTimeRestoreChecksThenRefuses (#1425): a malformed request is a
// 400 naming the field, as Google's is; a well-formed one is a 501 naming the
// Backup and DR Service, which no datasource can exist without.
func TestPointInTimeRestoreChecksThenRefuses(t *testing.T) {
	ts := httptest.NewServer(NewServer("", newFake()))
	defer ts.Close()
	svc, err := sqladmin.NewService(context.Background(), option.WithEndpoint(ts.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	const ds = "projects/proj/locations/us-central1/backupVaults/vault/dataSources/ds"
	for _, c := range []struct {
		name string
		ctx  sqladmin.PointInTimeRestoreContext
		code int
		want string
	}{
		{"no datasource", sqladmin.PointInTimeRestoreContext{TargetInstance: "target", PointInTime: "2026-01-01T00:00:00Z"}, 400, "datasource is required"},
		{"an instance as datasource", sqladmin.PointInTimeRestoreContext{Datasource: "projects/proj/instances/pg", TargetInstance: "target",
			PointInTime: "2026-01-01T00:00:00Z"}, 400, "not a Backup and DR datasource"},
		{"no target", sqladmin.PointInTimeRestoreContext{Datasource: ds, PointInTime: "2026-01-01T00:00:00Z"}, 400, "targetInstance is required"},
		{"bad target", sqladmin.PointInTimeRestoreContext{Datasource: ds, TargetInstance: "Bad_", PointInTime: "2026-01-01T00:00:00Z"}, 400, "instance name"},
		{"no time", sqladmin.PointInTimeRestoreContext{Datasource: ds, TargetInstance: "target"}, 400, "pointInTime is required"},
		{"bad time", sqladmin.PointInTimeRestoreContext{Datasource: ds, TargetInstance: "target", PointInTime: "yesterday"}, 400, "RFC 3339"},
		{"same zones", sqladmin.PointInTimeRestoreContext{Datasource: ds, TargetInstance: "target", PointInTime: "2026-01-01T00:00:00Z",
			PreferredZone: "us-central1-a", PreferredSecondaryZone: "us-central1-a"}, 400, "preferredSecondaryZone"},
		{"well formed", sqladmin.PointInTimeRestoreContext{Datasource: ds, TargetInstance: "target", PointInTime: "2026-01-01T00:00:00Z"},
			http.StatusNotImplemented, "backupdr.googleapis.com"},
	} {
		_, err := svc.Instances.PointInTimeRestore("projects/proj", &c.ctx).Do()
		var ge *googleapi.Error
		if !errors.As(err, &ge) || ge.Code != c.code || !strings.Contains(ge.Message, c.want) {
			t.Errorf("%s: %v, want %d with %q", c.name, err, c.code, c.want)
		}
	}
}
