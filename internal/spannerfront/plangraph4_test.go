package spannerfront

import (
	"strings"
	"testing"
)

// TestGraphPlansOfQuantifiedPathsCostsAndPathValues (#2029) plans Google's
// documented examples the planner refused before: quantified path
// patterns (a Recursive Union whose each edge's endpoints are held to the
// node patterns by a semi join), ANY CHEAPEST (an Anti Semi Apply of the
// paths to the cheaper ones), TO_JSON of a path, PROPERTY_EXISTS and
// COUNT of a group variable.
func TestGraphPlansOfQuantifiedPathsCostsAndPathValues(t *testing.T) {
	for _, c := range []struct {
		gql  string
		want []string
	}{
		{"GRAPH FinGraph MATCH (src:Account) ((:Account)-[:Transfers]->(mid:Account {is_blocked:true})){1,2} -[:Transfers]->(dst:Account) RETURN src.id AS src_account_id, dst.id AS dst_account_id",
			[]string{"Recursive Union", "Scan[TableScan Account"}},
		{"GRAPH FinGraph MATCH ((a:Account)-[t:Transfers]->(b:Account)){1,3} RETURN ARRAY_LENGTH(t) AS hops", []string{"Recursive Union"}},
		{"GRAPH FinGraph MATCH ((a:Account)-[t:Transfers]->(b:Account) WHERE t.amount > 100){2} RETURN COUNT(*) AS n", []string{"Recursive Union", "Aggregate"}},
		{"GRAPH FinGraph MATCH ANY CHEAPEST (a:Account)-[t:Transfers COST t.amount]->{1,3}(b:Account) LET total_cost = sum(t.amount) RETURN a.id AS a_id, b.id AS b_id, total_cost",
			[]string{"Anti Semi Apply", "Recursive Union"}},
		{"GRAPH FinGraph MATCH p=(account:Account {is_blocked: false})-[transfer:Transfers]-(dst:Account) RETURN TO_JSON(p) as results", []string{"Union All"}},
		{"GRAPH FinGraph MATCH (n:Person|Account WHERE PROPERTY_EXISTS(n, name)) RETURN n.name", []string{"Union All"}},
		{"GRAPH FinGraph MATCH ANY SHORTEST (a:Account {is_blocked:true})-[t:Transfers]->{1, 4} (b:Account) LET path_length = COUNT(t) RETURN a.id AS a_id, path_length, b.id AS b_id",
			[]string{"Anti Semi Apply", "Recursive Union"}},
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
		// each edge's destination is held to the node pattern {is_blocked: true}
		{"GRAPH FinGraph MATCH (src:Account) ((:Account)-[:Transfers]->(mid:Account {is_blocked:true})){1,2} -[:Transfers]->(dst:Account) RETURN src.id AS s, dst.id AS d",
			"EXISTS (SELECT 1 FROM Account AS cb_hsrc, Account AS cb_hdst WHERE cb_hsrc.id = cb_e1.id AND cb_hdst.id = cb_e1.to_id AND cb_hdst.is_blocked = (true))"},
		// a path's cost is the sum of its edges' COST
		{"GRAPH FinGraph MATCH ANY CHEAPEST (a:Account)-[t:Transfers COST t.amount]->{1,2}(b:Account) RETURN a.id AS a_id, b.id AS b_id",
			"((cb_e1.amount) + (cb_e2.amount)) AS cb_cost"},
		// PROPERTY_EXISTS is the element table's: Person has name, Account does not
		{"GRAPH FinGraph MATCH (n:Person) RETURN PROPERTY_EXISTS(n, name) AS x", "SELECT TRUE AS x"},
		{"GRAPH FinGraph MATCH (n:Account) RETURN PROPERTY_EXISTS(n, name) AS x", "SELECT FALSE AS x"},
		// the graph operators page's predicates
		{"GRAPH FinGraph MATCH (src:Account)<-[transfer:Transfers]-(dest:Account) WHERE NOT SAME(src, dest) RETURN src.id AS source_id, dest.id AS destination_id", "NOT (((src.id = dest.id)))"},
		{"GRAPH FinGraph MATCH (a:Account)-[transfer:Transfers]-(b:Account) WHERE a IS SOURCE of transfer RETURN a.id AS a_id, b.id AS b_id", "(transfer.id = a.id)"},
		{"GRAPH FinGraph MATCH (a1:Account)-[t1:Transfers]->(a2:Account)-[t2:Transfers]->(a3:Account)-[t3:Transfers]->(a4:Account) WHERE a1.id < a4.id RETURN ALL_DIFFERENT(t1, t2, t3) AS results", "NOT "},
		// COUNT of a group variable counts its edges
		{"GRAPH FinGraph MATCH (a:Account)-[t:Transfers]->{1,2}(b:Account) RETURN COUNT(t) AS n", "cb_len AS n"},
	} {
		sql, _, err := graphToSQLQuant(c.gql, finGraphs(t))
		if err != nil {
			t.Fatalf("%s: %v", c.gql, err)
		}
		if !strings.Contains(sql, c.want) {
			t.Errorf("%s: want %q in\n%s", c.gql, c.want, sql)
		}
	}
	// still refused by name
	for _, c := range []struct{ gql, what string }{
		{"GRAPH FinGraph MATCH ((a:Account)-[t:Transfers]->(b:Account)){1,3} RETURN b.id", "group variable b"},
		{"GRAPH FinGraph MATCH (TRAIL -[t1:Transfers]->()-[t2:Transfers]->()-[t3:Transfers]->){2} RETURN COUNT(*) AS n", "path mode in a quantified path pattern"},
		{"GRAPH FinGraph MATCH ((a:Account)-[t:Transfers]->(b:Account)-[u:Transfers]->(c:Account)){1,2} RETURN COUNT(*) AS n", "more than one edge"},
	} {
		_, _, err := graphToSQLQuant(c.gql, finGraphs(t))
		if _, ok := err.(unsupportedSQL); !ok || !strings.Contains(err.Error(), c.what) {
			t.Errorf("%s: %v; want a refusal naming %q", c.gql, err, c.what)
		}
	}
}
