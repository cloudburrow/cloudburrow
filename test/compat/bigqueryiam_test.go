//go:build compat

package compat

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/iam"
	"cloud.google.com/go/iam/apiv1/iampb"
	bq "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/googleapi"
)

// BigQuery IAM policies (#1231): stored and returned, never enforced, under
// ADR-0006 as amended by #1231.

// wantCode fails unless err is a googleapi error of code whose message
// contains part.
func wantCode(t *testing.T, what string, err error, code int, part string) {
	t.Helper()
	var e *googleapi.Error
	if !errors.As(err, &e) || e.Code != code || !strings.Contains(e.Message, part) {
		t.Errorf("%s returned %v, want %d naming %q", what, err, code, part)
	}
}

// TestBigQueryTableIAMIsStoredNotEnforced sets, reads and tests a table's
// and a view's policy through the official Go client's Table.IAM handle.
// A binding set reads back with a new etag; a stale etag is refused 409
// (ABORTED; BigQuery's code is UNVERIFIED); a condition is 501 naming the
// field; testIamPermissions returns every permission asked. Nothing is
// enforced: a policy naming someone else does not stop the caller's query.
// The policy goes with the table: one made again has none.
//
// covers: bigquery.tables.getIamPolicy, bigquery.tables.setIamPolicy, bigquery.tables.testIamPermissions
func TestBigQueryTableIAMIsStoredNotEnforced(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	svc := bigqueryService(t, h)
	ds, tbl := seedOrders(t, h, c)
	ctx := h.Context()

	view := ds.Table("orders_view")
	if err := view.Create(ctx, &bigquery.TableMetadata{ViewQuery: "SELECT id FROM `" + project + "." + ds.DatasetID + ".orders`"}); err != nil {
		t.Fatalf("create view: %v", err)
	}
	for _, target := range []*bigquery.Table{tbl, view} {
		handle := target.IAM()
		empty, err := handle.Policy(ctx)
		if err != nil {
			t.Fatalf("%s: Policy: %v", target.TableID, err)
		}
		if len(empty.Roles()) != 0 {
			t.Errorf("%s: a new table's policy has roles %v", target.TableID, empty.Roles())
		}
		stale := &iam.Policy{InternalProto: &iampb.Policy{Etag: empty.InternalProto.GetEtag()}}
		empty.Add("user:ada@example.com", "roles/bigquery.dataViewer")
		if err := handle.SetPolicy(ctx, empty); err != nil {
			t.Fatalf("%s: SetPolicy: %v", target.TableID, err)
		}
		got, err := handle.Policy(ctx)
		if err != nil {
			t.Fatalf("%s: Policy: %v", target.TableID, err)
		}
		if !got.HasRole("user:ada@example.com", "roles/bigquery.dataViewer") || len(got.Roles()) != 1 {
			t.Errorf("%s: read back %v", target.TableID, got.InternalProto)
		}
		if string(got.InternalProto.GetEtag()) == string(stale.InternalProto.GetEtag()) {
			t.Errorf("%s: the etag did not change", target.TableID)
		}
		// BigQuery's HTTP code for a stale etag is unmeasured; 409 is ABORTED's.
		stale.Add("user:bob@example.com", "roles/bigquery.dataOwner")
		wantCode(t, target.TableID+": a stale etag", handle.SetPolicy(ctx, stale), http.StatusConflict, "etag")
		// The Go client's handle drops a binding's condition, so a
		// condition is sent through the generated client.
		_, err = svc.Tables.SetIamPolicy("projects/"+project+"/datasets/"+ds.DatasetID+"/tables/"+target.TableID, &bq.SetIamPolicyRequest{Policy: &bq.Policy{
			Version: 3, Bindings: []*bq.Binding{{Role: "roles/bigquery.dataViewer", Members: []string{"user:ada@example.com"}, Condition: &bq.Expr{Expression: "true"}}},
		}}).Context(ctx).Do()
		wantCode(t, target.TableID+": a condition", err, http.StatusNotImplemented, "condition")
		perms, err := handle.TestPermissions(ctx, []string{"bigquery.tables.get", "bigquery.tables.getData"})
		if err != nil || strings.Join(perms, ",") != "bigquery.tables.get,bigquery.tables.getData" {
			t.Errorf("%s: TestPermissions = %v, %v; want both", target.TableID, perms, err)
		}
	}

	// Not enforced: the caller is not user:ada, and still reads every row.
	it, err := c.Query("SELECT COUNT(*) AS n FROM `" + project + "." + ds.DatasetID + ".orders`").Read(ctx)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	var row struct {
		N int64 `bigquery:"n"`
	}
	if err := it.Next(&row); err != nil || row.N != 4 {
		t.Errorf("rows = %d, %v; want 4", row.N, err)
	}

	// The policy goes with the table.
	if err := tbl.Delete(ctx); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{{Name: "id", Type: bigquery.IntegerFieldType}}}); err != nil {
		t.Fatalf("create again: %v", err)
	}
	if again, err := tbl.IAM().Policy(ctx); err != nil || len(again.Roles()) != 0 {
		t.Errorf("a table made again has %v, %v; want no roles", again, err)
	}
	// An empty set on a missing table, as iam_policy.proto documents.
	if perms, err := ds.Table("missing").IAM().TestPermissions(ctx, []string{"bigquery.tables.get"}); err != nil || len(perms) != 0 {
		t.Errorf("TestPermissions of a missing table = %v, %v; want none", perms, err)
	}
}

