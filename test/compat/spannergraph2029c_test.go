//go:build compat

package compat

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/spanner"
	database "cloud.google.com/go/spanner/admin/database/apiv1"
	databasepb "cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	instance "cloud.google.com/go/spanner/admin/instance/apiv1"
	instancepb "cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	sppb "cloud.google.com/go/spanner/apiv1/spannerpb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

// finGraphDDL is Google's FinGraph (Spanner Graph's "Set up and query"
// page): Person and Account nodes, Owns and Transfers edges.
var finGraphDDL = []string{
	`CREATE TABLE Person (id INT64 NOT NULL, name STRING(MAX), birthday TIMESTAMP, country STRING(MAX), city STRING(MAX)) PRIMARY KEY (id)`,
	`CREATE TABLE Account (id INT64 NOT NULL, create_time TIMESTAMP, is_blocked BOOL, nick_name STRING(MAX)) PRIMARY KEY (id)`,
	`CREATE TABLE PersonOwnAccount (id INT64 NOT NULL, account_id INT64 NOT NULL, create_time TIMESTAMP,
		FOREIGN KEY (account_id) REFERENCES Account (id)) PRIMARY KEY (id, account_id), INTERLEAVE IN PARENT Person ON DELETE CASCADE`,
	`CREATE TABLE AccountTransferAccount (id INT64 NOT NULL, to_id INT64 NOT NULL, amount FLOAT64, create_time TIMESTAMP NOT NULL, order_number STRING(MAX),
		FOREIGN KEY (to_id) REFERENCES Account (id)) PRIMARY KEY (id, to_id, create_time), INTERLEAVE IN PARENT Account ON DELETE CASCADE`,
	`CREATE OR REPLACE PROPERTY GRAPH FinGraph NODE TABLES (Account, Person) EDGE TABLES (
		PersonOwnAccount SOURCE KEY (id) REFERENCES Person (id) DESTINATION KEY (account_id) REFERENCES Account (id) LABEL Owns,
		AccountTransferAccount SOURCE KEY (id) REFERENCES Account (id) DESTINATION KEY (to_id) REFERENCES Account (id) LABEL Transfers)`,
}

func finTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// finGraphRows are the page's rows.
func finGraphRows() []*spanner.Mutation {
	acct := []string{"id", "create_time", "is_blocked", "nick_name"}
	pers := []string{"id", "name", "birthday", "country", "city"}
	own := []string{"id", "account_id", "create_time"}
	xfer := []string{"id", "to_id", "amount", "create_time", "order_number"}
	return []*spanner.Mutation{
		spanner.Insert("Account", acct, []any{int64(7), finTime("2020-01-10T06:22:20.222Z"), false, "Vacation Fund"}),
		spanner.Insert("Account", acct, []any{int64(16), finTime("2020-01-27T17:55:09.206Z"), true, "Vacation Fund"}),
		spanner.Insert("Account", acct, []any{int64(20), finTime("2020-02-18T05:44:20.655Z"), false, "Rainy Day Fund"}),
		spanner.Insert("Person", pers, []any{int64(1), "Alex", finTime("1991-12-21T00:00:00Z"), "Australia", "Adelaide"}),
		spanner.Insert("Person", pers, []any{int64(2), "Dana", finTime("1980-10-31T00:00:00Z"), "Czech_Republic", "Moravia"}),
		spanner.Insert("Person", pers, []any{int64(3), "Lee", finTime("1986-12-07T00:00:00Z"), "India", "Kollam"}),
		spanner.Insert("AccountTransferAccount", xfer, []any{int64(7), int64(16), 300.0, finTime("2020-08-29T15:28:58.647Z"), "304330008004315"}),
		spanner.Insert("AccountTransferAccount", xfer, []any{int64(7), int64(16), 100.0, finTime("2020-10-04T16:55:05.342Z"), "304120005529714"}),
		spanner.Insert("AccountTransferAccount", xfer, []any{int64(16), int64(20), 300.0, finTime("2020-09-25T02:36:14.926Z"), "103650009791820"}),
		spanner.Insert("AccountTransferAccount", xfer, []any{int64(20), int64(7), 500.0, finTime("2020-10-04T16:55:05.342Z"), "304120005529714"}),
		spanner.Insert("AccountTransferAccount", xfer, []any{int64(20), int64(16), 200.0, finTime("2020-10-17T03:59:40.247Z"), "302290001255747"}),
		spanner.Insert("PersonOwnAccount", own, []any{int64(1), int64(7), finTime("2020-01-10T06:22:20.222Z")}),
		spanner.Insert("PersonOwnAccount", own, []any{int64(2), int64(20), finTime("2020-01-27T17:55:09.206Z")}),
		spanner.Insert("PersonOwnAccount", own, []any{int64(3), int64(16), finTime("2020-02-18T05:44:20.655Z")}),
	}
}

