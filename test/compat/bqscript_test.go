//go:build compat

package compat

import (
	"strings"
	"testing"
)

// TestBqRunsAScriptWithControlFlow (#1201): bq query runs a script with
// DECLARE, WHILE and IF / ELSE, which the front carries out statement by
// statement, and prints the rows of the statement that ran last; the
// script's job is a SCRIPT parent whose children the official Go client
// lists (TestBigQueryScriptChildJobs).
//
// covers: bigquery.jobs.insert, bigquery.jobs.get, bigquery.jobs.getQueryResults
func TestBqRunsAScriptWithControlFlow(t *testing.T) {
	h := New(t)
	vars := bqEnv(t, h)
	b := newBqSession(t, vars, "--nouse_google_auth", bqToken)
	script := "DECLARE i INT64 DEFAULT 0;\n" +
		"WHILE i < 3 DO\n  SET i = i + 1;\nEND WHILE;\n" +
		"IF i = 3 THEN\n  SELECT 'three' AS s, i AS n;\nELSE\n  SELECT 'other' AS s, i AS n;\nEND IF"
	out := b.must("query", "--nouse_legacy_sql", "--format=json", script)
	t.Logf("bq query printed:\n%s", out)
	if !strings.Contains(out, `"s":"three"`) || !strings.Contains(out, `"n":"3"`) || strings.Contains(out, "other") {
		t.Errorf("bq query of the script printed %q, want the IF branch's row three, 3", out)
	}
}
