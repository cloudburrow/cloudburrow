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

// TestConsoleFirestorePipelineAndUserCreds (#1500): Create database with
// the Enterprise edition, Run pipeline on its page showing the results,
// and its User credentials tab, with Create user credentials showing the
// password once and Disable, Reset password and Delete on a row. A
// Standard database's page offers neither.
func TestConsoleFirestorePipelineAndUserCreds(t *testing.T) {
	docs, docsAddr, _, _ := fstest.Serve(t)
	a := firestoreadmin.New(docsAddr)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := callfront.Server(callfront.HandlerWith(docsAddr, &calllog.Ring{}, t.Logf, a), t.Logf)
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	p := firestoreDatabasesProvider{endpoint: l.Addr().String()}
	ctx := context.Background()

	if _, err := p.Create(ctx, "p", map[string]string{"database": "entdb", "location": "nam5", "concurrency": "PESSIMISTIC", "edition": "ENTERPRISE"}); err != nil {
		t.Fatalf("Create (Enterprise): %v", err)
	}
	docs.Seed("projects/p/databases/entdb")
	for _, act := range p.DetailActions(ctx, "p", []string{"(default)"}) {
		if act.ID == "runpipeline" || act.ID == "createcreds" {
			t.Errorf("(default), a Standard database, offers %s", act.Label)
		}
	}
	at := []string{"entdb"}
	if _, ok := fsAdminAction(p.DetailActions(ctx, "p", at), "runpipeline"); !ok {
		t.Fatal("an Enterprise database's page offers no Run pipeline")
	}
	res, err := p.ActAtResult(ctx, "p", at, "runpipeline", map[string]string{"source": "collection", "path": "cities",
		"field": "n", "op": "!=", "value": "cities/la", "limit": "10", "explain": "true"})
	if err != nil {
		t.Fatalf("Run pipeline: %v", err)
	}
	if len(res.Items) != 1 || res.Items[0].Name != "cities/sf" || !strings.Contains(res.Note, "results returned: 1") {
		t.Errorf("Run pipeline = %+v", res)
	}
	res, err = p.ActAtResult(ctx, "p", at, "runpipeline", map[string]string{"source": "collection_group", "path": "kids"})
	if err != nil || len(res.Items) != 1 || res.Items[0].Name != "orphans/missing/kids/k1" {
		t.Errorf("Run pipeline over a collection group = %+v, %v", res, err)
	}

	res, err = p.ActAtResult(ctx, "p", at, "createcreds", map[string]string{"userCredsId": "app-user"})
	if err != nil || len(res.Items) != 1 || res.Items[0].Fields["Password"] == "" {
		t.Fatalf("Create user credentials = %+v, %v", res, err)
	}
	first := res.Items[0].Fields["Password"]
	d, err := p.Detail(ctx, "p", at)
	if err != nil {
		t.Fatal(err)
	}
	var tab bool
	for _, s := range d.Sections {
		if s.ID == "usercreds" {
			tab = true
			if len(s.Listing.Items) != 1 || s.Listing.Items[0].Fields["State"] != "Enabled" || s.Listing.Items[0].Fields["Password"] != "" {
				t.Errorf("the User credentials tab = %+v", s.Listing.Items)
			}
		}
	}
	if !tab {
		t.Fatal("an Enterprise database's page has no User credentials tab")
	}
	row := []string{"entdb", firestoreCredsSegment, "app-user"}
	if _, ok := fsAdminAction(p.DetailActions(ctx, "p", row), "disablecreds"); !ok {
		t.Error("an enabled user's row offers no Disable")
	}
	if _, err := p.ActAtResult(ctx, "p", row, "disablecreds", nil); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if _, ok := fsAdminAction(p.DetailActions(ctx, "p", row), "enablecreds"); !ok {
		t.Error("a disabled user's row offers no Enable")
	}
	res, err = p.ActAtResult(ctx, "p", row, "resetpassword", nil)
	if err != nil || res.Items[0].Fields["Password"] == "" || res.Items[0].Fields["Password"] == first {
		t.Errorf("Reset password = %+v, %v: want a new password", res, err)
	}
	if _, err := p.ActAtResult(ctx, "p", row, "deletecreds", nil); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if acts := p.DetailActions(ctx, "p", row); len(acts) != 0 {
		t.Errorf("a deleted user's row offers %v", acts)
	}
}
