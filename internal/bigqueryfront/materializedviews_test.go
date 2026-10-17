package bigqueryfront

import (
	"strings"
	"testing"
)

func TestParseMVStatement(t *testing.T) {
	for _, c := range []struct {
		sql, kind, path, query, msg string
		ok, replace, ifCheck        bool
	}{
		{sql: "CREATE MATERIALIZED VIEW ds.mv AS SELECT k, SUM(v) s FROM ds.t GROUP BY k", ok: true, kind: "create",
			path: "ds.mv", query: "SELECT k, SUM(v) s FROM ds.t GROUP BY k"},
		{sql: "CREATE OR REPLACE MATERIALIZED VIEW ds.mv OPTIONS (enable_refresh = false, refresh_interval_minutes = 60) AS SELECT 1 FROM ds.t;",
			ok: true, kind: "create", path: "ds.mv", replace: true, query: "SELECT 1 FROM ds.t"},
		{sql: "CREATE MATERIALIZED VIEW IF NOT EXISTS ds.mv AS SELECT 1 FROM ds.t", ok: true, kind: "create", path: "ds.mv", ifCheck: true,
			query: "SELECT 1 FROM ds.t"},
		{sql: "CREATE MATERIALIZED VIEW ds.mv OPTIONS (refresh_interval_minutes = 0) AS SELECT 1 FROM ds.t", ok: true, kind: "create",
			path: "ds.mv", query: "SELECT 1 FROM ds.t", msg: "refresh_interval_minutes"},
		{sql: "ALTER MATERIALIZED VIEW IF EXISTS ds.mv SET OPTIONS (enable_refresh = TRUE)", ok: true, kind: "alter", path: "ds.mv", ifCheck: true},
		{sql: "DROP MATERIALIZED VIEW ds.mv", ok: true, kind: "drop", path: "ds.mv"},
		{sql: "CALL BQ.REFRESH_MATERIALIZED_VIEW('p.ds.mv')", ok: true, kind: "refresh", path: "p.ds.mv"},
		{sql: "CREATE VIEW ds.v AS SELECT 1"},
		{sql: "SELECT 'MATERIALIZED'"},
		{sql: "CREATE MATERIALIZED VIEW ds.mv AS SELECT 1 FROM ds.t; SELECT 1"},
	} {
		st, ok := parseMVStatement(c.sql)
		if ok != c.ok {
			t.Errorf("%s: ok %v", c.sql, ok)
			continue
		}
		if !ok {
			continue
		}
		if st.kind != c.kind || strings.Join(st.path, ".") != c.path || st.replace != c.replace || st.ifCheck != c.ifCheck ||
			strings.TrimSpace(st.d.query) != c.query || (c.msg == "") != (st.msg == "") || !strings.Contains(st.msg, c.msg) {
			t.Errorf("%s: %+v", c.sql, st)
		}
	}
}

func TestMVQueryLimit(t *testing.T) {
	for q, want := range map[string]string{
		"SELECT k, SUM(v) FROM ds.t GROUP BY k":           "",
		"SELECT k, CURRENT_TIMESTAMP() FROM ds.t":         "CURRENT_TIMESTAMP",
		"SELECT k FROM ds.t ORDER BY k":                   "ORDER BY",
		"SELECT k, ROW_NUMBER() OVER (ORDER BY k) FROM t": "analytic",
		"SELECT COUNT(DISTINCT k) FROM ds.t":              "COUNT(DISTINCT)",
		"SELECT 1":                                        "base table",
	} {
		got := mvQueryLimit(q, false)
		if (want == "") != (got == "") || !strings.Contains(got, want) {
			t.Errorf("%s: %q, want %q", q, got, want)
		}
	}
	if got := mvQueryLimit("SELECT k FROM ds.t ORDER BY k", true); got != "" {
		t.Errorf("non-incremental ORDER BY: %q", got)
	}
}

