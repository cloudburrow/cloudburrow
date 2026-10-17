//go:build compat

package compat

import (
	"math"
	"net/http"
	"strings"
	"testing"
)

// TestBigQueryDifferentialPrivacyMore (#1482): what #1455 left 501.
//
//   - HAVING, applied to the noisy rows: by an alias, by the select list's
//     aggregate call, and by a DP aggregate only HAVING calls, which takes
//     its share of the epsilon (query-syntax#dp_epsilon);
//   - SELECT WITH DIFFERENTIAL_PRIVACY as a subquery and a CTE's query, the
//     outer query reading its noisy rows;
//   - a script's statement, run in its turn (it reads what the statements
//     before it wrote), its noise and its budget's charge once;
//   - group_selection_strategy = PUBLIC_GROUPS with a public group join, as
//     GoogleSQL's reference documents it (query-syntax#dp_group_selection_strategy):
//     every public group kept, a group of one privacy unit too, a private
//     group not in the public table not, and refused with no public join.
//
// Noisy results cannot be compared with Google's, so with epsilon 1e20 (no
// noise) the rows are exact, and with epsilon 1 the counts are checked to
// vary and to stay within the Laplace noise's spread around the true value.
// COUNT(DISTINCT …) and max_rows_contributed, which BigQuery's reference
// does not document, stay 501.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.jobs.getQueryResults
func TestBigQueryDifferentialPrivacyMore(t *testing.T) {
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
	run("CREATE TABLE `" + d + ".professors` AS (" + dpProfessors + ")")
	run("CREATE TABLE `" + d + ".dp_units` AS SELECT u AS unit, 'big' AS g, 50 AS x FROM UNNEST(GENERATE_ARRAY(1, 200)) AS u, " +
		"UNNEST([1, 2, 3]) UNION ALL SELECT 0, 'small', 50")
	const exact = "OPTIONS(epsilon=1e20, delta=.01, max_groups_contributed=2, privacy_unit_column=id) "
	const count = "COUNT(*, contribution_bounds_per_group => (0, 100))"
	const sum = "SUM(quantity, contribution_bounds_per_group => (0, 100))"
	rowsOf := func(what, sql string, insert bool) [][]any {
		t.Helper()
		rows, err := dpQueryRows(t, c, h, sql, insert)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		var out [][]any
		for _, r := range rows {
			var o []any
			for _, v := range r {
				o = append(o, v)
			}
			out = append(out, o)
		}
		return out
	}
	// Without noise: pen 3 rows (58), pencil 4 (144), scissors 1 (8).
	for _, insert := range []bool{false, true} {
		got := rowsOf("HAVING by alias", "SELECT WITH DIFFERENTIAL_PRIVACY "+exact+"item, "+count+" AS n FROM `"+d+
			".professors` GROUP BY item HAVING n > 1 ORDER BY item", insert)
		if len(got) != 2 || got[0][0] != "pen" || got[0][1] != int64(3) || got[1][0] != "pencil" || got[1][1] != int64(4) {
			t.Errorf("HAVING n > 1 (insert %v): %v, want pen 3, pencil 4", insert, got)
		}
	}
	got := rowsOf("HAVING by the call", "SELECT WITH DIFFERENTIAL_PRIVACY "+exact+"item, "+count+" AS n FROM `"+d+
		".professors` GROUP BY item HAVING "+strings.ToLower(count)+" = 1", false)
	if len(got) != 1 || got[0][0] != "scissors" {
		t.Errorf("HAVING COUNT(*) = 1: %v, want scissors", got)
	}
	got = rowsOf("HAVING a hidden aggregate", "SELECT WITH DIFFERENTIAL_PRIVACY "+exact+"item, "+count+" AS n FROM `"+d+
		".professors` GROUP BY item HAVING "+sum+" > 50 AND item != 'pencil'", false)
	if len(got) != 1 || len(got[0]) != 2 || got[0][0] != "pen" || got[0][1] != int64(3) {
		t.Errorf("HAVING SUM(…) > 50 AND item != 'pencil': %v, want pen 3 (two columns)", got)
	}

	// As a subquery, and a CTE's query.
	got = rowsOf("a subquery", "SELECT item, n * 10 AS tens FROM (SELECT WITH DIFFERENTIAL_PRIVACY "+exact+"item, "+count+
		" AS n FROM `"+d+".professors` GROUP BY item) WHERE item != 'pen' ORDER BY item", false)
	if len(got) != 2 || got[0][0] != "pencil" || got[0][1] != int64(40) || got[1][0] != "scissors" || got[1][1] != int64(10) {
		t.Errorf("subquery: %v, want pencil 40, scissors 10", got)
	}
	got = rowsOf("a CTE", "WITH d AS (SELECT WITH DIFFERENTIAL_PRIVACY "+exact+"item, "+sum+" AS s FROM `"+d+
		".professors` GROUP BY item) SELECT SUM(s) AS total, COUNT(*) AS n_groups FROM d", true)
	if len(got) != 1 || got[0][0] != int64(210) || got[0][1] != int64(3) {
		t.Errorf("CTE: %v, want 210 over 3 groups", got)
	}
	// With noise, in a subquery: about 200, and varying.
	var counts []float64
	for i := 0; i < 10; i++ {
		got := rowsOf("a noisy subquery", "SELECT g, n FROM (SELECT WITH DIFFERENTIAL_PRIVACY "+
			"OPTIONS(epsilon=1, delta=1e-5, max_groups_contributed=1, privacy_unit_column=unit) "+
			"g, COUNT(*, contribution_bounds_per_group => (0, 1)) AS n FROM `"+d+".dp_units` GROUP BY g)", i%2 == 1)
		if len(got) != 1 || got[0][0] != "big" {
			t.Fatalf("noisy subquery %d: %v, want only big (small, of one unit, dropped)", i, got)
		}
		n := dpNumber(got[0][1])
		// Laplace, epsilon 1/2: scale 2.
		if math.Abs(n-200) > 40 {
			t.Errorf("noisy subquery %d: %v, far from 200", i, n)
		}
		counts = append(counts, n)
	}
	if distinct := map[float64]bool{}; true {
		for _, n := range counts {
			distinct[n] = true
		}
		if len(distinct) < 3 {
			t.Errorf("noisy subquery counts %v: no noise", counts)
		}
	}

	// A script: the statement reads what the script wrote before it.
	for _, insert := range []bool{false, true} {
		tbl := "`" + d + ".dp_script_" + map[bool]string{false: "q", true: "j"}[insert] + "`"
		got := rowsOf("a script", "DECLARE least INT64 DEFAULT 10;\n"+
			"CREATE TABLE "+tbl+" AS ("+dpProfessors+");\n"+
			"INSERT INTO "+tbl+" VALUES (160, 'scissors', 30), (170, 'scissors', 5);\n"+
			"SELECT WITH DIFFERENTIAL_PRIVACY "+exact+"item, "+count+" AS n FROM "+tbl+
			" WHERE quantity >= least GROUP BY item ORDER BY item", insert)
		// quantity >= 10: pen 3, pencil 4, scissors 1 (160's 30).
		if len(got) != 3 || got[0][1] != int64(3) || got[1][1] != int64(4) || got[2][0] != "scissors" || got[2][1] != int64(1) {
			t.Errorf("script (insert %v): %v, want pen 3, pencil 4, scissors 1", insert, got)
		}
	}
	// A script's budget is charged once a statement: 10 + 10 of 25.6, then
	// a third query of 10 is refused and one of 5 is not.
	policy := `'{"differential_privacy_policy": {"privacy_unit_column": "last_name", "max_epsilon_per_query": 10.01, ` +
		`"epsilon_budget": 25.6, "delta_per_query": 0.01, "delta_budget": 0.1, "max_groups_contributed": 2}}'`
	run("CREATE TABLE `" + d + ".ExamTable` AS (" + privacyExam + ")")
	run("CREATE VIEW `" + d + ".ExamView` OPTIONS (privacy_policy = " + policy + ") AS (SELECT * FROM `" + d + ".ExamTable`)")
	exam := "SELECT WITH DIFFERENTIAL_PRIVACY OPTIONS(epsilon=10) test_id, AVG(test_score) AS a FROM `" + d + ".ExamView` GROUP BY test_id"
	if _, err := dpQueryRows(t, c, h, "SELECT 1;\n"+exam+";\n"+exam, true); err != nil {
		t.Fatalf("script of two DP queries within the budget: %v", err)
	}
	_, err := dpQueryRows(t, c, h, exam, false)
	wantReason(t, "a third query of epsilon 10 after the script's two", err, http.StatusBadRequest, "invalidQuery")
	if want := "Privacy budget is not sufficient for table '" + d + ".ExamView' in this query."; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("third query: %v, want %q", err, want)
	}
	if _, err := dpQueryRows(t, c, h, strings.Replace(exam, "epsilon=10", "epsilon=5", 1), false); err != nil {
		t.Errorf("epsilon 5 within the budget after the script: %v (charged more than once?)", err)
	}

	// PUBLIC_GROUPS: the reference's example, the public groups by UNNEST.
	public := func(eps, from string) string {
		return "SELECT WITH DIFFERENTIAL_PRIVACY OPTIONS(epsilon=" + eps + ", delta=.01, max_groups_contributed=2, " +
			"group_selection_strategy=PUBLIC_GROUPS, privacy_unit_column=id) item, " + count + " AS n FROM " + from +
			" GROUP BY item ORDER BY item"
	}
	got = rowsOf("PUBLIC_GROUPS", public("1e20", "`"+d+".professors` RIGHT OUTER JOIN "+
		"(SELECT DISTINCT item FROM UNNEST(['pen', 'pencil', 'book']) AS item) USING (item)"), false)
	if len(got) != 3 || got[0][0] != "book" || got[0][1] != int64(0) || got[1][0] != "pen" || got[1][1] != int64(3) ||
		got[2][0] != "pencil" || got[2][1] != int64(4) {
		t.Errorf("PUBLIC_GROUPS: %v, want book 0, pen 3, pencil 4 (scissors not public)", got)
	}
	// With noise: the group of one unit (small) and of none (none) are
	// kept every time, which LAPLACE_THRESHOLD would drop.
	for i := 0; i < 6; i++ {
		got := rowsOf("noisy PUBLIC_GROUPS", "SELECT WITH DIFFERENTIAL_PRIVACY OPTIONS(epsilon=1, delta=1e-5, "+
			"max_groups_contributed=1, group_selection_strategy=PUBLIC_GROUPS, privacy_unit_column=unit) "+
			"g, COUNT(*, contribution_bounds_per_group => (0, 1)) AS n FROM (SELECT DISTINCT g FROM "+
			"UNNEST(['big', 'small', 'none']) AS g) AS p LEFT OUTER JOIN `"+d+".dp_units` USING (g) GROUP BY g ORDER BY g", i%2 == 1)
		if len(got) != 3 || got[0][0] != "big" || got[1][0] != "none" || got[2][0] != "small" {
			t.Fatalf("noisy PUBLIC_GROUPS %d: %v, want big, none and small", i, got)
		}
		// Laplace, epsilon 1 (no group selection's share): scale 1.
		for k, want := range []float64{200, 0, 1} {
			if n := dpNumber(got[k][1]); math.Abs(n-want) > 20 {
				t.Errorf("noisy PUBLIC_GROUPS %d: %v %v, far from %v", i, got[k][0], n, want)
			}
		}
	}

	// Refused.
	_, err = dpQueryRows(t, c, h, public("1", "`"+d+".professors`"), false)
	wantReason(t, "PUBLIC_GROUPS with no public group join", err, http.StatusBadRequest, "invalidQuery")
	for what, sql := range map[string]string{
		"COUNT(DISTINCT …)": "SELECT WITH DIFFERENTIAL_PRIVACY " + exact + "COUNT(DISTINCT item) FROM `" + d + ".professors`",
		"max_rows_contributed": "SELECT WITH DIFFERENTIAL_PRIVACY OPTIONS(epsilon=1, delta=.01, max_rows_contributed=1, " +
			"privacy_unit_column=id) COUNT(*) FROM `" + d + ".professors`",
	} {
		_, err := dpQueryRows(t, c, h, sql, false)
		wantReason(t, what, err, http.StatusNotImplemented, "notImplemented")
	}
}
