package bigqueryfront

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// privacyEmulator serves tables.get of ds.p, a view with columns u and x,
// ds.t, a table, and ds.around, a view of ds.p.
func privacyEmulator() http.Handler {
	schema := map[string]any{"fields": []any{map[string]any{"name": "u"}, map[string]any{"name": "x"}}}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/datasets/ds/tables/p"):
			writeJSON(w, http.StatusOK, map[string]any{"type": "VIEW", "creationTime": "1", "schema": schema,
				"view": map[string]any{"query": "SELECT * FROM ds.t"}})
		case strings.HasSuffix(r.URL.Path, "/datasets/ds/tables/t"):
			writeJSON(w, http.StatusOK, map[string]any{"type": "TABLE", "creationTime": "1", "schema": schema})
		case strings.HasSuffix(r.URL.Path, "/datasets/ds/tables/around"):
			writeJSON(w, http.StatusOK, map[string]any{"type": "VIEW", "creationTime": "1", "schema": schema,
				"view": map[string]any{"query": "SELECT * FROM `ds.p`"}})
		default:
			writeError(w, http.StatusNotFound, "notFound", "no")
		}
	})
}

func TestPrivacyViewOfPolicyView(t *testing.T) {
	s := &tableSettings{}
	s.set(streamKey("p", "ds", "p"), "1", moreOption{name: "privacy_policy",
		value: `{"aggregation_threshold_policy": {"threshold": 2, "privacy_unit_column": "u"}}`})
	f := front{next: privacyEmulator(), base: "/bigquery/v2/projects/p", tables: s}
	for sql, code := range map[string]int{
		// #1517: with no clause, BigQuery's "You must use ..." 400; with
		// one, still 501.
		"SELECT * FROM `ds.around`":                                   http.StatusBadRequest,
		"SELECT WITH AGGREGATION_THRESHOLD COUNT(*) FROM `ds.around`": http.StatusNotImplemented,
		"SELECT * FROM ds.p":                                          http.StatusBadRequest,
		"SELECT * FROM ds.t":                                          0,
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/bigquery/v2/projects/p/queries", nil)
		handled := f.checkReads(w, r, queryOptions{Query: sql}, false)
		if code == 0 && handled || code != 0 && (!handled || w.Code != code) {
			t.Errorf("%s: handled %v, %d %s; want %d", sql, handled, w.Code, w.Body, code)
		}
	}
}