func TestPartitionFilterRefusal(t *testing.T) {
	meta := map[string]any{"requirePartitionFilter": true, "timePartitioning": map[string]any{"type": "DAY", "field": "day"}}
	for sql, refused := range map[string]bool{
		"SELECT * FROM ds.t":                                 true,
		"SELECT * FROM ds.t WHERE day = '2024-01-01'":        false,
		"SELECT * FROM ds.t WHERE x = 1":                     true,
		"SELECT day FROM ds.t":                               true,
		"SELECT * FROM ds.t WHERE `day` > DATE '2024-01-01'": false,
		// #1456: as querying-partitioned-tables documents.
		"SELECT * FROM ds.t WHERE day = '2024-01-01' AND x = 'a'":                          false,
		"SELECT * FROM ds.t WHERE day = '2024-01-01' OR x = 'a'":                           true,
		"SELECT * FROM ds.t WHERE x = 'a' AND (day = '2024-01-01' OR day = '2024-01-02')":  false,
		"SELECT * FROM ds.t WHERE day >= y":                                                true,
		"SELECT * FROM ds.t WHERE day = (SELECT MAX(d) FROM ds.u)":                         true,
		"SELECT * FROM ds.t WHERE DATE_ADD(day, INTERVAL 1 DAY) > CURRENT_DATE()":          false,
		"SELECT * FROM ds.t WHERE day > DATE_SUB(CURRENT_DATE(), INTERVAL 1 DAY)":          false,
		"SELECT * FROM ds.t WHERE day + 1 > CURRENT_DATE()":                                true,
		"SELECT * FROM ds.t WHERE ABS(DATE_DIFF(day, CURRENT_DATE(), DAY)) < 2":            true,
		"SELECT * FROM ds.t WHERE EXTRACT(YEAR FROM day) = 2024":                           false,
		"SELECT * FROM ds.t WHERE EXTRACT(MONTH FROM day) = 1":                             true,
		"SELECT * FROM ds.t WHERE FORMAT_TIMESTAMP('%Y%m%d', day) = '20240101'":            false,
		"SELECT * FROM ds.t WHERE FORMAT_TIMESTAMP('%m', day) = '01'":                      true,
		"SELECT * FROM ds.t t WHERE t.day BETWEEN '2024-01-01' AND '2024-01-31' AND x = 1": false,
		"SELECT * FROM ds.t WHERE day = @d":                                                false,
		"SELECT * FROM ds.t WHERE x = 1 GROUP BY day":                                      true,
	} {
		reads, _, ok := tableReads(sql, "p", "")
		if !ok || len(reads) == 0 {
			t.Fatalf("%s: reads %v", sql, reads)
		}
		msg := partitionFilterRefusal(reads[0], meta, "p.ds.t")
		if (msg != "") != refused {
			t.Errorf("%s: %q", sql, msg)
		}
	}
	if msg := partitionFilterRefusal(tableRead{}, map[string]any{"timePartitioning": map[string]any{"type": "DAY"}}, "x"); msg != "" {
		t.Errorf("without the option: %q", msg)
	}
}

// TestMVScriptsAndStaleness (#1415): a materialized view's statements in a
// script are run in turn, and max_staleness is read as milliseconds.
func TestMVScriptsAndStaleness(t *testing.T) {
	for sql, want := range map[string]bool{
		"CREATE TABLE ds.t (x INT64); CREATE MATERIALIZED VIEW ds.mv AS SELECT x FROM ds.t": true,
		"SELECT 1; ALTER MATERIALIZED VIEW ds.mv SET OPTIONS (enable_refresh = FALSE)":      true,
		"SELECT 1; DROP MATERIALIZED VIEW ds.mv":                                            true,
		"CREATE TEMP TABLE x AS SELECT 1 AS a; DROP MATERIALIZED VIEW ds.mv":                false,
		"CREATE MATERIALIZED VIEW ds.mv AS SELECT x FROM ds.t":                              false,
	} {
		if got := hasControlFlow(sql); got != want {
			t.Errorf("hasControlFlow(%q) = %v, want %v", sql, got, want)
		}
	}
	v := checkDDL("CREATE TEMP TABLE x AS SELECT 1 AS a; CREATE MATERIALIZED VIEW ds.mv AS SELECT a FROM ds.t")
	if v.code != 501 || !strings.Contains(v.msg, "TEMP table") {
		t.Errorf("TEMP script: %+v", v)
	}
	for s, want := range map[string]int64{"0-0 0 4:0:0": 4 * 3600000, "0-0 1 0:30:0.5": 86400000 + 1800500, "0-1 0 0:0:0": 30 * 86400000} {
		if got, ok := intervalMillis(s); !ok || got != want {
			t.Errorf("intervalMillis(%q) = %d %v, want %d", s, got, ok, want)
		}
	}
	if _, ok := intervalMillis("4 hours"); ok {
		t.Error("intervalMillis read 4 hours")
	}
}
