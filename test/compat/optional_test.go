//go:build compat

package compat

import (
	"context"
	"fmt"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/datastore"
	"cloud.google.com/go/firestore"
	"cloud.google.com/go/spanner"
	database "cloud.google.com/go/spanner/admin/database/apiv1"
	databasepb "cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	instance "cloud.google.com/go/spanner/admin/instance/apiv1"
	instancepb "cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Environment variables for the optional emulators, exported by
// `cloudburrow up --services ...`.
const (
	EnvFirestore = "CLOUDBURROW_TEST_FIRESTORE"
	EnvDatastore = "CLOUDBURROW_TEST_DATASTORE"
	EnvBigtable  = "CLOUDBURROW_TEST_BIGTABLE"
	EnvSpanner   = "CLOUDBURROW_TEST_SPANNER"
)

// TestFirestoreDocumentCRUD covers documents, queries and a transaction
// through the official Firestore client.
func TestFirestoreDocumentCRUD(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvFirestore)
	t.Setenv("FIRESTORE_EMULATOR_HOST", addr)
	ctx := h.Context()

	c, err := firestore.NewClient(ctx, h.Project())
	if err != nil {
		t.Fatalf("firestore.NewClient: %v", err)
	}
	defer c.Close()

	coll := c.Collection("widgets")
	doc := coll.Doc("one")
	if _, err := doc.Set(ctx, map[string]any{"size": 3, "name": "first"}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	snap, err := doc.Get(ctx)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := snap.Data()["name"]; got != "first" {
		t.Errorf("name = %v, want first", got)
	}

	// A query, which is the part a document store must actually get right.
	if _, err := coll.Doc("two").Set(ctx, map[string]any{"size": 9, "name": "second"}); err != nil {
		t.Fatalf("Set two: %v", err)
	}
	docs, err := coll.Where("size", ">", 5).Documents(ctx).GetAll()
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(docs) != 1 || docs[0].Data()["name"] != "second" {
		t.Errorf("query returned %d docs, want only the size>5 one", len(docs))
	}

	// A transaction, which is the other thing worth proving.
	err = c.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		return tx.Set(doc, map[string]any{"size": 42, "name": "first"})
	})
	if err != nil {
		t.Fatalf("RunTransaction: %v", err)
	}
	snap, _ = doc.Get(ctx)
	if got := snap.Data()["size"]; fmt.Sprint(got) != "42" {
		t.Errorf("size after transaction = %v, want 42", got)
	}

	if _, err := doc.Delete(ctx); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

