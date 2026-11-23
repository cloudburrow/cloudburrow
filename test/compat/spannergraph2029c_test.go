//go:build compat

package compat

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/spanner"
	database "cloud.google.com/go/spanner/admin/database/apiv1"
	databasepb "cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	instance "cloud.google.com/go/spanner/admin/instance/apiv1"
	instancepb "cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

// rowStrings reads every row as text.
func rowStrings(it *spanner.RowIterator) ([]string, error) {
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
			cells = append(cells, v.Value.String())
		}
		out = append(out, strings.Join(cells, " | "))
	}
}

// TestSpannerGraphProbe logs what each query does in NORMAL and PLAN
// modes (CLOUDBURROW_GRAPH_PROBE=1 only).
func TestSpannerGraphProbe(t *testing.T) {
	file := os.Getenv("CLOUDBURROW_GRAPH_PROBE")
	if file == "" {
		t.Skip("probe only")
	}
	h := New(t)
	ctx, client := finGraphClient(t, h, "cb-graph-probe")
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for i, q := range strings.Split(string(raw), "\n;;\n") {
		q = strings.TrimSpace(q)
		if q == "" {
			continue
		}
		rows, err := rowStrings(client.Single().Query(ctx, spanner.Statement{SQL: q}))
		_, perr := client.Single().AnalyzeQuery(ctx, spanner.Statement{SQL: q})
		prow, prerr := rowStrings(client.Single().QueryWithStats(ctx, spanner.Statement{SQL: q}))
		t.Logf("#%d %s\n  NORMAL %d rows %v err=%v\n  PLAN err=%v\n  PROFILE %d rows err=%v", i+1, strings.Join(strings.Fields(q), " "), len(rows), rows, err, perr, len(prow), prerr)
	}
}
