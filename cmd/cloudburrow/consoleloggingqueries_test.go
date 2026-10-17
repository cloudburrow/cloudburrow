package main

import (
	"context"
	"testing"
)

// TestConsoleSavedQueriesAndLogScopes (#1519): Create saved query (a
// Logging query, and a SQL one with a made ID) and Create log scope store
// through the Logging JSON API, the screens list them, a bad one is refused
// with the API's message, and Delete removes them.
func TestConsoleSavedQueriesAndLogScopes(t *testing.T) {
	_, svc := loggingConsole(t)
	ctx := context.Background()
	const project = "proj-1"
	screen := loggingAdminScreen{svc: svc}
	sq := loggingSavedQueriesProvider{screen}
	name, err := sq.Create(ctx, project, map[string]string{"id": "errs", "displayName": "Errors", "location": "global",
		"kind": savedQueryLogging, "query": "severity>=ERROR", "visibility": "SHARED"})
	if err != nil || name != "projects/proj-1/locations/global/savedQueries/errs" {
		t.Fatalf("Create = %q, %v", name, err)
	}
	if _, err := sq.Create(ctx, project, map[string]string{"displayName": "SQL", "location": "us-central1",
		"kind": savedQuerySQL, "query": "SELECT 1", "visibility": "PRIVATE"}); err != nil {
		t.Fatal(err)
	}
	if _, err := sq.Create(ctx, project, map[string]string{"displayName": "x", "location": "global",
		"kind": savedQueryLogging, "query": "a", "visibility": "PUBLIC"}); err == nil {
		t.Error("visibility PUBLIC was accepted")
	}
	l, err := sq.List(ctx, project)
	if err != nil || len(l.Items) != 2 {
		t.Fatalf("List = %+v, %v", l.Items, err)
	}
	if err := sq.Delete(ctx, project, name); err != nil {
		t.Fatal(err)
	}
	if err := sq.Delete(ctx, "other", l.Items[1].Name); err == nil {
		t.Error("deleted another project's saved query")
	}

	ls := loggingLogScopesProvider{screen}
	scope, err := ls.Create(ctx, project, map[string]string{"id": "mine", "resourceNames": "projects/proj-1\nprojects/proj-2", "description": "d"})
	if err != nil || scope != "projects/proj-1/locations/global/logScopes/mine" {
		t.Fatalf("Create = %q, %v", scope, err)
	}
	if _, err := ls.Create(ctx, project, map[string]string{"id": "bad", "resourceNames": "buckets/x"}); err == nil {
		t.Error("a resource that is neither a project nor a view was accepted")
	}
	l, err = ls.List(ctx, project)
	if err != nil || len(l.Items) != 1 || l.Items[0].Fields["Resources"] != "projects/proj-1, projects/proj-2" {
		t.Fatalf("List = %+v, %v", l.Items, err)
	}
	if err := ls.Delete(ctx, project, scope); err != nil {
		t.Fatal(err)
	}
	if l, _ := ls.List(ctx, project); len(l.Items) != 0 {
		t.Errorf("after Delete, %+v", l.Items)
	}
}
