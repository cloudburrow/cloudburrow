//go:build compat

package compat

import (
	"strconv"
	"testing"

	bq "google.golang.org/api/bigquery/v2"
)

// TestBigQueryPrivacyPolicyTableOptions (#1580): INFORMATION_SCHEMA.
// TABLE_OPTIONS lists a table's and a view's privacy_policy. TABLE_OPTIONS'
// documentation lists no privacy_policy option; the DDL documents its value
// as a "JSON-formatted STRING", so it is a STRING option whose value is the
// DDL's JSON quoted (UNVERIFIED). A view's privacy_policy set by
// tables.patch (view.privacyPolicy) is listed as the DDL's JSON too, its
// keys in snake case; ALTER … SET OPTIONS (privacy_policy = NULL) drops it.
//
// covers: bigquery.jobs.query, bigquery.tables.patch
func TestBigQueryPrivacyPolicyTableOptions(t *testing.T) {
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
	option := func(table string) (typ, value string) {
		t.Helper()
		for _, r := range rows(t, ctx, c, "SELECT option_type, option_value FROM `"+d+
			"`.INFORMATION_SCHEMA.TABLE_OPTIONS WHERE table_name = '"+table+"' AND option_name = 'privacy_policy'") {
			return r[0].(string), r[1].(string)
		}
		return "", ""
	}
	tablePolicy := `{"aggregation_threshold_policy": {"threshold": 3, "privacy_unit_column": "last_name"}}`
	run("CREATE TABLE `" + d + ".ExamTable` OPTIONS (privacy_policy = '" + tablePolicy + "') AS (" + privacyExam + ")")
	if typ, v := option("ExamTable"); typ != "STRING" || v != strconv.Quote(tablePolicy) {
		t.Errorf("a table's privacy_policy: %s %s, want STRING %s", typ, v, strconv.Quote(tablePolicy))
	}

	viewPolicy := `{"differential_privacy_policy": {"max_epsilon_per_query": 10.01, "epsilon_budget": 25.6, ` +
		`"privacy_unit_column": "last_name", "delta_per_query": 0.01, "delta_budget": 0.1, "max_groups_contributed": 2}}`
	run("CREATE TABLE `" + d + ".Plain` AS (" + privacyExam + ")")
	run("CREATE VIEW `" + d + ".ExamView` OPTIONS (privacy_policy = '" + viewPolicy + "') AS (SELECT * FROM `" + d + ".Plain`)")
	if typ, v := option("ExamView"); typ != "STRING" || v != strconv.Quote(viewPolicy) {
		t.Errorf("a view's privacy_policy: %s %s", typ, v)
	}
	run("ALTER VIEW `" + d + ".ExamView` SET OPTIONS (privacy_policy = NULL)")
	if typ, v := option("ExamView"); typ != "" {
		t.Errorf("privacy_policy after SET OPTIONS (privacy_policy = NULL): %s %s", typ, v)
	}

	viewQuery := "SELECT * FROM `" + d + ".Plain`"
	run("CREATE VIEW `" + d + ".Patched` AS " + viewQuery)
	svc := bigqueryService(t, h)
	if _, err := svc.Tables.Patch(project, d, "Patched", &bq.Table{View: &bq.ViewDefinition{Query: viewQuery,
		PrivacyPolicy: &bq.PrivacyPolicy{JoinRestrictionPolicy: &bq.JoinRestrictionPolicy{JoinCondition: "JOIN_ALL",
			JoinAllowedColumns: []string{"test_id"}}}}}).Context(ctx).Do(); err != nil {
		t.Fatalf("tables.patch: %v", err)
	}
	want := strconv.Quote(`{"join_restriction_policy":{"join_allowed_columns":["test_id"],"join_condition":"JOIN_ALL"}}`)
	if typ, v := option("Patched"); typ != "STRING" || v != want {
		t.Errorf("a patched view's privacy_policy: %s %s, want STRING %s", typ, v, want)
	}
	if typ, _ := option("Plain"); typ != "" {
		t.Errorf("a table with no policy lists privacy_policy")
	}
}