// TestBigQueryRoutineIAMIsStoredNotEnforced sets, reads and tests a
// routine's policy through the official generated client, which is where
// the Go client has no routine IAM call. The routine still runs whatever
// the policy says, and one made again starts with none.
//
// covers: bigquery.routines.getIamPolicy, bigquery.routines.setIamPolicy, bigquery.routines.testIamPermissions
func TestBigQueryRoutineIAMIsStoredNotEnforced(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	svc := bigqueryService(t, h)
	ds, _ := seedOrders(t, h, c)
	ctx := h.Context()
	fn := "`" + project + "." + ds.DatasetID + ".plus_one`"
	if _, err := c.Query("CREATE FUNCTION " + fn + "(x INT64) AS (x + 1)").Read(ctx); err != nil {
		t.Fatalf("CREATE FUNCTION: %v", err)
	}
	resource := "projects/" + project + "/datasets/" + ds.DatasetID + "/routines/plus_one"

	empty, err := svc.Routines.GetIamPolicy(resource, &bq.GetIamPolicyRequest{Options: &bq.GetPolicyOptions{RequestedPolicyVersion: 3}}).Context(ctx).Do()
	if err != nil || len(empty.Bindings) != 0 || empty.Etag == "" {
		t.Fatalf("getIamPolicy of a new routine = %+v, %v", empty, err)
	}
	set, err := svc.Routines.SetIamPolicy(resource, &bq.SetIamPolicyRequest{Policy: &bq.Policy{
		Etag: empty.Etag, Version: 3, Bindings: []*bq.Binding{{Role: "roles/bigquery.dataViewer", Members: []string{"group:eng@example.com"}}},
	}}).Context(ctx).Do()
	if err != nil || set.Etag == empty.Etag || len(set.Bindings) != 1 {
		t.Fatalf("setIamPolicy = %+v, %v", set, err)
	}
	got, err := svc.Routines.GetIamPolicy(resource, &bq.GetIamPolicyRequest{}).Context(ctx).Do()
	if err != nil || got.Etag != set.Etag || len(got.Bindings) != 1 || got.Bindings[0].Members[0] != "group:eng@example.com" {
		t.Errorf("read back %+v, %v", got, err)
	}
	_, err = svc.Routines.SetIamPolicy(resource, &bq.SetIamPolicyRequest{Policy: &bq.Policy{Etag: empty.Etag}}).Context(ctx).Do()
	wantCode(t, "a stale etag", err, http.StatusConflict, "etag")
	perms, err := svc.Routines.TestIamPermissions(resource, &bq.TestIamPermissionsRequest{Permissions: []string{"bigquery.routines.get", "bigquery.routines.update"}}).Context(ctx).Do()
	if err != nil || len(perms.Permissions) != 2 {
		t.Errorf("testIamPermissions = %+v, %v; want both", perms, err)
	}

	// Not enforced: the function still runs for the caller.
	it, err := c.Query("SELECT " + fn + "(41) AS n").Read(ctx)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var row struct {
		N int64 `bigquery:"n"`
	}
	if err := it.Next(&row); err != nil || row.N != 42 {
		t.Errorf("plus_one(41) = %d, %v", row.N, err)
	}

	// It goes with the routine.
	if err := svc.Routines.Delete(project, ds.DatasetID, "plus_one").Context(ctx).Do(); err != nil {
		t.Fatalf("routines.delete: %v", err)
	}
	if _, err := svc.Routines.GetIamPolicy(resource, &bq.GetIamPolicyRequest{}).Context(ctx).Do(); !isCode(err, http.StatusNotFound) {
		t.Errorf("getIamPolicy of a deleted routine = %v, want 404", err)
	}
	if _, err := c.Query("CREATE FUNCTION " + fn + "(x INT64) AS (x + 1)").Read(ctx); err != nil {
		t.Fatalf("CREATE FUNCTION again: %v", err)
	}
	if again, err := svc.Routines.GetIamPolicy(resource, &bq.GetIamPolicyRequest{}).Context(ctx).Do(); err != nil || len(again.Bindings) != 0 {
		t.Errorf("a routine made again has %+v, %v; want no bindings", again, err)
	}
}