// TestDatastoreEntityCRUD covers entities, a query and a transaction.
func TestDatastoreEntityCRUD(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvDatastore)
	t.Setenv("DATASTORE_EMULATOR_HOST", addr)
	ctx := h.Context()

	c, err := datastore.NewClient(ctx, h.Project())
	if err != nil {
		t.Fatalf("datastore.NewClient: %v", err)
	}
	defer c.Close()

	type widget struct {
		Name string
		Size int
	}
	key := datastore.NameKey("Widget", "one", nil)
	if _, err := c.Put(ctx, key, &widget{Name: "first", Size: 3}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	var got widget
	if err := c.Get(ctx, key, &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "first" {
		t.Errorf("Name = %q, want first", got.Name)
	}

	if _, err := c.Put(ctx, datastore.NameKey("Widget", "two", nil), &widget{Name: "second", Size: 9}); err != nil {
		t.Fatalf("Put two: %v", err)
	}
	var results []widget
	if _, err := c.GetAll(ctx, datastore.NewQuery("Widget").FilterField("Size", ">", 5), &results); err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(results) != 1 || results[0].Name != "second" {
		t.Errorf("query returned %d entities, want only the Size>5 one", len(results))
	}

	_, err = c.RunInTransaction(ctx, func(tx *datastore.Transaction) error {
		_, err := tx.Put(key, &widget{Name: "first", Size: 42})
		return err
	})
	if err != nil {
		t.Fatalf("RunInTransaction: %v", err)
	}
	if err := c.Get(ctx, key, &got); err != nil {
		t.Fatal(err)
	}
	if got.Size != 42 {
		t.Errorf("Size after transaction = %d, want 42", got.Size)
	}

	if err := c.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

// TestBigtableTableAndRows covers table creation and row read/write/filter.
func TestBigtableTableAndRows(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvBigtable)
	t.Setenv("BIGTABLE_EMULATOR_HOST", addr)
	ctx := h.Context()

	const instanceID = "cb-instance"
	admin, err := bigtable.NewAdminClient(ctx, h.Project(), instanceID)
	if err != nil {
		t.Fatalf("NewAdminClient: %v", err)
	}
	defer admin.Close()

	const table = "widgets"
	if err := admin.CreateTable(ctx, table); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	t.Cleanup(func() { _ = admin.DeleteTable(h.Context(), table) })
	if err := admin.CreateColumnFamily(ctx, table, "info"); err != nil {
		t.Fatalf("CreateColumnFamily: %v", err)
	}

	c, err := bigtable.NewClient(ctx, h.Project(), instanceID)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()

	tbl := c.Open(table)
	mut := bigtable.NewMutation()
	mut.Set("info", "name", bigtable.Now(), []byte("first"))
	if err := tbl.Apply(ctx, "row-1", mut); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	row, err := tbl.ReadRow(ctx, "row-1")
	if err != nil {
		t.Fatalf("ReadRow: %v", err)
	}
	items := row["info"]
	if len(items) == 0 || string(items[0].Value) != "first" {
		t.Errorf("row = %v, want info:name=first", row)
	}

	// A filtered read, which is the operation Bigtable users actually rely on.
	var seen int
	err = tbl.ReadRows(ctx, bigtable.PrefixRange("row-"), func(bigtable.Row) bool {
		seen++
		return true
	}, bigtable.RowFilter(bigtable.FamilyFilter("info")))
	if err != nil {
		t.Fatalf("ReadRows: %v", err)
	}
	if seen != 1 {
		t.Errorf("filtered read saw %d rows, want 1", seen)
	}
}

// TestSpannerSchemaAndQuery covers instance, database, schema, a write and a
// query through the official Spanner clients.
func TestSpannerSchemaAndQuery(t *testing.T) {
	h := New(t)
	addr := h.Endpoint(EnvSpanner)
	t.Setenv("SPANNER_EMULATOR_HOST", addr)
	ctx := h.Context()

	opts := []option.ClientOption{
		option.WithEndpoint(addr),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	}

	instAdmin, err := instance.NewInstanceAdminClient(ctx, opts...)
	if err != nil {
		t.Fatalf("NewInstanceAdminClient: %v", err)
	}
	defer instAdmin.Close()

	const instanceID = "cb-instance"
	instOp, err := instAdmin.CreateInstance(ctx, &instancepb.CreateInstanceRequest{
		Parent:     "projects/" + h.Project(),
		InstanceId: instanceID,
		Instance: &instancepb.Instance{
			Config:      fmt.Sprintf("projects/%s/instanceConfigs/emulator-config", h.Project()),
			DisplayName: "CloudBurrow",
			NodeCount:   1,
		},
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if _, err := instOp.Wait(ctx); err != nil {
		t.Fatalf("waiting for the instance: %v", err)
	}

	dbAdmin, err := database.NewDatabaseAdminClient(ctx, opts...)
	if err != nil {
		t.Fatalf("NewDatabaseAdminClient: %v", err)
	}
	defer dbAdmin.Close()

	dbOp, err := dbAdmin.CreateDatabase(ctx, &databasepb.CreateDatabaseRequest{
		Parent:          fmt.Sprintf("projects/%s/instances/%s", h.Project(), instanceID),
		CreateStatement: "CREATE DATABASE testdb",
		ExtraStatements: []string{
			`CREATE TABLE Widgets (Id INT64 NOT NULL, Name STRING(MAX)) PRIMARY KEY (Id)`,
		},
	})
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	if _, err := dbOp.Wait(ctx); err != nil {
		t.Fatalf("waiting for the database: %v", err)
	}

	dbName := fmt.Sprintf("projects/%s/instances/%s/databases/testdb", h.Project(), instanceID)
	c, err := spanner.NewClient(ctx, dbName, opts...)
	if err != nil {
		t.Fatalf("spanner.NewClient: %v", err)
	}
	defer c.Close()

	if _, err := c.Apply(ctx, []*spanner.Mutation{
		spanner.Insert("Widgets", []string{"Id", "Name"}, []any{int64(1), "first"}),
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	row, err := c.Single().ReadRow(ctx, "Widgets", spanner.Key{int64(1)}, []string{"Name"})
	if err != nil {
		t.Fatalf("ReadRow: %v", err)
	}
	var name string
	if err := row.Column(0, &name); err != nil {
		t.Fatal(err)
	}
	if name != "first" {
		t.Errorf("Name = %q, want first", name)
	}

	// A SQL query, which is the reason to use Spanner at all.
	iter := c.Single().Query(ctx, spanner.Statement{SQL: "SELECT Name FROM Widgets WHERE Id = 1"})
	defer iter.Stop()
	qrow, err := iter.Next()
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	var queried string
	if err := qrow.Column(0, &queried); err != nil {
		t.Fatal(err)
	}
	if queried != "first" {
		t.Errorf("query returned %q, want first", queried)
	}
	_ = time.Now
}
