//go:build compat

package compat

import (
	"strings"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestQueueIamPolicyIsStoredNotEnforced (#366, ADR-0006) drives the three IAM
// methods on a queue through the official client: a set policy reads back
// with a new etag, a stale etag is ABORTED, a conditional binding is
// UNIMPLEMENTED, and TestIamPermissions returns what was asked. It asserts
// nothing about access: nothing is enforced.
// covers: google.cloud.tasks.v2.CloudTasks/GetIamPolicy, google.cloud.tasks.v2.CloudTasks/SetIamPolicy, google.cloud.tasks.v2.CloudTasks/TestIamPermissions
func TestQueueIamPolicyIsStoredNotEnforced(t *testing.T) {
	h := New(t)
	c := tasksClient(t, h)
	ctx := h.Context()
	name := queue(t, h, c, "compat-iam")

	empty, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
	if err != nil || len(empty.GetBindings()) != 0 {
		t.Fatalf("GetIamPolicy on a new queue = %v, %v; want the empty policy", empty, err)
	}
	member := "serviceAccount:app@" + h.Project() + ".iam.gserviceaccount.com"
	set, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name, Policy: &iampb.Policy{
		Etag:     empty.GetEtag(),
		Bindings: []*iampb.Binding{{Role: "roles/cloudtasks.enqueuer", Members: []string{member}}},
	}})
	if err != nil {
		t.Fatalf("SetIamPolicy: %v", err)
	}
	got, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
	if err != nil {
		t.Fatalf("GetIamPolicy: %v", err)
	}
	if b := got.GetBindings(); len(b) != 1 || b[0].GetRole() != "roles/cloudtasks.enqueuer" || strings.Join(b[0].GetMembers(), ",") != member {
		t.Errorf("bindings read back as %v", b)
	}
	if string(got.GetEtag()) != string(set.GetEtag()) || string(set.GetEtag()) == string(empty.GetEtag()) {
		t.Errorf("etags: empty %x, set %x, read %x; want a new etag that reads back", empty.GetEtag(), set.GetEtag(), got.GetEtag())
	}
	if _, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name, Policy: &iampb.Policy{Etag: empty.GetEtag()}}); status.Code(err) != codes.Aborted {
		t.Errorf("SetIamPolicy with a stale etag: %v, want ABORTED", err)
	}
	_, err = c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name, Policy: &iampb.Policy{Version: 3,
		Bindings: []*iampb.Binding{{Role: "roles/cloudtasks.enqueuer", Members: []string{member},
			Condition: &expr.Expr{Title: "later", Expression: `request.time > timestamp("2030-01-01T00:00:00Z")`}}}}})
	if status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), "condition") {
		t.Errorf("a conditional binding: %v, want UNIMPLEMENTED naming the condition", err)
	}
	perms := []string{"cloudtasks.tasks.create", "cloudtasks.queues.delete"}
	tp, err := c.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: name, Permissions: perms})
	if err != nil || strings.Join(tp.GetPermissions(), ",") != strings.Join(perms, ",") {
		t.Errorf("TestIamPermissions = %v, %v; want every requested permission", tp.GetPermissions(), err)
	}
}
