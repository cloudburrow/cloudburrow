//go:build compat

package compat

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"cloud.google.com/go/spanner"
	database "cloud.google.com/go/spanner/admin/database/apiv1"
	databasepb "cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	sppb "cloud.google.com/go/spanner/apiv1/spannerpb"
	"google.golang.org/api/iterator"
)

// plansDDL is the schema of Google's query plan pages
// (https://docs.cloud.google.com/spanner/docs/query-execution-plans and the
// query-operators-* pages), in each dialect.
var plansDDL = map[bool][]string{
	false: {
		`CREATE TABLE Singers (SingerId INT64 NOT NULL, FirstName STRING(1024), LastName STRING(1024), SingerInfo BYTES(MAX), BirthDate DATE) PRIMARY KEY (SingerId)`,
		`CREATE INDEX SingersByFirstLastName ON Singers(FirstName, LastName)`,
		`CREATE TABLE Albums (SingerId INT64 NOT NULL, AlbumId INT64 NOT NULL, AlbumTitle STRING(MAX), MarketingBudget INT64) PRIMARY KEY (SingerId, AlbumId), INTERLEAVE IN PARENT Singers ON DELETE CASCADE`,
		`CREATE INDEX AlbumsByAlbumTitle ON Albums(AlbumTitle)`,
		`CREATE TABLE Songs (SingerId INT64 NOT NULL, AlbumId INT64 NOT NULL, TrackId INT64 NOT NULL, SongName STRING(MAX), Duration INT64, SongGenre STRING(25)) PRIMARY KEY (SingerId, AlbumId, TrackId), INTERLEAVE IN PARENT Albums ON DELETE CASCADE`,
		`CREATE INDEX SongsBySongName ON Songs(SongName)`,
	},
	true: {
		`CREATE TABLE singers (singerid bigint NOT NULL, firstname varchar(1024), lastname varchar(1024), singerinfo bytea, birthdate date, PRIMARY KEY (singerid))`,
		`CREATE INDEX singersbyfirstlastname ON singers(firstname, lastname)`,
		`CREATE TABLE albums (singerid bigint NOT NULL, albumid bigint NOT NULL, albumtitle varchar, marketingbudget bigint, PRIMARY KEY (singerid, albumid)) INTERLEAVE IN PARENT singers ON DELETE CASCADE`,
		`CREATE INDEX albumsbyalbumtitle ON albums(albumtitle)`,
		`CREATE TABLE songs (singerid bigint NOT NULL, albumid bigint NOT NULL, trackid bigint NOT NULL, songname varchar, duration bigint, songgenre varchar(25), PRIMARY KEY (singerid, albumid, trackid)) INTERLEAVE IN PARENT albums ON DELETE CASCADE`,
		`CREATE INDEX songsbysongname ON songs(songname)`,
	},
}

