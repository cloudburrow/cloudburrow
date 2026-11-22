package spannerfront

import (
	"fmt"
	"strings"
	"testing"

	"cloud.google.com/go/spanner/apiv1/spannerpb"
)

// docsSchema is the schema of Google's query plan pages
// (query-execution-plans, query-operators-*).
func docsSchema(pg bool) *planSchema {
	s := &planSchema{pg: pg, tables: map[string]*planTable{}, indexes: map[string]*planIndex{}, views: map[string]string{}}
	tab := func(name, parent string, pk []string, cols ...string) {
		t := &planTable{name: name, parent: parent, cols: cols}
		for _, k := range pk {
			t.pk = append(t.pk, keyCol{name: k})
		}
		s.tables[strings.ToLower(name)] = t
	}
	tab("Singers", "", []string{"SingerId"}, "SingerId", "FirstName", "LastName", "SingerInfo", "BirthDate")
	tab("Albums", "Singers", []string{"SingerId", "AlbumId"}, "SingerId", "AlbumId", "AlbumTitle", "MarketingBudget")
	tab("Songs", "Albums", []string{"SingerId", "AlbumId", "TrackId"}, "SingerId", "AlbumId", "TrackId", "SongName", "Duration", "SongGenre")
	tab("Concerts", "", []string{"VenueId", "SingerId", "ConcertDate"}, "VenueId", "SingerId", "ConcertDate", "BeginTime", "EndTime", "TicketPrices")
	idx := func(name, table, parent string, storing []string, keys ...string) {
		ix := &planIndex{name: name, table: table, parent: parent, storing: storing}
		for _, k := range keys {
			d := strings.HasSuffix(k, " DESC")
			ix.keys = append(ix.keys, keyCol{name: strings.TrimSuffix(k, " DESC"), desc: d})
		}
		s.indexes[strings.ToLower(name)] = ix
	}
	idx("SingersByFirstLastName", "Singers", "", nil, "FirstName", "LastName")
	idx("AlbumsByAlbumTitle", "Albums", "", nil, "AlbumTitle")
	idx("AlbumsByAlbumTitle2", "Albums", "", []string{"MarketingBudget"}, "AlbumTitle")
	idx("SongsBySingerAlbumSongNameDesc", "Songs", "Albums", nil, "SingerId", "AlbumId", "SongName DESC")
	idx("SongsBySongName", "Songs", "", nil, "SongName")
	return s
}

// planTree draws a plan's relational operators, one per line, with their
// child link types, scan targets and conditions.
func planTree(nodes []*spannerpb.PlanNode) string {
	var b strings.Builder
	var walk func(i int32, depth int, typ string)
	walk = func(i int32, depth int, typ string) {
		n := nodes[i]
		md := n.GetMetadata().GetFields()
		name := n.GetDisplayName()
		if st := md["scan_type"].GetStringValue(); st != "" {
			name += "[" + st + " " + md["scan_target"].GetStringValue()
			if md["Full scan"].GetStringValue() == "true" {
				name += " full"
			}
			name += "]"
		}
		if ct := md["call_type"].GetStringValue(); ct != "" {
			name += "(" + ct + ")"
		}
		if it := md["iterator_type"].GetStringValue(); it != "" {
			name += "{" + it + "}"
		}
		if typ != "" {
			name = typ + ": " + name
		}
		fmt.Fprintf(&b, "%s%s\n", strings.Repeat("  ", depth), name)
		for _, l := range n.GetChildLinks() {
			c := nodes[l.GetChildIndex()]
			if c.GetKind() == spannerpb.PlanNode_RELATIONAL {
				walk(l.GetChildIndex(), depth+1, l.GetType())
			} else if l.GetType() == "Seek Condition" || l.GetType() == "Residual Condition" || l.GetType() == "Condition" {
				fmt.Fprintf(&b, "%s%s %s\n", strings.Repeat("  ", depth+1), l.GetType()+":", c.GetShortRepresentation().GetDescription())
			}
		}
	}
	walk(0, 0, "")
	return b.String()
}

func planFor(t *testing.T, sc *planSchema, sql string) ([]*spannerpb.PlanNode, *planOp) {
	t.Helper()
	ps, q, err := parseQuerySQL(sql, sc.pg)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	b := &planBuilder{ps: ps, schema: sc, ctes: map[string]*sqlCTE{}, cols: map[*sqlExpr]colRef{},
		subPlans: map[*sqlExpr]*planOp{}, orderFor: map[*sqlSelect][]sqlOrder{}}
	root, err := b.buildPlan(q)
	if err != nil {
		t.Fatalf("plan %q: %v", sql, err)
	}
	nodes, _ := planNodes(root)
	for i, n := range nodes {
		if n.GetIndex() != int32(i) {
			t.Fatalf("node %d has index %d", i, n.GetIndex())
		}
	}
	return nodes, root
}

