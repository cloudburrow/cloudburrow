package main

import (
	"context"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
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
