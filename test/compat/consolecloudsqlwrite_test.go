//go:build compat

package compat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// consoleSQLRun posts one statement to a Cloud SQL screen's editor, as the
// page does, in mode ("" for the default, read-only).
func consoleSQLRun(t *testing.T, addr, service, project, database, mode, statement string) (int, string) {
	t.Helper()
	req := map[string]any{"Path": []string{database}, "Statement": statement}
	if mode != "" {
		req["Mode"] = mode
	}
	body, _ := json.Marshal(req)
	return consoleDo(t, addr, http.MethodPost, "/api/query/"+service+"?project="+url.QueryEscape(project), string(body))
}

// consoleSQLWrite runs statement in the read-write mode and returns the
// console's answer, failing the test on a refusal.
func consoleSQLWrite(t *testing.T, addr, service, project, database, statement string) string {
	t.Helper()
	code, out := consoleSQLRun(t, addr, service, project, database, "read-write", statement)
	if code != http.StatusOK {
		t.Fatalf("console read-write %q = %d: %s", statement, code, out)
	}
	var got struct{ Message string }
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.Message == "" {
		t.Fatalf("console read-write %q answered %s, want a message", statement, out)
	}
	return got.Message
}

// consoleSQLRefused runs statement in mode and returns the refusal, failing
// the test when it is not one.
func consoleSQLRefused(t *testing.T, addr, service, project, database, mode, statement string) string {
	t.Helper()
	code, out := consoleSQLRun(t, addr, service, project, database, mode, statement)
	if code != http.StatusBadRequest {
		t.Fatalf("console %s %q = %d, want 400: %s", mode, statement, code, out)
	}
	return consoleError(t, out)
}

// consoleWriteSpec is the read-write mode a Cloud SQL page offers, from the
// same detail route the page reads.
func consoleWriteSpec(t *testing.T, addr, service, project string, path ...string) *struct{ Target, Hint, Run string } {
	t.Helper()
	q := url.Values{"project": {project}, "name": path}
	code, body := consoleDo(t, addr, http.MethodGet, "/api/detail/"+service+"?"+q.Encode(), "")
	var page struct {
		Query *struct {
			Write *struct{ Target, Hint, Run string }
		}
	}
	if code != http.StatusOK || json.Unmarshal([]byte(body), &page) != nil {
		t.Fatalf("console detail %v = %d: %s", path, code, body)
	}
	if page.Query == nil {
		return nil
	}
	return page.Query.Write
}