func TestPlanShapes(t *testing.T) {
	sc := docsSchema(false)
	for _, c := range []struct{ sql, want string }{
		{"SELECT s.SongName FROM Songs AS s", `
Distributed Union(Global)
  Distributed Union(Local)
    Serialize Result
      Scan[TableScan Songs full]
`},
		{"SELECT lastname FROM singers WHERE singerid = 1", `
Distributed Union(Global)
  Distributed Union(Local)
    Serialize Result
      Filter Scan
        Scan[TableScan Singers]
        Seek Condition: $SingerId = 1
`},
		{"SELECT s.lastname FROM singers@{FORCE_INDEX=SingersByFirstLastName} as s WHERE s.firstname = 'Catalina'", `
Distributed Union(Global)
  Distributed Union(Local)
    Serialize Result
      Filter Scan
        Scan[IndexScan SingersByFirstLastName]
        Seek Condition: $FirstName = 'Catalina'
`},
		{"SELECT s.SingerId, COUNT(*) AS SongCount FROM Songs AS s WHERE s.SingerId < 100 GROUP BY s.SingerId", `
Distributed Union(Global)
  Distributed Union(Local)
    Serialize Result
      Aggregate{Stream}
        Input: Filter Scan
          Scan[TableScan Songs]
          Seek Condition: $SingerId < 100
`},
		{"SELECT COUNT(*) FROM Songs", `
Serialize Result
  Aggregate(Global){Stream}
    Input: Distributed Union(Global)
      Distributed Union(Local)
        Aggregate(Local){Stream}
          Input: Scan[TableScan Songs full]
`},
		{"SELECT al.AlbumTitle, so.SongName FROM Albums AS al, Songs AS so WHERE al.SingerId = so.SingerId AND al.AlbumId = so.AlbumId", `
Distributed Union(Global)
  Distributed Union(Local)
    Serialize Result
      Cross Apply
        Input: Scan[TableScan Albums full]
        Map: Filter Scan
          Scan[TableScan Songs]
          Seek Condition: ($SingerId = $SingerId) AND ($AlbumId = $AlbumId)
`},
		{"SELECT a.AlbumTitle, s.SongName FROM Albums AS a JOIN@{JOIN_METHOD=HASH_JOIN} Songs AS s ON a.SingerId = s.SingerId AND a.AlbumId = s.AlbumId", `
Serialize Result
  Hash Join
    Build: Distributed Union(Global)
      Distributed Union(Local)
        Scan[TableScan Albums full]
    Probe: Distributed Union(Global)
      Distributed Union(Local)
        Scan[TableScan Songs full]
    Condition: $SingerId = $SingerId
    Condition: $AlbumId = $AlbumId
`},
		{"SELECT s.SongGenre FROM Songs AS s ORDER BY SongGenre LIMIT 3", `
Serialize Result
  Sort Limit(Global)
    Distributed Union(Global)
      Distributed Union(Local)
        Sort Limit(Local)
          Scan[TableScan Songs full]
`},
		{"SELECT 1 + 2 AS Result", `
Serialize Result
  Unit Relation
`},
		{"SELECT s.SongName, s.Duration FROM Songs@{force_index=SongsBySongName} AS s WHERE STARTS_WITH(s.SongName, \"B\")", `
Serialize Result
  Distributed Cross Apply
    Input: Create Batch
      Input: Distributed Union(Global)
        Distributed Union(Local)
          Filter Scan
            Scan[IndexScan SongsBySongName]
            Seek Condition: STARTS_WITH($SongName, "B")
    Map: Cross Apply
      Input: Scan[BatchScan $v1]
      Map: Filter Scan
        Scan[TableScan Songs]
        Seek Condition: ($SingerId' = $batched_SingerId) AND ($AlbumId' = $batched_AlbumId) AND ($TrackId' = $batched_TrackId)
`},
	} {
		nodes, _ := planFor(t, sc, c.sql)
		if got := planTree(nodes); strings.TrimSpace(got) != strings.TrimSpace(c.want) {
			t.Errorf("%s:\ngot\n%s\nwant\n%s", c.sql, got, c.want)
		}
	}
}

