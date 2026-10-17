//go:build compat

package compat

import (
	"testing"
	"time"

	"cloud.google.com/go/spanner"
	"cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestSpannerAddSplitPoints (#1523): AddSplitPoints, a hint Google
// documents as changing where a database's data is placed and never what a
// call reads or writes, is a stored no-op of the call front in the Spanner
// pod (the emulator, one server, never splits). Through the official
// database admin client: split points on a table and on an index are
// accepted, the rows read the same after, and the requests Google refuses
// are refused (an unknown table or index, an index named with another
// table, a split point without keys, an expiry beyond 30 days, an unknown
// database).
//
// covers: google.spanner.admin.database.v1.DatabaseAdmin/AddSplitPoints
func TestSpannerAddSplitPoints(t *testing.T) {
	f := newSpannerAdmin(t, "cb-split", "splitdb")
	ctx := f.ctx
	op, err := f.db.UpdateDatabaseDdl(ctx, &databasepb.UpdateDatabaseDdlRequest{Database: f.database,
		Statements: []string{"CREATE INDEX ItemsByName ON Items(Name)"}})
	if err == nil {
		err = op.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("CREATE INDEX: %v", err)
	}
	dc, err := spanner.NewClient(ctx, f.database, spannerOpts(f.addr)...)
	if err != nil {
		t.Fatal(err)
	}
	defer dc.Close()
	if _, err := dc.Apply(ctx, []*spanner.Mutation{spanner.Insert("Items", []string{"Id", "Name"}, []any{150, "kept"})}); err != nil {
		t.Fatal(err)
	}
	key := func(v *structpb.Value) []*databasepb.SplitPoints_Key {
		return []*databasepb.SplitPoints_Key{{KeyParts: &structpb.ListValue{Values: []*structpb.Value{v}}}}
	}
	if _, err := f.db.AddSplitPoints(ctx, &databasepb.AddSplitPointsRequest{Database: f.database, Initiator: "cloudburrow-compat",
		SplitPoints: []*databasepb.SplitPoints{
			{Table: "Items", Keys: key(structpb.NewStringValue("100"))},
			{Index: "ItemsByName", Keys: key(structpb.NewStringValue("m")), ExpireTime: timestamppb.New(time.Now().Add(48 * time.Hour))},
		}}); err != nil {
		t.Fatalf("AddSplitPoints: %v", err)
	}
	row, err := dc.Single().ReadRow(ctx, "Items", spanner.Key{150}, []string{"Name"})
	var name string
	if err == nil {
		err = row.Column(0, &name)
	}
	if err != nil || name != "kept" {
		t.Errorf("the row after AddSplitPoints = %q, %v", name, err)
	}
	for _, tc := range []struct {
		what string
		sp   *databasepb.SplitPoints
		db   string
		code codes.Code
	}{
		{"an unknown table", &databasepb.SplitPoints{Table: "Nope", Keys: key(structpb.NewStringValue("1"))}, f.database, codes.NotFound},
		{"an unknown index", &databasepb.SplitPoints{Index: "Nope", Keys: key(structpb.NewStringValue("1"))}, f.database, codes.NotFound},
		{"an index named with another table", &databasepb.SplitPoints{Table: "Other", Index: "ItemsByName", Keys: key(structpb.NewStringValue("1"))}, f.database, codes.InvalidArgument},
		{"no keys", &databasepb.SplitPoints{Table: "Items"}, f.database, codes.InvalidArgument},
		{"an expiry 31 days ahead", &databasepb.SplitPoints{Table: "Items", Keys: key(structpb.NewStringValue("1")),
			ExpireTime: timestamppb.New(time.Now().Add(31 * 24 * time.Hour))}, f.database, codes.InvalidArgument},
		{"an unknown database", &databasepb.SplitPoints{Table: "Items", Keys: key(structpb.NewStringValue("1"))}, f.instance + "/databases/gone", codes.NotFound},
	} {
		_, err := f.db.AddSplitPoints(ctx, &databasepb.AddSplitPointsRequest{Database: tc.db, SplitPoints: []*databasepb.SplitPoints{tc.sp}})
		if status.Code(err) != tc.code {
			t.Errorf("AddSplitPoints with %s: %v, want %s", tc.what, err, tc.code)
		}
	}
}
