//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"cloud.google.com/go/spanner"
	database "cloud.google.com/go/spanner/admin/database/apiv1"
	databasepb "cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	instance "cloud.google.com/go/spanner/admin/instance/apiv1"
	instancepb "cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	sppb "cloud.google.com/go/spanner/apiv1/spannerpb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// planTxn is what a shape runs in: a single-use read-only transaction or
// a read-write one.
type planTxn interface {
	AnalyzeQuery(ctx context.Context, s spanner.Statement) (*sppb.QueryPlan, error)
	QueryWithStats(ctx context.Context, s spanner.Statement) *spanner.RowIterator
	Query(ctx context.Context, s spanner.Statement) *spanner.RowIterator
}

// planShape is a statement whose plan a test checks.
type planShape struct {
	name, sql string
	rows      int64 // the rows it returns; -1 when it is a sample
	dml       bool  // a DML statement: rows is its row count
	rw        bool  // it needs a read-write transaction (FOR UPDATE, DML)
	check     func(t *testing.T, p planView)
}

var errPlanRollback = errors.New("roll back")

// runPlanShape runs a shape in NORMAL, PLAN and PROFILE modes, each in its
// own transaction (rolled back for a read-write one), and checks the
// plans and statistics.
func runPlanShape(t *testing.T, ctx context.Context, client *spanner.Client, s planShape) {
	t.Helper()
	in := func(f func(tx planTxn) error) {
		t.Helper()
		var err error
		if s.rw {
			_, err = client.ReadWriteTransaction(ctx, func(ctx context.Context, tx *spanner.ReadWriteTransaction) error {
				if err := f(tx); err != nil {
					return err
				}
				return errPlanRollback
			})
			if errors.Is(err, errPlanRollback) {
				err = nil
			}
		} else {
			err = f(client.Single())
		}
		if err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
	}
	count := func(it *spanner.RowIterator) (int64, error) {
		var n int64
		for {
			_, err := it.Next()
			if err == iterator.Done {
				return n, nil
			}
			if err != nil {
				return n, err
			}
			n++
		}
	}
	stmt := spanner.Statement{SQL: s.sql}
	want := func(n, rowCount int64) bool {
		if s.dml {
			return rowCount == s.rows
		}
		return s.rows < 0 || n == s.rows
	}
	// NORMAL: the statement itself runs
	in(func(tx planTxn) error {
		it := tx.Query(ctx, stmt)
		n, err := count(it)
		if err != nil {
			return fmt.Errorf("NORMAL: %w", err)
		}
		if !want(n, it.RowCount) {
			t.Errorf("%s: NORMAL returned %d rows, row count %d; want %d", s.name, n, it.RowCount, s.rows)
		}
		return nil
	})
	var planNodes int
	in(func(tx planTxn) error {
		plan, err := tx.AnalyzeQuery(ctx, stmt)
		if err != nil {
			return fmt.Errorf("PLAN: %w", err)
		}
		pv := planView{nodes: plan.GetPlanNodes()}
		planNodes = len(pv.nodes)
		if !s.dml {
			wellFormed(t, s.name+" (PLAN)", pv)
		}
		for _, n := range pv.nodes {
			if n.GetExecutionStats() != nil {
				t.Errorf("%s: PLAN node %d has execution statistics", s.name, n.GetIndex())
			}
		}
		s.check(t, pv)
		return nil
	})
	in(func(tx planTxn) error {
		it := tx.QueryWithStats(ctx, stmt)
		n, err := count(it)
		if err != nil {
			return fmt.Errorf("PROFILE: %w", err)
		}
		if !want(n, it.RowCount) {
			t.Errorf("%s: PROFILE returned %d rows, row count %d; want %d", s.name, n, it.RowCount, s.rows)
		}
		pp := planView{nodes: it.QueryPlan.GetPlanNodes()}
		if !s.dml {
			wellFormed(t, s.name+" (PROFILE)", pp)
		}
		s.check(t, pp)
		if len(pp.nodes) != planNodes {
			t.Errorf("%s: PROFILE's plan has %d nodes, PLAN's %d", s.name, len(pp.nodes), planNodes)
		}
		st := it.QueryStats
		if st["query_text"] != s.sql || st["elapsed_time"] == nil || st["rows_scanned"] == nil {
			t.Errorf("%s: query_stats %v", s.name, st)
		}
		var scanned int64
		for _, node := range pp.nodes {
			if node.GetKind() != sppb.PlanNode_RELATIONAL {
				continue
			}
			if node.GetExecutionStats().GetFields()["rows"] == nil || node.GetExecutionStats().GetFields()["latency"] == nil {
				t.Errorf("%s: PROFILE node %d (%s) has no rows or latency", s.name, node.GetIndex(), node.GetDisplayName())
			}
			scanned += statInt(node, "scanned_rows")
		}
		if fmt.Sprint(scanned) != st["rows_scanned"] {
			t.Errorf("%s: the scans' scanned_rows add up to %d, query_stats rows_scanned is %v", s.name, scanned, st["rows_scanned"])
		}
		root := statInt(pp.nodes[0], "rows")
		switch {
		case s.dml && pp.nodes[0].GetDisplayName() == "Apply Mutations" && root != s.rows:
			t.Errorf("%s: Apply Mutations changed %d rows, want %d", s.name, root, s.rows)
		case !s.dml && root != n:
			t.Errorf("%s: the root returns %d rows, the query %d", s.name, root, n)
		}
		if !s.dml && st["rows_returned"] != strconv.FormatInt(n, 10) {
			t.Errorf("%s: rows_returned %v, want %d", s.name, st["rows_returned"], n)
		}
		return nil
	})
}