func TestPlanPostgreSQL(t *testing.T) {
	sc := docsSchema(true)
	nodes, _ := planFor(t, sc, `SELECT s.SongName FROM Songs /*@ force_index=SongsBySongName */ AS s WHERE STARTS_WITH(s.SongName, 'B') AND s.singerid = $1`)
	got := planTree(nodes)
	if !strings.Contains(got, "Scan[IndexScan SongsBySongName]") || !strings.Contains(got, "Seek Condition: STARTS_WITH($SongName, 'B')") {
		t.Errorf("PostgreSQL plan:\n%s", got)
	}
}

func TestPlanRefusesWhatItDoesNotKnow(t *testing.T) {
	for _, sql := range []string{
		"SELECT a FROM t1 INTERSECT DISTINCT SELECT a FROM t1",
		"INSERT INTO Singers (SingerId) VALUES (1)",
		// not Spanner's: the engine refuses them before a plan is asked for
		"SELECT ROW_NUMBER() OVER (ORDER BY SingerId) FROM Singers",
		"WITH RECURSIVE t AS (SELECT 1 AS n) SELECT n FROM t",
		"SELECT * FROM Singers NATURAL JOIN Albums",
		"SELECT * FROM Singers LOOKUP JOIN Albums USING (SingerId)",
	} {
		ps, q, err := parseQuerySQL(sql, false)
		if err == nil {
			b := &planBuilder{ps: ps, schema: docsSchema(false), ctes: map[string]*sqlCTE{}, cols: map[*sqlExpr]colRef{},
				subPlans: map[*sqlExpr]*planOp{}, orderFor: map[*sqlSelect][]sqlOrder{}}
			_, err = b.buildPlan(q)
		}
		if _, ok := err.(unsupportedSQL); !ok {
			t.Errorf("%s: %v, want it refused by name", sql, err)
		}
	}
}

func TestPlanDML(t *testing.T) {
	sc := docsSchema(false)
	for _, c := range []struct{ sql, want string }{
		{"DELETE FROM Singers WHERE BirthDate IS NULL", `
Apply Mutations
  Distributed Union(Global)
    Distributed Union(Local)
      Filter Scan
        Scan[TableScan Singers full]
        Residual Condition: $BirthDate IS NULL
`},
		{"DELETE FROM Singers WHERE FirstName = 'Alice'", `
Apply Mutations
  Distributed Union(Global)
    Distributed Union(Local)
      Filter Scan
        Scan[IndexScan SingersByFirstLastName]
        Seek Condition: $FirstName = 'Alice'
`},
		{"UPDATE Albums SET MarketingBudget = MarketingBudget * 2 WHERE SingerId = 1 THEN RETURN AlbumId", `
Serialize Result
  Apply Mutations
    Distributed Union(Global)
      Distributed Union(Local)
        Filter Scan
          Scan[TableScan Albums]
          Seek Condition: $SingerId = 1
`},
		{"INSERT INTO Singers (SingerId, FirstName) VALUES (10, 'A'), (11, 'B')", `
Apply Mutations
  Union All
    Union Input
      Unit Relation
    Union Input
      Unit Relation
`},
	} {
		ps, st, err := parseStatementSQL(c.sql, false)
		if err != nil {
			t.Fatalf("parse %q: %v", c.sql, err)
		}
		b := &planBuilder{ps: ps, schema: sc, ctes: map[string]*sqlCTE{}, cols: map[*sqlExpr]colRef{},
			subPlans: map[*sqlExpr]*planOp{}, orderFor: map[*sqlSelect][]sqlOrder{}}
		root, err := b.buildDML(st.dml)
		if err != nil {
			t.Fatalf("plan %q: %v", c.sql, err)
		}
		nodes, _ := planNodes(root)
		if got := planTree(nodes); strings.TrimSpace(got) != strings.TrimSpace(c.want) {
			t.Errorf("%s:\ngot\n%s\nwant\n%s", c.sql, got, c.want)
		}
	}
}