// finGraphClient makes an Enterprise instance (Spanner Graph's edition)
// with FinGraph and its rows.
func finGraphClient(t *testing.T, h *Harness, instanceID string) (context.Context, *spanner.Client) {
	t.Helper()
	addr := h.Endpoint(EnvSpanner)
	t.Setenv("SPANNER_EMULATOR_HOST", addr)
	ctx := h.Context()
	ia, err := instance.NewInstanceAdminClient(ctx, spannerOpts(addr)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ia.Close() })
	instName := "projects/" + h.Project() + "/instances/" + instanceID
	op, err := ia.CreateInstance(ctx, &instancepb.CreateInstanceRequest{Parent: "projects/" + h.Project(), InstanceId: instanceID,
		Instance: &instancepb.Instance{Config: fmt.Sprintf("projects/%s/instanceConfigs/emulator-config", h.Project()), DisplayName: "FinGraph",
			NodeCount: 1, Edition: instancepb.Instance_ENTERPRISE}})
	if err == nil {
		_, err = op.Wait(ctx)
	}
	if err != nil && status.Code(err) != codes.AlreadyExists {
		t.Fatalf("CreateInstance (ENTERPRISE): %v", err)
	}
	t.Cleanup(func() { _ = ia.DeleteInstance(context.Background(), &instancepb.DeleteInstanceRequest{Name: instName}) })
	da, err := database.NewDatabaseAdminClient(ctx, spannerOpts(addr)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { da.Close() })
	dop, err := da.CreateDatabase(ctx, &databasepb.CreateDatabaseRequest{Parent: instName, CreateStatement: "CREATE DATABASE fingraph", ExtraStatements: finGraphDDL})
	if err == nil {
		_, err = dop.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("CreateDatabase with FinGraph: %v", err)
	}
	db := instName + "/databases/fingraph"
	t.Cleanup(func() { _ = da.DropDatabase(context.Background(), &databasepb.DropDatabaseRequest{Database: db}) })
	client, err := spanner.NewClient(ctx, db, spannerOpts(addr)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	if _, err := client.Apply(ctx, finGraphRows()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return ctx, client
}

// TestSpannerGraphQueryPlansOfEveryStatement (#2029) runs Google's Spanner
// Graph examples (the graph patterns and GQL linear query statements
// reference pages, on FinGraph) in NORMAL, PLAN and PROFILE modes through
// the official Go client: label expressions (|, !, %) and unlabeled
// patterns over several element tables plan as a Union All of each
// table's reads; edges in either direction; OPTIONAL MATCH as an Outer
// Apply; NEXT, WITH, LET, FOR, FILTER and ORDER BY / LIMIT statements; a
// {0,n} quantified edge with its own WHERE, its group variable in
// ARRAY_LENGTH and SUM (a Recursive Union whose base is the start nodes);
// TRAIL and ACYCLIC; ANY and ANY SHORTEST (one path per pair of
// endpoints: an Anti Semi Apply of the paths to the better ones); GQL's
// UNION ALL; and a subquery in the select list
// of a SELECT that aggregates. Each returns Google's documented rows; the
// engine's errors (unbounded quantifiers, a {0} path, a FROM-less SELECT
// with WHERE, a graph element returned) are the same in every mode; what
// the planner does not plan yet (a quantified path of more than one edge,
// a path variable as a value) runs in NORMAL mode
// and is refused by name in PLAN mode.
//
// covers: google.spanner.v1.Spanner/ExecuteSql, google.spanner.v1.Spanner/ExecuteStreamingSql
func TestSpannerGraphQueryPlansOfEveryStatement(t *testing.T) {
	h := New(t)
	ctx, client := finGraphClient(t, h, "cb-plans-fingraph")
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
		{name: "unlabeled node", sql: `GRAPH FinGraph MATCH (n) RETURN n.name, n.id`, rows: 6, check: has("Union All", "Union Input")},
		{name: "unlabeled edge", sql: `GRAPH FinGraph MATCH ()-[e]->() RETURN COUNT(e.id) AS results`, rows: 1, check: has("Union All", "Aggregate")},
		{name: "either direction, any label", sql: `GRAPH FinGraph MATCH ()-[e]-() RETURN COUNT(e.id) AS results`, rows: 1, check: has("Union All", "Aggregate")},
		{name: "edge alone", sql: `GRAPH FinGraph MATCH -[e]-> RETURN e.id`, rows: 8, check: has("Union All")},
		{name: "label or", sql: `GRAPH FinGraph MATCH (n:Person|Account) RETURN n.id, n.name, n.nick_name`, rows: 6, check: has("Union All")},
		{name: "label not", sql: `GRAPH FinGraph MATCH (n:!Person) RETURN n.id`, rows: 3, check: has("Scan")},
		{name: "label wildcard", sql: `GRAPH FinGraph MATCH (n:%) RETURN n.id`, rows: 6, check: has("Union All")},
		{name: "labels", sql: `GRAPH FinGraph MATCH (n) RETURN LABELS(n) AS label`, rows: 6, check: has("Union All")},
		{name: "edge WHERE", sql: `GRAPH FinGraph MATCH -[e:Owns WHERE e.create_time > '2020-01-14' AND e.create_time < '2020-05-14']-> RETURN e.id`, rows: 2, check: has("Scan")},
		{name: "either direction", sql: `GRAPH FinGraph MATCH (src:Account)-[transfer:Transfers]-(dst:Account) RETURN src.id AS src_id, transfer.amount, dst.id AS dst_id`, rows: 10, check: has("Union All", "Cross Apply")},
		{name: "abbreviated edge", sql: `GRAPH FinGraph MATCH (account:Account)<-(person:Person) RETURN account.id, person.name`, rows: 3, check: has("Cross Apply")},
		{name: "parenthesized path", sql: `GRAPH FinGraph MATCH ((src:Account)-[t1:Transfers]->(mid:Account))-[t2:Transfers]->(dst:Account) RETURN src.id AS src_account_id, mid.id AS mid_account_id, dst.id AS dst_account_id`, rows: 7, check: has("Cross Apply")},
		{name: "quantified, element comparison", sql: `GRAPH FinGraph MATCH (src:Account {id: 7})-[e:Transfers]->{1, 3}(dst:Account) WHERE src != dst RETURN ARRAY_LENGTH(e) AS hops, dst.id AS dst_account_id`, rows: 6,
			check: func(t *testing.T, p planView) {
				ru := p.named("Recursive Union")
				if len(ru) != 1 || !p.linkTypes(ru[0])["Base"] || len(p.scans("RecursiveSpoolScan")) != 1 {
					t.Errorf("want a Recursive Union over a Recursive Spool Scan:\n%s", p)
				}
			}},
		{name: "{0,2}, edge WHERE, LET over a group variable", sql: `GRAPH FinGraph MATCH (src:Account {id: 7})-[e:Transfers WHERE e.amount > 100]->{0,2} (dst:Account) WHERE src.id != dst.id LET total_amount = SUM(e.amount) RETURN src.id AS src_account_id, dst.id AS dst_account_id, ARRAY_LENGTH(e) AS number_of_hops, total_amount`, rows: 2,
			check: func(t *testing.T, p planView) {
				ru := p.named("Recursive Union")
				if len(ru) != 1 || len(p.find(func(n *sppb.PlanNode) bool { return md(n, "scan_target") == "Account" && md(n, "Full scan") == "true" })) == 0 {
					t.Errorf("want a Recursive Union whose base scans the start nodes:\n%s", p)
				}
				if p.nodes[0].GetExecutionStats() != nil && statInt(ru[0], "rows") != 12 {
					// 3 paths of no edge, 4 of one edge over 100, 5 of two
					t.Errorf("the Recursive Union returns %d paths, want 12", statInt(ru[0], "rows"))
				}
			}},
		{name: "WALK", sql: `GRAPH FinGraph MATCH WALK (a1:Account)-[t1:Transfers]->(a2:Account)-[t2:Transfers]->(a3:Account)-[t3:Transfers]->(a4:Account) WHERE a1.id < a4.id RETURN t1.id as transfer1_id, t2.id as transfer2_id, t3.id as transfer3_id`, rows: 3, check: has("Cross Apply")},
		{name: "ACYCLIC", sql: `GRAPH FinGraph MATCH ACYCLIC (a1:Account)-[t1:Transfers]->(a2:Account)-[t2:Transfers]->(a3:Account) RETURN a1.id as account1_id, a2.id as account2_id, a3.id as account3_id`, rows: 5, check: has("Cross Apply")},
		{name: "TRAIL", sql: `GRAPH FinGraph MATCH TRAIL (a1:Account)-[t1:Transfers]->(a2:Account)-[t2:Transfers]->(a3:Account)-[t3:Transfers]->(a4:Account) WHERE a1.id < a4.id RETURN t1.id as transfer1_id, t2.id as transfer2_id, t3.id as transfer3_id`, rows: 2, check: has("Cross Apply")},
		{name: "TRAIL, an edge twice", sql: `GRAPH FinGraph MATCH TRAIL (a1)-[t1]-(a2), (a2)-[t1]-(a3) RETURN COUNT(1) as num_paths`, rows: 1, check: has("Aggregate")},
		{name: "TRAIL quantified", sql: `GRAPH FinGraph MATCH TRAIL -[:Transfers]->{6} RETURN COUNT(1) as num_paths`, rows: 1, check: has("Recursive Union")},
		{name: "OPTIONAL MATCH", sql: `GRAPH FinGraph MATCH (n:Person) OPTIONAL MATCH (n:Person)-[:Owns]->(a:Account {is_blocked: TRUE}) RETURN n.name, a.id AS blocked_account_id`, rows: 3, check: has("Outer Apply")},
		{name: "NEXT", sql: `GRAPH FinGraph MATCH (src:Account)-[t:Transfers]->(dst:Account) RETURN dst, COUNT(*) AS n GROUP BY dst NEXT MATCH (dst)<-[:Owns]-(p:Person) RETURN p.name, dst.id, n`, rows: 3, check: has("Aggregate")},
		{name: "NEXT RETURN *", sql: `GRAPH FinGraph MATCH (n:Person) RETURN n.name, n.id ORDER BY n.id LIMIT 1 NEXT RETURN *`, rows: 1, check: has("Sort Limit")},
		{name: "LET", sql: `GRAPH FinGraph MATCH (p:Person) LET n = p.name RETURN n`, rows: 3, check: has("Scan")},
		{name: "FILTER", sql: `GRAPH FinGraph MATCH (p:Person) FILTER WHERE p.name <> 'Alex' RETURN p.name`, rows: 2, check: has("Filter Scan")},
		{name: "FOR", sql: `GRAPH FinGraph MATCH (a:Account) FOR x IN [1, 2] WITH OFFSET AS o RETURN a.id AS id, x, o`, rows: 6, check: has("Array Unnest")},
		{name: "WITH", sql: `GRAPH FinGraph MATCH (a:Account) WITH a.id AS id, a.is_blocked AS b RETURN id, b`, rows: 3, check: has("Scan")},
		{name: "ORDER BY LIMIT statements", sql: `GRAPH FinGraph MATCH (a:Account)-[t:Transfers]->(b:Account) ORDER BY t.amount DESC LIMIT 3 RETURN a.id AS a_id, t.amount`, rows: 3, check: has("Sort Limit")},
		{name: "ANY", sql: `GRAPH FinGraph MATCH ANY (a:Account {is_blocked: true})->(mid:Account)->(b:Account) RETURN a.id AS a_id, mid.id AS mid_id, b.id AS b_id`, rows: 2, check: has("Anti Semi Apply")},
		{name: "ANY SHORTEST", sql: `GRAPH FinGraph MATCH ANY SHORTEST (a:Account {is_blocked:true})-[t:Transfers]->{1, 4} (b:Account) RETURN a.id AS a_id, ARRAY_LENGTH(t) AS path_length, b.id AS b_id`, rows: 3, check: has("Anti Semi Apply", "Recursive Union")},
		{name: "ANY SHORTEST TRAIL subpath", sql: `GRAPH FinGraph MATCH ANY SHORTEST (TRAIL ->{1,4}) RETURN COUNT(1) as num_paths`, rows: 1, check: has("Anti Semi Apply")},
		{name: "ANY then ALL", sql: `GRAPH FinGraph MATCH ANY (a:Account {id: 20})->(mid:Account)->(a:Account) MATCH ALL (p:Person)->(mid) RETURN p.name, mid.id`, rows: 1, check: has("Anti Semi Apply")},
		{name: "GRAPH_TABLE RETURN", sql: `SELECT name FROM GRAPH_TABLE(FinGraph MATCH (n:Person) RETURN n.name) ORDER BY name`, rows: 3, check: has("Sort")},
		{name: "UNION ALL", sql: `GRAPH FinGraph MATCH (n:Person) RETURN n.id AS id UNION ALL MATCH (n:Account) RETURN n.id AS id`, rows: 6, check: has("Union All")},
		{name: "subquery in an aggregating select list", sql: `SELECT (SELECT COUNT(*) FROM Person) AS x, COUNT(*) AS c FROM Account`, rows: 1, check: has("Aggregate")},
		{name: "correlated subquery per group", sql: `SELECT a.is_blocked, (SELECT COUNT(*) FROM Person p WHERE p.id > a.n) AS x FROM (SELECT is_blocked, IF(is_blocked, 1, 0) AS n FROM Account) a GROUP BY a.is_blocked, a.n`, rows: 2, check: has("Aggregate")},
	} {
		runPlanShape(t, ctx, client, s)
	}
	// Google's documented results
	for _, c := range []struct {
		sql  string
		want []string
	}{
		{`GRAPH FinGraph MATCH ()-[e]->() RETURN COUNT(e.id) AS results`, []string{"8"}},
		{`GRAPH FinGraph MATCH ()-[e]-() RETURN COUNT(e.id) AS results`, []string{"16"}},
		{`GRAPH FinGraph MATCH (src:Account {id: 7})-[e:Transfers WHERE e.amount > 100]->{0,2} (dst:Account) WHERE src.id != dst.id LET total_amount = SUM(e.amount) RETURN src.id AS src_account_id, dst.id AS dst_account_id, ARRAY_LENGTH(e) AS number_of_hops, total_amount`,
			[]string{"7 16 1 300", "7 20 2 600"}},
		{`GRAPH FinGraph MATCH (n:Person) OPTIONAL MATCH (n:Person)-[:Owns]->(a:Account {is_blocked: TRUE}) RETURN n.name, a.id AS blocked_account_id`, []string{"Alex NULL", "Dana NULL", "Lee 16"}},
	} {
		for _, mode := range []string{"NORMAL", "PROFILE"} {
			var it *spanner.RowIterator
			if mode == "NORMAL" {
				it = client.Single().Query(ctx, spanner.Statement{SQL: c.sql})
			} else {
				it = client.Single().QueryWithStats(ctx, spanner.Statement{SQL: c.sql})
			}
			got, err := rowValues(it)
			// in any order: the emulator does not keep a GQL RETURN's
			// ORDER BY (#2029), so the comparison sorts
			sort.Strings(got)
			if err != nil || strings.Join(got, "; ") != strings.Join(c.want, "; ") {
				t.Errorf("%s in %s mode: %v (%v); want Google's %v", c.sql, mode, got, err, c.want)
			}
		}
	}
	// the engine's errors, the same in every mode
	for _, c := range []struct{ sql, msg string }{
		{`GRAPH FinGraph MATCH (s:Account)-[t:Transfers]->{1,}(d:Account) RETURN s.id AS s, d.id AS d`, "Unbounded quantifier is not supported"},
		{`GRAPH FinGraph MATCH (s:Account)-[t:Transfers]->+(d:Account) RETURN s.id AS s, d.id AS d`, "Unbounded quantifier is not supported"},
		{`GRAPH FinGraph MATCH ((a:Account)-[:Transfers]->(b:Account)){0} RETURN COUNT(*) AS n`, "Value of upper bound must be greater than zero"},
		{`GRAPH FinGraph MATCH (a:Account){1, 3} RETURN a.id`, "Quantifier cannot be used on a node pattern"},
		{`GRAPH FinGraph MATCH (n:Person) RETURN n`, "Returning graph-typed column is not supported"},
		{`GRAPH FinGraph MATCH (p:Person) LET a = p.id, b = a + 1 RETURN b`, "Unrecognized name: a"},
		{`SELECT 1 AS x WHERE TRUE`, "Query without FROM clause cannot have a WHERE clause"},
	} {
		for _, mode := range []string{"NORMAL", "PLAN", "PROFILE"} {
			var err error
			switch mode {
			case "NORMAL":
				_, err = rowValues(client.Single().Query(ctx, spanner.Statement{SQL: c.sql}))
			case "PLAN":
				_, err = client.Single().AnalyzeQuery(ctx, spanner.Statement{SQL: c.sql})
			default:
				_, err = rowValues(client.Single().QueryWithStats(ctx, spanner.Statement{SQL: c.sql}))
			}
			if status.Code(err) != codes.InvalidArgument || !strings.Contains(fmt.Sprint(err), c.msg) {
				t.Errorf("%s in %s mode: %v; want INVALID_ARGUMENT %q", c.sql, mode, err, c.msg)
			}
		}
	}
	// not planned yet: NORMAL runs them, PLAN refuses them by name
	for _, c := range []struct {
		sql  string
		rows int
		what string
	}{
		{`GRAPH FinGraph MATCH (src:Account) ((:Account)-[:Transfers]->(mid:Account {is_blocked:true})){1,2} -[:Transfers]->(dst:Account) RETURN src.id AS src_account_id, dst.id AS dst_account_id`, 3, "side by side"},
		{`GRAPH FinGraph MATCH p=(account:Account {is_blocked: false})-[transfer:Transfers]-(dst:Account) RETURN TO_JSON(p) as results`, 6, "path variable p"},
	} {
		got, err := rowValues(client.Single().Query(ctx, spanner.Statement{SQL: c.sql}))
		if err != nil || len(got) != c.rows {
			t.Errorf("%s: NORMAL %d rows (%v); want %d", c.sql, len(got), err, c.rows)
		}
		_, err = client.Single().AnalyzeQuery(ctx, spanner.Statement{SQL: c.sql})
		if status.Code(err) != codes.Unimplemented || !strings.Contains(fmt.Sprint(err), c.what) {
			t.Errorf("%s in PLAN mode: %v; want UNIMPLEMENTED naming %q", c.sql, err, c.what)
		}
	}
}

// rowValues reads every row as its values, space-separated.
func rowValues(it *spanner.RowIterator) ([]string, error) {
	defer it.Stop()
	var out []string
	for {
		r, err := it.Next()
		if err == iterator.Done {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		var cells []string
		for i := 0; i < r.Size(); i++ {
			var v spanner.GenericColumnValue
			if err := r.Column(i, &v); err != nil {
				return out, err
			}
			switch x := v.Value.GetKind().(type) {
			case *structpb.Value_StringValue:
				cells = append(cells, x.StringValue)
			case *structpb.Value_NumberValue:
				cells = append(cells, strconv.FormatFloat(x.NumberValue, 'f', -1, 64))
			case *structpb.Value_BoolValue:
				cells = append(cells, strconv.FormatBool(x.BoolValue))
			case *structpb.Value_NullValue:
				cells = append(cells, "NULL")
			default:
				cells = append(cells, v.Value.String())
			}
		}
		out = append(out, strings.Join(cells, " "))
	}
}
