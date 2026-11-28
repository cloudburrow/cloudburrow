//go:build compat

package compat

import (
	"sort"
	"strings"
	"testing"

	"cloud.google.com/go/spanner"
	database "cloud.google.com/go/spanner/admin/database/apiv1"
	"cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
)

// TestSpannerGraphQueryPlansOfEveryPattern (#2029) runs, through the
// official Go client in NORMAL, PLAN and PROFILE modes, the Spanner Graph
// shapes the planner refused before: quantified path patterns of several
// edges, edges in either direction, a path mode inside them, and their node
// variables read as group variables (a Recursive Union of the subpath's
// rows); a path mode on a part of a path; SIMPLE; IS_ACYCLIC, IS_TRAIL,
// IS_SIMPLE, NODES, EDGES, PATH_FIRST and PATH_LAST of a path; ELEMENT_ID,
// SOURCE_NODE_ID, DESTINATION_NODE_ID and PROPERTY_NAMES of an element
// (TO_JSON of one is TestSpannerGraphOnThePatchedEmulator's); IS LABELED; juxtaposed node patterns; a MATCH no element table
// matches (an Empty Relation); a search prefix with several path patterns;
// ALL PATHS; GROUP BY a RETURN's alias; properties a graph defines by an
// expression; and SQL's lambdas, array subscripts, namespaced functions,
// CAST, a WINDOW clause, TABLESAMPLE of a subquery and COLLATE. Each
// returns the engine's rows in every mode, and PROFILE's root counts them.
//
// covers: google.spanner.v1.Spanner/ExecuteSql, google.spanner.v1.Spanner/ExecuteStreamingSql
func TestSpannerGraphQueryPlansOfEveryPattern(t *testing.T) {
	h := New(t)
	ctx, client := finGraphClient(t, h, "cb-graph-every")
	// a graph whose properties are expressions (Google's CREATE PROPERTY
	// GRAPH: PROPERTIES (expression AS name))
	da, err := database.NewDatabaseAdminClient(ctx, spannerOpts(h.Endpoint(EnvSpanner))...)
	if err != nil {
		t.Fatal(err)
	}
	defer da.Close()
	op, err := da.UpdateDatabaseDdl(ctx, &databasepb.UpdateDatabaseDdlRequest{
		Database: "projects/" + h.Project() + "/instances/cb-graph-every/databases/fingraph",
		Statements: []string{`CREATE PROPERTY GRAPH G2 NODE TABLES (Account LABEL Acct PROPERTIES (id, nick_name, UPPER(nick_name) AS up, id * 2 AS dbl))
			EDGE TABLES (AccountTransferAccount SOURCE KEY (id) REFERENCES Account (id) DESTINATION KEY (to_id) REFERENCES Account (id)
			LABEL Xfer PROPERTIES (id, to_id, amount, amount > 250 AS big))`}})
	if err == nil {
		err = op.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("CREATE PROPERTY GRAPH G2: %v", err)
	}
	// the rows of FinGraph's transfers 7->16 (twice), 16->20, 20->7, 20->16
	for _, c := range []struct{ sql, want string }{
		{`GRAPH G2 MATCH (a:Acct) RETURN a.id AS i, a.up AS u, a.dbl AS d`, "16 VACATION FUND 32; 20 RAINY DAY FUND 40; 7 VACATION FUND 14"},
		{`GRAPH G2 MATCH (a:Acct) WHERE a.dbl > 20 RETURN a.id AS i`, "16; 20"},
		{`GRAPH FinGraph MATCH ((a:Account)-[t:Transfers]->(b:Account)-[u:Transfers]->(c:Account)){1,2} RETURN COUNT(*) AS n`, "24"},
		{`GRAPH FinGraph MATCH (a:Account)-[o:Owns]->(b:Account) RETURN COUNT(*) AS n`, "0"},
		{`GRAPH FinGraph MATCH p=(a:Account {id:7})-[t:Transfers]->{2}(b) RETURN PATH_FIRST(p).id AS f, PATH_LAST(p).id AS l, ARRAY_LENGTH(NODES(p)) AS n`, "7 20 3; 7 20 3"},
		{`GRAPH FinGraph MATCH p=(a:Account)-[t:Transfers]->(b)-[u:Transfers]->(c) RETURN IS_ACYCLIC(p) AS ac, COUNT(*) AS n GROUP BY ac`, "false 2; true 5"},
	} {
		for _, mode := range []string{"NORMAL", "PROFILE"} {
			var it *spanner.RowIterator
			if mode == "NORMAL" {
				it = client.Single().Query(ctx, spanner.Statement{SQL: c.sql})
			} else {
				it = client.Single().QueryWithStats(ctx, spanner.Statement{SQL: c.sql})
			}
			got, err := rowValues(it)
			sort.Strings(got)
			if err != nil || strings.Join(got, "; ") != c.want {
				t.Errorf("%s in %s mode: %v (%v); want %s", c.sql, mode, got, err, c.want)
			}
		}
	}
	has := func(names ...string) func(t *testing.T, p planView) {
		return func(t *testing.T, p planView) {
			t.Helper()
			for _, n := range names {
				if len(p.named(n)) == 0 {
					t.Errorf("want a %s:\n%s", n, p)
				}
			}
		}
	}
	// a Recursive Union whose recursive input reads the spool of the last
	// repetition's rows
	rec := func(t *testing.T, p planView) {
		t.Helper()
		has("Recursive Union")(t, p)
		for _, n := range p.named("Scan") {
			if n.GetMetadata().GetFields()["scan_type"].GetStringValue() == "RecursiveSpoolScan" {
				return
			}
		}
		t.Errorf("want a Scan of scan_type RecursiveSpoolScan:\n%s", p)
	}
	for _, s := range []planShape{
		// quantified path patterns read as rows of their subpath
		{name: "QPP of two edges", sql: `GRAPH FinGraph MATCH ((a:Account)-[t:Transfers]->(b:Account)-[u:Transfers]->(c:Account)){1,2} RETURN COUNT(*) AS n`, rows: 1, check: rec},
		{name: "QPP of two edges with a WHERE", sql: `GRAPH FinGraph MATCH ((a:Account)-[t:Transfers]->(b:Account)-[u:Transfers]->(c:Account) WHERE t.amount < u.amount){1,2} RETURN COUNT(*) AS n`, rows: 1, check: rec},
		{name: "QPP of an edge in either direction", sql: `GRAPH FinGraph MATCH ((a:Account)-[t:Transfers]-(b:Account)){1,2} RETURN COUNT(*) AS n`, rows: 1, check: rec},
		{name: "QPP of several element tables", sql: `GRAPH FinGraph MATCH (p:Person) ((x:Person)-[o:Owns]->(a:Account)-[t:Transfers]->(b:Account)<-[o2:Owns]-(y:Person)){1,2} RETURN p.name AS pn, COUNT(*) AS n GROUP BY pn`, rows: 3, check: rec},
		{name: "QPP's node as a group variable", sql: `GRAPH FinGraph MATCH (a:Account) ((x)-[t:Transfers]->(y) WHERE x.id <> y.id){1,2} (b:Account) RETURN a.id AS ai, b.id AS bi, ARRAY_LENGTH(x) AS l`, rows: 12, check: rec},
		{name: "TRAIL in a QPP", sql: `GRAPH FinGraph MATCH (x:Account) (TRAIL (a:Account)-[t:Transfers]->(b:Account)-[u:Transfers]->(c:Account)){1,2} RETURN COUNT(*) AS n`, rows: 1, check: rec},
		{name: "ACYCLIC QPP", sql: `GRAPH FinGraph MATCH ACYCLIC ((x:Account)-[t:Transfers]->(y:Account)-[u:Transfers]->(z:Account)){1,2} RETURN COUNT(*) AS n`, rows: 1, check: rec},
		{name: "SIMPLE QPP", sql: `GRAPH FinGraph MATCH SIMPLE ((x:Account)-[t:Transfers]->(y:Account)-[u:Transfers]->(z:Account)){1,2} RETURN COUNT(*) AS n`, rows: 1, check: rec},
		{name: "ANY SHORTEST over a QPP", sql: `GRAPH FinGraph MATCH ANY SHORTEST (a:Account {id:7}) ((x:Account)-[t:Transfers]->(y:Account)-[u:Transfers]->(z:Account)){1,2} (b:Account) RETURN b.id AS bi`, rows: 2, check: has("Anti Semi Apply", "Recursive Union")},
		{name: "QPP's path length", sql: `GRAPH FinGraph MATCH p = ((x:Account)-[t:Transfers]->(y:Account)-[u:Transfers]->(z:Account)){1,2} RETURN PATH_LENGTH(p) AS l, COUNT(*) AS n GROUP BY l`, rows: 2, check: rec},
		{name: "QPP in its own parentheses", sql: `GRAPH FinGraph MATCH (((a:Account)-[t:Transfers]->(b:Account))){1,2} RETURN COUNT(*) AS n`, rows: 1, check: has("Recursive Union")},
		{name: "ALL PATHS", sql: `GRAPH FinGraph MATCH ALL PATHS (a:Account)-[t:Transfers]->{1,2}(b:Account) RETURN COUNT(*) AS n`, rows: 1, check: has("Recursive Union")},
		// path modes and path functions
		{name: "a path mode on a part of a path", sql: `GRAPH FinGraph MATCH (a:Account)-[t:Transfers]->(b:Account) (ACYCLIC (b)-[u:Transfers]->(c:Account)-[w:Transfers]->(d:Account)) RETURN a.id AS ai, d.id AS di`, rows: 7, check: has("Cross Apply")},
		{name: "TRAIL on the start of a path", sql: `GRAPH FinGraph MATCH (TRAIL (a:Account)-[t:Transfers]->(b:Account)-[u:Transfers]->(c:Account)) -[w:Transfers]->(d:Account) RETURN COUNT(*) AS n`, rows: 1, check: has("Aggregate")},
		{name: "SIMPLE quantified edge", sql: `GRAPH FinGraph MATCH SIMPLE (a:Account {id: 7})-[t:Transfers]->{1,3}(b:Account) RETURN b.id AS bi, ARRAY_LENGTH(t) AS l`, rows: 6, check: has("Recursive Union")},
		{name: "IS_ACYCLIC, IS_TRAIL, IS_SIMPLE grouped", sql: `GRAPH FinGraph MATCH p=(a:Account)-[t:Transfers]->{1,3}(b) RETURN IS_ACYCLIC(p) AS ac, IS_TRAIL(p) AS tr, IS_SIMPLE(p) AS si, COUNT(*) AS n GROUP BY ac, tr, si`, rows: 4, check: has("Aggregate", "Recursive Union")},
		{name: "NODES, EDGES, PATH_FIRST, PATH_LAST", sql: `GRAPH FinGraph MATCH p=(a:Account {id:7})-[t:Transfers]->{2}(b) RETURN ARRAY_LENGTH(NODES(p)) AS n, ARRAY_LENGTH(EDGES(p)) AS e, PATH_FIRST(p).id AS f, PATH_LAST(p).id AS l`, rows: 2, check: has("Recursive Union")},
		// element functions and predicates
		// (TO_JSON of an element: TestSpannerGraphOnThePatchedEmulator, as
		// Google's emulator refuses a graph element for TO_JSON's argument)
		{name: "ELEMENT_ID", sql: `GRAPH FinGraph MATCH (a:Account)-[t:Transfers]->(b) RETURN SOURCE_NODE_ID(t) AS s, DESTINATION_NODE_ID(t) AS d, ELEMENT_ID(a) AS e`, rows: 5, check: has("Cross Apply")},
		{name: "PROPERTY_NAMES", sql: `GRAPH FinGraph MATCH (a:Account) RETURN PROPERTY_NAMES(a) AS n`, rows: 3, check: has("Scan")},
		{name: "IS NOT LABELED", sql: `GRAPH FinGraph MATCH (a) WHERE a IS NOT LABELED Account RETURN a.id AS i`, rows: 3, check: has("Union All", "Filter")},
		{name: "juxtaposed node patterns", sql: `GRAPH FinGraph MATCH (a:Account)(b:Account {id: 7}) RETURN a.id AS ai, b.id AS bi`, rows: 1, check: has("Scan")},
		{name: "no element table matches", sql: `GRAPH FinGraph MATCH (a:Account)-[o:Owns]->(b:Account) RETURN COUNT(*) AS n`, rows: 1, check: has("Empty Relation", "Aggregate")},
		{name: "OPTIONAL MATCH no element table matches", sql: `GRAPH FinGraph MATCH (a:Account) OPTIONAL MATCH (a)<-[o:Owns]-(p:Account) RETURN a.id AS ai, p.id AS pi`, rows: 3, check: has("Scan")},
		{name: "a search prefix with two path patterns", sql: `GRAPH FinGraph MATCH ANY (a:Account {id:7})-[t:Transfers]->{1,2}(b:Account), (b)-[u:Transfers]->(c) RETURN c.id AS ci`, rows: 3, check: has("Anti Semi Apply")},
		{name: "a statement with no MATCH", sql: `GRAPH FinGraph LET x = 1, y = 2 FILTER x < y RETURN x + y AS z`, rows: 1, check: has("Filter")},
		{name: "ORDER BY COLLATE", sql: `GRAPH FinGraph MATCH (a:Account) RETURN a.nick_name AS n ORDER BY a.nick_name COLLATE "und:ci"`, rows: 3, check: has("Sort")},
		// properties defined by expressions
		{name: "expression properties", sql: `GRAPH G2 MATCH (a:Acct)-[t:Xfer]->(b) RETURN a.up AS u, t.big AS g`, rows: 5, check: has("Cross Apply")},
		{name: "expression property grouped by alias", sql: `GRAPH G2 MATCH (a:Acct) RETURN a.up AS u, COUNT(*) AS n GROUP BY u`, rows: 2, check: has("Aggregate")},
		{name: "expression property in a quantified edge", sql: `GRAPH G2 MATCH (a:Acct)-[t:Xfer WHERE t.big]->{1,2}(b) RETURN COUNT(*) AS n`, rows: 1, check: has("Recursive Union")},
		// SQL
		{name: "lambdas", sql: `SELECT ARRAY_FILTER([1,2,3], e -> e > 1) AS f, ARRAY_TRANSFORM([1,2], e -> e * 2) AS g`, rows: 1, check: has("Unit Relation")},
		{name: "array subscripts", sql: `SELECT [1,2][OFFSET(0)] AS a, [1,2][SAFE_ORDINAL(3)] AS b`, rows: 1, check: has("Unit Relation")},
		{name: "a namespaced function", sql: `SELECT NET.IPV4_TO_INT64(b'\x01\x02\x03\x04') AS n`, rows: 1, check: has("Unit Relation")},
		{name: "CAST", sql: `SELECT CAST(id AS STRING) AS s, SAFE_CAST('x' AS INT64) AS x FROM Account`, rows: 3, check: has("Scan")},
		{name: "a WINDOW clause", sql: `SELECT id FROM Account WINDOW w AS (ORDER BY id), v AS w`, rows: 3, check: has("Scan")},
		{name: "TABLESAMPLE of a subquery", sql: `SELECT * FROM (SELECT id FROM Account WHERE id > 7) TABLESAMPLE RESERVOIR (1 ROWS)`, rows: 1, check: has("Random Id Assign")},
		{name: "the largest LIMIT with an OFFSET", sql: `SELECT id FROM Account ORDER BY id LIMIT 9223372036854775807 OFFSET 1`, rows: 2, check: has("Sort Limit")},
		{name: "GROUP BY an alias of a subquery", sql: `SELECT (SELECT 1) AS x, COUNT(*) AS n FROM Account GROUP BY x`, rows: 1, check: has("Aggregate")},
	} {
		runPlanShape(t, ctx, client, s)
	}
}
