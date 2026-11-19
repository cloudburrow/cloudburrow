package main

import (
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
)

// ACCEPT_EULA is set on a SQL Server instance's server only when its
// record names who accepted Microsoft's licence and when (#1916).
func TestSQLServerBackendEULA(t *testing.T) {
	spec := sqladmin.Spec{Name: "ms", DatabaseVersion: "SQLSERVER_2022_STANDARD", SAPassword: "Cb9!x"}
	for _, a := range []*sqladmin.EULAAcceptance{nil, {}, {AcceptedBy: "dev"}, {AcceptedAt: "now"}} {
		spec.MSSQLEULA = a
		b := sqlServerBackend(spec)
		if _, ok := b.Env["ACCEPT_EULA"]; ok {
			t.Errorf("acceptance %+v: ACCEPT_EULA set", a)
		}
		for _, l := range strings.Split(b.Manifest("ns", "inst"), "\n") {
			if strings.Contains(l, "ACCEPT_EULA") {
				t.Errorf("acceptance %+v: manifest has %q", a, l)
			}
		}
	}
	spec.MSSQLEULA = &sqladmin.EULAAcceptance{AcceptedBy: "dev", AcceptedAt: "2026-11-19T00:00:00Z", Via: sqladmin.EULAViaFlag}
	b := sqlServerBackend(spec)
	if b.Env["ACCEPT_EULA"] != "Y" || b.Env["MSSQL_PID"] != "Developer" || b.Port != 1433 ||
		!strings.Contains(b.Image, "mssql/server:2022-latest@sha256:") || b.MountPath != "/var/opt/mssql" {
		t.Errorf("accepted backend: %+v", b)
	}
}

// up's acceptance is recorded as the user's, with how it was given; none
// is made without the opt-in.
func TestMSSQLEULAOf(t *testing.T) {
	if a := mssqlEULAOf(config.Config{}, "now"); a != nil {
		t.Errorf("no opt-in: %+v", a)
	}
	cfg := config.Config{AcceptMSSQLEULA: true}
	cfg.Source.MSSQLEULA = "env"
	a := mssqlEULAOf(cfg, "2026-11-19T00:00:00Z")
	if a == nil || a.Via != sqladmin.EULAViaEnv || a.AcceptedBy == "" || a.AcceptedAt != "2026-11-19T00:00:00Z" {
		t.Errorf("env opt-in: %+v", a)
	}
	lines := mssqlEULALines(mssqlEULAStatus{Up: a, Instances: map[string]*sqladmin.EULAAcceptance{"ms": a, "old": nil}})
	if !strings.Contains(lines[0], "accepted by") || !strings.Contains(strings.Join(lines, "\n"), "old: no acceptance on record") {
		t.Errorf("status lines: %q", lines)
	}
	if l := mssqlEULALines(mssqlEULAStatus{}); !strings.Contains(l[0], "not accepted") {
		t.Errorf("status with none: %q", l)
	}
}
