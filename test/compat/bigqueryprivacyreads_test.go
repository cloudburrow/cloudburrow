//go:build compat

package compat

import (
	"net/http"
	"strings"
	"testing"
)

// TestBigQueryPrivacyPolicyReads (#1502): the reads of an aggregation
// threshold or join restriction view that #1426 left 501.
//
//   - SELECT WITH AGGREGATION_THRESHOLD is part of every SELECT's syntax
//     (query-syntax#agg_threshold_clause), so in a parenthesized subquery,
//     a CTE's query, the query after a WITH list and CREATE TABLE ... AS's
//     query it drops the groups below the threshold, as for a direct read,
//     and the outer query reads its rows; its errors are the same 400s.
//   - A script reads the view a statement at a time: each statement is
//     checked in its turn, so the statements before a refused one ran.
//   - A join that is not INNER, by each join_condition's documented wording
//     (analysis-rules#join_restriction_policy): JOIN_BLOCKED "can't be
//     joined along any column"; JOIN_ALL's columns "must be inner joined
//     upon", so a LEFT JOIN on them is refused; JOIN_NOT_REQUIRED allows a
//     join on its join_allowed_columns of any kind and refuses a CROSS JOIN.
//
// What BigQuery does not document stays 501: a read of the view outside
// its own SELECT WITH AGGREGATION_THRESHOLD (in a subquery or CTE whose
// rows are not aggregated, through another view), and a JOIN_ANY met only
// by a LEFT JOIN.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.jobs.getQueryResults
func TestBigQueryPrivacyPolicyReads(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	d := ds.DatasetID
	run := func(sql string) {
		t.Helper()
		if err := bqRun(ctx, c, sql, false); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	run("CREATE TABLE `" + d + ".ExamTable` AS (" + privacyExam + ")")
	run("CREATE TABLE `" + d + ".StudentTable` AS (SELECT 'Ivanov' AS last_name, 490 AS test_score UNION ALL " +
		"SELECT 'Wang', 500 UNION ALL SELECT 'Hansen', 510 UNION ALL SELECT 'Silva', 550 UNION ALL SELECT 'Devi', 580)")
	view := func(name, policy string) string {
		run("CREATE VIEW `" + d + "." + name + "` OPTIONS (privacy_policy = '" + policy + "') AS (SELECT * FROM `" + d + ".ExamTable`)")
		return "`" + d + "." + name + "`"
	}
	exam := view("ExamView", `{"aggregation_threshold_policy": {"threshold": 3, "privacy_unit_column": "last_name"}}`)
	allV := view("AllView", `{"join_restriction_policy": {"join_condition": "JOIN_ALL", "join_allowed_columns": ["test_score", "last_name"]}}`)
	anyV := view("AnyView", `{"join_restriction_policy": {"join_condition": "JOIN_ANY", "join_allowed_columns": ["test_score"]}}`)
	notReq := view("NotRequired", `{"join_restriction_policy": {"join_condition": "JOIN_NOT_REQUIRED", "join_allowed_columns": ["test_score"]}}`)
	blocked := view("Blocked", `{"join_restriction_policy": {"join_condition": "JOIN_BLOCKED"}}`)
	both := view("Both", `{"aggregation_threshold_policy": {"threshold": 3, "privacy_unit_column": "last_name"}, `+
		`"join_restriction_policy": {"join_condition": "JOIN_ANY", "join_allowed_columns": ["test_score", "test_id"]}}`)
	student := "`" + d + ".StudentTable`"
	inner := "SELECT WITH AGGREGATION_THRESHOLD test_id, COUNT(DISTINCT last_name) AS n, AVG(test_score) AS avg_score FROM " +
		exam + " GROUP BY test_id"

	// The docs' rows: P91 3 510, U25 4 516, C83 (2 students) dropped.
	for _, insert := range []bool{false, true} {
		for what, sql := range map[string]string{
			"a subquery":           "SELECT * FROM (" + inner + ") WHERE n >= 3",
			"a CTE":                "WITH scores AS (" + inner + ") SELECT test_id, n, avg_score FROM scores",
			"a query after a WITH": "WITH ids AS (SELECT 'P91' AS id) SELECT WITH AGGREGATION_THRESHOLD test_id, COUNT(DISTINCT last_name), AVG(test_score) FROM " + exam + " GROUP BY test_id",
		} {
			got, err := privacyRows(t, c, h, sql, insert)
			if err != nil || strings.Join(got, "|") != "P91 3 510|U25 4 516" {
				t.Errorf("%s (insert %v): %v, %v; want P91 3 510, U25 4 516", what, insert, got, err)
			}
		}
	}
	got, err := privacyRows(t, c, h, "SELECT s.last_name, e.n FROM "+student+" s JOIN ("+inner+") e ON e.test_id = 'P91' WHERE s.last_name = 'Devi'", false)
	if err != nil || strings.Join(got, "|") != "Devi 3" {
		t.Errorf("a subquery joined: %v, %v; want Devi 3", got, err)
	}
	run("CREATE TABLE `" + d + ".Scores` AS " + inner)
	if got, err := privacyRows(t, c, h, "SELECT test_id, n FROM `"+d+".Scores`", false); err != nil || strings.Join(got, "|") != "P91 3|U25 4" {
		t.Errorf("CREATE TABLE ... AS SELECT WITH AGGREGATION_THRESHOLD: %v, %v; want P91 3, U25 4", got, err)
	}
	// The aggregation threshold and its JOIN_ANY in a subquery.
	got, err = privacyRows(t, c, h, "SELECT * FROM (SELECT WITH AGGREGATION_THRESHOLD test_id, COUNT(*) AS n FROM "+both+
		" JOIN "+student+" USING (test_score) GROUP BY test_id)", false)
	if err != nil || strings.Join(got, "|") != "U25 4" {
		t.Errorf("JOIN_ANY in a subquery: %v, %v; want U25 4", got, err)
	}

	// A script, a statement at a time: it writes the threshold's rows to a
	// table, and reads them with a variable.
	for _, insert := range []bool{false, true} {
		kept := "`" + d + ".Kept" + map[bool]string{false: "Q", true: "J"}[insert] + "`"
		got, err := privacyRows(t, c, h, "DECLARE least INT64 DEFAULT 4;\n"+
			"CREATE TABLE "+kept+" AS "+inner+";\n"+
			"SELECT test_id, n FROM "+kept+" WHERE n >= least", insert)
		if err != nil || strings.Join(got, "|") != "U25 4" {
			t.Errorf("a script (insert %v): %v, %v; want U25 4", insert, got, err)
		}
	}
	tbl := "`" + d + ".BeforeRefused`"
	_, err = privacyRows(t, c, h, "CREATE TABLE "+tbl+" AS SELECT 1 AS x;\nSELECT * FROM "+exam, false)
	wantReason(t, "a script's statement without the clause", err, http.StatusBadRequest, "invalidQuery")
	if err != nil && !strings.Contains(err.Error(), "You must use SELECT WITH AGGREGATION_THRESHOLD") {
		t.Errorf("a script's statement without the clause: %v", err)
	}
	if got, err := privacyRows(t, c, h, "SELECT x FROM "+tbl, false); err != nil || strings.Join(got, "|") != "1" {
		t.Errorf("the statement before the refused one: %v, %v; want it run", got, err)
	}
	_, err = privacyRows(t, c, h, "SELECT 1;\nSELECT * FROM "+allV+" INNER JOIN "+student+" USING (last_name)", false)
	if err == nil || !strings.Contains(err.Error(), "Joining must occur on all of the following columns") {
		t.Errorf("a script's join: %v, want JOIN_ALL's error", err)
	}

	// Joins that are not INNER.
	if got, err := privacyRows(t, c, h, "SELECT COUNT(*) FROM "+notReq+" LEFT JOIN "+student+" USING (test_score)", false); err != nil || strings.Join(got, "|") != "11" {
		t.Errorf("JOIN_NOT_REQUIRED LEFT JOIN on an allowed column: %v, %v; want 11", got, err)
	}
	for _, c2 := range []struct{ what, sql, want string }{
		{"JOIN_ALL by a LEFT JOIN", "SELECT * FROM " + allV + " e LEFT JOIN " + student + " s ON e.test_score = s.test_score AND e.last_name = s.last_name",
			"Joining must occur on all of the following columns [test_score, last_name] on table " + d + ".AllView."},
		{"JOIN_BLOCKED by a LEFT JOIN", "SELECT * FROM " + blocked + " LEFT JOIN " + student + " USING (test_score)",
			"Join occurring on a restricted column."},
		{"JOIN_BLOCKED by a CROSS JOIN", "SELECT * FROM " + blocked + " CROSS JOIN " + student,
			"Join occurring on a restricted column."},
		{"JOIN_NOT_REQUIRED by a RIGHT JOIN on another column", "SELECT * FROM " + notReq + " RIGHT JOIN " + student + " USING (last_name)",
			"Join occurring on a restricted column."},
		{"JOIN_NOT_REQUIRED by a CROSS JOIN", "SELECT * FROM " + notReq + " CROSS JOIN " + student,
			"Join occurring on a restricted column."},
		{"JOIN_ANY by a LEFT JOIN on another column", "SELECT * FROM " + anyV + " LEFT JOIN " + student + " USING (last_name)",
			"Joining must occur on at least one of the following columns [test_score] on table " + d + ".AnyView."},
		{"a subquery's lower threshold", "SELECT * FROM (SELECT WITH AGGREGATION_THRESHOLD OPTIONS(threshold=2) COUNT(*) FROM " + exam + ")", ""},
		{"a subquery projecting the privacy unit", "SELECT * FROM (SELECT WITH AGGREGATION_THRESHOLD last_name, COUNT(*) FROM " + exam + " GROUP BY last_name)", ""},
	} {
		_, err := privacyRows(t, c, h, c2.sql, false)
		wantReason(t, c2.what, err, http.StatusBadRequest, "invalidQuery")
		if err != nil && !strings.Contains(err.Error(), c2.want) {
			t.Errorf("%s: %v, want %q", c2.what, err, c2.want)
		}
	}

	// What BigQuery does not document stays 501.
	run("CREATE VIEW `" + d + ".Around` AS SELECT * FROM " + exam)
	for what, sql := range map[string]string{
		"a read in a subquery, not aggregated":     "SELECT * FROM (SELECT * FROM " + exam + ")",
		"a CTE read by the clause":                 "WITH e AS (SELECT * FROM " + exam + ") SELECT WITH AGGREGATION_THRESHOLD COUNT(*) FROM e",
		"a view reading the view":                  "SELECT * FROM `" + d + ".Around`",
		"JOIN_ANY met by a LEFT JOIN only":         "SELECT * FROM " + anyV + " LEFT JOIN " + student + " USING (test_score)",
		"two clauses in one query":                 "SELECT (" + inner + " LIMIT 1), (" + inner + " LIMIT 1)",
		"the clause over a subquery of the view":   "SELECT WITH AGGREGATION_THRESHOLD COUNT(*) FROM (SELECT * FROM " + exam + ")",
		"a script's read in a subquery, in a turn": "SELECT 1;\nSELECT * FROM (SELECT * FROM " + exam + ")",
	} {
		_, err := privacyRows(t, c, h, sql, false)
		wantReason(t, what, err, http.StatusNotImplemented, "notImplemented")
	}
}
