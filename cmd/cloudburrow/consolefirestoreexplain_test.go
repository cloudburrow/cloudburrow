package main

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/callfront"
	"github.com/cloudburrow/cloudburrow/internal/calllog"
	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin"
	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin/fstest"
)

// TestConsoleFirestoreTextIndexAndExplain (#1738): on an Enterprise
// database's page, Create index makes a text index (a text field, the
// MongoDB compatible API scope and a language), which the Indexes tab
// shows as such, and Query Explain runs the explain command through the
// front and lists its sections; a Standard database offers no Query
// Explain.
func TestConsoleFirestoreTextIndexAndExplain(t *testing.T) {
	docs, docsAddr, _, _ := fstest.Serve(t)
	a := firestoreadmin.New(docsAddr)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a.NewMongoServer(l.Addr().String(), t.Logf)
	srv := callfront.Server(callfront.HandlerWith(docsAddr, &calllog.Ring{}, t.Logf, a), t.Logf)
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	p := firestoreDatabasesProvider{endpoint: l.Addr().String(), mongo: "127.0.0.1:9026"}
	ctx := context.Background()
	if _, err := p.Create(ctx, "p", map[string]string{"database": "entdb", "location": "nam5", "concurrency": "PESSIMISTIC", "edition": "ENTERPRISE"}); err != nil {
		t.Fatalf("Create (Enterprise): %v", err)
	}
	docs.Seed("projects/p/databases/entdb")
	at := []string{"entdb"}

	if err := p.ActAt(ctx, "p", at, "createindex", map[string]string{"collectionGroup": "books",
		"fields": "title text\nsummary text", "queryScope": "COLLECTION_GROUP", "apiScope": "MONGODB_COMPATIBLE_API", "textLanguage": "fr"}); err != nil {
		t.Fatalf("Create index (text): %v", err)
	}
	d, err := p.Detail(ctx, "p", at)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range d.Sections {
		for _, it := range s.Listing.Items {
			if it.Fields["Collection group"] == "books" && strings.Contains(it.Fields["Fields"], "title Text") &&
				it.Fields["API scope"] == "MongoDB compatible API, text language fr" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("the Indexes tab shows no text index of books: %+v", d.Sections)
	}

	for _, act := range p.DetailActions(ctx, "p", []string{"(default)"}) {
		if act.ID == "explainquery" {
			t.Error("(default), a Standard database, offers Query Explain")
		}
	}
	if _, ok := fsAdminAction(p.DetailActions(ctx, "p", at), "explainquery"); !ok {
		t.Fatal("an Enterprise database's page offers no Query Explain")
	}
	res, err := p.ActAtResult(ctx, "p", at, "explainquery", map[string]string{"collection": "cities", "command": "find",
		"query": `{"n": {"$gte": 1}}`, "sort": `{"n": -1}`, "limit": "5", "verbosity": "executionStats"})
	if err != nil {
		t.Fatalf("Query Explain (find): %v", err)
	}
	sections := map[string]string{}
	for _, it := range res.Items {
		sections[it.Name] = it.Fields["Value"]
	}
	if sections["queryPlanner"] == "" || sections["executionStats"] == "" {
		t.Errorf("Query Explain (find) = %+v", res)
	}
	if res, err := p.ActAtResult(ctx, "p", at, "explainquery", map[string]string{"collection": "cities", "command": "aggregate",
		"query": `[{"$match": {"n": 1}}]`, "verbosity": "queryPlanner"}); err != nil || len(res.Items) == 0 {
		t.Errorf("Query Explain (aggregate) = %+v, %v", res, err)
	}
	if _, err := p.ActAtResult(ctx, "p", at, "explainquery", map[string]string{"collection": "cities", "query": `{"n": `}); err == nil {
		t.Error("Query Explain took a filter that is not JSON")
	}
}
