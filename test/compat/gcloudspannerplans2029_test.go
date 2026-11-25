//go:build compat

package compat

import (
	"encoding/json"
	"strings"
	"testing"

	"cloud.google.com/go/spanner"
)

// TestGcloudSpannerExecuteSQLQueryModes (#2029), in the emulators shard:
// `gcloud spanner databases execute-sql --query-mode=PLAN|PROFILE`,
// configured by `cloudburrow gcloud-setup` alone, over Spanner's REST API
// (sessions.executeSql). PLAN prints the query plan as gcloud draws it
// (each node's kind and display name, its metadata and short
// representation) and no rows; PROFILE prints the query statistics table,
// the plan with each operator's executions and latency, and the rows. With
// --format=json the ResultSet's stats carry the same plan, with execution
// statistics in PROFILE mode only.
//
// covers: google.spanner.v1.Spanner/ExecuteSql
func TestGcloudSpannerExecuteSQLQueryModes(t *testing.T) {
	f := newSpannerAdmin(t, "gc-plans", "gcplansdb") // skips without CLOUDBURROW_TEST_SPANNER
	New(t).Endpoint(EnvSpannerREST)
	c, err := spanner.NewClient(f.ctx, f.database, spannerOpts(f.addr)...)
	if err != nil {
		t.Fatalf("spanner.NewClient: %v", err)
	}
	t.Cleanup(c.Close)
	if _, err := c.Apply(f.ctx, []*spanner.Mutation{
		spanner.Insert("Items", []string{"Id", "Name"}, []any{int64(1), "one"}),
		spanner.Insert("Items", []string{"Id", "Name"}, []any{int64(2), "two"}),
		spanner.Insert("Items", []string{"Id", "Name"}, []any{int64(3), "three"}),
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	gc := gcloudAdminSession(t, f.project, "spanner")
	inst := strings.TrimPrefix(f.instance, "projects/"+f.project+"/instances/")
	db := f.database[strings.LastIndex(f.database, "/")+1:]
	const sql = "SELECT Name FROM Items WHERE Name = 'two'"
	exec := func(mode string, extra ...string) string {
		return gc(append([]string{"spanner", "databases", "execute-sql", db, "--instance=" + inst, "--sql=" + sql, "--query-mode=" + mode}, extra...)...)
	}

	plan := exec("PLAN")
	for _, w := range []string{"RELATIONAL Distributed Union", "RELATIONAL Serialize Result", "RELATIONAL Filter Scan", "RELATIONAL Scan",
		"scan_target: Items", "scan_type: TableScan", "Full scan: true", "call_type: Global", "SCALAR "} {
		if !strings.Contains(plan, w) {
			t.Errorf("execute-sql --query-mode=PLAN does not print %q:\n%s", w, plan)
		}
	}
	if strings.Contains(plan, "latency") || strings.Contains(plan, "rows_returned") {
		t.Errorf("execute-sql --query-mode=PLAN prints statistics:\n%s", plan)
	}

	prof := exec("PROFILE")
	for _, w := range []string{"TOTAL_ELAPSED_TIME", "CPU_TIME", "ROWS_RETURNED", "ROWS_SCANNED", "RELATIONAL Distributed Union",
		"RELATIONAL Scan", "1 execution", "total latency", "two"} {
		if !strings.Contains(prof, w) {
			t.Errorf("execute-sql --query-mode=PROFILE does not print %q:\n%s", w, prof)
		}
	}

	type node struct {
		Index          int32                        `json:"index"`
		Kind           string                       `json:"kind"`
		DisplayName    string                       `json:"displayName"`
		ChildLinks     []struct{ ChildIndex int32 } `json:"childLinks"`
		Metadata       map[string]any               `json:"metadata"`
		ExecutionStats map[string]any               `json:"executionStats"`
	}
	type result struct {
		Rows  [][]any `json:"rows"`
		Stats struct {
			QueryPlan  struct{ PlanNodes []node } `json:"queryPlan"`
			QueryStats map[string]any             `json:"queryStats"`
		} `json:"stats"`
	}
	for _, mode := range []string{"PLAN", "PROFILE"} {
		var r result
		out := exec(mode, "--format=json")
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			t.Fatalf("execute-sql --query-mode=%s --format=json: %v\n%s", mode, err, out)
		}
		nodes := r.Stats.QueryPlan.PlanNodes
		if len(nodes) < 5 || nodes[0].DisplayName != "Distributed Union" || nodes[0].Kind != "RELATIONAL" {
			t.Fatalf("%s: the plan is %+v; want it rooted at a relational Distributed Union", mode, nodes)
		}
		var scan *node
		for i := range nodes {
			n := &nodes[i]
			if n.Index != int32(i) {
				t.Errorf("%s: node %d has index %d", mode, i, n.Index)
			}
			for _, l := range n.ChildLinks {
				if l.ChildIndex < 0 || int(l.ChildIndex) >= len(nodes) {
					t.Errorf("%s: node %d links to node %d", mode, i, l.ChildIndex)
				}
			}
			if n.DisplayName == "Scan" {
				scan = n
			}
			if (mode == "PLAN") != (n.ExecutionStats == nil) && n.Kind == "RELATIONAL" {
				t.Errorf("%s: relational node %d (%s) execution statistics = %v", mode, i, n.DisplayName, n.ExecutionStats)
			}
		}
		if scan == nil || scan.Metadata["scan_target"] != "Items" {
			t.Errorf("%s: no Scan of Items in %+v", mode, nodes)
		}
		switch mode {
		case "PLAN":
			if len(r.Rows) != 0 {
				t.Errorf("PLAN returned rows %v", r.Rows)
			}
		case "PROFILE":
			if len(r.Rows) != 1 || r.Stats.QueryStats["rows_returned"] != "1" || r.Stats.QueryStats["rows_scanned"] != "3" {
				t.Errorf("PROFILE returned rows %v and query statistics %v; want one row, 1 returned and 3 scanned", r.Rows, r.Stats.QueryStats)
			}
			if rows, _ := scan.ExecutionStats["rows"].(map[string]any); rows["total"] != "3" {
				t.Errorf("PROFILE: the Scan's rows are %v; want 3", scan.ExecutionStats["rows"])
			}
		}
	}
}
