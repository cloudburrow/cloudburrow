//go:build compat

package compat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
)

// consoleMySQLQuery posts one statement to the Cloud SQL for MySQL screen's
// query editor, as the page does, and returns the status and the body.
func consoleMySQLQuery(t *testing.T, addr, project, database, statement string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"Path": []string{database}, "Statement": statement})
	return consoleDo(t, addr, http.MethodPost, "/api/query/cloudsql-mysql?project="+url.QueryEscape(project), string(body))
}

// consoleSQLRow is one row of a console listing.
type consoleSQLRow struct {
	Name   string
	Fields map[string]string
}

// consoleMySQLSections reads a Cloud SQL for MySQL page through the console
// API, as the page does: each section's rows by the section's ID. A page or a
// section that could not be read fails the test with its reason.
func consoleMySQLSections(t *testing.T, addr, project string, path ...string) map[string][]consoleSQLRow {
	t.Helper()
	q := url.Values{"project": {project}, "name": path}
	code, body := consoleDo(t, addr, http.MethodGet, "/api/detail/cloudsql-mysql?"+q.Encode(), "")
	var page struct {
		Unavailable string
		Sections    []struct {
			ID      string
			Listing struct {
				Unavailable string
				Items       []consoleSQLRow
			}
		}
	}
	if code != http.StatusOK || json.Unmarshal([]byte(body), &page) != nil || page.Unavailable != "" {
		t.Fatalf("console detail %v = %d: %s", path, code, body)
	}
	out := map[string][]consoleSQLRow{}
	for _, s := range page.Sections {
		if s.Listing.Unavailable != "" {
			t.Errorf("section %s of %v: %s", s.ID, path, s.Listing.Unavailable)
		}
		out[s.ID] = s.Listing.Items
	}
	return out
}

