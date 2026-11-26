package spannerfront

import (
	"strings"
	"testing"
)

// TestGraphPlansOfSubpathsModesAndElementFunctions (#2029) plans the
// shapes the planner refused last: quantified path patterns of several
// edges (a Recursive Union of the subpath's rows), path modes on part of a
// path, a MATCH no element table matches (an Empty Relation), a
// quantified path pattern in parentheses of its own and ALL PATHS.
func TestGraphPlansOfSubpathsModesAndElementFunctions(t *testing.T) {
	for _, c := range []struct {
		gql  string
		want []string
	}{
		{"GRAPH FinGraph MATCH ((a:Account)-[t:Transfers]->(b:Account)-[u:Transfers]->(c:Account)){1,2} RETURN COUNT(*) AS n", []string{"Recursive Union", "RecursiveSpoolScan"}},
		{"GRAPH FinGraph MATCH ((a:Account)-[t:Transfers]-(b:Account)){1,2} RETURN COUNT(*) AS n", []string{"Recursive Union", "Union All"}},
		{"GRAPH FinGraph MATCH (a:Account)-[t:Transfers]->(b:Account) (ACYCLIC (b)-[u:Transfers]->(c:Account)-[w:Transfers]->(d:Account)) RETURN a.id AS ai, d.id AS di", []string{"Cross Apply"}},
		{"GRAPH FinGraph MATCH (a:Account)-[o:Owns]->(b:Account) RETURN COUNT(*) AS n", []string{"Empty Relation", "Aggregate"}},
		{"GRAPH FinGraph MATCH (((a:Account)-[t:Transfers]->(b:Account))){1,2} RETURN COUNT(*) AS n", []string{"Recursive Union"}},
		{"GRAPH FinGraph MATCH ALL PATHS (a:Account)-[t:Transfers]->{1,2}(b:Account) RETURN COUNT(*) AS n", []string{"Recursive Union"}},
	} {
		sql, got := graphPlan(t, c.gql)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s:\nSQL %s\nplan\n%s\nwant %q", c.gql, sql, got, w)
			}
		}
	}
	// the SQL the counts read
	for _, c := range []struct{ gql, want string }{
		// a path mode on part of a path holds only that part's elements
		{"GRAPH FinGraph MATCH (a:Account)-[t:Transfers]->(b:Account) (ACYCLIC (b)-[u:Transfers]->(c:Account)) RETURN COUNT(*) AS n",
			"CONCAT('Account', '|', FORMAT('%T', b.id)) != CONCAT('Account', '|', FORMAT('%T', c.id))"},
		// the subpath's rows are one WITH clause every repetition reads
		{"GRAPH FinGraph MATCH ((a:Account)-[t:Transfers]->(b:Account)-[u:Transfers]->(c:Account)){1,2} RETURN COUNT(*) AS n", "WITH cb_sg_"},
		// GROUP BY a RETURN's alias groups by its expression
		{"GRAPH FinGraph MATCH p=(a:Account)-[t:Transfers]->(b)-[u:Transfers]->(c) RETURN IS_ACYCLIC(p) AS ac, COUNT(*) AS n GROUP BY ac", "GROUP BY ("},
	} {
		sql, _, err := graphToSQLQuant(c.gql, finGraphs(t))
		if err != nil {
			t.Fatalf("%s: %v", c.gql, err)
		}
		if !strings.Contains(sql, c.want) {
			t.Errorf("%s: want %q in\n%s", c.gql, c.want, sql)
		}
	}
}

// TestHoistWith moves the subpath rows' WITH clauses to the start of the
// statement, once each, leaving quoted parentheses alone.
func TestHoistWith(t *testing.T) {
	in := "SELECT (SELECT COUNT(*) FROM (WITH cb_sg_q AS (SELECT ')' AS x FROM (SELECT 1)) SELECT 1 FROM cb_sg_q AS a) AS r) FROM (WITH cb_sg_q AS (SELECT ')' AS x FROM (SELECT 1)) SELECT 2) AS z"
	want := "WITH cb_sg_q AS (SELECT ')' AS x FROM (SELECT 1)) SELECT (SELECT COUNT(*) FROM (SELECT 1 FROM cb_sg_q AS a) AS r) FROM (SELECT 2) AS z"
	if got := hoistWith(in); got != want {
		t.Errorf("hoistWith:\n got %s\nwant %s", got, want)
	}
	if got := hoistWith("WITH a AS (SELECT 1) SELECT (SELECT 1 FROM (WITH cb_sg_q AS (SELECT 2) SELECT 1 FROM cb_sg_q) AS y) FROM a"); got !=
		"WITH cb_sg_q AS (SELECT 2), a AS (SELECT 1) SELECT (SELECT 1 FROM (SELECT 1 FROM cb_sg_q) AS y) FROM a" {
		t.Errorf("hoistWith with a WITH of its own: %s", got)
	}
	if s := "SELECT 1"; hoistWith(s) != s {
		t.Errorf("hoistWith changed %q", s)
	}
}

// TestQualifyColumns reads an expression property's columns from the
// element table's alias, not its functions, fields, words or types.
func TestQualifyColumns(t *testing.T) {
	for in, want := range map[string]string{
		"UPPER(nick_name)":                 "(UPPER(a.nick_name))",
		"id * 2":                           "(a.id * 2)",
		"amount > 250":                     "(a.amount > 250)",
		"CAST(id AS STRING)":               "(CAST(a.id AS STRING))",
		"CASE WHEN x IS NULL THEN 0 END":   "(CASE WHEN a.x IS NULL THEN 0 END)",
		"s.f":                              "(a.s.f)",
		"`order` + 1":                      "(a.`order` + 1)",
		"EXTRACT(YEAR FROM create_time)":   "(EXTRACT(YEAR FROM a.create_time))",
		"IF(is_blocked, 'yes', nick_name)": "(IF(a.is_blocked, 'yes', a.nick_name))",
	} {
		got, err := qualifyColumns(in, "a")
		if err != nil || got != want {
			t.Errorf("qualifyColumns(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

// TestIDListConditions: a path's identities are all different with no
// subquery, and SIMPLE lets the first be the last.
func TestIDListConditions(t *testing.T) {
	l := idList{{one: "x"}, {arr: "q.cb_pn", flag: "q.cb_pnd"}, {one: "y"}}
	want := "(NOT ARRAY_INCLUDES(q.cb_pn, x) AND x != y AND q.cb_pnd AND NOT ARRAY_INCLUDES(q.cb_pn, y))"
	if got := l.distinct(); got != want {
		t.Errorf("distinct: %s\nwant %s", got, want)
	}
	if got := l.simple("x", "y"); !strings.Contains(got, " OR x = y AND ") || strings.Contains(got, "SELECT") {
		t.Errorf("simple: %s", got)
	}
	if got := (idList{{one: "x"}}).distinct(); got != "TRUE" {
		t.Errorf("one identity: %s", got)
	}
}