// plansDB creates the database of Google's plan pages, with their rows.
func plansDB(t *testing.T, ctx context.Context, addr, project, dbID string, pg bool) *spanner.Client {
	t.Helper()
	parent := spannerDatabase(t, ctx, addr, project, "cb-plans", dbID+"seed",
		`CREATE TABLE Seed (Id INT64 NOT NULL) PRIMARY KEY (Id)`)
	parent = parent[:strings.LastIndex(parent, "/databases/")]
	dbAdmin, err := database.NewDatabaseAdminClient(ctx, spannerOpts(addr)...)
	if err != nil {
		t.Fatalf("NewDatabaseAdminClient: %v", err)
	}
	defer dbAdmin.Close()
	req := &databasepb.CreateDatabaseRequest{Parent: parent, CreateStatement: "CREATE DATABASE " + dbID}
	if pg {
		req.CreateStatement = `CREATE DATABASE "` + dbID + `"`
		req.DatabaseDialect = databasepb.DatabaseDialect_POSTGRESQL
	} else {
		req.ExtraStatements = plansDDL[false]
	}
	op, err := dbAdmin.CreateDatabase(ctx, req)
	if err != nil {
		t.Fatalf("CreateDatabase %s: %v", dbID, err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("CreateDatabase %s wait: %v", dbID, err)
	}
	db := parent + "/databases/" + dbID
	t.Cleanup(func() {
		a, err := database.NewDatabaseAdminClient(context.Background(), spannerOpts(addr)...)
		if err == nil {
			_ = a.DropDatabase(context.Background(), &databasepb.DropDatabaseRequest{Database: db})
			a.Close()
		}
	})
	if pg {
		up, err := dbAdmin.UpdateDatabaseDdl(ctx, &databasepb.UpdateDatabaseDdlRequest{Database: db, Statements: plansDDL[true]})
		if err != nil {
			t.Fatalf("UpdateDatabaseDdl: %v", err)
		}
		if err := up.Wait(ctx); err != nil {
			t.Fatalf("UpdateDatabaseDdl wait: %v", err)
		}
	}
	client, err := spanner.NewClient(ctx, db, spannerOpts(addr)...)
	if err != nil {
		t.Fatalf("spanner.NewClient: %v", err)
	}
	t.Cleanup(client.Close)
	name := func(s string) string {
		if pg {
			return strings.ToLower(s)
		}
		return s
	}
	ins := func(table string, cols []string, rows ...[]any) []*spanner.Mutation {
		var out []*spanner.Mutation
		for i := range cols {
			cols[i] = name(cols[i])
		}
		for _, r := range rows {
			out = append(out, spanner.Insert(name(table), cols, r))
		}
		return out
	}
	var ms []*spanner.Mutation
	ms = append(ms, ins("Singers", []string{"SingerId", "FirstName", "LastName"},
		[]any{int64(1), "Marc", "Richards"}, []any{int64(2), "Catalina", "Smith"}, []any{int64(3), "Alice", "Trentor"},
		[]any{int64(4), "Lea", "Martin"}, []any{int64(5), "David", "Lomond"})...)
	ms = append(ms, ins("Albums", []string{"SingerId", "AlbumId", "AlbumTitle"},
		[]any{int64(1), int64(1), "Total Junk"}, []any{int64(1), int64(2), "Go, Go, Go"}, []any{int64(2), int64(1), "Green"},
		[]any{int64(2), int64(2), "Forever Hold Your Peace"}, []any{int64(2), int64(3), "Terrified"},
		[]any{int64(3), int64(1), "Nothing To Do With Me"}, []any{int64(4), int64(1), "Play"})...)
	ms = append(ms, ins("Songs", []string{"SingerId", "AlbumId", "TrackId", "SongName", "Duration", "SongGenre"},
		[]any{int64(2), int64(1), int64(1), "Let's Get Back Together", int64(182), "COUNTRY"},
		[]any{int64(2), int64(1), int64(2), "Starting Again", int64(156), "ROCK"},
		[]any{int64(2), int64(1), int64(3), "I Knew You Were Magic", int64(294), "BLUES"},
		[]any{int64(2), int64(1), int64(4), "42", int64(185), "CLASSICAL"},
		[]any{int64(2), int64(1), int64(5), "Blue", int64(238), "BLUES"},
		[]any{int64(2), int64(1), int64(6), "Nothing Is The Same", int64(303), "BLUES"},
		[]any{int64(2), int64(1), int64(7), "The Second Time", int64(255), "ROCK"},
		[]any{int64(2), int64(3), int64(1), "Fight Story", int64(194), "ROCK"},
		[]any{int64(3), int64(1), int64(1), "Not About The Guitar", int64(278), "BLUES"})...)
	if _, err := client.Apply(ctx, ms); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return client
}

// planView reads a plan: each relational node, its metadata and links.
type planView struct {
	nodes []*sppb.PlanNode
}

func (p planView) find(f func(*sppb.PlanNode) bool) []*sppb.PlanNode {
	var out []*sppb.PlanNode
	for _, n := range p.nodes {
		if f(n) {
			out = append(out, n)
		}
	}
	return out
}

func (p planView) named(name string) []*sppb.PlanNode {
	return p.find(func(n *sppb.PlanNode) bool {
		return n.GetKind() == sppb.PlanNode_RELATIONAL && n.GetDisplayName() == name
	})
}

func md(n *sppb.PlanNode, k string) string { return n.GetMetadata().GetFields()[k].GetStringValue() }

func (p planView) scans(stype string) []*sppb.PlanNode {
	return p.find(func(n *sppb.PlanNode) bool { return n.GetDisplayName() == "Scan" && md(n, "scan_type") == stype })
}

// linkTypes are the types of a node's links to relational and scalar
// children.
func (p planView) linkTypes(n *sppb.PlanNode) map[string]bool {
	out := map[string]bool{}
	for _, l := range n.GetChildLinks() {
		out[l.GetType()] = true
	}
	return out
}

func (p planView) String() string {
	var b strings.Builder
	for _, n := range p.nodes {
		fmt.Fprintf(&b, "%d %s %s %q %v\n", n.GetIndex(), n.GetKind(), n.GetDisplayName(), n.GetShortRepresentation().GetDescription(), n.GetMetadata().AsMap())
	}
	return b.String()
}

// wellFormed checks Google's documented shape of a plan: node i has index
// i, node 0 is the relational root, every child link points at a node, and
// scalar nodes carry a short representation.
func wellFormed(t *testing.T, what string, p planView) {
	t.Helper()
	if len(p.nodes) == 0 {
		t.Fatalf("%s: no plan nodes", what)
	}
	if p.nodes[0].GetKind() != sppb.PlanNode_RELATIONAL {
		t.Errorf("%s: the root is %s, want RELATIONAL", what, p.nodes[0].GetKind())
	}
	scalars := 0
	for i, n := range p.nodes {
		if n.GetIndex() != int32(i) {
			t.Errorf("%s: node %d has index %d", what, i, n.GetIndex())
		}
		if n.GetKind() == sppb.PlanNode_KIND_UNSPECIFIED || n.GetDisplayName() == "" {
			t.Errorf("%s: node %d has no kind or name: %v", what, i, n)
		}
		if n.GetKind() == sppb.PlanNode_SCALAR {
			scalars++
			if n.GetShortRepresentation().GetDescription() == "" {
				t.Errorf("%s: scalar node %d has no short representation", what, i)
			}
		}
		for _, l := range n.GetChildLinks() {
			if l.GetChildIndex() <= int32(i) || int(l.GetChildIndex()) >= len(p.nodes) {
				t.Errorf("%s: node %d links to node %d", what, i, l.GetChildIndex())
			}
		}
	}
	if scalars == 0 {
		t.Errorf("%s: no SCALAR nodes", what)
	}
	if len(p.named("Serialize Result")) != 1 {
		t.Errorf("%s: want one Serialize Result:\n%s", what, p)
	}
}

func statInt(n *sppb.PlanNode, k string) int64 {
	v, _ := strconv.ParseInt(n.GetExecutionStats().GetFields()[k].GetStructValue().GetFields()["total"].GetStringValue(), 10, 64)
	return v
}

// TestSpannerQueryPlans (#2029) drives the official Go client's
// AnalyzeQuery (PLAN) and QueryWithStats (PROFILE) over the shapes Google's
// plan pages draw, on their schema and rows, in both dialects: a full table
// scan, a seek on the key, an index read (FORCE_INDEX), a full scan with a
// residual condition where no index covers the query, a co-located join (Cross Apply), a hash
// join (JOIN_METHOD=HASH_JOIN), and aggregates (a Stream Aggregate below
// the Distributed Union, a global/local pair for a scalar COUNT). PROFILE
// returns the rows, the same plan with every operator's execution
// statistics, and query statistics that agree with them.
//
// covers: google.spanner.v1.Spanner/ExecuteSql, google.spanner.v1.Spanner/ExecuteStreamingSql
func TestSpannerQueryPlans(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvSpanner)
	t.Setenv("SPANNER_EMULATOR_HOST", addr)
	ctx := h.Context()
	for _, pg := range []bool{false, true} {
		dialect, dbID := "GoogleSQL", "plansgsql"
		if pg {
			dialect, dbID = "PostgreSQL", "planspg"
		}
		t.Run(dialect, func(t *testing.T) {
			client := plansDB(t, ctx, addr, h.Project(), dbID, pg)
			q := func(gsql, pgsql string) string {
				if pg {
					return pgsql
				}
				return gsql
			}
			shapes := []struct {
				name, sql string
				rows      int64
				check     func(t *testing.T, p planView)
			}{
				{"scan", q(`SELECT s.SongName FROM Songs AS s`, `SELECT s.songname FROM songs AS s`), 9, func(t *testing.T, p planView) {
					ts := p.scans("TableScan")
					if len(ts) != 1 || !strings.EqualFold(md(ts[0], "scan_target"), "Songs") || md(ts[0], "Full scan") != "true" {
						t.Errorf("want one full Table Scan of Songs:\n%s", p)
					}
					du := p.named("Distributed Union")
					if len(du) != 2 || md(du[0], "call_type") != "Global" || md(du[1], "call_type") != "Local" || p.nodes[0] != du[0] {
						t.Errorf("want a global Distributed Union at the root over a local one:\n%s", p)
					}
				}},
				{"seek on the key", q(`SELECT LastName FROM Singers WHERE SingerId = 1`, `SELECT lastname FROM singers WHERE singerid = 1`), 1, func(t *testing.T, p planView) {
					ts := p.scans("TableScan")
					fs := p.named("Filter Scan")
					if len(ts) != 1 || md(ts[0], "Full scan") != "" || len(fs) != 1 || !p.linkTypes(fs[0])["Seek Condition"] {
						t.Errorf("want a Filter Scan with a Seek Condition over a Table Scan that is not full:\n%s", p)
					}
				}},
				{"index", q(`SELECT s.LastName FROM Singers@{FORCE_INDEX=SingersByFirstLastName} AS s WHERE s.FirstName = 'Catalina'`,
					`SELECT s.lastname FROM singers /*@ FORCE_INDEX=singersbyfirstlastname */ AS s WHERE s.firstname = 'Catalina'`), 1, func(t *testing.T, p planView) {
					is := p.scans("IndexScan")
					if len(is) != 1 || !strings.EqualFold(md(is[0], "scan_target"), "SingersByFirstLastName") || md(is[0], "Full scan") != "" || len(p.scans("TableScan")) != 0 {
						t.Errorf("want one Index Scan of SingersByFirstLastName, seeking, and no Table Scan:\n%s", p)
					}
				}},
				{"no index", q(`SELECT s.BirthDate FROM Singers AS s WHERE s.FirstName = 'Catalina'`,
					`SELECT s.birthdate FROM singers AS s WHERE s.firstname = 'Catalina'`), 1, func(t *testing.T, p planView) {
					ts := p.scans("TableScan")
					fs := p.named("Filter Scan")
					if len(ts) != 1 || md(ts[0], "Full scan") != "true" || len(p.scans("IndexScan")) != 0 || len(fs) != 1 || !p.linkTypes(fs[0])["Residual Condition"] {
						t.Errorf("want a full Table Scan with a Residual Condition, and no Index Scan:\n%s", p)
					}
				}},
				{"join", q(`SELECT al.AlbumTitle, so.SongName FROM Albums AS al, Songs AS so WHERE al.SingerId = so.SingerId AND al.AlbumId = so.AlbumId`,
					`SELECT al.albumtitle, so.songname FROM albums AS al, songs AS so WHERE al.singerid = so.singerid AND al.albumid = so.albumid`), 9, func(t *testing.T, p planView) {
					ca := p.named("Cross Apply")
					if len(ca) != 1 || !p.linkTypes(ca[0])["Input"] || !p.linkTypes(ca[0])["Map"] || len(p.scans("TableScan")) != 2 {
						t.Errorf("want a Cross Apply (Input, Map) over two Table Scans:\n%s", p)
					}
				}},
				{"hash join", q(`SELECT a.AlbumTitle, s.SongName FROM Albums AS a JOIN@{JOIN_METHOD=HASH_JOIN} Songs AS s ON a.SingerId = s.SingerId AND a.AlbumId = s.AlbumId`,
					`SELECT a.albumtitle, s.songname FROM albums AS a JOIN /*@ JOIN_METHOD=HASH_JOIN */ songs AS s ON a.singerid = s.singerid AND a.albumid = s.albumid`), 9, func(t *testing.T, p planView) {
					hj := p.named("Hash Join")
					if len(hj) != 1 || !p.linkTypes(hj[0])["Build"] || !p.linkTypes(hj[0])["Probe"] || !p.linkTypes(hj[0])["Condition"] {
						t.Errorf("want a Hash Join with Build, Probe and Condition:\n%s", p)
					}
				}},
				{"aggregate", q(`SELECT s.SingerId, COUNT(*) AS SongCount FROM Songs AS s WHERE s.SingerId < 100 GROUP BY s.SingerId`,
					`SELECT s.singerid, COUNT(*) AS songcount FROM songs AS s WHERE s.singerid < 100 GROUP BY s.singerid`), 2, func(t *testing.T, p planView) {
					ag := p.named("Aggregate")
					if len(ag) != 1 || md(ag[0], "iterator_type") != "Stream" {
						t.Errorf("want one Stream Aggregate:\n%s", p)
					}
				}},
				{"scalar aggregate", q(`SELECT COUNT(*) FROM Songs`, `SELECT COUNT(*) FROM songs`), 1, func(t *testing.T, p planView) {
					ag := p.named("Aggregate")
					if len(ag) != 2 || md(ag[0], "call_type") != "Global" || md(ag[1], "call_type") != "Local" {
						t.Errorf("want a global Aggregate over a local one:\n%s", p)
					}
				}},
			}
			for _, s := range shapes {
				stmt := spanner.Statement{SQL: s.sql}
				plan, err := client.Single().AnalyzeQuery(ctx, stmt)
				if err != nil {
					t.Fatalf("%s: AnalyzeQuery: %v", s.name, err)
				}
				pv := planView{nodes: plan.GetPlanNodes()}
				wellFormed(t, s.name+" (PLAN)", pv)
				for _, n := range pv.nodes {
					if n.GetExecutionStats() != nil {
						t.Errorf("%s: PLAN node %d has execution statistics", s.name, n.GetIndex())
					}
				}
				s.check(t, pv)

				it := client.Single().QueryWithStats(ctx, stmt)
				var got int64
				for {
					_, err := it.Next()
					if err == iterator.Done {
						break
					}
					if err != nil {
						t.Fatalf("%s: QueryWithStats: %v", s.name, err)
					}
					got++
				}
				if got != s.rows {
					t.Errorf("%s: PROFILE returned %d rows, want %d", s.name, got, s.rows)
				}
				pp := planView{nodes: it.QueryPlan.GetPlanNodes()}
				wellFormed(t, s.name+" (PROFILE)", pp)
				s.check(t, pp)
				if len(pp.nodes) != len(pv.nodes) {
					t.Errorf("%s: PROFILE's plan has %d nodes, PLAN's %d", s.name, len(pp.nodes), len(pv.nodes))
				}
				st := it.QueryStats
				for _, k := range []string{"elapsed_time", "cpu_time", "rows_returned", "rows_scanned", "query_text"} {
					if st[k] == nil || st[k] == "" {
						t.Errorf("%s: query_stats has no %s: %v", s.name, k, st)
					}
				}
				if st["query_text"] != s.sql || st["rows_returned"] != strconv.FormatInt(s.rows, 10) {
					t.Errorf("%s: query_stats query_text %q rows_returned %v; want the query and %d", s.name, st["query_text"], st["rows_returned"], s.rows)
				}
				var scanned int64
				for _, n := range pp.nodes {
					if n.GetKind() != sppb.PlanNode_RELATIONAL {
						continue
					}
					if n.GetExecutionStats() == nil || n.GetExecutionStats().GetFields()["rows"] == nil || n.GetExecutionStats().GetFields()["latency"] == nil {
						t.Errorf("%s: PROFILE node %d (%s) has no rows or latency: %v", s.name, n.GetIndex(), n.GetDisplayName(), n.GetExecutionStats())
					}
					scanned += statInt(n, "scanned_rows")
					// a filter never returns more rows than it reads
					if n.GetDisplayName() == "Filter Scan" {
						for _, l := range n.GetChildLinks() {
							c := pp.nodes[l.GetChildIndex()]
							if c.GetKind() == sppb.PlanNode_RELATIONAL && statInt(n, "rows") > statInt(c, "rows") {
								t.Errorf("%s: Filter Scan returns %d rows of its scan's %d", s.name, statInt(n, "rows"), statInt(c, "rows"))
							}
						}
					}
				}
				if fmt.Sprint(scanned) != st["rows_scanned"] {
					t.Errorf("%s: the scans' scanned_rows add up to %d, query_stats rows_scanned is %v", s.name, scanned, st["rows_scanned"])
				}
				if statInt(pp.nodes[0], "rows") != s.rows {
					t.Errorf("%s: the root returns %d rows, want %d", s.name, statInt(pp.nodes[0], "rows"), s.rows)
				}
				if s.name == "scan" {
					if ts := pp.scans("TableScan"); len(ts) == 1 && statInt(ts[0], "scanned_rows") != 9 {
						t.Errorf("scan: the Table Scan of Songs scanned %d rows, want 9", statInt(ts[0], "scanned_rows"))
					}
				}
				if s.name == "seek on the key" {
					if ts := pp.scans("TableScan"); len(ts) == 1 && statInt(ts[0], "rows") != 1 {
						t.Errorf("seek: the Table Scan returned %d rows, want 1 (a seek, not the 5 rows of Singers)", statInt(ts[0], "rows"))
					}
				}
			}
		})
	}
}