// mysqlDBNamed is the application's user connected to database, the way an
// application given the database's name would connect.
func mysqlDBNamed(t *testing.T, h *Harness, database string) *sql.DB {
	t.Helper()
	cfg := mysql.NewConfig()
	cfg.User, cfg.Passwd, cfg.Net, cfg.Addr, cfg.DBName = "cloudburrow", os.Getenv(EnvMySQLPassword), "tcp", h.Endpoint(EnvMySQL), database
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestConsoleCloudSQLMySQLSchemaAndReadOnlyQuery.
//
// The Cloud SQL for MySQL screen's schema browser and SQL editor (#868),
// against the instance's MySQL, with a table go-sql-driver/mysql created as
// the application's user. The console lists the server (its version, in the
// listing's note) and its database; the database's page lists the table, and
// the table's page its columns and primary key as information_schema holds
// them. The editor returns the rows a SELECT reads, and MySQL itself refuses
// every write, DDL statement and read of a database the query was not
// scoped to — including the DDL that START TRANSACTION READ ONLY alone lets
// through, because MySQL commits implicitly before it. After all of them the
// table still holds exactly its two rows, read back by the driver.
func TestConsoleCloudSQLMySQLSchemaAndReadOnlyQuery(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	db := mysqlDB(t, h)
	ctx := h.Context()
	project := h.Project()

	table := "console_" + strings.NewReplacer("cb-test-", "", "-", "_").Replace(project)
	t.Cleanup(func() { _, _ = db.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+table) })
	for _, q := range []string{
		"CREATE TABLE " + table + " (id INT PRIMARY KEY, name VARCHAR(40) NOT NULL, note TEXT NULL)",
		"INSERT INTO " + table + " VALUES (1, 'sprocket', NULL), (2, 'gear', 'teeth')",
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	// The list: the server named in its note, and the application's database.
	var list struct {
		Note  string
		Items []struct{ Name string }
	}
	code, out := consoleDo(t, addr, http.MethodGet, "/api/resources/cloudsql-mysql?project="+project, "")
	if code != http.StatusOK || json.Unmarshal([]byte(out), &list) != nil {
		t.Fatalf("console list = %d: %s", code, out)
	}
	var version string
	if err := db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list.Note, "MySQL "+version) {
		t.Errorf("the listing's note %q does not name the server, MySQL %s", list.Note, version)
	}
	listed := map[string]bool{}
	for _, it := range list.Items {
		listed[it.Name] = true
	}
	if !listed["cloudburrow"] {
		t.Errorf("the console lists %v, without the application's database cloudburrow", list.Items)
	}
	for _, system := range []string{"mysql", "information_schema", "performance_schema", "sys"} {
		if listed[system] {
			t.Errorf("the console lists MySQL's own %s database", system)
		}
	}

	// The database's page lists the table; the table's page its columns.
	tables := consoleMySQLSections(t, addr, project, "cloudburrow")["tables"]
	if !slices.ContainsFunc(tables, func(r consoleSQLRow) bool { return r.Name == table }) {
		t.Errorf("the database's Tables section does not list %s: %v", table, tables)
	}
	sections := consoleMySQLSections(t, addr, project, "cloudburrow", table)
	columns := sections["columns"]
	if indexes := sections["indexes"]; len(indexes) != 1 || indexes[0].Name != "PRIMARY" || indexes[0].Fields["Columns"] != "id" {
		t.Errorf("the table's indexes = %v, want PRIMARY on id", indexes)
	}
	want := map[string]map[string]string{
		"id":   {"Type": "int", "Nullable": "NO", "Key": "PRI"},
		"name": {"Type": "varchar(40)", "Nullable": "NO"},
		"note": {"Type": "text", "Nullable": "YES"},
	}
	if len(columns) != len(want) {
		t.Errorf("the table's page lists %d columns, want %d: %v", len(columns), len(want), columns)
	}
	for _, c := range columns {
		for field, v := range want[c.Name] {
			if c.Fields[field] != v {
				t.Errorf("column %s %s = %q, want %q", c.Name, field, c.Fields[field], v)
			}
		}
	}

	// A SELECT reads the rows, NULL as an em dash.
	code, out = consoleMySQLQuery(t, addr, project, "cloudburrow", "SELECT id, name, note FROM "+table+" ORDER BY id")
	var result struct {
		Listing struct {
			NameColumn string
			Items      []struct {
				Name   string
				Fields map[string]string
			}
		}
	}
	if code != http.StatusOK || json.Unmarshal([]byte(out), &result) != nil {
		t.Fatalf("console SELECT = %d: %s", code, out)
	}
	rows := result.Listing.Items
	if len(rows) != 2 || rows[0].Name != "1" || rows[0].Fields["name"] != "sprocket" ||
		rows[0].Fields["note"] != "—" || rows[1].Fields["note"] != "teeth" {
		t.Errorf("console SELECT rows = %+v, want (1, sprocket, —) and (2, gear, teeth)", rows)
	}

	// MySQL refuses each of these, with its own error. DROP TABLE and CREATE
	// TABLE are the ones a read-only transaction alone would have run.
	for _, stmt := range []string{
		"INSERT INTO " + table + " VALUES (3, 'cog', NULL)",
		"UPDATE " + table + " SET name = 'x'",
		"DELETE FROM " + table,
		"DROP TABLE " + table,
		"CREATE TABLE console_escape (id INT)",
		"TRUNCATE TABLE " + table,
		"SELECT User FROM mysql.user",
	} {
		code, out := consoleMySQLQuery(t, addr, project, "cloudburrow", stmt)
		if code != http.StatusBadRequest {
			t.Errorf("%s: console = %d, want 400: %s", stmt, code, out)
			continue
		}
		if msg := consoleError(t, out); !strings.Contains(msg, "Error 1142") {
			t.Errorf("%s: refusal %q is not MySQL's command-denied error 1142", stmt, msg)
		}
	}
	// A system database is refused before MySQL is asked.
	code, out = consoleMySQLQuery(t, addr, project, "mysql", "SELECT 1")
	if code != http.StatusBadRequest || !strings.Contains(consoleError(t, out), "system databases") {
		t.Errorf("a query of the mysql database = %d: %s, want the console's refusal", code, out)
	}

	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil || n != 2 {
		t.Errorf("after the refused statements the table has %d rows (%v), want 2", n, err)
	}
	var escaped int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = 'cloudburrow' AND TABLE_NAME = 'console_escape'").Scan(&escaped); err != nil || escaped != 0 {
		t.Errorf("console_escape exists after a refused CREATE TABLE (%d, %v)", escaped, err)
	}
}

