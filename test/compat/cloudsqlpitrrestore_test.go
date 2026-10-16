//go:build compat

package compat

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/googleapi"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// pitrDatasource is a Backup and DR datasource name, in the format
// instances.pointInTimeRestore takes.
func pitrDatasource(project string) string {
	return "projects/" + project + "/locations/us-central1/backupVaults/vault-1/dataSources/ds-1"
}

// TestCloudSQLPointInTimeRestoreFromBackupDR (#1425): through the official
// Go client, instances.pointInTimeRestore checks its request as Google does
// (a datasource that is not a Backup and DR datasource, a missing
// targetInstance or pointInTime, are 400s naming the field). A well-formed
// one names a datasource no backup plan association made: without the
// Backup and DR Service it is a 501 naming --services backupdr, with it a
// 404 (#1433); no target instance is created. A restore from a real
// datasource is TestBackupDRCloudSQLRestore.
// covers: sql.instances.pointInTimeRestore, sql.instances.get
func TestCloudSQLPointInTimeRestoreFromBackupDR(t *testing.T) {
	h := New(t)
	unknown, unknownWant := http.StatusNotImplemented, "--services backupdr"
	if os.Getenv(EnvBackupDR) != "" {
		unknown, unknownWant = http.StatusNotFound, "does not exist"
	}
	svc := sqlAdminService(t, h)
	p := h.Project()
	target := fmt.Sprintf("pitr-dr-%d", time.Now().UnixNano()%1e6)
	at := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	for _, c := range []struct {
		what string
		ctx  sqladmin.PointInTimeRestoreContext
		code int
		want string
	}{
		{"an instance as the datasource", sqladmin.PointInTimeRestoreContext{Datasource: "projects/" + p + "/instances/postgres",
			TargetInstance: target, PointInTime: at}, http.StatusBadRequest, "not a Backup and DR datasource"},
		{"no targetInstance", sqladmin.PointInTimeRestoreContext{Datasource: pitrDatasource(p), PointInTime: at},
			http.StatusBadRequest, "targetInstance is required"},
		{"no pointInTime", sqladmin.PointInTimeRestoreContext{Datasource: pitrDatasource(p), TargetInstance: target},
			http.StatusBadRequest, "pointInTime is required"},
		{"a datasource", sqladmin.PointInTimeRestoreContext{Datasource: pitrDatasource(p), TargetInstance: target, PointInTime: at,
			PreferredZone: "us-central1-a"}, unknown, unknownWant},
	} {
		_, err := svc.Instances.PointInTimeRestore("projects/"+p, &c.ctx).Do()
		var ge *googleapi.Error
		if !errors.As(err, &ge) || ge.Code != c.code || !strings.Contains(ge.Message, c.want) {
			t.Errorf("pointInTimeRestore with %s: %v; want HTTP %d naming %q", c.what, err, c.code, c.want)
		}
	}
	_, err := svc.Instances.Get(p, target).Do()
	wantHTTP(t, "instances.get of the refused restore's target", err, http.StatusNotFound)
}

// TestGcloudSQLPointInTimeRestoreRefused (#1425): `gcloud sql instances
// point-in-time-restore DATASOURCE TARGET POINT_IN_TIME` reaches the server,
// through the configuration `cloudburrow gcloud-setup` writes with egress to
// Google blocked, and fails naming the Backup and DR datasource that does not
// exist (or the service that is not served); no target is created.
// covers: sql.instances.pointInTimeRestore, sql.instances.get
func TestGcloudSQLPointInTimeRestoreRefused(t *testing.T) {
	h := New(t)
	// The Admin API first: gcloud's sql override is vouched only where it is
	// served, and elsewhere gcloud would reach for sqladmin.googleapis.com
	// (main run 38946612957, served shard).
	svc := sqlAdminService(t, h)
	g := newGcloudSession(t, h)
	target := fmt.Sprintf("gcloud-pitr-dr-%d", time.Now().UnixNano()%1e6)
	out, err := g.run(nil, "sql", "instances", "point-in-time-restore", pitrDatasource(g.project), target,
		time.Now().UTC().Add(-time.Minute).Format(time.RFC3339), "--project", g.project, "--quiet")
	if err == nil || !strings.Contains(out, "Backup and DR") {
		t.Errorf("gcloud sql instances point-in-time-restore: %v\n%s\nwant a failure naming the Backup and DR Service", err, out)
	}
	_, err = svc.Instances.Get(g.project, target).Do()
	wantHTTP(t, "instances.get of the refused restore's target", err, http.StatusNotFound)
}