// TestConsoleCloudSQLReadWriteEditor.
//
// The Cloud SQL for PostgreSQL editor's read-write mode (#995), through the
// console API, read back with pgx. The database's page offers the mode,
// naming the database. A CREATE TABLE sent in it is committed and answered
// with PostgreSQL's command tag; an INSERT of two rows is answered with two
// rows affected, an UPDATE with one and a DELETE with one, and pgx reads the
// table as each left it. A duplicate key is PostgreSQL's own 23505 error,
// and a two-statement write whose second fails leaves nothing of its first,
// because both ran in one transaction that was rolled back. The default mode
// is still read-only: the same INSERT there is PostgreSQL's read-only
// refusal and changes nothing. A DROP TABLE removes the table from
// pg_catalog.
func TestConsoleCloudSQLReadWriteEditor(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	ctx := h.Context()
	project := h.Project()
	pg := pgConnect(t, h, "cloudburrow")

	table := "console_rw_" + strings.NewReplacer("cb-test-", "", "-", "_").Replace(project)
	t.Cleanup(func() { _, _ = pg.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) })
	const service = "cloudsql"

	spec := consoleWriteSpec(t, addr, service, project, "cloudburrow")
	if spec == nil || spec.Target != "database cloudburrow on the local PostgreSQL server" || spec.Run != "Run statement" {
		t.Fatalf("the database's page offers the read-write mode %+v", spec)
	}

	rows := func() string {
		t.Helper()
		r, err := pg.Query(ctx, "SELECT id::text || '=' || name FROM "+table+" ORDER BY id")
		if err != nil {
			t.Fatalf("pgx reading %s: %v", table, err)
		}
		defer r.Close()
		var out []string
		for r.Next() {
			var s string
			if err := r.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		return strings.Join(out, ",")
	}

	for _, c := range []struct{ statement, want, rows string }{
		{"CREATE TABLE " + table + " (id integer PRIMARY KEY, name text NOT NULL)",
			"Committed. PostgreSQL answered CREATE TABLE.", ""},
		{"INSERT INTO " + table + " VALUES (1, 'one'), (2, 'two');",
			"Committed. PostgreSQL answered INSERT 0 2: 2 rows affected.", "1=one,2=two"},
		{"UPDATE " + table + " SET name = 'uno' WHERE id = 1",
			"Committed. PostgreSQL answered UPDATE 1: 1 row affected.", "1=uno,2=two"},
		{"DELETE FROM " + table + " WHERE id = 2",
			"Committed. PostgreSQL answered DELETE 1: 1 row affected.", "1=uno"},
	} {
		if got := consoleSQLWrite(t, addr, service, project, "cloudburrow", c.statement); got != c.want {
			t.Errorf("%s: answered %q, want %q", c.statement, got, c.want)
		}
		if got := rows(); got != c.rows {
			t.Errorf("after %s pgx reads %q, want %q", c.statement, got, c.rows)
		}
	}

	// PostgreSQL's own refusals, which change nothing.
	if msg := consoleSQLRefused(t, addr, service, project, "cloudburrow", "read-write",
		"INSERT INTO "+table+" VALUES (1, 'again')"); !strings.Contains(msg, "SQLSTATE 23505") {
		t.Errorf("a duplicate key was refused with %q, want PostgreSQL's 23505", msg)
	}
	if msg := consoleSQLRefused(t, addr, service, project, "cloudburrow", "read-write",
		"INSERT INTO "+table+" VALUES (3, 'three'); INSERT INTO "+table+" VALUES (1, 'dup')"); !strings.Contains(msg, "SQLSTATE 23505") {
		t.Errorf("the two-statement write was refused with %q, want PostgreSQL's 23505", msg)
	}
	if msg := consoleSQLRefused(t, addr, service, project, "cloudburrow", "",
		"INSERT INTO "+table+" VALUES (4, 'four')"); !strings.Contains(msg, "read-only transaction") {
		t.Errorf("an INSERT in the default mode was refused with %q, want PostgreSQL's read-only refusal", msg)
	}
	if got := rows(); got != "1=uno" {
		t.Errorf("after the refused writes pgx reads %q, want 1=uno alone", got)
	}

	if got := consoleSQLWrite(t, addr, service, project, "cloudburrow", "DROP TABLE "+table); got != "Committed. PostgreSQL answered DROP TABLE." {
		t.Errorf("DROP TABLE answered %q", got)
	}
	var n int
	if err := pg.QueryRow(ctx, "SELECT count(*) FROM pg_catalog.pg_tables WHERE tablename = $1", table).Scan(&n); err != nil || n != 0 {
		t.Errorf("pg_tables lists %s %d times after the console's DROP TABLE (%v)", table, n, err)
	}
}

