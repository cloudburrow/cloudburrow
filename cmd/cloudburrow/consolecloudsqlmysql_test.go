package main

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/components"
)

// unreachableMySQL is a screen whose server address nothing listens on, so a
// refusal that reaches MySQL fails differently from one that does not.
func unreachableMySQL() cloudSQLMySQLProvider {
	return newCloudSQLMySQLProvider("127.0.0.1:1", components.MySQLCredentials{Password: "p", RootPassword: "r"})
}

// TestCloudSQLMySQLRefusesBeforeReachingTheServer (#868). The initial
// database, MySQL's own system databases and names that are not plain
// identifiers are refused with a message saying which, before any
// connection: a refusal that depended on the server would turn into
// "cannot reach MySQL" whenever it was down, and the system databases hold
// the password hashes the query account would be granted SELECT on.
func TestCloudSQLMySQLRefusesBeforeReachingTheServer(t *testing.T) {
	ctx := context.Background()
	p := unreachableMySQL()

	for _, c := range []struct {
		what string
		err  error
		want string
	}{
		{"drop cloudburrow", p.Delete(ctx, "demo", "cloudburrow"), "the database the server was initialised with"},
		{"drop mysql", p.Delete(ctx, "demo", "mysql"), "system databases"},
		{"drop Performance_Schema", p.Delete(ctx, "demo", "Performance_Schema"), "system databases"},
		{"drop a bad name", p.Delete(ctx, "demo", "x`; DROP DATABASE y"), "only lowercase letters"},
		{"create sys", func() error { _, err := p.Create(ctx, "demo", map[string]string{"database": "sys"}); return err }(), "system databases"},
		{"create a bad name", func() error { _, err := p.Create(ctx, "demo", map[string]string{"database": "9lives"}); return err }(), "cannot start with a digit"},
		{"create no name", func() error { _, err := p.Create(ctx, "demo", map[string]string{}); return err }(), "a database name is required"},
		{"query mysql", func() error { _, err := p.Query(ctx, "demo", []string{"mysql"}, "SELECT 1"); return err }(), "system databases"},
		{"query no database", func() error { _, err := p.Query(ctx, "demo", nil, "SELECT 1"); return err }(), "a database is required"},
		{"query without a reader password", func() error {
			_, err := cloudSQLMySQLProvider{endpoint: "127.0.0.1:1"}.Query(ctx, "demo", []string{"app"}, "SELECT 1")
			return err
		}(), "newCloudSQLMySQLProvider"},
		{"page information_schema", func() error { _, err := p.Page(ctx, "demo", []string{"information_schema"}, "0"); return err }(), "system databases"},
	} {
		if c.err == nil || !strings.Contains(c.err.Error(), c.want) {
			t.Errorf("%s: %v, want an error containing %q", c.what, c.err, c.want)
		}
		if c.err != nil && strings.Contains(c.err.Error(), "cannot reach") {
			t.Errorf("%s reached for the server before refusing: %v", c.what, c.err)
		}
	}

	d, err := p.Detail(ctx, "demo", []string{"mysql"})
	if err != nil || !strings.Contains(d.Unavailable, "system databases") {
		t.Errorf("the mysql database's page = %+v, %v; want it refused as a system database", d, err)
	}
	if d, _ := p.Detail(ctx, "demo", []string{"mysql", "user"}); !strings.Contains(d.Unavailable, "system databases") {
		t.Errorf("mysql.user's page = %+v; want it refused as a system database", d)
	}
	// An unreachable server is said to be unreachable, not listed as empty.
	if l, err := p.List(ctx, "demo"); err != nil || !strings.Contains(l.Unavailable, "cannot reach MySQL") {
		t.Errorf("the list with no server = %+v, %v; want it unavailable", l, err)
	}
}

// TestCloudSQLMySQLQueryAccountPassword: each screen gets its own password for
// the query account, written into CREATE USER as a quoted literal, so it must
// be one that needs no escaping.
func TestCloudSQLMySQLQueryAccountPassword(t *testing.T) {
	a, b := unreachableMySQL(), unreachableMySQL()
	if a.readerPassword == b.readerPassword {
		t.Error("two screens generated the same query-account password")
	}
	for _, pw := range []string{a.readerPassword, b.readerPassword} {
		if err := validMySQLPassword(pw); err != nil {
			t.Errorf("generated password %q: %v", pw, err)
		}
	}
	for _, bad := range []string{"", "it's", `back\slash`, "sp ace"} {
		if validMySQLPassword(bad) == nil {
			t.Errorf("validMySQLPassword accepted %q", bad)
		}
	}
	if got, want := mysqlAccount("o'brien"), `'o''brien'@'%'`; got != want {
		t.Errorf("mysqlAccount = %s, want %s", got, want)
	}
}

// TestHumanBytesReadsLikePgSizePretty: the MySQL screen's Size column in the
// units the PostgreSQL screen's pg_size_pretty uses.
func TestHumanBytesReadsLikePgSizePretty(t *testing.T) {
	for n, want := range map[int64]string{
		0: "0 bytes", 16384: "16 kB", 10239: "10239 bytes", 5 << 20: "5120 kB", 50 << 20: "50 MB", 20 << 30: "20 GB",
	} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