// TestPrivacySubqueries (#1502): SELECT WITH AGGREGATION_THRESHOLD in a
// subquery, a CTE's query, after a WITH list or as CREATE TABLE ... AS's
// query is rewritten in its place; a read outside its own clause is still
// refused.
func TestPrivacySubqueries(t *testing.T) {
	s := &tableSettings{}
	s.set(streamKey("p", "ds", "p"), "1", moreOption{name: "privacy_policy",
		value: `{"aggregation_threshold_policy": {"threshold": 2, "privacy_unit_column": "u"}}`})
	var sent []string
	emu := privacyEmulator()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			q, _ := body["query"].(string)
			sent = append(sent, q)
			writeError(w, http.StatusTeapot, "sent", "sent")
			return
		}
		emu.ServeHTTP(w, r)
	})
	f := front{next: next, base: "/bigquery/v2/projects/p", tables: s}
	for _, c := range []struct {
		sql, want string
		code      int
	}{
		{"SELECT * FROM (SELECT WITH AGGREGATION_THRESHOLD x, COUNT(*) AS n FROM ds.p AS e GROUP BY x) WHERE n > 1",
			"SELECT * FROM (SELECT x, COUNT(*) AS n FROM ds.p AS e GROUP BY x HAVING COUNT(DISTINCT `e`.`u`) >= 2) WHERE n > 1", 0},
		{"WITH c AS (SELECT WITH AGGREGATION_THRESHOLD x, COUNT(*) AS n FROM ds.p AS e GROUP BY x) SELECT * FROM c",
			"WITH c AS (SELECT x, COUNT(*) AS n FROM ds.p AS e GROUP BY x HAVING COUNT(DISTINCT `e`.`u`) >= 2) SELECT * FROM c", 0},
		{"WITH c AS (SELECT 1 AS k) SELECT WITH AGGREGATION_THRESHOLD COUNT(*) FROM ds.p AS e",
			"WITH c AS (SELECT 1 AS k) SELECT COUNT(*) FROM ds.p AS e GROUP BY CAST(NULL AS BOOL)  HAVING COUNT(DISTINCT `e`.`u`) >= 2", 0},
		{"SELECT * FROM (SELECT WITH AGGREGATION_THRESHOLD OPTIONS(threshold=1) COUNT(*) FROM ds.p AS e)", "", http.StatusBadRequest},
		// #1517: no clause anywhere, BigQuery's 400 "You must use ...".
		{"SELECT * FROM (SELECT * FROM ds.p)", "", http.StatusBadRequest},
		{"WITH c AS (SELECT * FROM ds.p) SELECT COUNT(*) FROM c", "", http.StatusBadRequest},
		{"WITH c AS (SELECT * FROM ds.p) SELECT WITH AGGREGATION_THRESHOLD COUNT(*) FROM c", "", http.StatusNotImplemented},
		{"SELECT * FROM (SELECT WITH AGGREGATION_THRESHOLD COUNT(*) FROM (SELECT * FROM ds.p))", "", http.StatusNotImplemented},
		// #1517: two clauses, each rewritten in its place; one in another
		// is a multi-level aggregation.
		{"SELECT (SELECT WITH AGGREGATION_THRESHOLD COUNT(*) FROM ds.p e), (SELECT WITH AGGREGATION_THRESHOLD SUM(x) FROM ds.p)",
			"SELECT (SELECT COUNT(*) FROM ds.p e GROUP BY CAST(NULL AS BOOL)  HAVING COUNT(DISTINCT `e`.`u`) >= 2), " +
				"(SELECT SUM(x) FROM ds.p AS `p` GROUP BY CAST(NULL AS BOOL)  HAVING COUNT(DISTINCT `p`.`u`) >= 2)", 0},
		{"WITH a AS (SELECT WITH AGGREGATION_THRESHOLD x, COUNT(*) n FROM ds.p GROUP BY x), " +
			"b AS (SELECT WITH AGGREGATION_THRESHOLD OPTIONS(threshold=3) x, SUM(x) s FROM ds.p e GROUP BY x) SELECT * FROM a JOIN b USING (x)",
			"WITH a AS (SELECT x, COUNT(*) n FROM ds.p AS `p` GROUP BY x HAVING COUNT(DISTINCT `p`.`u`) >= 2), " +
				"b AS (SELECT x, SUM(x) s FROM ds.p e GROUP BY x HAVING COUNT(DISTINCT `e`.`u`) >= 3) SELECT * FROM a JOIN b USING (x)", 0},
		{"SELECT * FROM (SELECT WITH AGGREGATION_THRESHOLD COUNT(*) n FROM (SELECT WITH AGGREGATION_THRESHOLD x, COUNT(*) FROM ds.p GROUP BY x))",
			"", http.StatusBadRequest},
		{"SELECT WITH AGGREGATION_THRESHOLD COUNT(*) FROM (SELECT WITH AGGREGATION_THRESHOLD x, COUNT(*) FROM ds.p GROUP BY x)",
			"", http.StatusBadRequest},
	} {
		sent = nil
		w := httptest.NewRecorder()
		body, _ := json.Marshal(map[string]any{"query": c.sql})
		r := httptest.NewRequest("POST", "/bigquery/v2/projects/p/queries", bytes.NewReader(body))
		handled := f.checkReads(w, r, queryOptions{Query: c.sql}, false)
		switch {
		case c.code != 0 && (!handled || w.Code != c.code):
			t.Errorf("%s: handled %v, %d %s; want %d", c.sql, handled, w.Code, w.Body, c.code)
		case c.code == 0 && (len(sent) == 0 || sent[len(sent)-1] != c.want):
			t.Errorf("%s:\n sent %q (%d %s)\nwant %q", c.sql, sent, w.Code, w.Body, c.want)
		}
	}
}

func TestParsePrivacyPolicy(t *testing.T) {
	p, why := parsePrivacyPolicy(`{"aggregation_threshold_policy": {"threshold": 3, "privacy_unit_columns": ["u"]},
		"join_restriction_policy": {"join_condition": "JOIN_ANY", "join_allowed_columns": ["a", "b"]}}`)
	if why != "" || !p.hasAT || p.threshold != 3 || p.unit != "u" || p.join != "JOIN_ANY" || len(p.allowed) != 2 {
		t.Errorf("DDL form: %+v %q", p, why)
	}
	p, why = parsePrivacyPolicy(`{"aggregationThresholdPolicy": {"threshold": "5", "privacyUnitColumns": ["u"]}}`)
	if why != "" || p.threshold != 5 || p.unit != "u" {
		t.Errorf("REST form: %+v %q", p, why)
	}
	for _, bad := range []string{`[]`, `{}`, `{"aggregation_threshold_policy": {"threshold": 3}}`,
		`{"join_restriction_policy": {"join_condition": "JOIN_SOME"}}`} {
		if _, why := parsePrivacyPolicy(bad); why == "" {
			t.Errorf("%s: read", bad)
		}
	}
}

