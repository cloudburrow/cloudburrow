package main

import (
	"context"
	"net"
	"strings"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"

	"github.com/cloudburrow/cloudburrow/internal/callfront"
	"github.com/cloudburrow/cloudburrow/internal/calllog"
	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin"
	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin/fstest"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestConsoleFirestorePipelineAndUserCreds (#1500): Create database with
// the Enterprise edition, Run pipeline on its page showing the results,
// and its Security tab (#1845), with Add User showing the
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
	reg := resourcemanager.New(store.NewMemory())
	if _, err := reg.EnsureExists("p"); err != nil {
		t.Fatal(err)
	}
	p := firestoreDatabasesProvider{endpoint: l.Addr().String(), mongo: "127.0.0.1:9026", projects: reg}
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
	// #1523: Output update writes the set field into each document, and
	// delete deletes each one, both showing the count modified.
	res, err = p.ActAtResult(ctx, "p", at, "runpipeline", map[string]string{"source": "collection", "path": "cities",
		"output": "update", "setField": "visited", "setValue": "true"})
	if err != nil || len(res.Items) != 1 || res.Items[0].Fields["Fields"] != "documents_modified: 2" {
		t.Errorf("Run pipeline, update = %+v, %v; want documents_modified: 2", res, err)
	}
	res, err = p.ActAtResult(ctx, "p", at, "runpipeline", map[string]string{"source": "collection", "path": "cities",
		"field": "visited", "op": "==", "value": "true"})
	if err != nil || len(res.Items) != 2 {
		t.Errorf("cities visited after the update = %+v, %v; want 2", res, err)
	}
	res, err = p.ActAtResult(ctx, "p", at, "runpipeline", map[string]string{"source": "collection", "path": "cities",
		"field": "n", "op": "==", "value": "cities/la", "output": "delete"})
	if err != nil || len(res.Items) != 1 || res.Items[0].Fields["Fields"] != "documents_modified: 1" {
		t.Errorf("Run pipeline, delete = %+v, %v; want documents_modified: 1", res, err)
	}
	if _, err := p.ActAtResult(ctx, "p", at, "runpipeline", map[string]string{"source": "collection", "path": "cities", "readTime": "yesterday"}); err == nil {
		t.Error("a read time that is not RFC 3339 was accepted")
	}

	// Google's Add User (#1845, round 3): Username and a role, granted on
	// this database alone.
	add, ok := fsAdminAction(p.DetailActions(ctx, "p", at), "createcreds")
	if !ok || add.Label != "Add User" || add.Submit != "Add user" || len(add.Fields) != 2 || add.Fields[0].Label != "Username" || add.Fields[1].Label != "Role" {
		t.Errorf("Add User = %+v", add)
	}
	if _, err := p.ActAtResult(ctx, "p", at, "createcreds", map[string]string{"userCredsId": "bad-role", "role": "roles/owner"}); err == nil {
		t.Error("Add User took a role it does not offer")
	}
	res, err = p.ActAtResult(ctx, "p", at, "createcreds", map[string]string{"userCredsId": "app-user", "role": "roles/datastore.user"})
	if err != nil || len(res.Items) != 1 || res.Items[0].Fields["Password"] == "" {
		t.Fatalf("Add User = %+v, %v", res, err)
	}
	if got := res.Items[0].Fields["Role"]; got != "Cloud Datastore User" {
		t.Errorf("Add User's dialog Role = %q, want Cloud Datastore User (note %q)", got, res.Note)
	}
	pol, err := resourcemanager.NewProjectsServer(reg).GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: resourcemanager.V3Name("p"),
		Options: &iampb.GetPolicyOptions{RequestedPolicyVersion: 3}})
	if err != nil {
		t.Fatal(err)
	}
	var granted bool
	for _, b := range pol.GetBindings() {
		if b.GetRole() == "roles/datastore.user" && len(b.GetMembers()) == 1 && strings.HasSuffix(b.GetMembers()[0], "/databases/entdb/userCreds/app-user") &&
			b.GetCondition().GetExpression() == `resource.name == "projects/p/databases/entdb"` {
			granted = true
		}
	}
	if !granted {
		t.Errorf("Add User granted no conditional roles/datastore.user binding: %v", pol.GetBindings())
	}
	first := res.Items[0].Fields["Password"]
	// #1703: the connection string a MongoDB driver authenticates with.
	const wantCS = "mongodb://app-user:PASSWORD@127.0.0.1:9026/entdb?loadBalanced=true&authMechanism=SCRAM-SHA-256&tls=true&retryWrites=false"
	if cs := res.Items[0].Fields["Connection string"]; cs != wantCS {
		t.Errorf("Create user credentials' connection string = %q, want %q", cs, wantCS)
	}
	d, err := p.Detail(ctx, "p", at)
	if err != nil {
		t.Fatal(err)
	}
	var shown bool
	for _, prop := range d.Summary {
		if prop.Label == "Connection string" && prop.Value == "mongodb://127.0.0.1:9026/entdb?loadBalanced=true&tls=true&retryWrites=false" {
			shown = true
		}
	}
	if !shown {
		t.Errorf("an Enterprise database's page shows no base connection string: %+v", d.Summary)
	}
	var tab bool
	for _, s := range d.Sections {
		if s.ID == "usercreds" {
			tab = true
			if s.Label != "Security" || len(s.Actions) != 1 || s.Actions[0].Label != "Add User" {
				t.Errorf("the users tab is %q with %+v; want Google's Security with Add User", s.Label, s.Actions)
			}
			if len(s.Listing.Items) != 1 || s.Listing.Items[0].Fields["State"] != "Enabled" || s.Listing.Items[0].Fields["Password"] != "" ||
				s.Listing.Items[0].Fields["Role"] != "Cloud Datastore User" {
				t.Errorf("the Security tab = %+v", s.Listing.Items)
			}
		}
	}
	if !tab {
		t.Fatal("an Enterprise database's page has no Security tab")
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
