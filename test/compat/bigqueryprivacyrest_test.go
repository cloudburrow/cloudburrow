//go:build compat

package compat

import (
	"math"
	"net/http"
	"strings"
	"testing"
)

// TestBigQueryPrivacyPolicyRest (#1517): the privacy_policy reads and
// differential privacy forms #1502 left 501, measured against Google's
// analysis rules, query syntax (the AGGREGATION_THRESHOLD and
// DIFFERENTIAL_PRIVACY clauses, the join operation), lexical structure and
// differentially private aggregate function pages.
//
//   - Two SELECT WITH AGGREGATION_THRESHOLD in one query: the clause is part
//     of every SELECT's syntax, so each subquery's or CTE's drops its own
//     groups below the threshold. One inside another is "a multi-level
//     aggregation query", 400.
//   - UNVERIFIED: a query with no privacy clause at all that reads an
//     aggregation threshold or differential privacy view in a subquery, a
//     CTE or through another view is BigQuery's documented 400 "You must
//     use SELECT WITH ...".
//   - UNVERIFIED: JOIN_ANY's "At least one column in join_allowed_columns
//     must be joined upon" names no kind of join, so a LEFT JOIN upon an
//     allowed column meets it; JOIN_ALL met by its INNER JOIN allows a
//     LEFT JOIN alongside on allowed columns.
//   - A NATURAL join is a syntax error: NATURAL is a reserved keyword and the
//     join operation has no NATURAL form.
//   - UNVERIFIED: an expression over differentially private aggregates is
//     evaluated on their noisy values, as HAVING is; each call is one more
//     aggregate with its share of the epsilon, and the query is charged its
//     epsilon once against the view's budget.
//
// What Google documents nowhere stays 501: a read of the view outside its
// own clause in a query with a privacy clause, a view of the view read with
// the clause, COUNT(DISTINCT …) and max_rows_contributed in SELECT WITH
// DIFFERENTIAL_PRIVACY.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.jobs.getQueryResults
func TestBigQueryPrivacyPolicyRest(t *testing.T) {
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
	student := "`" + d + ".StudentTable`"

	// Two clauses: the docs' rows (P91 3, U25 4; C83's 2 students dropped),
	// and at threshold 4 only U25.
	for _, insert := range []bool{false, true} {
		got, err := privacyRows(t, c, h, "WITH a AS (SELECT WITH AGGREGATION_THRESHOLD test_id, COUNT(DISTINCT last_name) AS n FROM "+
			exam+" GROUP BY test_id), b AS (SELECT WITH AGGREGATION_THRESHOLD OPTIONS(threshold=4) test_id, "+
			"COUNT(DISTINCT last_name) AS m FROM "+exam+" GROUP BY test_id) "+
			"SELECT a.test_id, a.n, b.m FROM a LEFT JOIN b USING (test_id) ORDER BY a.test_id", insert)
		if err != nil || strings.Join(got, "|") != "P91 3 <nil>|U25 4 4" {
			t.Errorf("two clauses in CTEs (insert %v): %v, %v; want P91 3 <nil>, U25 4 4", insert, got, err)
		}
	}
	got, err := privacyRows(t, c, h, "SELECT (SELECT WITH AGGREGATION_THRESHOLD COUNT(*) FROM "+exam+" WHERE test_id = 'P91'), "+
		"(SELECT WITH AGGREGATION_THRESHOLD COUNT(*) FROM "+exam+" WHERE test_id = 'C83')", false)
	if err != nil || strings.Join(got, "|") != "3 <nil>" {
		t.Errorf("two scalar subqueries: %v, %v; want 3 <nil> (C83 below the threshold)", got, err)
	}

	// 400s.
	for _, c2 := range []struct{ what, sql, want string }{
		{"one clause inside another", "SELECT WITH AGGREGATION_THRESHOLD COUNT(*) FROM (SELECT WITH AGGREGATION_THRESHOLD test_id, " +
			"COUNT(*) AS n FROM " + exam + " GROUP BY test_id)", "multi-level aggregation"},
		{"a subquery read, no clause", "SELECT * FROM (SELECT * FROM " + exam + ")",
			"You must use SELECT WITH AGGREGATION_THRESHOLD for this query because a privacy policy is set on " + d + ".ExamView."},
		{"a CTE read, no clause", "WITH e AS (SELECT * FROM " + exam + ") SELECT COUNT(*) FROM e",
			"You must use SELECT WITH AGGREGATION_THRESHOLD"},
		{"a NATURAL join", "SELECT * FROM " + anyV + " NATURAL JOIN " + student, "Syntax error"},
	} {
		_, err := privacyRows(t, c, h, c2.sql, false)
		wantReason(t, c2.what, err, http.StatusBadRequest, "invalidQuery")
		if err != nil && !strings.Contains(err.Error(), c2.want) {
			t.Errorf("%s: %v, want %q", c2.what, err, c2.want)
		}
	}
	run("CREATE VIEW `" + d + ".Around` AS SELECT * FROM " + exam)
	_, err = privacyRows(t, c, h, "SELECT * FROM `"+d+".Around`", false)
	wantReason(t, "a view of the view, no clause", err, http.StatusBadRequest, "invalidQuery")
	if err != nil && !strings.Contains(err.Error(), "You must use SELECT WITH AGGREGATION_THRESHOLD") {
		t.Errorf("a view of the view: %v", err)
	}

	// JOIN_ANY by a LEFT JOIN on its allowed column: all 11 rows of the
	// view, 5 of them matched.
	got, err = privacyRows(t, c, h, "SELECT COUNT(*), COUNT(s.last_name) FROM "+anyV+" e LEFT JOIN "+student+
		" s USING (test_score)", false)
	if err != nil || strings.Join(got, "|") != "11 5" {
		t.Errorf("JOIN_ANY by a LEFT JOIN: %v, %v; want 11 5", got, err)
	}
	// JOIN_ALL by its INNER JOIN, a LEFT JOIN alongside on allowed columns.
	got, err = privacyRows(t, c, h, "SELECT COUNT(*) FROM "+allV+" e INNER JOIN "+student+" s USING (test_score, last_name) "+
		"LEFT JOIN "+student+" o ON o.test_score = e.test_score", false)
	if err != nil || strings.Join(got, "|") != "5" {
		t.Errorf("JOIN_ALL with a LEFT JOIN alongside: %v, %v; want 5", got, err)
	}

	// Still 501, nothing run.
	for what, sql := range map[string]string{
		"a CTE read by the clause":               "WITH e AS (SELECT * FROM " + exam + ") SELECT WITH AGGREGATION_THRESHOLD COUNT(*) FROM e",
		"the clause over a subquery of the view": "SELECT WITH AGGREGATION_THRESHOLD COUNT(*) FROM (SELECT * FROM " + exam + ")",
		"the clause over a view of the view":     "SELECT WITH AGGREGATION_THRESHOLD COUNT(*) FROM `" + d + ".Around`",
	} {
		_, err := privacyRows(t, c, h, sql, false)
		wantReason(t, what, err, http.StatusNotImplemented, "notImplemented")
	}

	// Differential privacy: an expression over DP aggregates. Without noise
	// the reference's professors give pen 58 / 3, pencil 144 / 4.
	run("CREATE TABLE `" + d + ".professors` AS (" + dpProfessors + ")")
	const exact = "OPTIONS(epsilon=1e20, delta=.01, max_groups_contributed=2, privacy_unit_column=id) "
	const count = "COUNT(*, contribution_bounds_per_group => (0, 100))"
	const sum = "SUM(quantity, contribution_bounds_per_group => (0, 100))"
	for _, insert := range []bool{false, true} {
		rows, err := dpQueryRows(t, c, h, "SELECT WITH DIFFERENTIAL_PRIVACY "+exact+"item, ROUND("+sum+" / "+count+", 2) AS per_row, "+
			count+" + 1 plus FROM `"+d+".professors` GROUP BY item HAVING per_row > 10 ORDER BY item", insert)
		if err != nil {
			t.Fatalf("an expression (insert %v): %v", insert, err)
		}
		if len(rows) != 2 || rows[0][0] != "pen" || dpNumber(rows[0][1]) != 19.33 || dpNumber(rows[0][2]) != 4 ||
			rows[1][0] != "pencil" || dpNumber(rows[1][1]) != 36 || dpNumber(rows[1][2]) != 5 {
			t.Errorf("an expression (insert %v): %v, want pen 19.33 4, pencil 36 5", insert, rows)
		}
	}
	// A DP aggregate inside another is still a multi-level aggregation;
	// another aggregate in the expression is not a DP one.
	for what, sql := range map[string]string{
		"SUM of a COUNT":           "SELECT WITH DIFFERENTIAL_PRIVACY " + exact + "1 + SUM(" + count + ") FROM `" + d + ".professors`",
		"MAX in an expression":     "SELECT WITH DIFFERENTIAL_PRIVACY " + exact + count + " + MAX(quantity) FROM `" + d + ".professors`",
		"an expression of no unit": "SELECT WITH DIFFERENTIAL_PRIVACY OPTIONS(epsilon=1, delta=.01) " + count + " * 2 FROM `" + d + ".professors`",
	} {
		_, err := dpQueryRows(t, c, h, sql, false)
		wantReason(t, what, err, http.StatusBadRequest, "invalidQuery")
	}

	// The budget and contribution bounds with an expression: the policy's
	// privacy unit, max_groups_contributed 1 (each student counted in at
	// most one group: 5 students in all), its epsilon charged once per
	// query, and the refusal once the budget is spent.
	policy := `{"differential_privacy_policy": {"privacy_unit_column": "last_name", "max_epsilon_per_query": 10, ` +
		`"epsilon_budget": 25, "delta_per_query": 0.01, "delta_budget": 1, "max_groups_contributed": 1}}`
	dpV := view("DPView", policy)
	query := "SELECT WITH DIFFERENTIAL_PRIVACY OPTIONS(epsilon=10) test_id, COUNT(*, contribution_bounds_per_group => (0, 1)) * 10 AS tens, " +
		"COUNT(*, contribution_bounds_per_group => (0, 1)) AS n FROM " + dpV + " GROUP BY test_id"
	for i, insert := range []bool{false, true} {
		rows, err := dpQueryRows(t, c, h, query, insert)
		if err != nil {
			t.Fatalf("budgeted query %d: %v", i+1, err)
		}
		total := 0.0
		for _, r := range rows {
			// Each count's noise is independent; the expression is its
			// own count's value times ten, so a whole multiple of ten.
			if tens := dpNumber(r[1]); math.Mod(math.Abs(tens), 10) != 0 {
				t.Errorf("budgeted query %d: %v is not a COUNT times ten", i+1, r)
			}
			total += dpNumber(r[2])
		}
		// 5 students, one group each: the noisy counts' sum is near 5
		// (epsilon 10 over three aggregates: scale 0.3 each).
		if len(rows) > 0 && math.Abs(total-5) > 6 {
			t.Errorf("budgeted query %d: counts sum to %v, far from 5 students", i+1, total)
		}
	}
	_, err = dpQueryRows(t, c, h, query, false)
	wantReason(t, "the third budgeted query", err, http.StatusBadRequest, "invalidQuery")
	if want := "Privacy budget is not sufficient for table '" + d + ".DPView' in this query."; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("third budgeted query: %v, want %q", err, want)
	}
	_, err = dpQueryRows(t, c, h, "SELECT * FROM (SELECT * FROM "+dpV+")", false)
	wantReason(t, "a DP view in a subquery, no clause", err, http.StatusBadRequest, "invalidQuery")
	if err != nil && !strings.Contains(err.Error(), "You must use SELECT WITH DIFFERENTIAL_PRIVACY") {
		t.Errorf("a DP view in a subquery: %v", err)
	}

	// Documented nowhere: 501.
	for what, sql := range map[string]string{
		"COUNT(DISTINCT …)": "SELECT WITH DIFFERENTIAL_PRIVACY " + exact + "COUNT(DISTINCT item) FROM `" + d + ".professors`",
		"max_rows_contributed": "SELECT WITH DIFFERENTIAL_PRIVACY OPTIONS(epsilon=1, delta=.01, max_rows_contributed=1, " +
			"privacy_unit_column=id) COUNT(*) FROM `" + d + ".professors`",
	} {
		_, err := dpQueryRows(t, c, h, sql, false)
		wantReason(t, what, err, http.StatusNotImplemented, "notImplemented")
	}
}