func TestRewriteAggregationThreshold(t *testing.T) {
	at := func(sql string) *privacyRead {
		reads, _, _ := tableReads(sql, "p", "")
		return &privacyRead{tr: reads[0], name: "ds.v", pol: privacyPolicy{hasAT: true, threshold: 3, unit: "u"}}
	}
	for sql, want := range map[string]string{
		"SELECT WITH AGGREGATION_THRESHOLD k, COUNT(*) FROM ds.v AS e GROUP BY k ORDER BY k":                        "SELECT k, COUNT(*) FROM ds.v AS e GROUP BY k HAVING COUNT(DISTINCT `e`.`u`) >= 3 ORDER BY k",
		"SELECT WITH AGGREGATION_THRESHOLD OPTIONS(threshold=4) k, SUM(x) FROM ds.v e GROUP BY k HAVING SUM(x) > 1": "SELECT k, SUM(x) FROM ds.v e GROUP BY k HAVING (SUM(x) > 1) AND COUNT(DISTINCT `e`.`u`) >= 4",
		"SELECT WITH AGGREGATION_THRESHOLD COUNT(DISTINCT u) FROM ds.v AS e":                                        "SELECT COUNT(DISTINCT u) FROM ds.v AS e GROUP BY CAST(NULL AS BOOL)  HAVING COUNT(DISTINCT `e`.`u`) >= 3",
	} {
		toks, _ := lex(sql)
		got, msg, _ := rewriteAggregationThreshold(sql, toks, at(sql))
		if got != want || msg != "" {
			t.Errorf("%s:\n got %s %s\nwant %s", sql, got, msg, want)
		}
	}
	for sql, unsupported := range map[string]bool{
		"SELECT WITH AGGREGATION_THRESHOLD u, COUNT(*) FROM ds.v AS e GROUP BY u":                  false,
		"SELECT WITH AGGREGATION_THRESHOLD e.u, COUNT(*) FROM ds.v AS e GROUP BY 1":                false,
		"SELECT WITH AGGREGATION_THRESHOLD k, MAX(x) FROM ds.v AS e GROUP BY k":                    false,
		"SELECT WITH AGGREGATION_THRESHOLD SUM(COUNT(*)) FROM ds.v AS e":                           false,
		"SELECT WITH AGGREGATION_THRESHOLD OPTIONS(threshold=2) COUNT(*) FROM ds.v AS e":           false,
		"SELECT WITH AGGREGATION_THRESHOLD * FROM ds.v AS e":                                       false,
		"SELECT WITH AGGREGATION_THRESHOLD COUNT(*) OVER () FROM ds.v AS e":                        true,
		"SELECT WITH AGGREGATION_THRESHOLD ds.agg(x) FROM ds.v AS e":                               true,
		"SELECT WITH AGGREGATION_THRESHOLD COUNT(*) FROM ds.v AS e UNION ALL SELECT 1 FROM ds.v e": true,
	} {
		toks, _ := lex(sql)
		_, msg, uns := rewriteAggregationThreshold(sql, toks, at(sql))
		if msg == "" || uns != unsupported {
			t.Errorf("%s: %q unsupported %v, want refused (unsupported %v)", sql, msg, uns, unsupported)
		}
	}
}

