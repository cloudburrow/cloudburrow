package sqladmin

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	sqladminapi "google.golang.org/api/sqladmin/v1"
)

func sqlServerClient(t *testing.T, prov Provisioner, state string) (*Server, *sqladminapi.Service) {
	t.Helper()
	srv := NewServer("", newFake())
	srv.EnableInstanceWrites(prov, state)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	svc, err := sqladminapi.NewService(context.Background(), option.WithEndpoint(ts.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	return srv, svc
}

func msInstance(name, version string) *sqladminapi.DatabaseInstance {
	return &sqladminapi.DatabaseInstance{Name: name, DatabaseVersion: version, RootPassword: "Str0ng!Passw0rd",
		Settings: &sqladminapi.Settings{Tier: "db-custom-2-7680"}}
}

// With no acceptance of Microsoft's licence, a SQL Server instance is
// refused with FAILED_PRECONDITION saying how to accept (#1916), whatever
// the node can run, and nothing is started or recorded.
func TestSQLServerRefusedWithoutEULA(t *testing.T) {
	prov := legacyProv{fakeProv: newFakeProv()}
	state := filepath.Join(t.TempDir(), "instances.json")
	_, svc := sqlServerClient(t, prov, state)
	for _, v := range SQLServerVersions() {
		_, err := svc.Instances.Insert("proj", msInstance("ms", v)).Do()
		var ge *googleapi.Error
		if err == nil || !errorsAs(err, &ge) || ge.Code != 412 {
			t.Fatalf("%s with no acceptance: %v, want 412", v, err)
		}
		for _, want := range []string{"--accept-mssql-eula", "CLOUDBURROW_ACCEPT_MSSQL_EULA=Y", MSSQLEULAURL, "never accepts it on your behalf"} {
			if !strings.Contains(ge.Message, want) {
				t.Errorf("%s refusal %q does not say %q", v, ge.Message, want)
			}
		}
	}
	if len(prov.started) != 0 {
		t.Errorf("servers started with no acceptance: %v", prov.started)
	}
	if b, err := os.ReadFile(state); err == nil && strings.Contains(string(b), "SQLSERVER") {
		t.Errorf("a SQL Server instance was recorded: %s", b)
	}
	// A header that is not exactly Y accepts nothing.
	call := svc.Instances.Insert("proj", msInstance("ms", "SQLSERVER_2022_STANDARD"))
	call.Header().Set(EULAHeader, "yes")
	if _, err := call.Do(); err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("%s: yes: %v, want 400", EULAHeader, err)
	}
}

func errorsAs(err error, ge **googleapi.Error) bool {
	e, ok := err.(*googleapi.Error)
	if ok {
		*ge = e
	}
	return ok
}

// An acceptance, `up`'s or the request's own, is recorded on the instance
// with who, when and how, and the server is then started (#1916).
func TestSQLServerEULARecorded(t *testing.T) {
	prov := legacyProv{fakeProv: newFakeProv()}
	state := filepath.Join(t.TempDir(), "instances.json")
	srv, svc := sqlServerClient(t, prov, state)

	call := svc.Instances.Insert("proj", msInstance("byconsole", "SQLSERVER_2022_EXPRESS"))
	call.Header().Set(EULAHeader, "Y")
	op, err := call.Do()
	if err != nil {
		t.Fatal(err)
	}
	wait(t, svc, "proj", op)

	srv.AcceptMSSQLEULA(&EULAAcceptance{AcceptedBy: "dev", AcceptedAt: "2026-11-19T10:00:00Z", Via: EULAViaFlag})
	op, err = svc.Instances.Insert("proj", msInstance("byflag", "SQLSERVER_2025_ENTERPRISE")).Do()
	if err != nil {
		t.Fatal(err)
	}
	wait(t, svc, "proj", op)

	b, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	var specs []Spec
	if err := json.Unmarshal(b, &specs); err != nil {
		t.Fatal(err)
	}
	got := map[string]Spec{}
	for _, sp := range specs {
		got[sp.Name] = sp
	}
	if a := got["byconsole"].MSSQLEULA; a == nil || a.Via != EULAViaConsole || a.AcceptedBy == "" || a.AcceptedAt == "" {
		t.Errorf("console acceptance recorded as %+v", a)
	}
	if a := got["byflag"].MSSQLEULA; a == nil || *a != (EULAAcceptance{AcceptedBy: "dev", AcceptedAt: "2026-11-19T10:00:00Z", Via: EULAViaFlag}) {
		t.Errorf("up's acceptance recorded as %+v", a)
	}
	for _, n := range []string{"byconsole", "byflag"} {
		if got[n].SAPassword == "" || got[n].RootPassword == "" {
			t.Errorf("%s: no sa or sqlserver password kept", n)
		}
	}
	prov.mu.Lock()
	_, started := prov.started["byflag"]
	prov.mu.Unlock()
	if !started {
		t.Error("the accepted instance's server was not started")
	}
	// The sa password is never answered.
	in, err := svc.Instances.Get("proj", "byflag").Do()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := in.MarshalJSON()
	if strings.Contains(string(raw), got["byflag"].SAPassword) || strings.Contains(string(raw), "Str0ng!Passw0rd") {
		t.Error("instances.get answers a password")
	}
}

// What a SQL Server instance does not serve here is 501, and so is any
// SQL Server instance on a node that cannot run linux/amd64 images.
func TestSQLServerLimits(t *testing.T) {
	prov := legacyProv{fakeProv: newFakeProv()}
	srv, svc := sqlServerClient(t, prov, "")
	srv.AcceptMSSQLEULA(&EULAAcceptance{AcceptedBy: "dev", AcceptedAt: "now", Via: EULAViaEnv})
	for name, in := range map[string]*sqladminapi.DatabaseInstance{
		"regional": {Settings: &sqladminapi.Settings{Tier: "t", AvailabilityType: "REGIONAL"}},
		"backups":  {Settings: &sqladminapi.Settings{Tier: "t", BackupConfiguration: &sqladminapi.BackupConfiguration{Enabled: true}}},
	} {
		in.Name, in.DatabaseVersion, in.RootPassword = name, "SQLSERVER_2019_WEB", "Str0ng!Passw0rd"
		if _, err := svc.Instances.Insert("proj", in).Do(); err == nil || !strings.Contains(err.Error(), "501") {
			t.Errorf("%s: %v, want 501", name, err)
		}
	}
	bad := msInstance("flags", "SQLSERVER_2019_WEB")
	bad.Settings.DatabaseFlags = []*sqladminapi.DatabaseFlags{{Name: "max_connections", Value: "5"}}
	if _, err := svc.Instances.Insert("proj", bad).Do(); err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("a PostgreSQL flag on SQL Server: %v, want 400", err)
	}
	nopw := msInstance("nopw", "SQLSERVER_2019_WEB")
	nopw.RootPassword = ""
	if _, err := svc.Instances.Insert("proj", nopw).Do(); err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("no rootPassword: %v, want 400", err)
	}
	if _, err := svc.Instances.Insert("proj", &sqladminapi.DatabaseInstance{Name: "rs", DatabaseVersion: "SQLSERVER_2022_STANDARD",
		InstanceType: "ON_PREMISES_INSTANCE", OnPremisesConfiguration: &sqladminapi.OnPremisesConfiguration{HostPort: "h:1433"}}).Do(); err == nil ||
		!strings.Contains(err.Error(), "501") {
		t.Errorf("a SQL Server source representation: %v, want 501", err)
	}

	arm := legacyProv{fakeProv: newFakeProv(), why: "the cluster's node is aarch64 with no linux/amd64 emulation"}
	srv2, svc2 := sqlServerClient(t, arm, "")
	srv2.AcceptMSSQLEULA(&EULAAcceptance{AcceptedBy: "dev", AcceptedAt: "now", Via: EULAViaEnv})
	if _, err := svc2.Instances.Insert("proj", msInstance("arm", "SQLSERVER_2022_STANDARD")).Do(); err == nil ||
		!strings.Contains(err.Error(), "501") || !strings.Contains(err.Error(), "aarch64") {
		t.Errorf("on a node that cannot run amd64: %v, want 501 naming why", err)
	}
	if err := sqlServerEngine(namedFake{fake: newFake(), name: "x", version: "SQLSERVER_2017_EXPRESS"}, "a backup"); err == nil {
		t.Error("backups of a SQL Server instance are not refused")
	}
}