// TestSpannerQueryPlansForEveryShape (#2029) drives the official Go
// client's Query (NORMAL), AnalyzeQuery (PLAN) and QueryWithStats
// (PROFILE) over the shapes the first planner refused, in both dialects,
// on the schema and rows of Google's plan pages: INTERSECT and EXCEPT (ALL
// and DISTINCT), a view, TABLESAMPLE (BERNOULLI and RESERVOIR, which
// Google's unary operators page plans with Random Id Assign), FOR UPDATE,
// a HASH join (GoogleSQL's join_method) and INSERT ... ON CONFLICT (DO
// NOTHING and DO UPDATE). Window functions, WITH RECURSIVE, NATURAL and
// LOOKUP joins are not Spanner's (its query syntax pages have none, and
// "a CTE can't reference itself"): the engine refuses them in every mode,
// which the test pins.
//
// covers: google.spanner.v1.Spanner/ExecuteSql, google.spanner.v1.Spanner/ExecuteStreamingSql
func TestSpannerQueryPlansForEveryShape(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvSpanner)
	t.Setenv("SPANNER_EMULATOR_HOST", addr)
	ctx := h.Context()
	for _, pg := range []bool{false, true} {
		dialect, dbID := "GoogleSQL", "shapesgsql"
		if pg {
			dialect, dbID = "PostgreSQL", "shapespg"
		}
		t.Run(dialect, func(t *testing.T) {
			client := plansDB(t, ctx, addr, h.Project(), dbID, pg)
			q := func(gsql, pgsql string) string {
				if pg {
					return pgsql
				}
				return gsql
			}
			view := q(`CREATE VIEW SingerNames SQL SECURITY INVOKER AS SELECT Singers.SingerId AS SingerId, Singers.FirstName AS FirstName FROM Singers`,
				`CREATE VIEW singernames SQL SECURITY INVOKER AS SELECT singers.singerid AS singerid, singers.firstname AS firstname FROM singers`)
			a, err := database.NewDatabaseAdminClient(ctx, spannerOpts(addr)...)
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			op, err := a.UpdateDatabaseDdl(ctx, &databasepb.UpdateDatabaseDdlRequest{Database: client.DatabaseName(), Statements: []string{view}})
			if err == nil {
				err = op.Wait(ctx)
			}
			if err != nil {
				t.Fatalf("CREATE VIEW: %v", err)
			}
			setOp := func(op string, all bool) func(t *testing.T, p planView) {
				return func(t *testing.T, p planView) {
					t.Helper()
					if len(p.named("Union All")) != 1 || len(p.named("Union Input")) != 2 || len(p.named("Filter")) != 1 || len(p.scans("TableScan")) != 2 {
						t.Errorf("%s: want a Filter over a Hash Aggregate over a Union All of two Union Inputs, each a Table Scan:\n%s", op, p)
					}
					ag := p.named("Aggregate")
					if len(ag) != 1 || md(ag[0], "iterator_type") != "Hash" {
						t.Errorf("%s: want one Hash Aggregate:\n%s", op, p)
					}
					if all != (len(p.named("Array Unnest")) == 1 && len(p.named("Cross Apply")) == 1) {
						t.Errorf("%s: ALL repeats the kept rows with a Cross Apply over an Array Unnest, DISTINCT does not:\n%s", op, p)
					}
				}
			}
			sample := func(name string) func(t *testing.T, p planView) {
				return func(t *testing.T, p planView) {
					t.Helper()
					ra := p.named("Random Id Assign")
					top := p.named(name)
					if len(ra) != 1 || len(top) == 0 || len(p.scans("TableScan")) != 1 || md(p.scans("TableScan")[0], "Full scan") != "true" {
						t.Errorf("want %s over Random Id Assign over a full Table Scan:\n%s", name, p)
					}
				}
			}
			apply := func(t *testing.T, p planView) {
				t.Helper()
				if p.nodes[0].GetDisplayName() != "Apply Mutations" && !(p.nodes[0].GetDisplayName() == "Serialize Result" && len(p.named("Apply Mutations")) == 1) {
					t.Errorf("want Apply Mutations at the root, or under Serialize Result:\n%s", p)
				}
			}
			shapes := []planShape{
				{name: "INTERSECT DISTINCT", sql: q(`SELECT SingerId FROM Albums INTERSECT DISTINCT SELECT SingerId FROM Songs`,
					`SELECT singerid FROM albums INTERSECT SELECT singerid FROM songs`), rows: 2, check: setOp("INTERSECT DISTINCT", false)},
				{name: "INTERSECT ALL", sql: q(`SELECT SingerId FROM Albums INTERSECT ALL SELECT SingerId FROM Songs`,
					`SELECT singerid FROM albums INTERSECT ALL SELECT singerid FROM songs`), rows: 4, check: setOp("INTERSECT ALL", true)},
				{name: "EXCEPT DISTINCT", sql: q(`SELECT SingerId FROM Albums EXCEPT DISTINCT SELECT SingerId FROM Songs`,
					`SELECT singerid FROM albums EXCEPT SELECT singerid FROM songs`), rows: 2, check: setOp("EXCEPT DISTINCT", false)},
				{name: "EXCEPT ALL", sql: q(`SELECT SingerId FROM Albums EXCEPT ALL SELECT SingerId FROM Songs`,
					`SELECT singerid FROM albums EXCEPT ALL SELECT singerid FROM songs`), rows: 3, check: setOp("EXCEPT ALL", true)},
				{name: "view", sql: q(`SELECT n.FirstName FROM SingerNames AS n WHERE n.SingerId > 2`, `SELECT n.firstname FROM singernames AS n WHERE n.singerid > 2`), rows: 3,
					check: func(t *testing.T, p planView) {
						if ts := p.scans("TableScan"); len(ts) != 1 || !strings.EqualFold(md(ts[0], "scan_target"), "Singers") {
							t.Errorf("want the view read as its definition, a Table Scan of Singers:\n%s", p)
						}
					}},
				{name: "TABLESAMPLE RESERVOIR", sql: q(`SELECT s.SongName FROM Songs AS s TABLESAMPLE RESERVOIR (2 ROWS)`,
					`SELECT s.songname FROM songs AS s TABLESAMPLE spanner.reservoir (2)`), rows: 2, check: sample("Sort Limit")},
				{name: "TABLESAMPLE BERNOULLI", sql: q(`SELECT s.SongName FROM Songs AS s TABLESAMPLE BERNOULLI (100 PERCENT)`,
					`SELECT s.songname FROM songs AS s TABLESAMPLE BERNOULLI (100)`), rows: 9, check: sample("Filter")},
				{name: "TABLESAMPLE BERNOULLI, sampled", sql: q(`SELECT s.SongName FROM Songs AS s TABLESAMPLE BERNOULLI (50 PERCENT)`,
					`SELECT s.songname FROM songs AS s TABLESAMPLE BERNOULLI (50)`), rows: -1, check: sample("Filter")},
				{name: "FOR UPDATE", sql: q(`SELECT LastName FROM Singers WHERE SingerId = 1 FOR UPDATE`, `SELECT lastname FROM singers WHERE singerid = 1 FOR UPDATE`),
					rows: 1, rw: true, check: func(t *testing.T, p planView) {
						if fs := p.named("Filter Scan"); len(fs) != 1 || !p.linkTypes(fs[0])["Seek Condition"] {
							t.Errorf("want the query's own plan, a seek on the key:\n%s", p)
						}
					}},
				{name: "ON CONFLICT DO NOTHING", sql: q(`INSERT INTO Singers (SingerId, FirstName) VALUES (1, 'Marcus'), (9, 'Nina') ON CONFLICT (SingerId) DO NOTHING`,
					`INSERT INTO singers (singerid, firstname) VALUES (1, 'Marcus'), (9, 'Nina') ON CONFLICT (singerid) DO NOTHING`), rows: 1, dml: true, rw: true, check: apply},
				{name: "ON CONFLICT DO UPDATE", sql: q(`INSERT INTO Singers (SingerId, FirstName) VALUES (1, 'Marcus') ON CONFLICT (SingerId) DO UPDATE SET FirstName = excluded.FirstName THEN RETURN SingerId, FirstName`,
					`INSERT INTO singers (singerid, firstname) VALUES (1, 'Marcus') ON CONFLICT (singerid) DO UPDATE SET firstname = excluded.firstname RETURNING singerid, firstname`), rows: 1, dml: true, rw: true, check: apply},
			}
			if !pg {
				shapes = append(shapes, planShape{name: "HASH JOIN", sql: `SELECT a.AlbumTitle, s.SongName FROM Albums AS a HASH JOIN Songs AS s ON a.SingerId = s.SingerId AND a.AlbumId = s.AlbumId`,
					rows: 9, check: func(t *testing.T, p planView) {
						if hj := p.named("Hash Join"); len(hj) != 1 || !p.linkTypes(hj[0])["Build"] || !p.linkTypes(hj[0])["Probe"] {
							t.Errorf("want a Hash Join with Build and Probe:\n%s", p)
						}
					}})
			}
			for _, s := range shapes {
				runPlanShape(t, ctx, client, s)
			}
			// not Spanner's: refused in every mode, by the engine
			refused := []string{
				q(`SELECT SingerId, ROW_NUMBER() OVER (PARTITION BY SingerId ORDER BY AlbumId) FROM Albums`, `SELECT singerid, ROW_NUMBER() OVER (PARTITION BY singerid ORDER BY albumid) FROM albums`),
				`WITH RECURSIVE t AS (SELECT 1 AS n UNION ALL SELECT n + 1 FROM t WHERE n < 5) SELECT n FROM t`,
				q(`SELECT * FROM Albums NATURAL JOIN Songs`, `SELECT * FROM albums NATURAL JOIN songs`),
			}
			if !pg {
				refused = append(refused, `SELECT a.AlbumTitle FROM Albums AS a LOOKUP JOIN Songs AS s ON a.SingerId = s.SingerId`)
			}
			for _, sql := range refused {
				for _, mode := range []sppb.ExecuteSqlRequest_QueryMode{sppb.ExecuteSqlRequest_NORMAL, sppb.ExecuteSqlRequest_PLAN, sppb.ExecuteSqlRequest_PROFILE} {
					m := mode
					it := client.Single().QueryWithOptions(ctx, spanner.Statement{SQL: sql}, spanner.QueryOptions{Mode: &m})
					_, err := it.Next()
					it.Stop()
					if c := status.Code(err); c != codes.InvalidArgument && c != codes.Unimplemented || strings.Contains(fmt.Sprint(err), "CloudBurrow builds") {
						t.Errorf("%s in %s mode: %v; want the engine's refusal", sql, mode, err)
					}
				}
			}
		})
	}
}

