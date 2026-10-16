//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	bq "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/iterator"
)

// privacyExam is the ExamTable of BigQuery's analysis rules examples
// (https://cloud.google.com/bigquery/docs/analysis-rules).
const privacyExam = `SELECT "Hansen" AS last_name, "P91" AS test_id, 510 AS test_score UNION ALL
  SELECT "Wang", "U25", 500 UNION ALL SELECT "Wang", "C83", 520 UNION ALL SELECT "Wang", "U25", 460 UNION ALL
  SELECT "Hansen", "C83", 420 UNION ALL SELECT "Hansen", "C83", 560 UNION ALL SELECT "Devi", "U25", 580 UNION ALL
  SELECT "Devi", "P91", 480 UNION ALL SELECT "Ivanov", "U25", 490 UNION ALL SELECT "Ivanov", "P91", 540 UNION ALL
  SELECT "Silva", "U25", 550`

// privacyRows runs sql (a query job when insert) and returns its rows as
// text, sorted.
func privacyRows(t *testing.T, c *bigquery.Client, h *Harness, sql string, insert bool) ([]string, error) {
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
	var out []string
	for {
		var row []bigquery.Value
		err := it.Next(&row)
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, err
		}
		parts := make([]string, len(row))
		for i, v := range row {
			parts[i] = fmt.Sprint(v)
		}
		out = append(out, strings.Join(parts, " "))
	}
	sort.Strings(out)
	return out, nil
}