// TestPlanRefusedShapes (#2029): the shapes the first planner refused.
func TestPlanRefusedShapes(t *testing.T) {
	sc := docsSchema(false)
	sc.views["singernames"] = "SELECT Singers.SingerId AS SingerId, Singers.FirstName AS FirstName FROM Singers"
	for _, c := range []struct{ sql, want string }{
		{"SELECT SingerId FROM Albums INTERSECT DISTINCT SELECT SingerId FROM Songs", `
Serialize Result
  Filter
    Aggregate(Global){Hash}
      Input: Union All
        Union Input
          Distributed Union(Global)
            Distributed Union(Local)
              Scan[TableScan Albums full]
        Union Input
          Distributed Union(Global)
            Distributed Union(Local)
              Scan[TableScan Songs full]
    Condition: ($left_count > 0) AND ($right_count > 0)
`},
		{"SELECT SingerId FROM Albums EXCEPT ALL SELECT SingerId FROM Songs", `
Serialize Result
  Cross Apply
    Input: Filter
      Aggregate(Global){Hash}
        Input: Union All
          Union Input
            Distributed Union(Global)
              Distributed Union(Local)
                Scan[TableScan Albums full]
          Union Input
            Distributed Union(Global)
              Distributed Union(Local)
                Scan[TableScan Songs full]
      Condition: ($left_count > $right_count)
    Map: Array Unnest
`},
		{"SELECT FirstName FROM SingerNames WHERE SingerId = 1", `
Serialize Result
  Filter
    Distributed Union(Global)
      Distributed Union(Local)
        Scan[TableScan Singers full]
    Condition: $SingerId = 1
`},
		{"SELECT s.SongName FROM Songs AS s TABLESAMPLE RESERVOIR (2 ROWS)", `
Serialize Result
  Sort Limit
    Random Id Assign
      Distributed Union(Global)
        Distributed Union(Local)
          Scan[TableScan Songs full]
`},
		{"SELECT s.SongName FROM Songs AS s TABLESAMPLE BERNOULLI (10 PERCENT) WHERE s.Duration > 200", `
Serialize Result
  Filter
    Filter
      Random Id Assign
        Distributed Union(Global)
          Distributed Union(Local)
            Scan[TableScan Songs full]
      Condition: ($random_id < (10 / 100))
    Condition: $Duration > 200
`},
		{"SELECT LastName FROM Singers WHERE SingerId = 1 FOR UPDATE", `
Distributed Union(Global)
  Distributed Union(Local)
    Serialize Result
      Filter Scan
        Scan[TableScan Singers]
        Seek Condition: $SingerId = 1
`},
		{"SELECT a.AlbumTitle FROM Albums AS a LEFT OUTER HASH JOIN Songs AS s ON a.SingerId = s.SingerId", `
Serialize Result
  Hash Join
    Build: Distributed Union(Global)
      Distributed Union(Local)
        Scan[TableScan Albums full]
    Probe: Distributed Union(Global)
      Distributed Union(Local)
        Scan[TableScan Songs full]
    Condition: $SingerId = $SingerId
`},
	} {
		nodes, _ := planFor(t, sc, c.sql)
		if got := planTree(nodes); strings.TrimSpace(got) != strings.TrimSpace(c.want) {
			t.Errorf("%s:\ngot\n%s\nwant\n%s", c.sql, got, c.want)
		}
	}
	// INSERT ... ON CONFLICT plans as the INSERT it is
	for _, sql := range []string{
		"INSERT INTO Singers (SingerId, FirstName) VALUES (1, 'X') ON CONFLICT (SingerId) DO NOTHING",
		"INSERT INTO Singers AS s (SingerId, FirstName) VALUES (1, 'X') ON CONFLICT (SingerId) DO UPDATE SET FirstName = excluded.FirstName WHERE s.FirstName IS NULL THEN RETURN SingerId",
	} {
		ps, st, err := parseStatementSQL(sql, false)
		if err != nil {
			t.Fatalf("parse %q: %v", sql, err)
		}
		b := &planBuilder{ps: ps, schema: sc, ctes: map[string]*sqlCTE{}, cols: map[*sqlExpr]colRef{},
			subPlans: map[*sqlExpr]*planOp{}, orderFor: map[*sqlSelect][]sqlOrder{}}
		root, err := b.buildDML(st.dml)
		if err != nil {
			t.Fatalf("plan %q: %v", sql, err)
		}
		nodes, _ := planNodes(root)
		if got := planTree(nodes); !strings.Contains(got, "Apply Mutations\n") || !strings.Contains(got, "Unit Relation") {
			t.Errorf("%s:\n%s", sql, got)
		}
	}
	// PostgreSQL: INTERSECT binds tighter than UNION; TABLESAMPLE SPANNER.RESERVOIR
	pg := docsSchema(true)
	nodes, _ := planFor(t, pg, "SELECT singerid FROM albums UNION ALL SELECT singerid FROM songs INTERSECT SELECT singerid FROM singers")
	if got := planTree(nodes); !strings.Contains(got, "Union All\n    Union Input\n") || strings.Count(got, "Union All") != 2 || !strings.Contains(got, "Filter") {
		t.Errorf("PostgreSQL UNION ALL over INTERSECT:\n%s", got)
	}
	nodes, _ = planFor(t, pg, "SELECT s.songname FROM songs AS s TABLESAMPLE spanner.reservoir (2)")
	if got := planTree(nodes); !strings.Contains(got, "Sort Limit\n    Random Id Assign") {
		t.Errorf("PostgreSQL SPANNER.RESERVOIR:\n%s", got)
	}
}
