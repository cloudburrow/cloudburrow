package main

import (
	"context"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// TestConsoleLogViewPermissions (#1519): each view's row, _AllLogs' too,
// has Permissions; saving it replaces the view's policy, which the
// IAMPolicy service then reads back, and the form opens filled with it; a
// line that is not a role and its principals is refused.
func TestConsoleLogViewPermissions(t *testing.T) {
	_, svc := loggingConsole(t)
	ctx := context.Background()
	const project = "proj-1"
	p := loggingBucketsProvider{loggingAdminScreen{svc: svc}}
	bucket := "projects/" + project + "/locations/global/buckets/_Default"
	view := bucket + "/views/_AllLogs"
	path := []string{bucket, "_AllLogs"}
	found := false
	for _, a := range p.DetailActions(ctx, project, path) {
		found = found || a.ID == viewPermissions
	}
	if !found {
		t.Fatal("no Permissions on _AllLogs")
	}
	if err := p.ActAt(ctx, project, path, viewPermissions, map[string]string{"bindings": "viewer user:a@example.com"}); err == nil {
		t.Error("a line without a role was accepted")
	}
	if err := p.ActAt(ctx, project, path, viewPermissions, map[string]string{
		"bindings": "roles/logging.viewAccessor user:a@example.com, group:g@example.com"}); err != nil {
		t.Fatal(err)
	}
	pol, err := svc.admin.ViewIAM().GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: view})
	if err != nil || len(pol.GetBindings()) != 1 || len(pol.GetBindings()[0].GetMembers()) != 2 {
		t.Fatalf("GetIamPolicy = %v, %v", pol, err)
	}
	for _, a := range p.DetailActions(ctx, project, path) {
		if a.ID == viewPermissions && a.Fields[0].Default != "roles/logging.viewAccessor group:g@example.com, user:a@example.com" {
			t.Errorf("the form opens with %q", a.Fields[0].Default)
		}
	}
	if err := p.ActAt(ctx, project, path, viewPermissions, map[string]string{"bindings": ""}); err != nil {
		t.Fatal(err)
	}
	if pol, _ := svc.admin.ViewIAM().GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: view}); len(pol.GetBindings()) != 0 {
		t.Errorf("after clearing, bindings = %v", pol.GetBindings())
	}
}

// TestConsoleLogViewPermissionsTab (#1748): a view's page has the
// Permissions tab, with conditions, and a bucket's has none; a conditional
// binding granted there is read back at version 3, and the row's text form
// leaves it out and keeps it when it saves the others.
func TestConsoleLogViewPermissionsTab(t *testing.T) {
	_, svc := loggingConsole(t)
	ctx := context.Background()
	const project = "proj-1"
	p := loggingBucketsProvider{loggingAdminScreen{svc: svc}}
	bucket := "projects/" + project + "/locations/global/buckets/_Default"
	view := bucket + "/views/_AllLogs"
	path := []string{bucket, "_AllLogs"}
	if p.PolicyOn(ctx, []string{bucket}) != nil {
		t.Error("a bucket's page has a Permissions tab")
	}
	if tgt := p.PolicyOn(ctx, path); tgt == nil || !tgt.Conditions {
		t.Fatalf("a view's tab = %+v; want one with conditions", tgt)
	}
	if _, err := p.GetPolicy(ctx, "other", path); err == nil {
		t.Error("another project's bucket was read")
	}
	cur, err := p.GetPolicy(ctx, project, path)
	if err != nil {
		t.Fatal(err)
	}
	cond := &console.Condition{Title: "until 2099", Expression: `request.time < timestamp("2099-01-01T00:00:00Z")`}
	cur.Bindings = append(cur.Bindings, console.Binding{Role: "roles/logging.viewAccessor", Members: []string{"user:a@example.com"}, Conditional: cond})
	if err := p.SetPolicy(ctx, project, path, cur); err != nil {
		t.Fatal(err)
	}
	got, err := p.GetPolicy(ctx, project, path)
	if err != nil || len(got.Bindings) != 1 || got.Bindings[0].Conditional == nil || got.Bindings[0].Conditional.Title != "until 2099" {
		t.Fatalf("GetPolicy after a conditional grant = %+v, %v", got, err)
	}
	for _, a := range p.DetailActions(ctx, project, path) {
		if a.ID == viewPermissions && a.Fields[0].Default != "" {
			t.Errorf("the text form opens with the conditional binding: %q", a.Fields[0].Default)
		}
	}
	if err := p.ActAt(ctx, project, path, viewPermissions, map[string]string{"bindings": "roles/logging.viewAccessor group:g@example.com"}); err != nil {
		t.Fatal(err)
	}
	pol, err := svc.admin.ViewIAM().GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: view, Options: &iampb.GetPolicyOptions{RequestedPolicyVersion: 3}})
	if err != nil || pol.GetVersion() != 3 || len(pol.GetBindings()) != 2 {
		t.Fatalf("after the text form GetIamPolicy = %v, %v; want version 3 with both bindings", pol, err)
	}
}
