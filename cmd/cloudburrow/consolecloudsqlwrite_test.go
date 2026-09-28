package main

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// TestCloudSQLReadWriteModeIsOfferedWhereTheEditorIs (#995). Both Cloud SQL
// screens offer the read-write mode on a database's page and its tables'
// pages, where the editor runs against that database, named in the
// confirmation; nowhere else — not deeper, and not on MySQL's own system
// databases, which the console does not open. The mode's words say who the
// statement runs as, and the MySQL mode that its read-only account is not
// the one that writes.
func TestCloudSQLReadWriteModeIsOfferedWhereTheEditorIs(t *testing.T) {
	for _, c := range []struct {
		name   string
		sw     console.StatementWriter
		engine string
	}{
		{"postgres", cloudSQLProvider{endpoint: "127.0.0.1:1"}, "PostgreSQL"},
		{"mysql", unreachableMySQL(), "MySQL"},
	} {
		for _, path := range [][]string{{"app"}, {"app", "widgets"}} {
			spec := c.sw.WriteSpec(path)
			if spec == nil {
				t.Errorf("%s %v offers no read-write mode", c.name, path)
				continue
			}
			if want := "database app on the local " + c.engine + " server"; spec.Target != want {
				t.Errorf("%s %v confirms writes to %q, want %q", c.name, path, spec.Target, want)
			}
			if spec.Run != "Run statement" || spec.Placeholder == "" || spec.Confirm == "" {
				t.Errorf("%s %v mode is %+v, want its own Run, Confirm and Placeholder", c.name, path, spec)
			}
			if !strings.Contains(spec.Hint, "runs as "+components.CloudSQLUser) ||
				!strings.Contains(spec.Confirm, components.CloudSQLUser) {
				t.Errorf("%s %v mode does not say it writes as %s: %+v", c.name, path, components.CloudSQLUser, spec)
			}
		}
		for _, path := range [][]string{nil, {"app", "widgets", "id"}} {
			if spec := c.sw.WriteSpec(path); spec != nil {
				t.Errorf("%s %v offers a read-write mode: %+v", c.name, path, spec)
			}
		}
	}
	mysql := unreachableMySQL()
	for _, system := range []string{"mysql", "sys", "Performance_Schema", "information_schema"} {
		if spec := mysql.WriteSpec([]string{system}); spec != nil {
			t.Errorf("MySQL's own %s database offers a read-write mode", system)
		}
	}
	if spec := mysql.WriteSpec([]string{"app"}); !strings.Contains(spec.Hint, mysqlConsoleReader+", is not used") {
		t.Errorf("the MySQL mode's hint %q does not say the read-only account is not the one that writes", spec.Hint)
	}
}

// TestCloudSQLWritesRefuseBeforeReachingTheServer (#995). What the
// read-write editors refuse on their own — no database, no statement, a
// statement past the limit, a MySQL system database, a MySQL screen without
// the application user's password — is refused with that reason before any
// connection, so the answer does not turn into "cannot open" when the server
// is down. Anything else reaches the server: here, one that is not there.
func TestCloudSQLWritesRefuseBeforeReachingTheServer(t *testing.T) {
	ctx := context.Background()
	pg := cloudSQLProvider{endpoint: "127.0.0.1:1"}
	my := unreachableMySQL()
	long := strings.Repeat("x", maxCloudSQLWriteBytes+1)
	for _, c := range []struct {
		what string
		err  error
		want string
	}{
		{"postgres no database", func() error { _, err := pg.WriteReport(ctx, "demo", nil, "SELECT 1"); return err }(), "a database is required"},
		{"postgres empty", func() error { _, err := pg.WriteReport(ctx, "demo", []string{"app"}, "  \n"); return err }(), "a statement is required"},
		{"postgres too long", func() error { _, err := pg.Write(ctx, "demo", []string{"app"}, long); return err }(), "accepts at most"},
		{"mysql no database", func() error { _, err := my.WriteReport(ctx, "demo", nil, "SELECT 1"); return err }(), "a database is required"},
		{"mysql system database", func() error {
			_, err := my.WriteReport(ctx, "demo", []string{"mysql"}, "DELETE FROM user")
			return err
		}(), "system databases"},
		{"mysql empty", func() error { _, err := my.Write(ctx, "demo", []string{"app"}, ""); return err }(), "a statement is required"},
		{"mysql too long", func() error { _, err := my.Write(ctx, "demo", []string{"app"}, long); return err }(), "accepts at most"},
		{"mysql without the app password", func() error {
			p := newCloudSQLMySQLProvider("127.0.0.1:1", components.MySQLCredentials{RootPassword: "r"})
			_, err := p.WriteReport(ctx, "demo", []string{"app"}, "CREATE TABLE t (id INT)")
			return err
		}(), "does not have cloudburrow's password"},
	} {
		if c.err == nil || !strings.Contains(c.err.Error(), c.want) {
			t.Errorf("%s: %v, want an error containing %q", c.what, c.err, c.want)
		}
		if c.err != nil && strings.Contains(c.err.Error(), "cannot open") {
			t.Errorf("%s reached for the server before refusing: %v", c.what, c.err)
		}
	}
	for name, err := range map[string]error{
		"postgres": func() error {
			_, err := pg.WriteReport(ctx, "demo", []string{"app"}, "CREATE TABLE t (id int)")
			return err
		}(),
		"mysql": func() error {
			_, err := my.WriteReport(ctx, "demo", []string{"app"}, "CREATE TABLE t (id INT)")
			return err
		}(),
	} {
		if err == nil {
			t.Errorf("%s: a write to a server that is not there succeeded", name)
		}
	}
}

// TestPostgresWriteReportIsPostgreSQLsCommandTag (#995). A committed
// statement is answered with PostgreSQL's own command tag, and with the rows
// it affected where the tag counts them — never "0 rows affected" for a
// CREATE TABLE, which changes none.
func TestPostgresWriteReportIsPostgreSQLsCommandTag(t *testing.T) {
	for tag, want := range map[string]string{
		"INSERT 0 2":   "Committed. PostgreSQL answered INSERT 0 2: 2 rows affected.",
		"UPDATE 1":     "Committed. PostgreSQL answered UPDATE 1: 1 row affected.",
		"DELETE 0":     "Committed. PostgreSQL answered DELETE 0: 0 rows affected.",
		"MERGE 3":      "Committed. PostgreSQL answered MERGE 3: 3 rows affected.",
		"CREATE TABLE": "Committed. PostgreSQL answered CREATE TABLE.",
		"DROP TABLE":   "Committed. PostgreSQL answered DROP TABLE.",
		"SELECT 4":     "Committed. PostgreSQL answered SELECT 4; Read-only shows a query's rows, so run it there.",
		"":             "Committed.",
	} {
		if got := postgresWriteReport(pgconn.NewCommandTag(tag)); got != want {
			t.Errorf("tag %q is reported %q, want %q", tag, got, want)
		}
	}
}
