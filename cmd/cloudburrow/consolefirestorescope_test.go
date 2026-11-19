package main

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// The Data panel's database (#1845): ?database= names it, (default) when
// absent, and every document name the screens build is in it.
func TestFirestoreDataAddressesTheChosenDatabase(t *testing.T) {
	ctx := context.Background()
	if got := firestoreDatabaseIn(ctx); got != "(default)" {
		t.Errorf("no database named: %q, want (default)", got)
	}
	ctx = console.WithScopeValue(ctx, firestoreDatabaseKey, "orders-db")
	if got := firestoreDatabaseIn(ctx); got != "orders-db" {
		t.Errorf("database=orders-db: %q", got)
	}
	name, err := firestoreDocumentName(ctx, "demo", "users", "alice")
	if err != nil || name != "projects/demo/databases/orders-db/documents/users/alice" {
		t.Errorf("document name = %q, %v", name, err)
	}
	if got := firestoreDatabaseName(ctx, "demo"); got != "projects/demo/databases/orders-db" {
		t.Errorf("database name = %q", got)
	}
	if _, local := firestoreLocalReference(ctx, "demo", "projects/demo/databases/(default)/documents/users/alice"); local {
		t.Error("a reference into (default) is local to orders-db")
	}
	if (firestoreProvider{}).ScopeKey() != "database" {
		t.Error("the Firestore screens' scope is not ?database=")
	}
}

// Add field's type is Google's dropdown, and Delete collection and Delete
// document fields are offered where Google's menus have them.
func TestFirestoreDataPanelActions(t *testing.T) {
	var typ console.Field
	for _, f := range firestoreValueFields(true, "", "") {
		if f.Name == "type" {
			typ = f
		}
	}
	if typ.Type != "select" || strings.Join(typ.Options, ",") != strings.Join(firestoreTypes, ",") {
		t.Errorf("Add field's type is %+v; want a select of %v", typ, firestoreTypes)
	}
	p := firestoreProvider{}
	ids := func(path ...string) string {
		var out []string
		for _, a := range p.DetailActions(context.Background(), "demo", path) {
			out = append(out, a.ID)
		}
		return strings.Join(out, ",")
	}
	if got := ids("users"); !strings.Contains(got, "deletecollection") {
		t.Errorf("a collection offers %s, without Delete collection", got)
	}
	if got := ids("users", "alice"); !strings.Contains(got, "deletefields") || !strings.Contains(got, "deletedocument") {
		t.Errorf("a document offers %s, without Delete document and Delete document fields", got)
	}
}
