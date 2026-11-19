package main

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"cloud.google.com/go/firestore"
	admin "cloud.google.com/go/firestore/apiv1/admin"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// The Data viewer's database (#1845, round 2). Google's Data panel browses
// any database of the project, chosen in its database picker; the Firestore
// screens read only (default) before. The database now travels in the URL as
// ?database=, the way Bigtable's instance does (#1360), and every read and
// change the screens make — collections, documents, fields, Start collection,
// Add document, Add field, Delete — addresses it.

// firestoreDatabaseKey is the query parameter the chosen database travels in.
const firestoreDatabaseKey = "database"

// firestoreDefaultDatabase is the database a request naming none means.
const firestoreDefaultDatabase = "(default)"

// ScopeKey implements console.Scoper.
func (firestoreProvider) ScopeKey() string { return firestoreDatabaseKey }

// firestoreDatabaseIn is the database ctx's request names, or (default).
func firestoreDatabaseIn(ctx context.Context) string {
	if v := console.ScopeValue(ctx, firestoreDatabaseKey); v != "" {
		return v
	}
	return firestoreDefaultDatabase
}

var _ console.Scoper = firestoreProvider{}

// databaseScope is the Data viewer's database picker: (default), which the
// emulator serves without it being created, then the project's other
// Firestore Native databases, by ID. A Datastore mode database is browsed on
// the Datastore page, as on Google's console.
func (p firestoreProvider) databaseScope(ctx context.Context, project string) *console.Scope {
	sc := &console.Scope{Key: firestoreDatabaseKey, Label: "Database", Selected: firestoreDatabaseIn(ctx),
		Default: firestoreDefaultDatabase, Options: []console.ScopeOption{{ID: firestoreDefaultDatabase}}}
	c, err := admin.NewFirestoreAdminClient(ctx, localOpts(p.endpoint)...)
	if err != nil {
		return sc
	}
	defer c.Close()
	resp, err := c.ListDatabases(ctx, &adminpb.ListDatabasesRequest{Parent: "projects/" + project})
	if err != nil {
		return sc
	}
	var ids []string
	for _, db := range resp.GetDatabases() {
		if id := dbID(db.GetName()); id != firestoreDefaultDatabase && db.GetType() != adminpb.Database_DATASTORE_MODE {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		sc.Options = append(sc.Options, console.ScopeOption{ID: id})
	}
	known := false
	for _, o := range sc.Options {
		known = known || o.ID == sc.Selected
	}
	if !known {
		sc.Options = append(sc.Options, console.ScopeOption{ID: sc.Selected, Label: sc.Selected + " (not found)"})
	}
	return sc
}

// deleteCollection is Google's Delete collection: every document in the
// collection, a document that does not exist but has subcollections
// included, and every document of their subcollections, depth first.
func (p firestoreProvider) deleteCollection(ctx context.Context, project, path string) error {
	if project == "" {
		return errors.New("choose a project first")
	}
	ctx, cancel := context.WithTimeout(ctx, 4*dbTimeout)
	defer cancel()
	c, err := p.client(ctx, project)
	if err != nil {
		return err
	}
	defer c.Close()
	col, err := firestoreCollection(c, path)
	if err != nil {
		return err
	}
	return deleteCollectionTree(ctx, col)
}

func deleteCollectionTree(ctx context.Context, col *firestore.CollectionRef) error {
	refs, err := col.DocumentRefs(ctx).GetAll()
	if err != nil {
		return fmt.Errorf("listing %s: %w", col.Path, err)
	}
	for _, ref := range refs {
		subs, err := ref.Collections(ctx).GetAll()
		if err != nil {
			return fmt.Errorf("listing the subcollections of %s: %w", ref.ID, err)
		}
		for _, sub := range subs {
			if err := deleteCollectionTree(ctx, sub); err != nil {
				return err
			}
		}
		if _, err := ref.Delete(ctx); err != nil {
			return fmt.Errorf("deleting %s: %w", ref.ID, err)
		}
	}
	return nil
}

// firestoreTypes are the types Google's Add field offers, in its order.
var firestoreTypes = []string{"string", "number", "boolean", "map", "array", "null", "timestamp", "geopoint", "reference", "bytes"}

// firestoreValueFields are a Firestore field's inputs: its name, the type
// picker (a dropdown, as Google's Add field has) and the value.
func firestoreValueFields(requireField bool, typ, value string) []console.Field {
	fields := valueFields("field", firestoreTypeList, firestoreValueHelp, firestoreTypePattern, requireField, typ, value)
	for i := range fields {
		if fields[i].Name == "type" {
			fields[i].Type = "select"
			fields[i].Options = firestoreTypes
			fields[i].Help = ""
		}
	}
	return fields
}

// deleteFields is Google's Delete document fields: the document is written
// with no fields, so it and its subcollections stay.
func (p firestoreProvider) deleteFields(ctx context.Context, project, collection, id string) error {
	if project == "" {
		return errors.New("choose a project first")
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx, project)
	if err != nil {
		return err
	}
	defer c.Close()
	col, err := firestoreCollection(c, collection)
	if err != nil {
		return err
	}
	doc := col.Doc(id)
	if doc == nil {
		return fmt.Errorf("%q is not a document ID: an ID has no slash", id)
	}
	if _, err := doc.Get(ctx); err != nil {
		return err
	}
	_, err = doc.Set(ctx, map[string]any{})
	return err
}
