//go:build compat

package compat

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"
)

// TestBigQueryDDLMoreTableOptions (#1390): the table options max_staleness,
// default_rounding_mode, tags, require_partition_filter,
// enable_change_history, storage_uri and privacy_policy of BigQuery's
// table_option_list
// (https://cloud.google.com/bigquery/docs/reference/standard-sql/data-definition-language#table_option_list),
// set by CREATE TABLE and ALTER TABLE SET OPTIONS, read back through
// Table.Metadata and INFORMATION_SCHEMA.TABLE_OPTIONS. A query of a table
// with require_partition_filter and no filter on its partitioning column
// fails 400 with BigQuery's message, and one with the filter runs; a
// query of a table with a privacy_policy is 501, the policy not enforced.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tables.get
func TestBigQueryDDLMoreTableOptions(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	run := func(sql string, insert bool) {
		t.Helper()
		if err := bqRun(ctx, c, sql, insert); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	options := func(table string) map[string]string {
		t.Helper()
		out := map[string]string{}
		for _, r := range rows(t, ctx, c, "SELECT option_name, option_value FROM `"+ds.DatasetID+
			"`.INFORMATION_SCHEMA.TABLE_OPTIONS WHERE table_name = '"+table+"'") {
			out[r[0].(string)] = r[1].(string)
		}
		return out
	}
	ref := "`" + ds.DatasetID + ".more`"
	run("CREATE TABLE "+ref+" (day DATE, n NUMERIC) PARTITION BY day OPTIONS (max_staleness = INTERVAL 4 HOUR, "+
		"default_rounding_mode = 'ROUND_HALF_EVEN', tags = [('123/env', 'prod')], require_partition_filter = TRUE, "+
		"enable_change_history = TRUE, storage_uri = 'gs://bucket/more')", false)
	md, err := ds.Table("more").Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if md.MaxStaleness == nil || md.MaxStaleness.String() != "0-0 0 4:0:0" {
		t.Errorf("maxStaleness %v, want 4 hours", md.MaxStaleness)
	}
	if md.ResourceTags["123/env"] != "prod" || !md.RequirePartitionFilter {
		t.Errorf("resourceTags %v, requirePartitionFilter %v", md.ResourceTags, md.RequirePartitionFilter)
	}
	if md.BigLakeConfiguration == nil || md.BigLakeConfiguration.StorageURI != "gs://bucket/more" {
		t.Errorf("biglakeConfiguration %+v", md.BigLakeConfiguration)
	}
	got := options("more")
	for name, want := range map[string]string{"max_staleness": `INTERVAL "0-0 0 4:0:0" YEAR TO SECOND`,
		"default_rounding_mode": `"ROUND_HALF_EVEN"`, "tags": `[STRUCT("123/env", "prod")]`,
		"require_partition_filter": "true", "enable_change_history": "true", "storage_uri": `"gs://bucket/more"`} {
		if got[name] != want {
			t.Errorf("TABLE_OPTIONS %s = %q, want %q (all: %v)", name, got[name], want, got)
		}
	}

	// require_partition_filter.
	run("INSERT INTO "+ref+" (day, n) VALUES (DATE '2024-01-01', 1)", false)
	for _, insert := range []bool{false, true} {
		err := bqRun(ctx, c, "SELECT * FROM "+ref, insert)
		wantReason(t, "a query with no partition filter", err, http.StatusBadRequest, "invalidQuery")
		want := "Cannot query over table '" + project + "." + ds.DatasetID + ".more' without a filter over column(s) 'day' " +
			"that can be used for partition elimination"
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("insert %v: %v, want %q", insert, err, want)
		}
	}
	if got := rows(t, ctx, c, "SELECT n FROM "+ref+" WHERE day = DATE '2024-01-01'"); len(got) != 1 {
		t.Errorf("with the filter: %v", got)
	}

	// ALTER sets and clears them.
	for _, insert := range []bool{false, true} {
		run("ALTER TABLE "+ref+" SET OPTIONS (max_staleness = NULL, tags = NULL, require_partition_filter = FALSE, "+
			"enable_change_history = NULL, default_rounding_mode = 'ROUND_HALF_AWAY_FROM_ZERO')", insert)
	}
	md, err = ds.Table("more").Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if md.MaxStaleness != nil || len(md.ResourceTags) != 0 || md.RequirePartitionFilter {
		t.Errorf("after ALTER: maxStaleness %v, tags %v, requirePartitionFilter %v", md.MaxStaleness, md.ResourceTags,
			md.RequirePartitionFilter)
	}
	got = options("more")
	if got["default_rounding_mode"] != `"ROUND_HALF_AWAY_FROM_ZERO"` || got["enable_change_history"] != "" || got["tags"] != "" {
		t.Errorf("TABLE_OPTIONS after ALTER: %v", got)
	}
	if got := rows(t, ctx, c, "SELECT n FROM "+ref); len(got) != 1 {
		t.Errorf("without require_partition_filter: %v", got)
	}

	// privacy_policy: kept, a query refused.
	run("CREATE TABLE `"+ds.DatasetID+".private` (user STRING, x INT64) OPTIONS (privacy_policy = "+
		`'{"aggregation_threshold_policy": {"threshold": 3, "privacy_unit_columns": "user"}}')`, false)
	if got := options("private"); !strings.Contains(got["privacy_policy"], "aggregation_threshold_policy") {
		t.Errorf("TABLE_OPTIONS privacy_policy: %v", got)
	}
	err = bqRun(ctx, c, "SELECT COUNT(*) FROM `"+ds.DatasetID+".private`", false)
	wantReason(t, "a query of a table with a privacy policy", err, http.StatusNotImplemented, "notImplemented")

	// A bad value: 400.
	err = bqRun(ctx, c, "CREATE TABLE `"+ds.DatasetID+".bad` (x INT64) OPTIONS (default_rounding_mode = 'UP')", false)
	wantReason(t, "default_rounding_mode 'UP'", err, http.StatusBadRequest, "invalidQuery")
}