// TestConsoleCloudSQLMySQLReadWriteEditor.
//
// The Cloud SQL for MySQL editor's read-write mode (#995), through the
// console API, read back with go-sql-driver/mysql as the application's user.
// The database's page offers the mode, naming the database. In it, the
// console's statements run as cloudburrow, the application's user, never as
// the read-only cloudburrow_console account: CREATE TABLE, an INSERT of two
// rows (answered 2 rows affected), an UPDATE, a DELETE and DROP TABLE each
// commit, and the driver reads the table as each left it; CURRENT_USER() in
// a table the write made names cloudburrow. A duplicate key is MySQL's own
// error 1062. The default mode is unchanged: the same INSERT and CREATE TABLE
// are still MySQL's command-denied 1142 for cloudburrow_console, so writing
// did not widen the read-only account, and nothing they name exists.
func TestConsoleCloudSQLMySQLReadWriteEditor(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	ctx := h.Context()
	project := h.Project()
	db := mysqlDB(t, h)
	const service = "cloudsql-mysql"

	table := "console_rw_" + strings.NewReplacer("cb-test-", "", "-", "_").Replace(project)
	t.Cleanup(func() { _, _ = db.ExecContext(context.Background(), "DROP TABLE IF EXISTS "+table) })

	spec := consoleWriteSpec(t, addr, service, project, "cloudburrow")
	if spec == nil || spec.Target != "database cloudburrow on the local MySQL server" || spec.Run != "Run statement" {
		t.Fatalf("the database's page offers the read-write mode %+v", spec)
	}
	rows := func() string {
		t.Helper()
		r, err := db.QueryContext(ctx, "SELECT CONCAT(id, '=', name) FROM "+table+" ORDER BY id")
		if err != nil {
			t.Fatalf("reading %s: %v", table, err)
		}
		defer r.Close()
		var out []string
		for r.Next() {
			var s string
			if err := r.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		return strings.Join(out, ",")
	}

	for _, c := range []struct{ statement, want, rows string }{
		{"CREATE TABLE " + table + " (id INT PRIMARY KEY, name VARCHAR(40) NOT NULL, who VARCHAR(300) NULL)",
			"Committed as cloudburrow. MySQL answered: 0 rows affected.", ""},
		{"INSERT INTO " + table + " (id, name, who) VALUES (1, 'one', CURRENT_USER()), (2, 'two', NULL);",
			"Committed as cloudburrow. MySQL answered: 2 rows affected.", "1=one,2=two"},
		{"UPDATE " + table + " SET name = 'uno' WHERE id = 1",
			"Committed as cloudburrow. MySQL answered: 1 row affected.", "1=uno,2=two"},
		{"DELETE FROM " + table + " WHERE id = 2",
			"Committed as cloudburrow. MySQL answered: 1 row affected.", "1=uno"},
	} {
		if got := consoleSQLWrite(t, addr, service, project, "cloudburrow", c.statement); got != c.want {
			t.Errorf("%s: answered %q, want %q", c.statement, got, c.want)
		}
		if got := rows(); got != c.rows {
			t.Errorf("after %s the driver reads %q, want %q", c.statement, got, c.rows)
		}
	}
	var who string
	if err := db.QueryRowContext(ctx, "SELECT who FROM "+table+" WHERE id = 1").Scan(&who); err != nil || !strings.HasPrefix(who, "cloudburrow@") {
		t.Errorf("the console's INSERT ran as %q (%v), want the application's user cloudburrow", who, err)
	}

	if msg := consoleSQLRefused(t, addr, service, project, "cloudburrow", "read-write",
		"INSERT INTO "+table+" (id, name) VALUES (1, 'again')"); !strings.Contains(msg, "Error 1062") {
		t.Errorf("a duplicate key was refused with %q, want MySQL's 1062", msg)
	}
	// The read-only editor is as it was: cloudburrow_console still holds
	// only SELECT and SHOW VIEW.
	for _, stmt := range []string{
		"INSERT INTO " + table + " (id, name) VALUES (5, 'five')",
		"CREATE TABLE " + table + "_escape (id INT)",
	} {
		if msg := consoleSQLRefused(t, addr, service, project, "cloudburrow", "", stmt); !strings.Contains(msg, "Error 1142") ||
			!strings.Contains(msg, "cloudburrow_console") {
			t.Errorf("%s in the default mode was refused with %q, want MySQL's 1142 for cloudburrow_console", stmt, msg)
		}
	}
	if got := rows(); got != "1=uno" {
		t.Errorf("after the refused writes the driver reads %q, want 1=uno alone", got)
	}

	if got := consoleSQLWrite(t, addr, service, project, "cloudburrow", "DROP TABLE "+table); got != "Committed as cloudburrow. MySQL answered: 0 rows affected." {
		t.Errorf("DROP TABLE answered %q", got)
	}
	for _, name := range []string{table, table + "_escape"} {
		var got string
		err := db.QueryRowContext(ctx, "SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = 'cloudburrow' AND TABLE_NAME = ?", name).Scan(&got)
		if !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("information_schema.TABLES has %s after the console's DROP TABLE and a refused CREATE (%q, %v)", name, got, err)
		}
	}
}