// TestBigQueryPrivacyPolicyAggregationThreshold (#1426): a view's
// aggregation threshold analysis rule, CREATE VIEW ... OPTIONS(privacy_policy
// = '{"aggregation_threshold_policy": ...}'), as BigQuery's examples show
// it: SELECT WITH AGGREGATION_THRESHOLD drops the groups with fewer
// distinct privacy units than the threshold, the query's threshold may
// raise the view's but not lower it, the privacy unit column cannot be
// projected, only the clause's aggregate functions run, and a query
// without the clause, or the view read through another view or
// tabledata.list of a table with a policy, is refused. The clause with
// OPTIONS on a table with no policy, a policy set by CREATE TABLE, and one
// set by tables.patch (view.privacyPolicy) are enforced too.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.jobs.getQueryResults, bigquery.tables.patch, bigquery.tabledata.list
func TestBigQueryPrivacyPolicyAggregationThreshold(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
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
	run("CREATE VIEW `" + d + ".ExamView` OPTIONS (privacy_policy = " +
		`'{"aggregation_threshold_policy": {"threshold": 3, "privacy_unit_column": "last_name"}}'` +
		") AS (SELECT * FROM `" + d + ".ExamTable`)")
	query := func(options string) string {
		return "SELECT WITH AGGREGATION_THRESHOLD " + options + " test_id, COUNT(DISTINCT last_name) AS student_count, " +
			"AVG(test_score) AS avg_test_score FROM `" + d + ".ExamView` GROUP BY test_id"
	}
	for _, insert := range []bool{false, true} {
		got, err := privacyRows(t, c, h, query(""), insert)
		if err != nil || strings.Join(got, "|") != "P91 3 510|U25 4 516" {
			t.Errorf("insert %v: threshold 3: %v, %v; want P91 3 510, U25 4 516 (C83 dropped)", insert, got, err)
		}
		got, err = privacyRows(t, c, h, query("OPTIONS(threshold=4)"), insert)
		if err != nil || strings.Join(got, "|") != "U25 4 516" {
			t.Errorf("insert %v: threshold 4: %v, %v; want U25 4 516", insert, got, err)
		}
		_, err = privacyRows(t, c, h, "SELECT * FROM `"+d+".ExamView`", insert)
		wantReason(t, "a query without the clause", err, http.StatusBadRequest, "invalidQuery")
	}
	if got, err := privacyRows(t, c, h, query("OPTIONS(threshold=5)"), false); err != nil || len(got) != 0 {
		t.Errorf("threshold 5: %v, %v; want no rows", got, err)
	}
	// A WHERE and a HAVING of the query's own, and no GROUP BY.
	got, err := privacyRows(t, c, h, "SELECT WITH AGGREGATION_THRESHOLD test_id, SUM(test_score) FROM `"+d+
		".ExamView` WHERE test_score > 400 GROUP BY test_id HAVING SUM(test_score) > 1600", false)
	if err != nil || strings.Join(got, "|") != "U25 2580" {
		t.Errorf("WHERE and HAVING: %v, %v; want U25 2580", got, err)
	}
	got, err = privacyRows(t, c, h, "SELECT WITH AGGREGATION_THRESHOLD COUNT(*) FROM `"+d+".ExamView`", false)
	if err != nil || strings.Join(got, "|") != "11" {
		t.Errorf("no GROUP BY: %v, %v; want 11", got, err)
	}
	got, err = privacyRows(t, c, h, "SELECT WITH AGGREGATION_THRESHOLD COUNT(*) FROM `"+d+".ExamView` WHERE last_name = 'Wang'", false)
	if err != nil || len(got) != 0 {
		t.Errorf("one privacy unit: %v, %v; want no rows", got, err)
	}
	for what, sql := range map[string]string{
		"a lower threshold":            query("OPTIONS(threshold=2)"),
		"another privacy unit column":  query("OPTIONS(privacy_unit_column=test_id)"),
		"the privacy unit projected":   "SELECT WITH AGGREGATION_THRESHOLD last_name, COUNT(*) FROM `" + d + ".ExamView` GROUP BY last_name",
		"SELECT *":                     "SELECT WITH AGGREGATION_THRESHOLD * FROM `" + d + ".ExamView`",
		"MAX, not an allowed function": "SELECT WITH AGGREGATION_THRESHOLD test_id, MAX(test_score) FROM `" + d + ".ExamView` GROUP BY test_id",
		"no aggregate":                 "SELECT WITH AGGREGATION_THRESHOLD test_id FROM `" + d + ".ExamView` GROUP BY test_id",
	} {
		_, err := privacyRows(t, c, h, sql, false)
		wantReason(t, what, err, http.StatusBadRequest, "invalidQuery")
	}
	// What the front cannot check is 501, never answered.
	_, err = privacyRows(t, c, h, "SELECT * FROM (SELECT * FROM `"+d+".ExamView`)", false)
	wantReason(t, "a read in a subquery", err, http.StatusNotImplemented, "notImplemented")
	run("CREATE VIEW `" + d + ".Around` AS SELECT * FROM `" + d + ".ExamView`")
	_, err = privacyRows(t, c, h, "SELECT * FROM `"+d+".Around`", false)
	wantReason(t, "a view reading the view", err, http.StatusNotImplemented, "notImplemented")

	// The clause on a table with no policy, the docs' first example.
	got, err = privacyRows(t, c, h, "SELECT WITH AGGREGATION_THRESHOLD OPTIONS(threshold=3, privacy_unit_column=last_name) "+
		"test_id, COUNT(DISTINCT last_name), AVG(test_score) FROM `"+d+".ExamTable` GROUP BY test_id", false)
	if err != nil || strings.Join(got, "|") != "P91 3 510|U25 4 516" {
		t.Errorf("the clause on a table: %v, %v", got, err)
	}

	// A table's policy, set by CREATE TABLE OPTIONS.
	run("CREATE TABLE `" + d + ".Exams` OPTIONS (privacy_policy = " +
		`'{"aggregation_threshold_policy": {"threshold": 4, "privacy_unit_column": "last_name"}}'` +
		") AS (" + privacyExam + ")")
	got, err = privacyRows(t, c, h, "SELECT WITH AGGREGATION_THRESHOLD test_id, COUNT(*) FROM `"+d+".Exams` GROUP BY test_id", true)
	if err != nil || strings.Join(got, "|") != "U25 5" {
		t.Errorf("a table's policy: %v, %v; want U25 5", got, err)
	}
	err = ds.Table("Exams").Read(ctx).Next(&[]bigquery.Value{})
	wantReason(t, "tabledata.list of a table with a policy", err, http.StatusNotImplemented, "notImplemented")

	// tables.patch of a view with a view.privacyPolicy (the generated
	// client: the Go client's ViewQuery has no privacy policy).
	viewQuery := "SELECT * FROM `" + d + ".ExamTable`"
	run("CREATE VIEW `" + d + ".Patched` AS " + viewQuery)
	svc := bigqueryService(t, h)
	if _, err := svc.Tables.Patch(project, d, "Patched", &bq.Table{View: &bq.ViewDefinition{Query: viewQuery,
		PrivacyPolicy: &bq.PrivacyPolicy{AggregationThresholdPolicy: &bq.AggregationThresholdPolicy{Threshold: 3,
			PrivacyUnitColumns: []string{"last_name"}}}}}).Context(ctx).Do(); err != nil {
		t.Fatalf("tables.patch: %v", err)
	}
	_, err = privacyRows(t, c, h, "SELECT * FROM `"+d+".Patched`", false)
	wantReason(t, "a query of a view patched with a policy", err, http.StatusBadRequest, "invalidQuery")
	got, err = privacyRows(t, c, h, "SELECT WITH AGGREGATION_THRESHOLD test_id, COUNT(*) FROM `"+d+".Patched` GROUP BY test_id", false)
	if err != nil || strings.Join(got, "|") != "P91 3|U25 5" {
		t.Errorf("a patched view's policy: %v, %v; want P91 3, U25 5", got, err)
	}
}