// TestBigQueryMaterializedViews (#1390): CREATE MATERIALIZED VIEW, as
// BigQuery documents it
// (https://cloud.google.com/bigquery/docs/materialized-views-create), makes
// a table of type MATERIALIZED_VIEW with its materializedView definition;
// a query of it reads its query's current results, even after a change to
// a base table before a refresh, unless max_staleness allows the last
// refresh's (#1415); CALL BQ.REFRESH_MATERIALIZED_VIEW and the automatic
// refresh, once a base table changed and the refresh interval passed (the
// front's clock moved forward), set lastRefreshTime; ALTER MATERIALIZED VIEW SET
// OPTIONS changes it; INFORMATION_SCHEMA.MATERIALIZED_VIEWS lists it;
// tables.insert with a materializedView makes one; DROP MATERIALIZED VIEW
// deletes it. Its statements, and ALTER TABLE SET OPTIONS, run in a
// multi-statement script, each a child job (#1415); in a script with a
// TEMP table they are 501. A query BigQuery refuses as a materialized view's (a
// non-deterministic function, ORDER BY) is 400.
//
// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.tables.get, bigquery.tables.insert
func TestBigQueryMaterializedViews(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	ds := validationDataset(t, h, c)
	ctx := h.Context()
	run := func(sql string, insert bool) {
		t.Helper()
		if err := bqRun(ctx, c, sql, insert); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	path := func(table string) string { return "`" + ds.DatasetID + "." + table + "`" }
	sum := func(table string) int64 {
		t.Helper()
		got := rows(t, ctx, c, "SELECT SUM(s) FROM "+path(table))
		if len(got) != 1 || got[0][0] == nil {
			t.Fatalf("SUM of %s: %v", table, got)
		}
		return got[0][0].(int64)
	}
	run("CREATE TABLE "+path("base")+" (k STRING, v INT64)", false)
	run("INSERT INTO "+path("base")+" (k, v) VALUES ('a', 1), ('b', 2)", false)
	query := "SELECT k, SUM(v) AS s FROM " + path("base") + " GROUP BY k"
	for _, insert := range []bool{false, true} {
		run("CREATE OR REPLACE MATERIALIZED VIEW "+path("mv")+" OPTIONS (enable_refresh = TRUE, "+
			"refresh_interval_minutes = 60, description = 'totals') AS "+query, insert)
	}
	md, err := ds.Table("mv").Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if md.Type != bigquery.MaterializedView || md.MaterializedView == nil || md.Description != "totals" {
		t.Fatalf("type %q, definition %+v, description %q", md.Type, md.MaterializedView, md.Description)
	}
	mv := md.MaterializedView
	if mv.Query != query || !mv.EnableRefresh || mv.RefreshInterval != time.Hour || mv.LastRefreshTime.IsZero() {
		t.Errorf("definition %+v", mv)
	}
	if got := sum("mv"); got != 3 {
		t.Errorf("SUM of the view: %d, want 3", got)
	}

	lastRefresh := func(table string) time.Time {
		t.Helper()
		md, err := ds.Table(table).Metadata(ctx)
		if err != nil || md.MaterializedView == nil {
			t.Fatalf("%s: %v %+v", table, err, md)
		}
		return md.MaterializedView.LastRefreshTime
	}
	created := lastRefresh("mv")

	// #1415: a read after a change to a base table sees it, as BigQuery's
	// read of the base tables does, before any refresh.
	run("INSERT INTO "+path("base")+" (k, v) VALUES ('a', 10)", false)
	if got := sum("mv"); got != 13 {
		t.Errorf("after a change to the base table: %d, want 13 (current)", got)
	}
	if got := lastRefresh("mv"); !got.Equal(created) {
		t.Errorf("lastRefreshTime %v moved from %v within the refresh interval", got, created)
	}
	advanceBigQueryClock(t, h, time.Minute)
	run("CALL BQ.REFRESH_MATERIALIZED_VIEW('"+ds.DatasetID+".mv')", false)
	refreshed := lastRefresh("mv")
	if !refreshed.After(created) {
		t.Errorf("lastRefreshTime %v after CALL BQ.REFRESH_MATERIALIZED_VIEW, want after %v", refreshed, created)
	}
	if got := sum("mv"); got != 13 {
		t.Errorf("after CALL BQ.REFRESH_MATERIALIZED_VIEW: %d, want 13", got)
	}
	// The automatic refresh: a base table changed, and the interval passed.
	run("INSERT INTO "+path("base")+" (k, v) VALUES ('b', 100)", false)
	advanceBigQueryClock(t, h, 61*time.Minute)
	auto := lastRefresh("mv")
	if !auto.After(refreshed) {
		t.Errorf("lastRefreshTime %v after the refresh interval, want after %v", auto, refreshed)
	}
	if got := sum("mv"); got != 113 {
		t.Errorf("after the refresh interval: %d, want 113", got)
	}

	// ALTER MATERIALIZED VIEW SET OPTIONS.
	run("ALTER MATERIALIZED VIEW "+path("mv")+" SET OPTIONS (enable_refresh = FALSE, max_staleness = INTERVAL 2 HOUR)", true)
	md, err = ds.Table("mv").Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if md.MaterializedView == nil || md.MaterializedView.EnableRefresh || md.MaxStaleness == nil ||
		md.MaxStaleness.String() != "0-0 0 2:0:0" {
		t.Errorf("after ALTER: %+v, maxStaleness %v", md.MaterializedView, md.MaxStaleness)
	}
	// Within max_staleness, the last refresh's results; past it, current
	// ones; with enable_refresh false, no refresh.
	run("INSERT INTO "+path("base")+" (k, v) VALUES ('b', 1000)", false)
	advanceBigQueryClock(t, h, 61*time.Minute)
	if got := sum("mv"); got != 113 {
		t.Errorf("within max_staleness: %d, want 113 (the last refresh's)", got)
	}
	advanceBigQueryClock(t, h, 60*time.Minute)
	if got := sum("mv"); got != 1113 {
		t.Errorf("past max_staleness: %d, want 1113 (current)", got)
	}
	if got := lastRefresh("mv"); !got.Equal(auto) {
		t.Errorf("lastRefreshTime %v with enable_refresh false, want %v", got, auto)
	}

	// INFORMATION_SCHEMA.MATERIALIZED_VIEWS.
	got := rows(t, ctx, c, "SELECT table_name, last_refresh_time IS NOT NULL FROM `"+ds.DatasetID+
		"`.INFORMATION_SCHEMA.MATERIALIZED_VIEWS")
	if len(got) != 1 || got[0][0] != "mv" || got[0][1] != true {
		t.Errorf("MATERIALIZED_VIEWS: %v", got)
	}

	// The documented limits of its query.
	for _, q := range []string{"SELECT k, CURRENT_TIMESTAMP() AS t FROM " + path("base"),
		"SELECT k, v FROM " + path("base") + " ORDER BY k"} {
		err := bqRun(ctx, c, "CREATE MATERIALIZED VIEW "+path("bad")+" AS "+q, false)
		wantReason(t, q, err, http.StatusBadRequest, "invalidQuery")
	}
	if tableExists(t, ctx, ds.Table("bad")) {
		t.Error("a refused materialized view was made")
	}

	// tables.insert with a materializedView.
	err = ds.Table("mv2").Create(ctx, &bigquery.TableMetadata{MaterializedView: &bigquery.MaterializedViewDefinition{
		Query: query, EnableRefresh: true, RefreshInterval: 2 * time.Hour}})
	if err != nil {
		t.Fatalf("tables.insert: %v", err)
	}
	md, err = ds.Table("mv2").Metadata(ctx)
	if err != nil || md.Type != bigquery.MaterializedView || md.MaterializedView.RefreshInterval != 2*time.Hour {
		t.Errorf("tables.insert made %v %+v", err, md)
	}
	if got := sum("mv2"); got != 1113 {
		t.Errorf("SUM of mv2: %d, want 1113", got)
	}

	// #1415: in a multi-statement script, each a child job of the script's.
	script := "CREATE TABLE " + path("sbase") + " AS SELECT 1 AS v;\n" +
		"CREATE MATERIALIZED VIEW " + path("smv") + " OPTIONS (enable_refresh = FALSE) AS SELECT SUM(v) AS s FROM " + path("sbase") + ";\n" +
		"ALTER MATERIALIZED VIEW " + path("smv") + " SET OPTIONS (description = 'scripted');\n" +
		"ALTER TABLE " + path("sbase") + " SET OPTIONS (description = 'base');\n" +
		"INSERT INTO " + path("sbase") + " VALUES (2);\n" +
		"CALL BQ.REFRESH_MATERIALIZED_VIEW('" + ds.DatasetID + ".smv');\n" +
		"SELECT s FROM " + path("smv")
	job, err := c.Query(script).Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	it, err := job.Read(ctx)
	if err != nil {
		t.Fatalf("the script: %v", err)
	}
	var out []bigquery.Value
	if err := it.Next(&out); err != nil || len(out) != 1 || out[0] != int64(3) {
		t.Errorf("the script's rows: %v %v, want [3]", out, err)
	}
	md, err = ds.Table("smv").Metadata(ctx)
	if err != nil || md.Type != bigquery.MaterializedView || md.Description != "scripted" || md.MaterializedView.EnableRefresh {
		t.Errorf("the script's materialized view: %v %+v", err, md)
	}
	if md, err := ds.Table("sbase").Metadata(ctx); err != nil || md.Description != "base" {
		t.Errorf("the script's ALTER TABLE SET OPTIONS: %v %+v", err, md)
	}
	types := map[string]bool{}
	kids := job.Children(ctx)
	for {
		k, err := kids.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if qs, ok := k.LastStatus().Statistics.Details.(*bigquery.QueryStatistics); ok {
			types[qs.StatementType] = true
		}
	}
	for _, want := range []string{"CREATE_MATERIALIZED_VIEW", "ALTER_MATERIALIZED_VIEW", "CALL"} {
		if !types[want] {
			t.Errorf("no child job of statementType %s: %v", want, types)
		}
	}
	// #1434: with a TEMP table too (TestBigQueryMaterializedViewsInTempScripts).
	run("CREATE TEMP TABLE x AS SELECT 1 AS a; DROP MATERIALIZED VIEW "+path("smv")+"; SELECT * FROM x", false)
	run("DROP TABLE "+path("sbase"), false)
	if tableExists(t, ctx, ds.Table("smv")) {
		t.Error("the script's DROP MATERIALIZED VIEW left smv")
	}

	// DROP MATERIALIZED VIEW.
	for _, name := range []string{"mv", "mv2"} {
		run("DROP MATERIALIZED VIEW "+path(name), false)
		if tableExists(t, ctx, ds.Table(name)) {
			t.Errorf("DROP MATERIALIZED VIEW left %s", name)
		}
	}
	run("DROP MATERIALIZED VIEW IF EXISTS "+path("mv"), false)
	err = bqRun(ctx, c, "DROP MATERIALIZED VIEW "+path("mv"), false)
	wantReason(t, "DROP MATERIALIZED VIEW of none", err, http.StatusNotFound, "notFound")
}
