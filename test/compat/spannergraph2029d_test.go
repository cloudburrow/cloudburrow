//go:build compat

package compat

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"cloud.google.com/go/spanner"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestSpannerGraphOnThePatchedEmulator (#2029) runs, through the official Go
// client in NORMAL, PLAN and PROFILE modes, Google's Spanner Graph examples
// that need CloudBurrow's patched emulator (third_party/spanner-emulator):
// a GQL RETURN ... ORDER BY comes back in its order (the emulator had
// swapped its first two rows), as plain SQL's ORDER BY always did;
// PROPERTY_EXISTS, SAME, ALL_DIFFERENT and IS SOURCE OF; COUNT of a group
// variable; and ANY CHEAPEST with an edge COST. Each returns the rows Google's graph patterns and graph operators
// pages show. An instance that runs Google's emulator image (the patched
// CLI embeds no build: make spanner-binaries) skips, naming why.
//
// covers: google.spanner.v1.Spanner/ExecuteSql, google.spanner.v1.Spanner/ExecuteStreamingSql
func TestSpannerGraphOnThePatchedEmulator(t *testing.T) {
	h := New(t)
	ctx, client := finGraphClient(t, h, "cb-graph-patched")
	probe := `GRAPH FinGraph MATCH (n:Person WHERE PROPERTY_EXISTS(n, name)) RETURN n.id`
	if _, err := rowValues(client.Single().Query(ctx, spanner.Statement{SQL: probe})); status.Code(err) == codes.Unimplemented &&
		strings.Contains(fmt.Sprint(err), "PROPERTY_EXISTS") {
		t.Skip("the instance runs Google's Spanner emulator image, not CloudBurrow's patched build (this CLI embeds no build: make spanner-binaries, #2029)")
	}
	// rows in the order the query gives them
	for _, c := range []struct {
		sql     string
		want    []string
		ordered bool
	}{
		{`GRAPH FinGraph MATCH (n:Person) RETURN n.name ORDER BY n.name`, []string{"Alex", "Dana", "Lee"}, true},
		{`GRAPH FinGraph MATCH (n:Person) RETURN n.name ORDER BY n.name DESC`, []string{"Lee", "Dana", "Alex"}, true},
		{`GRAPH FinGraph MATCH (a:Account)-[t:Transfers]->(b:Account) RETURN a.id AS a_id, t.amount AS amount ORDER BY amount DESC, a_id`,
			[]string{"20 500", "7 300", "16 300", "20 200", "7 100"}, true},
		{`GRAPH FinGraph MATCH (n:Person) RETURN n.name ORDER BY n.name LIMIT 2`, []string{"Alex", "Dana"}, true},
		// plain SQL's ORDER BY: never affected
		{`SELECT name FROM Person ORDER BY name DESC`, []string{"Lee", "Dana", "Alex"}, true},
		{`SELECT id FROM Account ORDER BY id`, []string{"7", "16", "20"}, true},
		// the graph operators page: PROPERTY_EXISTS
		{`GRAPH FinGraph MATCH (n:Person|Account WHERE PROPERTY_EXISTS(n, name)) RETURN n.name`, []string{"Alex", "Dana", "Lee"}, false},
		// the graph patterns page: COUNT of a group variable, ANY CHEAPEST
		{`GRAPH FinGraph MATCH ANY SHORTEST (a:Account {is_blocked:true})-[t:Transfers]->{1, 4} (b:Account) LET path_length = COUNT(t) RETURN a.id AS a_id, path_length, b.id AS b_id`,
			[]string{"16 1 20", "16 2 16", "16 2 7"}, false},
		{`GRAPH FinGraph MATCH ANY CHEAPEST (a:Account)-[t:Transfers COST t.amount]->{1,3}(b:Account) LET total_cost = sum(t.amount) RETURN a.id AS a_id, b.id AS b_id, total_cost`,
			[]string{"16 16 500", "16 20 300", "16 7 800", "20 16 200", "20 20 500", "20 7 500", "7 16 100", "7 20 400", "7 7 900"}, false},
	} {
		for _, mode := range []string{"NORMAL", "PROFILE"} {
			var it *spanner.RowIterator
			if mode == "NORMAL" {
				it = client.Single().Query(ctx, spanner.Statement{SQL: c.sql})
			} else {
				it = client.Single().QueryWithStats(ctx, spanner.Statement{SQL: c.sql})
			}
			got, err := rowValues(it)
			if !c.ordered {
				sort.Strings(got)
			}
			if err != nil || strings.Join(got, "; ") != strings.Join(c.want, "; ") {
				t.Errorf("%s in %s mode: %v (%v); want Google's %v", c.sql, mode, got, err, c.want)
			}
		}
		if _, err := client.Single().AnalyzeQuery(ctx, spanner.Statement{SQL: c.sql}); err != nil {
			t.Errorf("%s in PLAN mode: %v", c.sql, err)
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
	for _, s := range []planShape{
		{name: "GQL ORDER BY", sql: `GRAPH FinGraph MATCH (n:Person) RETURN n.name ORDER BY n.name`, rows: 3, check: has("Sort")},
		{name: "PROPERTY_EXISTS", sql: `GRAPH FinGraph MATCH (n:Person|Account WHERE PROPERTY_EXISTS(n, name)) RETURN n.name`, rows: 3, check: has("Union All")},
		{name: "COUNT of a group variable", sql: `GRAPH FinGraph MATCH ANY SHORTEST (a:Account {is_blocked:true})-[t:Transfers]->{1, 4} (b:Account) LET path_length = COUNT(t) RETURN a.id AS a_id, path_length, b.id AS b_id`, rows: 3, check: has("Anti Semi Apply", "Recursive Union")},
		{name: "SAME", sql: `GRAPH FinGraph MATCH (src:Account)<-[transfer:Transfers]-(dest:Account) WHERE NOT SAME(src, dest) RETURN src.id AS source_id, dest.id AS destination_id`, rows: 5, check: has("Scan")},
		{name: "IS SOURCE OF", sql: `GRAPH FinGraph MATCH (a:Account)-[transfer:Transfers]-(b:Account) WHERE a IS SOURCE of transfer RETURN a.id AS a_id, b.id AS b_id`, rows: 5, check: has("Union All")},
		{name: "ALL_DIFFERENT", sql: `GRAPH FinGraph MATCH (a1:Account)-[t1:Transfers]->(a2:Account)-[t2:Transfers]->(a3:Account)-[t3:Transfers]->(a4:Account) WHERE a1.id < a4.id RETURN ALL_DIFFERENT(t1, t2, t3) AS results`, rows: 3, check: has("Cross Apply")},
		{name: "ANY CHEAPEST", sql: `GRAPH FinGraph MATCH ANY CHEAPEST (a:Account)-[t:Transfers COST t.amount]->{1,3}(b:Account) LET total_cost = sum(t.amount) RETURN a.id AS a_id, b.id AS b_id, total_cost`, rows: 9, check: has("Anti Semi Apply", "Recursive Union")},
	} {
		runPlanShape(t, ctx, client, s)
	}
	// COST's documented runtime error: a cost that is not positive
	bad := `GRAPH FinGraph MATCH ANY CHEAPEST (a:Account)-[t:Transfers COST t.amount - 1000]->{1,2}(b:Account) RETURN a.id AS a_id, b.id AS b_id`
	if _, err := rowValues(client.Single().Query(ctx, spanner.Statement{SQL: bad})); err == nil {
		t.Errorf("%s: want an error for a cost that is not positive", bad)
	}
}