// TestBigQueryRowAccessPolicyIAMIsItsGrantees reads a row access policy's
// IAM policy through the official generated client: its grantees, holding
// roles/bigquery.filteredDataViewer (RowAccessPolicy.grantees names them
// the policy's members; UNVERIFIED against Google). The API has no
// rowAccessPolicies.setIamPolicy; update changes the grantees.
//
// covers: bigquery.rowAccessPolicies.getIamPolicy, bigquery.rowAccessPolicies.testIamPermissions
func TestBigQueryRowAccessPolicyIAMIsItsGrantees(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	svc := bigqueryService(t, h)
	ds, tbl := seedOrders(t, h, c)
	ctx := h.Context()
	d, tb := ds.DatasetID, tbl.TableID
	pol := &bq.RowAccessPolicy{
		RowAccessPolicyReference: &bq.RowAccessPolicyReference{ProjectId: project, DatasetId: d, TableId: tb, PolicyId: "eu"},
		FilterPredicate:          "region = 'eu'",
		Grantees:                 []string{"allUsers", "user:ada@example.com"},
	}
	if _, err := svc.RowAccessPolicies.Insert(project, d, tb, pol).Context(ctx).Do(); err != nil {
		t.Fatalf("rowAccessPolicies.insert: %v", err)
	}
	resource := "projects/" + project + "/datasets/" + d + "/tables/" + tb + "/rowAccessPolicies/eu"
	got, err := svc.RowAccessPolicies.GetIamPolicy(resource, &bq.GetIamPolicyRequest{}).Context(ctx).Do()
	if err != nil || len(got.Bindings) != 1 || got.Bindings[0].Role != "roles/bigquery.filteredDataViewer" ||
		strings.Join(got.Bindings[0].Members, ",") != "allUsers,user:ada@example.com" {
		t.Fatalf("getIamPolicy = %+v, %v", got, err)
	}
	pol.Grantees = []string{"user:bob@example.com"}
	if _, err := svc.RowAccessPolicies.Update(project, d, tb, "eu", pol).Context(ctx).Do(); err != nil {
		t.Fatalf("rowAccessPolicies.update: %v", err)
	}
	if got, err = svc.RowAccessPolicies.GetIamPolicy(resource, &bq.GetIamPolicyRequest{}).Context(ctx).Do(); err != nil ||
		len(got.Bindings) != 1 || strings.Join(got.Bindings[0].Members, ",") != "user:bob@example.com" {
		t.Errorf("after update = %+v, %v", got, err)
	}
	perms, err := svc.RowAccessPolicies.TestIamPermissions(resource, &bq.TestIamPermissionsRequest{
		Permissions: []string{"bigquery.rowAccessPolicies.getFilteredData"},
	}).Context(ctx).Do()
	if err != nil || len(perms.Permissions) != 1 {
		t.Errorf("testIamPermissions = %+v, %v", perms, err)
	}
	if _, err := svc.RowAccessPolicies.GetIamPolicy(resource+"x", &bq.GetIamPolicyRequest{}).Context(ctx).Do(); !isCode(err, http.StatusNotFound) {
		t.Errorf("getIamPolicy of a missing policy = %v, want 404", err)
	}
}
