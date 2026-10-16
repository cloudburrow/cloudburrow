//go:build compat

package compat

import (
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"
)

// dpQueryRows runs sql (a query job when insert) and returns its rows.
func dpQueryRows(t *testing.T, c *bigquery.Client, h *Harness, sql string, insert bool) ([][]bigquery.Value, error) {
	t.Helper()
	q := c.Query(sql)
	var it *bigquery.RowIterator
	var err error
	if insert {
		job, jerr := q.Run(h.Context())
		if jerr != nil {
			return nil, jerr
		}
		it, err = job.Read(h.Context())
	} else {
		it, err = q.Read(h.Context())
	}
	if err != nil {
		return nil, err
	}
	var out [][]bigquery.Value
	for {
		var row []bigquery.Value
		err := it.Next(&row)
		if err == iterator.Done {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
}

func dpNumber(v bigquery.Value) float64 {
	switch x := v.(type) {
	case int64:
		return float64(x)
	case float64:
		return x
	}
	return math.NaN()
}

// professors is the professors table of the DIFFERENTIAL_PRIVACY clause's
// examples (query-syntax#dp_clause).
const dpProfessors = `SELECT 101 AS id, "pencil" AS item, 24 AS quantity UNION ALL
  SELECT 123, "pen", 16 UNION ALL SELECT 123, "pencil", 10 UNION ALL SELECT 123, "pencil", 38 UNION ALL
  SELECT 101, "pen", 19 UNION ALL SELECT 101, "pen", 23 UNION ALL SELECT 130, "scissors", 8 UNION ALL
  SELECT 150, "pencil", 72`

// TestBigQueryPrivacyPolicyDifferentialPrivacy (#1455, was 501 in #1426):
// SELECT WITH DIFFERENTIAL_PRIVACY with the reference's options and its DP
// aggregate functions, through jobs.query and jobs.insert. Noisy results
// cannot be compared with Google's, so this checks what the noise model
// guarantees: with epsilon 1e20 (no noise) the reference's example gives
// its rows (pencil 40, pen 18.5, scissors 8); with a small epsilon the
// results vary around the true value and stay within the Laplace noise's
// spread; a group of one privacy unit is dropped (group thresholding);
// max_groups_contributed and contribution_bounds_per_group bound each
// unit's contributions.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.jobs.getQueryResults
func TestBigQueryPrivacyPolicyDifferentialPrivacy(t *testing.T) {
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
	// The reference's "Remove noise" example.
	for _, insert := range []bool{false, true} {
		rows, err := dpQueryRows(t, c, h, "SELECT WITH DIFFERENTIAL_PRIVACY "+
			"OPTIONS(epsilon=1e20, delta=.01, max_groups_contributed=2, privacy_unit_column=id) "+
			"item, AVG(quantity, contribution_bounds_per_group => (0,100)) AS average_quantity "+
			"FROM `"+d+".professors` GROUP BY item ORDER BY item", insert)
		if err != nil {
			t.Fatalf("insert %v: %v", insert, err)
		}
		want := []struct {
			item string
			avg  float64
		}{{"pen", 18.5}, {"pencil", 40}, {"scissors", 8}}
		if len(rows) != len(want) {
			t.Fatalf("insert %v: %v, want pen 18.5, pencil 40, scissors 8", insert, rows)
		}
		for i, w := range want {
			if rows[i][0] != w.item || math.Abs(dpNumber(rows[i][1])-w.avg) > 1e-6 {
				t.Errorf("insert %v: row %d %v, want %s %v", insert, i, rows[i], w.item, w.avg)
			}
		}
	}
	// COUNT, SUM and PERCENTILE_CONT, without noise: SUM of INT64 is INT64.
	rows, err := dpQueryRows(t, c, h, "SELECT WITH DIFFERENTIAL_PRIVACY "+
		"OPTIONS(epsilon=1e20, delta=.01, max_groups_contributed=2, privacy_unit_column=id) "+
		"item, COUNT(*, contribution_bounds_per_group => (0, 100)) AS times_requested, "+
		"SUM(quantity, contribution_bounds_per_group => (0, 100)) AS quantity, "+
		"PERCENTILE_CONT(quantity, 0.5, contribution_bounds_per_row => (0, 100)) AS median "+
		"FROM `"+d+".professors` GROUP BY item ORDER BY item", false)
	if err != nil || len(rows) != 3 {
		t.Fatalf("COUNT, SUM, PERCENTILE_CONT: %v, %v", rows, err)
	}
	if rows[0][1] != int64(3) || rows[0][2] != int64(58) || rows[1][1] != int64(4) || rows[1][2] != int64(144) ||
		rows[2][1] != int64(1) || rows[2][2] != int64(8) {
		t.Errorf("COUNT and SUM: %v, want pen 3 58, pencil 4 144, scissors 1 8", rows)
	}
	if m := dpNumber(rows[2][3]); math.Abs(m-8) > 1 {
		t.Errorf("PERCENTILE_CONT of scissors: %v, want about 8", rows[2][3])
	}

	// A table of 200 privacy units in group "big", each with 3 rows of 50,
	// and one unit in group "small".
	run("CREATE TABLE `" + d + ".dp_units` AS SELECT u AS unit, 'big' AS g, 50 AS x FROM UNNEST(GENERATE_ARRAY(1, 200)) AS u, " +
		"UNNEST([1, 2, 3]) UNION ALL SELECT 0, 'small', 50")
	const noisy = 20
	var counts []float64
	for i := 0; i < noisy; i++ {
		rows, err := dpQueryRows(t, c, h, "SELECT WITH DIFFERENTIAL_PRIVACY "+
			"OPTIONS(epsilon=1, delta=1e-5, max_groups_contributed=1, privacy_unit_column=unit) "+
			"g, COUNT(*, contribution_bounds_per_group => (0, 1)) AS n, SUM(x, contribution_bounds_per_group => (0, 10)) AS s "+
			"FROM `"+d+".dp_units` GROUP BY g", i%2 == 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0][0] != "big" {
			t.Fatalf("run %d: %v; want only the group big (small, of one privacy unit, dropped)", i, rows)
		}
		n, s := dpNumber(rows[0][1]), dpNumber(rows[0][2])
		// Laplace, epsilon 1/3 per aggregate: scale 3 for n (bounds 0..1),
		// 30 for s (0..10); 200 units clamped to 1 and to 10.
		if math.Abs(n-200) > 60 {
			t.Errorf("run %d: count %v, far from 200", i, n)
		}
		if math.Abs(s-2000) > 600 {
			t.Errorf("run %d: sum %v, far from 2000: contributions not clamped to 10 per unit", i, s)
		}
		counts = append(counts, n)
	}
	distinct := map[float64]bool{}
	mean := 0.0
	for _, n := range counts {
		distinct[n] = true
		mean += n / noisy
	}
	if len(distinct) < 4 {
		t.Errorf("counts %v: too few distinct values for Laplace noise of scale 3", counts)
	}
	if math.Abs(mean-200) > 4 {
		t.Errorf("mean count %v over %d runs, want about 200 (unbiased noise)", mean, noisy)
	}
	// max_groups_contributed = 1: 30 units in 3 groups each, 5 rows in each
	// group clamped to 2: each unit counted in one group only.
	run("CREATE TABLE `" + d + ".dp_groups` AS SELECT u AS unit, g FROM UNNEST(GENERATE_ARRAY(1, 30)) AS u, " +
		"UNNEST(['a', 'b', 'c']) AS g, UNNEST(GENERATE_ARRAY(1, 5))")
	rows, err = dpQueryRows(t, c, h, "SELECT WITH DIFFERENTIAL_PRIVACY "+
		"OPTIONS(epsilon=1e20, delta=.01, max_groups_contributed=1, privacy_unit_column=unit) "+
		"g, COUNT(*, contribution_bounds_per_group => (0, 2)) AS n FROM `"+d+".dp_groups` GROUP BY g", false)
	if err != nil {
		t.Fatal(err)
	}
	total := 0.0
	for _, r := range rows {
		total += dpNumber(r[1])
	}
	if total != 60 {
		t.Errorf("max_groups_contributed 1: %v (total %v), want a total of 60", rows, total)
	}
	// Refused as the reference says.
	for what, sql := range map[string]string{
		"MAX, not a DP aggregate": "SELECT WITH DIFFERENTIAL_PRIVACY OPTIONS(epsilon=1, delta=.01, privacy_unit_column=id) " +
			"item, MAX(quantity) FROM `" + d + ".professors` GROUP BY item",
		"no privacy unit column": "SELECT WITH DIFFERENTIAL_PRIVACY OPTIONS(epsilon=1, delta=.01) COUNT(*) FROM `" + d + ".professors`",
		"PERCENTILE_CONT without bounds": "SELECT WITH DIFFERENTIAL_PRIVACY OPTIONS(epsilon=1, delta=.01, privacy_unit_column=id) " +
			"PERCENTILE_CONT(quantity, 0.5) FROM `" + d + ".professors`",
		"a multi-level aggregation": "SELECT WITH DIFFERENTIAL_PRIVACY OPTIONS(epsilon=1, delta=.01, privacy_unit_column=id) " +
			"SUM(AVG(quantity)) FROM `" + d + ".professors`",
	} {
		_, err := dpQueryRows(t, c, h, sql, false)
		wantReason(t, what, err, http.StatusBadRequest, "invalidQuery")
	}
	_, err = dpQueryRows(t, c, h, "SELECT * FROM (SELECT WITH DIFFERENTIAL_PRIVACY OPTIONS(epsilon=1, delta=.01, "+
		"privacy_unit_column=id) COUNT(*) AS n FROM `"+d+".professors`)", false)
	wantReason(t, "a SELECT WITH DIFFERENTIAL_PRIVACY in a subquery", err, http.StatusNotImplemented, "notImplemented")
}

// TestBigQueryDifferentialPrivacyPolicyBudget (#1455): a view's
// differential_privacy_policy, as the analysis rules' examples show it:
// the query must use SELECT WITH DIFFERENTIAL_PRIVACY, takes the policy's
// epsilon, delta, privacy unit and max_groups_contributed, is refused
// "Epsilon is too high: 1e+20, policy for table … allows max 10.01" above
// max_epsilon_per_query, and, after two queries of epsilon 10 against an
// epsilon_budget of 25.6, "Privacy budget is not sufficient for table '…'
// in this query."; ALTER VIEW SET OPTIONS of the policy resets the budget.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.jobs.getQueryResults
func TestBigQueryDifferentialPrivacyPolicyBudget(t *testing.T) {
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
	policy := `'{"differential_privacy_policy": {"privacy_unit_column": "last_name", "max_epsilon_per_query": 10.01, ` +
		`"epsilon_budget": 25.6, "delta_per_query": 0.01, "delta_budget": 0.1, "max_groups_contributed": 2}}'`
	run("CREATE TABLE `" + d + ".ExamTable` AS (" + privacyExam + ")")
	run("CREATE VIEW `" + d + ".ExamView` OPTIONS (privacy_policy = " + policy + ") AS (SELECT * FROM `" + d + ".ExamTable`)")
	query := func(options string) string {
		return "SELECT WITH DIFFERENTIAL_PRIVACY " + options + " test_id, AVG(test_score) AS average_test_score " +
			"FROM `" + d + ".ExamView` GROUP BY test_id"
	}
	_, err := dpQueryRows(t, c, h, query("OPTIONS(epsilon=1e20)"), false)
	wantReason(t, "an epsilon above max_epsilon_per_query", err, http.StatusBadRequest, "invalidQuery")
	if want := "Epsilon is too high: 1e+20, policy for table " + d + ".ExamView allows max 10.01"; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("epsilon 1e20: %v, want %q", err, want)
	}
	for what, sql := range map[string]string{
		"a query without the clause":  "SELECT * FROM `" + d + ".ExamView`",
		"another delta":               query("OPTIONS(epsilon=1, delta=0.5)"),
		"another privacy unit column": query("OPTIONS(epsilon=1, privacy_unit_column=test_id)"),
		"more groups than the policy": query("OPTIONS(epsilon=1, max_groups_contributed=3)"),
	} {
		_, err := dpQueryRows(t, c, h, sql, false)
		wantReason(t, what, err, http.StatusBadRequest, "invalidQuery")
	}
	// The budget: 10 + 10 runs, the third 10 would pass 25.6.
	for i, insert := range []bool{false, true} {
		rows, err := dpQueryRows(t, c, h, query("OPTIONS(epsilon=10)"), insert)
		if err != nil {
			t.Fatalf("query %d: %v", i+1, err)
		}
		for _, r := range rows {
			if len(r) != 2 || fmt.Sprint(r[0]) == "" {
				t.Errorf("query %d: row %v", i+1, r)
			}
		}
	}
	_, err = dpQueryRows(t, c, h, query("OPTIONS(epsilon=10)"), false)
	wantReason(t, "the third query", err, http.StatusBadRequest, "invalidQuery")
	if want := "Privacy budget is not sufficient for table '" + d + ".ExamView' in this query."; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("third query: %v, want %q", err, want)
	}
	// A smaller epsilon still fits (20 + 5 <= 25.6).
	if _, err := dpQueryRows(t, c, h, query("OPTIONS(epsilon=5)"), false); err != nil {
		t.Errorf("epsilon 5 within the budget: %v", err)
	}
	// Updating the rule resets the budget.
	run("ALTER VIEW `" + d + ".ExamView` SET OPTIONS (privacy_policy = " + strings.Replace(policy, "25.6", "25.7", 1) + ")")
	if _, err := dpQueryRows(t, c, h, query("OPTIONS(epsilon=10)"), false); err != nil {
		t.Errorf("after ALTER VIEW: %v, want the budget reset", err)
	}
}