// TestConsoleCloudSQLMySQLCreateAndDropDatabase.
//
// The Cloud SQL for MySQL screen's create and drop, as the PostgreSQL
// screen's (#699, #868). A database the console created is one the
// application's own user can open and create a table in with
// go-sql-driver/mysql, so the console granted it; a console drop removes it.
// Dropping the database the server was initialised with, or a system
// database, is refused with the provider's message, and cloudburrow is still
// there.
func TestConsoleCloudSQLMySQLCreateAndDropDatabase(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	ctx := h.Context()
	project := h.Project()
	app := mysqlDB(t, h)

	name := "console_" + strings.NewReplacer("cb-test-", "", "-", "_").Replace(project)
	t.Cleanup(func() {
		_, _ = consoleDo(t, addr, http.MethodDelete,
			"/api/resources/cloudsql-mysql?project="+project+"&name="+url.QueryEscape(name), "")
	})
	exists := func(db string) bool {
		t.Helper()
		var got string
		err := app.QueryRowContext(ctx, "SELECT SCHEMA_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?", db).Scan(&got)
		if errors.Is(err, sql.ErrNoRows) {
			return false
		}
		if err != nil {
			t.Fatalf("reading information_schema.SCHEMATA: %v", err)
		}
		return true
	}

	code, out := consoleDo(t, addr, http.MethodPost, "/api/resources/cloudsql-mysql?project="+project,
		fmt.Sprintf(`{"database":%q}`, name))
	if code != http.StatusOK {
		t.Fatalf("console create = %d: %s", code, out)
	}
	if !exists(name) {
		t.Fatalf("the application's user cannot see %s after the console created it", name)
	}
	// A real database the application can use: its user opens it by name and
	// creates a table in it.
	db := mysqlDBNamed(t, h, name)
	if _, err := db.ExecContext(ctx, "CREATE TABLE made_by_the_app (id INT)"); err != nil {
		t.Fatalf("the application's user cannot create a table in the console-created database: %v", err)
	}
	var current string
	if err := db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&current); err != nil || current != name {
		t.Fatalf("DATABASE() = %q, %v; want %q", current, err, name)
	}
	_ = db.Close()

	code, out = consoleDo(t, addr, http.MethodDelete,
		"/api/resources/cloudsql-mysql?project="+project+"&name="+url.QueryEscape(name), "")
	if code != http.StatusOK {
		t.Fatalf("console drop = %d: %s", code, out)
	}
	if exists(name) {
		t.Errorf("%s is still in information_schema.SCHEMATA after a console drop", name)
	}

	for db, want := range map[string]string{
		"cloudburrow": `"cloudburrow" is the database the server was initialised with and cannot be dropped here`,
		"mysql":       `"mysql" is one of MySQL's own system databases, which this console does not open`,
	} {
		code, out = consoleDo(t, addr, http.MethodDelete,
			"/api/resources/cloudsql-mysql?project="+project+"&name="+db, "")
		if code != http.StatusBadRequest {
			t.Errorf("console drop of %s = %d, want 400: %s", db, code, out)
			continue
		}
		if msg := consoleError(t, out); msg != want {
			t.Errorf("refusal = %q, want %q", msg, want)
		}
	}
	if !exists("cloudburrow") {
		t.Error("the initial database is gone after a refused drop")
	}
}