// TestSpannerGraphQueryPlans (#2029): a graph query (Spanner Graph, on an
// Enterprise instance) in NORMAL, PLAN and PROFILE modes, through the
// official Go client: each node and edge pattern reads its element table,
// an edge joining its nodes on the property graph's keys; an inline
// property filter on a node's key seeks; a quantified edge pattern
// ({1,2}) is a Recursive Union of the first edges and a Cross Apply of a
// Recursive Spool Scan to the next (Google's binary operators page);
// GRAPH_TABLE plans as the subquery it is. Spanner Graph is GoogleSQL's
// only.
//
// covers: google.spanner.v1.Spanner/ExecuteSql, google.spanner.v1.Spanner/ExecuteStreamingSql
func TestSpannerGraphQueryPlans(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvSpanner)
	t.Setenv("SPANNER_EMULATOR_HOST", addr)
	ctx := h.Context()
	ia, err := instance.NewInstanceAdminClient(ctx, spannerOpts(addr)...)
	if err != nil {
		t.Fatal(err)
	}
	defer ia.Close()
	instName := "projects/" + h.Project() + "/instances/cb-plans-graph"
	op, err := ia.CreateInstance(ctx, &instancepb.CreateInstanceRequest{Parent: "projects/" + h.Project(), InstanceId: "cb-plans-graph",
		Instance: &instancepb.Instance{Config: fmt.Sprintf("projects/%s/instanceConfigs/emulator-config", h.Project()), DisplayName: "Graph plans",
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
	defer da.Close()
	ddl := append(append([]string{}, plansDDL[false]...),
		`CREATE TABLE Collaborations (SingerId INT64 NOT NULL, OtherId INT64 NOT NULL) PRIMARY KEY (SingerId, OtherId)`,
		`CREATE PROPERTY GRAPH MusicGraph NODE TABLES (Singers, Albums) EDGE TABLES (
			Albums AS Released SOURCE KEY (SingerId) REFERENCES Singers (SingerId) DESTINATION KEY (SingerId, AlbumId) REFERENCES Albums (SingerId, AlbumId),
			Collaborations SOURCE KEY (SingerId) REFERENCES Singers (SingerId) DESTINATION KEY (OtherId) REFERENCES Singers (SingerId))`)
	dop, err := da.CreateDatabase(ctx, &databasepb.CreateDatabaseRequest{Parent: instName, CreateStatement: "CREATE DATABASE graphplans", ExtraStatements: ddl})
	if err == nil {
		_, err = dop.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("CreateDatabase with a property graph: %v", err)
	}
	db := instName + "/databases/graphplans"
	t.Cleanup(func() { _ = da.DropDatabase(context.Background(), &databasepb.DropDatabaseRequest{Database: db}) })
	client, err := spanner.NewClient(ctx, db, spannerOpts(addr)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	var ms []*spanner.Mutation
	for i, n := range []string{"Marc", "Catalina", "Alice"} {
		ms = append(ms, spanner.Insert("Singers", []string{"SingerId", "FirstName"}, []any{int64(i + 1), n}))
	}
	ms = append(ms,
		spanner.Insert("Albums", []string{"SingerId", "AlbumId", "AlbumTitle"}, []any{int64(1), int64(1), "Total Junk"}),
		spanner.Insert("Albums", []string{"SingerId", "AlbumId", "AlbumTitle"}, []any{int64(1), int64(2), "Go, Go, Go"}),
		spanner.Insert("Albums", []string{"SingerId", "AlbumId", "AlbumTitle"}, []any{int64(2), int64(1), "Green"}),
		spanner.Insert("Collaborations", []string{"SingerId", "OtherId"}, []any{int64(1), int64(2)}),
		spanner.Insert("Collaborations", []string{"SingerId", "OtherId"}, []any{int64(2), int64(3)}))
	if _, err := client.Apply(ctx, ms); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	joined := func(t *testing.T, p planView) {
		t.Helper()
		if len(p.find(func(n *sppb.PlanNode) bool { return strings.Contains(n.GetDisplayName(), "Apply") })) == 0 || len(p.scans("TableScan")) != 3 {
			t.Errorf("want the edge joined to its nodes (an Apply) over three Table Scans:\n%s", p)
		}
	}
	for _, s := range []planShape{
		{name: "edge", sql: `GRAPH MusicGraph MATCH (s:Singers)-[:Released]->(a:Albums) RETURN s.FirstName, a.AlbumTitle`, rows: 3, check: joined},
		{name: "seek", sql: `GRAPH MusicGraph MATCH (s:Singers {SingerId: 1})-[:Released]->(a:Albums) RETURN a.AlbumTitle ORDER BY a.AlbumTitle`, rows: 2,
			check: func(t *testing.T, p planView) {
				joined(t, p)
				if len(p.find(func(n *sppb.PlanNode) bool {
					return n.GetDisplayName() == "Filter Scan" && p.linkTypes(n)["Seek Condition"]
				})) == 0 || len(p.named("Sort")) != 1 {
					t.Errorf("want a seek on Singers' key and a Sort:\n%s", p)
				}
			}},
		{name: "incoming edge", sql: `GRAPH MusicGraph MATCH (o:Singers)<-[:Collaborations]-(s:Singers) WHERE s.FirstName = 'Catalina' RETURN o.FirstName`, rows: 1, check: joined},
		{name: "aggregate", sql: `GRAPH MusicGraph MATCH (s:Singers)-[:Released]->(a:Albums) RETURN s.SingerId, COUNT(*) AS albums GROUP BY s.SingerId`, rows: 2,
			check: func(t *testing.T, p planView) {
				if len(p.named("Aggregate")) == 0 {
					t.Errorf("want an Aggregate:\n%s", p)
				}
			}},
		{name: "quantified path", sql: `GRAPH MusicGraph MATCH (s:Singers {SingerId: 1})-[:Collaborations]->{1,2}(o:Singers) RETURN o.FirstName ORDER BY o.FirstName`, rows: 2,
			check: func(t *testing.T, p planView) {
				ru := p.named("Recursive Union")
				if len(ru) != 1 || !p.linkTypes(ru[0])["Base"] || !p.linkTypes(ru[0])["Recursive"] || len(p.scans("RecursiveSpoolScan")) != 1 {
					t.Errorf("want a Recursive Union (Base, Recursive) over a Recursive Spool Scan:\n%s", p)
				}
				if p.nodes[0].GetExecutionStats() != nil && statInt(ru[0], "rows") != 3 {
					t.Errorf("the Recursive Union returns %d paths, want 3 (1->2, 2->3, 1->2->3)", statInt(ru[0], "rows"))
				}
			}},
		{name: "GRAPH_TABLE", sql: `SELECT g.FirstName FROM GRAPH_TABLE(MusicGraph MATCH (s:Singers) WHERE s.SingerId < 3 COLUMNS (s.FirstName)) AS g`, rows: 2,
			check: func(t *testing.T, p planView) {
				if fs := p.named("Filter Scan"); len(fs) != 1 || !p.linkTypes(fs[0])["Seek Condition"] {
					t.Errorf("want a seek on Singers' key:\n%s", p)
				}
			}},
	} {
		runPlanShape(t, ctx, client, s)
	}
}
