package main

import (
	"context"
	"testing"

	"cloud.google.com/go/datastore"
	"cloud.google.com/go/datastore/apiv1/datastorepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// A key in the page's chosen database is the page's own key, not a
// foreign one (#1883); one in (default) is foreign there.
func TestDatastoreKeyValueIsDatabaseAware(t *testing.T) {
	pk := func(db string) *datastorepb.Key {
		return &datastorepb.Key{PartitionId: &datastorepb.PartitionId{ProjectId: "demo", DatabaseId: db},
			Path: []*datastorepb.Key_PathElement{{Kind: "Order", IdType: &datastorepb.Key_PathElement_Id{Id: 7}}}}
	}
	if _, ok := datastoreKeyValue(pk("db2"), datastoreHome{project: "demo", database: "db2"}).(*datastore.Key); !ok {
		t.Error("a key in the chosen database is foreign")
	}
	f, ok := datastoreKeyValue(pk(""), datastoreHome{project: "demo", database: "db2"}).(datastoreForeignKey)
	if !ok || f.Database != "(default)" {
		t.Errorf("a (default) key on db2's page = %#v, want foreign in (default)", f)
	}
	if _, ok := datastoreKeyValue(pk(""), datastoreHome{project: "demo"}).(*datastore.Key); !ok {
		t.Error("a (default) key on (default)'s page is foreign")
	}
}

// Each raw call carries the chosen database, in its request and its
// routing header.
func TestDatastoreDatabaseInterceptor(t *testing.T) {
	ctx := console.WithScopeValue(context.Background(), "database", "db2")
	req := &datastorepb.LookupRequest{ProjectId: "demo"}
	var header []string
	invoke := func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		md, _ := metadata.FromOutgoingContext(ctx)
		header = md.Get("x-goog-request-params")
		return nil
	}
	if err := datastoreDatabaseInterceptor(ctx, "/google.datastore.v1.Datastore/Lookup", req, nil, nil, invoke); err != nil {
		t.Fatal(err)
	}
	if req.DatabaseId != "db2" || len(header) != 1 || header[0] != "project_id=demo&database_id=db2" {
		t.Errorf("request database %q, routing header %v", req.DatabaseId, header)
	}
	req = &datastorepb.LookupRequest{ProjectId: "demo"}
	header = nil
	_ = datastoreDatabaseInterceptor(context.Background(), "/google.datastore.v1.Datastore/Lookup", req, nil, nil, invoke)
	if req.DatabaseId != "" || len(header) != 0 {
		t.Errorf("(default): request database %q, routing header %v", req.DatabaseId, header)
	}
}
