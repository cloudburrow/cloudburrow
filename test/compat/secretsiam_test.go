//go:build compat

package compat

import (
	"strings"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestSecretIamPolicyIsStoredNotEnforced (#365, ADR-0006) drives the three IAM
// methods through the official client: a set policy reads back, a stale etag
// is ABORTED, a conditional binding is UNIMPLEMENTED, and TestIamPermissions
// returns what was asked. It asserts nothing about access: nothing is
// enforced, which is the point.
// covers: google.cloud.secretmanager.v1.SecretManagerService/GetIamPolicy, google.cloud.secretmanager.v1.SecretManagerService/SetIamPolicy, google.cloud.secretmanager.v1.SecretManagerService/TestIamPermissions
func TestSecretIamPolicyIsStoredNotEnforced(t *testing.T) {
	h := New(t)
	c := secretsClient(t, h)
	ctx := h.Context()
	sec, err := c.CreateSecret(ctx, &secretmanagerpb.CreateSecretRequest{
		Parent: secretsParent(h), SecretId: "compat-iam",
		Secret: &secretmanagerpb.Secret{Replication: &secretmanagerpb.Replication{
			Replication: &secretmanagerpb.Replication_Automatic_{Automatic: &secretmanagerpb.Replication_Automatic{}}}},
	})
	if err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	t.Cleanup(func() { _ = c.DeleteSecret(ctx, &secretmanagerpb.DeleteSecretRequest{Name: sec.Name}) })

	empty, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: sec.Name,
		Options: &iampb.GetPolicyOptions{RequestedPolicyVersion: 3}})
	if err != nil || len(empty.GetBindings()) != 0 {
		t.Fatalf("GetIamPolicy on a new secret = %v, %v; want the empty policy", empty, err)
	}

	member := "serviceAccount:app@" + h.Project() + ".iam.gserviceaccount.com"
	set, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: sec.Name, Policy: &iampb.Policy{
		Etag:     empty.GetEtag(),
		Bindings: []*iampb.Binding{{Role: "roles/secretmanager.secretAccessor", Members: []string{member}}},
	}})
	if err != nil {
		t.Fatalf("SetIamPolicy: %v", err)
	}
	got, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: sec.Name})
	if err != nil {
		t.Fatalf("GetIamPolicy: %v", err)
	}
	if b := got.GetBindings(); len(b) != 1 || b[0].GetRole() != "roles/secretmanager.secretAccessor" ||
		len(b[0].GetMembers()) != 1 || b[0].GetMembers()[0] != member {
		t.Errorf("bindings read back as %v", b)
	}
	if string(got.GetEtag()) != string(set.GetEtag()) || string(set.GetEtag()) == string(empty.GetEtag()) {
		t.Errorf("etags: empty %x, set %x, read %x; want a new etag that reads back", empty.GetEtag(), set.GetEtag(), got.GetEtag())
	}

	_, err = c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: sec.Name, Policy: &iampb.Policy{Etag: empty.GetEtag()}})
	if status.Code(err) != codes.Aborted {
		t.Errorf("SetIamPolicy with a stale etag: %v, want ABORTED", err)
	}
	_, err = c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: sec.Name, Policy: &iampb.Policy{Version: 3,
		Bindings: []*iampb.Binding{{Role: "roles/secretmanager.secretAccessor", Members: []string{member},
			Condition: &expr.Expr{Title: "later", Expression: `request.time > timestamp("2030-01-01T00:00:00Z")`}}}}})
	if status.Code(err) != codes.Unimplemented || !strings.Contains(err.Error(), "condition") {
		t.Errorf("a conditional binding: %v, want UNIMPLEMENTED naming the condition", err)
	}

	perms := []string{"secretmanager.versions.access", "secretmanager.secrets.delete"}
	tp, err := c.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: sec.Name, Permissions: perms})
	if err != nil || strings.Join(tp.GetPermissions(), ",") != strings.Join(perms, ",") {
		t.Errorf("TestIamPermissions = %v, %v; want every requested permission", tp.GetPermissions(), err)
	}
}
