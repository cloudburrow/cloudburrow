//go:build compat

package compat

import (
	"testing"

	"cloud.google.com/go/spanner"
	database "cloud.google.com/go/spanner/admin/database/apiv1"
	databasepb "cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
)

// TestSpannerQueryPlansRefusedForEveryShape (#2029) drives the official Go
// client's AnalyzeQuery (PLAN) and QueryWithStats (PROFILE) over the query
// shapes Google's plan pages draw: a table scan, a filter on the key and on
// a column, a join, and a read through a secondary index (FORCE_INDEX).
//
// None of them gets a plan, because none can be faithful. Read in the
// emulator's source (cloud-spanner-emulator d357ec1d, Apache-2.0): a query
// is run by GoogleSQL's reference evaluator (backend/query/query_engine.cc
// EvaluateQuery: googlesql::PreparedQuery Prepare + Execute), and every
// table it reads is read whole from the base table
// (backend/query/queryable_table.cc CreateEvaluatorTableIterator:
// KeySet::All, no seek); FORCE_INDEX names are only validated
// (query_validator.cc indexes_used_), and join_method hints are validated
// and dropped. So the structure the emulator executes has no Distributed
// Union, no seek condition, no Index Scan and no Hash or Apply Join: a plan
// built from it would not be Spanner's, and a plan in Spanner's operator
// names would not be the executed one. Each shape is refused UNIMPLEMENTED,
// and the same query in NORMAL mode returns its rows.
//
// covers: google.spanner.v1.Spanner/ExecuteStreamingSql
func TestSpannerQueryPlansRefusedForEveryShape(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvSpanner)
	t.Setenv("SPANNER_EMULATOR_HOST", addr)
	ctx := h.Context()
	db := spannerDatabase(t, ctx, addr, h.Project(), "cb-plans", "plansdb",
		`CREATE TABLE Singers (SingerId INT64 NOT NULL, Name STRING(MAX)) PRIMARY KEY (SingerId)`)

	dbAdmin, err := database.NewDatabaseAdminClient(ctx, spannerOpts(addr)...)
	if err != nil {
		t.Fatalf("NewDatabaseAdminClient: %v", err)
	}
	defer dbAdmin.Close()
	op, err := dbAdmin.UpdateDatabaseDdl(ctx, &databasepb.UpdateDatabaseDdlRequest{
		Database: db,
		Statements: []string{
			`CREATE TABLE Albums (SingerId INT64 NOT NULL, AlbumId INT64 NOT NULL, Title STRING(MAX)) PRIMARY KEY (SingerId, AlbumId), INTERLEAVE IN PARENT Singers ON DELETE CASCADE`,
			`CREATE INDEX SingersByName ON Singers(Name)`,
		},
	})
	if err != nil {
		t.Fatalf("UpdateDatabaseDdl: %v", err)
	}
	if err := op.Wait(ctx); err != nil {
		t.Fatalf("UpdateDatabaseDdl wait: %v", err)
	}

	client, err := spanner.NewClient(ctx, db, spannerOpts(addr)...)
	if err != nil {
		t.Fatalf("spanner.NewClient: %v", err)
	}
	defer client.Close()
	if _, err := client.Apply(ctx, []*spanner.Mutation{
		spanner.Insert("Singers", []string{"SingerId", "Name"}, []any{int64(1), "Marc"}),
		spanner.Insert("Singers", []string{"SingerId", "Name"}, []any{int64(2), "Catalina"}),
		spanner.Insert("Albums", []string{"SingerId", "AlbumId", "Title"}, []any{int64(1), int64(1), "Total Junk"}),
		spanner.Insert("Albums", []string{"SingerId", "AlbumId", "Title"}, []any{int64(2), int64(1), "Green"}),
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	shapes := []struct {
		name, sql string
		rows      int
	}{
		{"table scan", `SELECT SingerId, Name FROM Singers`, 2},
		{"filter on the key", `SELECT Name FROM Singers WHERE SingerId = 1`, 1},
		{"filter on a column", `SELECT SingerId FROM Singers WHERE Name LIKE 'M%'`, 1},
		{"join", `SELECT s.Name, a.Title FROM Singers s JOIN Albums a ON s.SingerId = a.SingerId`, 2},
		{"hash join hint", `SELECT s.Name, a.Title FROM Singers s JOIN@{JOIN_METHOD=HASH_JOIN} Albums a ON s.SingerId = a.SingerId`, 2},
		{"index", `SELECT SingerId, Name FROM Singers@{FORCE_INDEX=SingersByName} WHERE Name = 'Catalina'`, 1},
	}
	for _, s := range shapes {
		stmt := spanner.Statement{SQL: s.sql}

		n := 0
		it := client.Single().Query(ctx, stmt)
		for {
			_, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatalf("%s: NORMAL: %v", s.name, err)
			}
			n++
		}
		it.Stop()
		if n != s.rows {
			t.Errorf("%s: NORMAL returned %d rows, want %d", s.name, n, s.rows)
		}

		if plan, err := client.Single().AnalyzeQuery(ctx, stmt); spanner.ErrCode(err) != codes.Unimplemented {
			t.Errorf("%s: AnalyzeQuery = %v, %v; want UNIMPLEMENTED (no faithful plan)", s.name, plan, err)
		}

		pit := client.Single().QueryWithStats(ctx, stmt)
		_, err := pit.Next()
		pit.Stop()
		if spanner.ErrCode(err) != codes.Unimplemented {
			t.Errorf("%s: QueryWithStats (PROFILE) = %v; want UNIMPLEMENTED", s.name, err)
		} else if pit.QueryPlan != nil {
			t.Errorf("%s: QueryWithStats refused but carried a plan: %v", s.name, pit.QueryPlan)
		}
	}
}
