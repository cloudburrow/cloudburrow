//go:build compat

package compat

import (
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// Cloud SQL for PostgreSQL across a restart (#701), as the MySQL probe does
// it. The fixture is written by a setup test CI runs alone just before
// `stop`, not by the suite: TestStateRestoresCloudSQL's `state load`
// replaces the database's contents, so a row the suite left could depend on
// test order.
const (
	envCloudSQLSetup  = "CLOUDBURROW_TEST_CLOUDSQL_SETUP"
	envCloudSQLExpect = "CLOUDBURROW_TEST_CLOUDSQL_EXPECT"
	cloudSQLProbeRow  = "written-before-stop"
)

// TestCloudSQLRestartSetup leaves a table with one row for
// TestCloudSQLAcrossRestart.
func TestCloudSQLRestartSetup(t *testing.T) {
	if os.Getenv(envCloudSQLSetup) == "" {
		t.Skipf("%s is not set: CI runs this alone, just before stop", envCloudSQLSetup)
	}
	h := New(t)
	c := pgConnect(t, h, "cloudburrow")
	for _, q := range []string{
		"DROP TABLE IF EXISTS restart_probe",
		"CREATE TABLE restart_probe (v text NOT NULL)",
		"INSERT INTO restart_probe VALUES ('" + cloudSQLProbeRow + "')",
	} {
		if _, err := c.Exec(h.Context(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// TestCloudSQLAcrossRestart measures durability: after stop/up in
// persistent mode the row is there, from the PVC; in ephemeral mode the
// server starts empty and the table does not exist.
func TestCloudSQLAcrossRestart(t *testing.T) {
	expect := os.Getenv(envCloudSQLExpect)
	if expect == "" {
		t.Skipf("%s is not set: this runs only after CI restarts the instance", envCloudSQLExpect)
	}
	h := New(t)
	c := pgConnect(t, h, "cloudburrow")
	var v string
	err := c.QueryRow(h.Context(), "SELECT v FROM restart_probe").Scan(&v)
	switch expect {
	case "present":
		if err != nil || v != cloudSQLProbeRow {
			t.Fatalf("persistent mode after stop/up: %q, %v; want the row written before stop", v, err)
		}
	case "absent":
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "42P01" {
			t.Fatalf("ephemeral mode after stop/up: %q, %v; want no table (42P01 undefined_table)", v, err)
		}
	default:
		t.Fatalf("%s must be present or absent, not %q", envCloudSQLExpect, expect)
	}
}
