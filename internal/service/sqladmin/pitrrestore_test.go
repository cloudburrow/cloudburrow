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
		{"well formed, without Backup and DR", sqladmin.PointInTimeRestoreContext{Datasource: ds, TargetInstance: "target", PointInTime: "2026-01-01T00:00:00Z"},
			http.StatusNotImplemented, "--services backupdr"},
	} {
		_, err := svc.Instances.PointInTimeRestore("projects/proj", &c.ctx).Do()
		var ge *googleapi.Error
		if !errors.As(err, &ge) || ge.Code != c.code || !strings.Contains(ge.Message, c.want) {
			t.Errorf("%s: %v, want %d with %q", c.name, err, c.code, c.want)
		}
	}
}

// fakeBackupDR knows one datasource, of another project's instance, and
// one backup, whose run does not exist.
type fakeBackupDR struct{}

func (fakeBackupDR) DataSourceInstance(name string) (string, string, error) {
	if strings.HasSuffix(name, "/dataSources/other") {
		return "elsewhere", "pg", nil
	}
	return "", "", errors.New("The Backup and DR data source " + name + " does not exist.")
}

func (fakeBackupDR) BackupRunID(name string) (string, error) {
	if strings.HasSuffix(name, "/backups/b") {
		return "404", nil
	}
	return "", errors.New("The Backup and DR backup " + name + " does not exist.")
}

// TestPointInTimeRestoreResolvesTheDatasource (#1433): with the Backup and
// DR Service, an unknown datasource is a 404 and one of another project's
// instance is refused naming the project; restoreBackup's backupdrBackup is
// resolved the same way.
func TestPointInTimeRestoreResolvesTheDatasource(t *testing.T) {
	srv := NewServer("", newFake())
	srv.SetBackupDR(fakeBackupDR{})
	if err := srv.EnableBackups(&fakeDumper{data: map[string]string{}}, t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	svc, err := sqladmin.NewService(context.Background(), option.WithEndpoint(ts.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	const vault = "projects/proj/locations/us-central1/backupVaults/vault/dataSources/"
	for _, c := range []struct {
		ds   string
		code int
		want string
	}{
		{vault + "missing", 404, "does not exist"},
		{vault + "other", 501, "projects/elsewhere"},
	} {
		_, err := svc.Instances.PointInTimeRestore("projects/proj", &sqladmin.PointInTimeRestoreContext{Datasource: c.ds,
			TargetInstance: "target", PointInTime: "2026-01-01T00:00:00Z"}).Do()
		var ge *googleapi.Error
		if !errors.As(err, &ge) || ge.Code != c.code || !strings.Contains(ge.Message, c.want) {
			t.Errorf("%s: %v, want %d with %q", c.ds, err, c.code, c.want)
		}
	}
	for _, c := range []struct {
		backup string
		code   int
		want   string
	}{
		{"projects/proj/instances/pg/backups/1", 400, "not a Backup and DR backup"},
		{vault + "d/backups/missing", 404, "does not exist"},
		{vault + "d/backups/b", 404, "has no data"},
	} {
		_, err := svc.Instances.RestoreBackup("proj", "pg", &sqladmin.InstancesRestoreBackupRequest{BackupdrBackup: c.backup}).Do()
		var ge *googleapi.Error
		if !errors.As(err, &ge) || ge.Code != c.code || !strings.Contains(ge.Message, c.want) {
			t.Errorf("restoreBackup %s: %v, want %d with %q", c.backup, err, c.code, c.want)
		}
	}
}