func TestCheckJoinRestriction(t *testing.T) {
	cols := map[string]bool{"a": true, "b": true, "c": true}
	check := func(sql, cond string, allowed ...string) (string, bool) {
		reads, _, _ := tableReads(sql, "p", "")
		for _, tr := range reads {
			if tr.table == "v" {
				return checkJoinRestriction(privacyRead{tr: tr, name: "ds.v", columns: cols,
					pol: privacyPolicy{hasJoin: true, join: cond, allowed: allowed}})
			}
		}
		t.Fatalf("%s: no read of ds.v", sql)
		return "", false
	}
	for _, c := range []struct {
		sql, cond string
		allowed   []string
		want      string
	}{
		{"SELECT * FROM ds.v JOIN ds.o USING (a)", "JOIN_ANY", []string{"a", "b"}, ""},
		{"SELECT * FROM ds.o JOIN ds.v x ON o.a = x.a AND o.b = x.b", "JOIN_ALL", []string{"a", "b"}, ""},
		{"SELECT * FROM ds.v JOIN ds.o USING (a)", "JOIN_ALL", []string{"a", "b"}, "Joining must occur on all"},
		{"SELECT * FROM ds.v", "JOIN_ANY", []string{"a"}, "Joining must occur on at least one"},
		{"SELECT * FROM ds.v JOIN ds.o USING (c)", "JOIN_ANY", []string{"a"}, "Joining must occur on at least one"},
		{"SELECT * FROM ds.v JOIN ds.o USING (a, c)", "JOIN_ANY", []string{"a"}, "Join occurring on a restricted column."},
		{"SELECT * FROM ds.v", "JOIN_NOT_REQUIRED", nil, "At least one allowed column"},
		{"SELECT * FROM ds.v", "JOIN_NOT_REQUIRED", []string{"a"}, ""},
		{"SELECT * FROM ds.v JOIN ds.o USING (b)", "JOIN_NOT_REQUIRED", []string{"a"}, "Join occurring on a restricted column."},
		{"SELECT * FROM ds.v JOIN ds.o USING (a)", "JOIN_BLOCKED", nil, "Join occurring on a restricted column."},
		{"SELECT * FROM ds.v", "JOIN_BLOCKED", nil, ""},
		{"SELECT * FROM ds.v, ds.o", "JOIN_BLOCKED", nil, "Join occurring on a restricted column."},
		// #1502: joins that are not INNER, and a read in a subquery.
		{"SELECT * FROM ds.v LEFT JOIN ds.o USING (a)", "JOIN_BLOCKED", nil, "Join occurring on a restricted column."},
		{"SELECT * FROM ds.v LEFT JOIN ds.o USING (a)", "JOIN_ALL", []string{"a"}, "Joining must occur on all"},
		{"SELECT * FROM ds.v x FULL OUTER JOIN ds.o ON x.a = o.a", "JOIN_ALL", []string{"a"}, "Joining must occur on all"},
		{"SELECT * FROM ds.v CROSS JOIN ds.o", "JOIN_ALL", []string{"a"}, "Joining must occur on all"},
		{"SELECT * FROM ds.v LEFT JOIN ds.o USING (c)", "JOIN_ANY", []string{"a"}, "Joining must occur on at least one"},
		{"SELECT * FROM ds.v RIGHT JOIN ds.o USING (a, c)", "JOIN_ANY", []string{"a"}, "Join occurring on a restricted column."},
		{"SELECT * FROM ds.v LEFT JOIN ds.o USING (a)", "JOIN_NOT_REQUIRED", []string{"a"}, ""},
		{"SELECT * FROM ds.v LEFT OUTER JOIN ds.o USING (b)", "JOIN_NOT_REQUIRED", []string{"a"}, "Join occurring on a restricted column."},
		{"SELECT * FROM ds.v CROSS JOIN ds.o", "JOIN_NOT_REQUIRED", []string{"a"}, "Join occurring on a restricted column."},
		{"SELECT * FROM ds.v, ds.o", "JOIN_NOT_REQUIRED", []string{"a"}, "Join occurring on a restricted column."},
		{"SELECT * FROM ds.o LEFT JOIN (SELECT WITH AGGREGATION_THRESHOLD a, COUNT(*) FROM ds.v JOIN ds.p USING (a) GROUP BY a) USING (a)",
			"JOIN_ANY", []string{"a"}, ""},
		{"SELECT * FROM (SELECT WITH AGGREGATION_THRESHOLD a, COUNT(*) FROM ds.v GROUP BY a) JOIN ds.o USING (a)",
			"JOIN_ANY", []string{"a"}, "Joining must occur on at least one"},
		// #1517: JOIN_ANY's "must be joined upon" names no kind of join;
		// JOIN_ALL met allows other joins; NATURAL is a syntax error.
		{"SELECT * FROM ds.v LEFT JOIN ds.o USING (a)", "JOIN_ANY", []string{"a"}, ""},
		{"SELECT * FROM ds.v x FULL JOIN ds.o ON x.a = o.a CROSS JOIN ds.q", "JOIN_ANY", []string{"a"}, ""},
		{"SELECT * FROM ds.v JOIN ds.o USING (a) LEFT JOIN ds.q USING (a)", "JOIN_ALL", []string{"a"}, ""},
		{"SELECT * FROM ds.v JOIN ds.o USING (a) LEFT JOIN ds.q USING (c)", "JOIN_ALL", []string{"a"}, "Join occurring on a restricted column."},
		{"SELECT * FROM ds.v NATURAL JOIN ds.o", "JOIN_ANY", []string{"a"}, "Syntax error: Unexpected keyword NATURAL"},
		{"SELECT * FROM ds.v NATURAL JOIN ds.o", "JOIN_BLOCKED", nil, "Syntax error: Unexpected keyword NATURAL"},
	} {
		msg, uns := check(c.sql, c.cond, c.allowed...)
		if uns || c.want == "" && msg != "" || c.want != "" && !strings.HasPrefix(msg, c.want) {
			t.Errorf("%s %s %v: %q (unsupported %v), want %q", c.sql, c.cond, c.allowed, msg, uns, c.want)
		}
	}
}
