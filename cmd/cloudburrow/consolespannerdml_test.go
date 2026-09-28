package main

import (
	"context"
	"strings"
	"testing"
)

// TestSpannerStatementClassifier: what each Spanner editor mode accepts
// (#798). A DML statement is refused on the read-only path, however it is
// dressed (comments, a statement hint, lower case); a SELECT is refused on the
// read-write path, so it is never wrapped in a write transaction; DDL is
// refused on both; and a keyword or semicolon inside a literal, a quoted
// identifier or a comment does not count.
func TestSpannerStatementClassifier(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		sql string
		// readOnly and readWrite are substrings of each path's refusal, or
		// empty where that path accepts the statement.
		readOnly, readWrite string
	}{
		{"SELECT 1", "", "switch the editor to Read-only"},
		{"select * from Widgets;", "", "switch the editor to Read-only"},
		{"(SELECT 1) UNION ALL (SELECT 2)", "", "switch the editor to Read-only"},
		{"WITH t AS (SELECT 1 AS x) SELECT x FROM t", "", "switch the editor to Read-only"},
		{"SELECT 'DELETE FROM Widgets'", "", "switch the editor to Read-only"},
		{"SELECT `delete` FROM Widgets -- ; DELETE FROM Widgets", "", "switch the editor to Read-only"},
		{"INSERT INTO Widgets (Id) VALUES (1)", "switch it to Read-write", ""},
		{"insert or update into Widgets (Id) values (1);", "switch it to Read-write", ""},
		{"UPDATE Widgets SET Name = 'a;b' WHERE Id = 1", "switch it to Read-write", ""},
		{"-- tidy up\nDELETE FROM Widgets WHERE true", "switch it to Read-write", ""},
		{"/* hint */ @{LOCK_SCANNED_RANGES=exclusive} UPDATE Widgets SET Name = 'x' WHERE true", "switch it to Read-write", ""},
		{"SELECT 1; DELETE FROM Widgets WHERE true", "switch it to Read-write", "one DML statement at a time"},
		{"INSERT INTO Widgets (Id) VALUES (1); INSERT INTO Widgets (Id) VALUES (2)", "switch it to Read-write", "one DML statement at a time"},
		{"CREATE TABLE T (Id INT64) PRIMARY KEY (Id)", "is DDL", "is DDL"},
		{"drop table Widgets", "is DDL", "is DDL"},
		{"ALTER TABLE Widgets ADD COLUMN C INT64", "is DDL", "is DDL"},
		{"EXPLAIN SELECT 1", "", "not EXPLAIN"},
		{"'unterminated", "", "unterminated"},
	} {
		err := readOnlySpanner(c.sql)
		checkRefusal(t, "read-only", c.sql, err, c.readOnly)
		_, err = writableSpanner(c.sql)
		checkRefusal(t, "read-write", c.sql, err, c.readWrite)
	}
}

func checkRefusal(t *testing.T, mode, sql string, err error, want string) {
	t.Helper()
	switch {
	case want == "" && err != nil:
		t.Errorf("%s refused %q: %v", mode, sql, err)
	case want != "" && err == nil:
		t.Errorf("%s accepted %q, want a refusal containing %q", mode, sql, want)
	case want != "" && !strings.Contains(err.Error(), want):
		t.Errorf("%s refused %q with %q, want %q", mode, sql, err, want)
	}
}

// TestSpannerWriteIsBounded: a read-write statement past the limit is refused
// with the limit named, and the terminator is not sent as part of the
// statement.
func TestSpannerWriteIsBounded(t *testing.T) {
	t.Parallel()
	long := "DELETE FROM Widgets WHERE Name = '" + strings.Repeat("x", maxSpannerDMLBytes) + "'"
	if _, err := writableSpanner(long); err == nil || !strings.Contains(err.Error(), "at most 16384") {
		t.Errorf("an over-long statement = %v, want the limit named", err)
	}
	got, err := writableSpanner("  DELETE FROM Widgets WHERE Id = 1 ;;\n")
	if err != nil || got != "DELETE FROM Widgets WHERE Id = 1" {
		t.Errorf("writableSpanner = %q, %v; want the statement without its terminator", got, err)
	}
}

// TestSpannerEditorRefusesBeforeSending: each mode's refusal comes before a
// client is opened, so the endpoint is never dialled. The provider here has
// no endpoint at all; reaching Spanner would fail with a different message.
func TestSpannerEditorRefusesBeforeSending(t *testing.T) {
	t.Parallel()
	p := spannerProvider{endpoint: "127.0.0.1:1"}
	path := []string{"main", "orders"}
	ctx := context.Background()

	if _, err := p.Query(ctx, "demo", path, "DELETE FROM Widgets WHERE true"); err == nil ||
		!strings.Contains(err.Error(), "the editor is read-only") {
		t.Errorf("a DELETE on the read-only path = %v, want the read-only refusal", err)
	}
	if _, err := p.Write(ctx, "demo", path, "SELECT * FROM Widgets"); err == nil ||
		!strings.Contains(err.Error(), "switch the editor to Read-only") {
		t.Errorf("a SELECT on the read-write path = %v, want the read-write refusal", err)
	}
	if spec := p.WriteSpec([]string{"main"}); spec != nil {
		t.Errorf("an instance offers a write mode: %+v", spec)
	}
	if spec := p.WriteSpec(path); spec == nil || !strings.Contains(spec.Target, "orders") {
		t.Errorf("a database's write mode = %+v, want it to name the database", spec)
	}
}