// Each Google edition's local MSSQL_PID, per Microsoft's MSSQL_PID tables.
func TestMSSQLPID(t *testing.T) {
	for v, want := range map[string]string{
		"SQLSERVER_2017_EXPRESS": "Express", "SQLSERVER_2019_WEB": "Developer", "SQLSERVER_2022_STANDARD": "Developer",
		"SQLSERVER_2022_ENTERPRISE": "Developer", "SQLSERVER_2025_EXPRESS": "Express",
		"SQLSERVER_2025_STANDARD": "StandardDeveloper", "SQLSERVER_2025_ENTERPRISE": "EnterpriseDeveloper",
	} {
		if got, why := MSSQLPID(v); got != want || why == "" {
			t.Errorf("MSSQLPID(%s) = %q %q, want %q", v, got, why, want)
		}
	}
	if len(SQLServerVersions()) != 15 || IsSQLServer("SQLSERVER_2025_WEB") {
		t.Errorf("versions: %v", SQLServerVersions())
	}
}

// Google's SQL Server flags become sp_configure and trace flag statements.
func TestSQLServerFlagStatements(t *testing.T) {
	stmts, err := SetupStatements("pw'x", []Flag{{Name: "1222", Value: "on"}, {Name: "max degree of parallelism", Value: "4"},
		{Name: "cloudsql enable linked servers", Value: "on"}})
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(stmts, "\n")
	for _, want := range []string{"CREATE SERVER ROLE [CustomerDbRootRole]", "CREATE LOGIN [sqlserver] WITH PASSWORD = N'pw''x'",
		"DBCC TRACEON(1222, -1)", "EXEC sp_configure N'max degree of parallelism', 4", "RECONFIGURE WITH OVERRIDE"} {
		if !strings.Contains(all, want) {
			t.Errorf("setup lacks %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, "linked") {
		t.Errorf("Cloud SQL's own setting was applied:\n%s", all)
	}
	for _, f := range []Flag{{"user connections", "5"}, {"cross db ownership chaining", "on"}, {"fill factor (%)", "101"}, {"nope", "1"}} {
		if err := checkSQLServerFlags([]Flag{f}); err == nil {
			t.Errorf("%+v accepted", f)
		}
	}
	got, err := listFlags("SQLSERVER_2019_STANDARD")
	if err != nil {
		t.Fatal(err)
	}
	if items := got.(map[string]any)["items"].([]any); len(items) < 50 {
		t.Errorf("flags.list for SQL Server: %d flags", len(items))
	}
}