// TestBigQueryPrivacyPolicyJoinRestriction (#1426): a view's
// join_restriction_policy, as BigQuery's list overlap examples show it:
// JOIN_ANY and JOIN_ALL require an INNER JOIN on one or all of
// join_allowed_columns, JOIN_NOT_REQUIRED allows no join but refuses one on
// another column (and any query when it allows no column), JOIN_BLOCKED
// refuses every join, each with BigQuery's message. A join restriction with
// an aggregation threshold needs both.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert
func TestBigQueryPrivacyPolicyJoinRestriction(t *testing.T) {
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
		return d + "." + name
	}
	anyV := view("AnyView", `{"join_restriction_policy": {"join_condition": "JOIN_ANY", "join_allowed_columns": ["test_score", "last_name"]}}`)
	allV := view("AllView", `{"join_restriction_policy": {"join_condition": "JOIN_ALL", "join_allowed_columns": ["test_score", "last_name"]}}`)
	notReq := view("NotRequired", `{"join_restriction_policy": {"join_condition": "JOIN_NOT_REQUIRED"}}`)
	notReqCol := view("NotRequiredCol", `{"join_restriction_policy": {"join_condition": "JOIN_NOT_REQUIRED", "join_allowed_columns": ["test_score"]}}`)
	blocked := view("Blocked", `{"join_restriction_policy": {"join_condition": "JOIN_BLOCKED"}}`)
	both := view("Both", `{"aggregation_threshold_policy": {"threshold": 3, "privacy_unit_column": "last_name"}, `+
		`"join_restriction_policy": {"join_condition": "JOIN_ANY", "join_allowed_columns": ["test_score", "test_id"]}}`)
	student := "`" + d + ".StudentTable`"

	for _, insert := range []bool{false, true} {
		got, err := privacyRows(t, c, h, "SELECT test_score, e.last_name FROM `"+anyV+"` e INNER JOIN "+student+" USING (test_score)", insert)
		if err != nil || strings.Join(got, "|") != "490 Ivanov|500 Wang|510 Hansen|550 Silva|580 Devi" {
			t.Errorf("insert %v: JOIN_ANY on test_score: %v, %v", insert, got, err)
		}
	}
	got, err := privacyRows(t, c, h, "SELECT COUNT(*) FROM `"+allV+"` e JOIN "+student+" s ON e.test_score = s.test_score AND e.last_name = s.last_name", false)
	if err != nil || strings.Join(got, "|") != "5" {
		t.Errorf("JOIN_ALL on both: %v, %v", got, err)
	}
	if got, err := privacyRows(t, c, h, "SELECT COUNT(*) FROM `"+notReqCol+"`", false); err != nil || strings.Join(got, "|") != "11" {
		t.Errorf("JOIN_NOT_REQUIRED, no join: %v, %v", got, err)
	}
	if got, err := privacyRows(t, c, h, "SELECT COUNT(*) FROM `"+notReqCol+"` JOIN "+student+" USING (test_score)", false); err != nil || strings.Join(got, "|") != "5" {
		t.Errorf("JOIN_NOT_REQUIRED on an allowed column: %v, %v", got, err)
	}
	got, err = privacyRows(t, c, h, "SELECT WITH AGGREGATION_THRESHOLD test_id, COUNT(*) FROM `"+both+"` JOIN "+student+
		" USING (test_score) GROUP BY test_id", false)
	if err != nil || strings.Join(got, "|") != "U25 4" {
		t.Errorf("aggregation threshold and JOIN_ANY: %v, %v; want U25 4", got, err)
	}

	for _, c2 := range []struct{ what, sql, want string }{
		{"JOIN_ALL on one column", "SELECT * FROM `" + allV + "` INNER JOIN " + student + " USING (last_name)",
			"Joining must occur on all of the following columns [test_score, last_name] on table " + allV + "."},
		{"JOIN_ALL with no join", "SELECT * FROM `" + allV + "`",
			"Joining must occur on all of the following columns [test_score, last_name] on table " + allV + "."},
		{"JOIN_ANY on another column", "SELECT WITH AGGREGATION_THRESHOLD COUNT(*) FROM `" + both + "` INNER JOIN " + student + " USING (last_name)",
			"Joining must occur on at least one of the following columns [test_score, test_id] on table " + both + "."},
		{"JOIN_NOT_REQUIRED, no allowed column", "SELECT * FROM `" + notReq + "`",
			"At least one allowed column must be specified with join_condition = 'JOIN_NOT_REQUIRED'."},
		{"JOIN_NOT_REQUIRED on another column", "SELECT * FROM `" + notReqCol + "` INNER JOIN " + student + " USING (last_name)",
			"Join occurring on a restricted column."},
		{"JOIN_BLOCKED", "SELECT * FROM `" + blocked + "` INNER JOIN " + student + " USING (test_score)",
			"Join occurring on a restricted column."},
		{"the threshold missing", "SELECT * FROM `" + both + "` JOIN " + student + " USING (test_score)", ""},
	} {
		_, err := privacyRows(t, c, h, c2.sql, false)
		wantReason(t, c2.what, err, http.StatusBadRequest, "invalidQuery")
		if err != nil && !strings.Contains(err.Error(), c2.want) {
			t.Errorf("%s: %v, want %q", c2.what, err, c2.want)
		}
	}
	_, err = privacyRows(t, c, h, "SELECT * FROM `"+anyV+"` e LEFT JOIN "+student+" USING (test_score)", false)
	wantReason(t, "a LEFT JOIN", err, http.StatusNotImplemented, "notImplemented")
}
