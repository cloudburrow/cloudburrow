package iampolicy

import (
	"bytes"
	"strings"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

func code(err error) codes.Code { return status.Code(apierror.Wrap(err)) }

func set(t *testing.T, current *Stored, p *iampb.Policy) (Stored, error) {
	t.Helper()
	return Set(current, &iampb.SetIamPolicyRequest{Policy: p})
}

// A resource with no policy answers the empty policy, and its etag is the
// one a first SetIamPolicy may carry.
func TestTheEmptyPolicyHasAnEtagASetMayCarry(t *testing.T) {
	empty, err := Get(nil, nil)
	if err != nil || empty.GetVersion() != 1 || len(empty.GetBindings()) != 0 || len(empty.GetEtag()) == 0 {
		t.Fatalf("empty policy = %v, %v", empty, err)
	}
	if _, err := set(t, nil, &iampb.Policy{Etag: empty.GetEtag(), Bindings: []*iampb.Binding{{Role: "roles/viewer", Members: []string{"user:a@example.com"}}}}); err != nil {
		t.Errorf("a set carrying the empty policy's etag: %v", err)
	}
}

// A set returns the bindings with a new etag; a set carrying the old etag
// afterwards is ABORTED, and one carrying no etag overwrites.
func TestSetChangesTheEtagAndAStaleOneIsAborted(t *testing.T) {
	first, err := set(t, nil, &iampb.Policy{Bindings: []*iampb.Binding{{Role: "roles/viewer", Members: []string{"user:a@example.com"}}}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := set(t, &first, &iampb.Policy{Etag: first.Etag, Bindings: []*iampb.Binding{{Role: "roles/editor", Members: []string{"user:b@example.com"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first.Etag, second.Etag) {
		t.Error("the etag did not change on set")
	}
	if got := second.Proto().GetBindings(); len(got) != 1 || got[0].GetRole() != "roles/editor" {
		t.Errorf("bindings = %v", got)
	}
	if _, err := set(t, &second, &iampb.Policy{Etag: first.Etag}); code(err) != codes.Aborted {
		t.Errorf("a stale etag returned %v, want ABORTED", err)
	}
	if _, err := set(t, &second, &iampb.Policy{}); err != nil {
		t.Errorf("a set with no etag: %v", err)
	}
}

// Conditions and audit configs are refused with the field named, never
// stored and ignored. Version 3 without a condition is accepted: Terraform
// sends it on every *_iam_member.
func TestConditionsAndAuditConfigsAreUnimplemented(t *testing.T) {
	for name, c := range map[string]struct {
		p    *iampb.Policy
		mask []string
		want string
	}{
		"condition": {p: &iampb.Policy{Version: 3, Bindings: []*iampb.Binding{
			{Role: "roles/viewer", Members: []string{"user:a@example.com"}},
			{Role: "roles/viewer", Members: []string{"user:b@example.com"}, Condition: &expr.Expr{Expression: "true"}},
		}}, want: "bindings[1].condition"},
		"audit configs":      {p: &iampb.Policy{AuditConfigs: []*iampb.AuditConfig{{Service: "allServices"}}}, want: "audit_configs"},
		"audit configs mask": {p: &iampb.Policy{}, mask: []string{"audit_configs"}, want: "audit_configs"},
	} {
		req := &iampb.SetIamPolicyRequest{Policy: c.p}
		if c.mask != nil {
			req.UpdateMask = &fieldmaskpb.FieldMask{Paths: c.mask}
		}
		_, err := Set(nil, req)
		if code(err) != codes.Unimplemented || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want UNIMPLEMENTED naming %s", name, err, c.want)
		}
	}
	if _, err := set(t, nil, &iampb.Policy{Version: 3, Bindings: []*iampb.Binding{{Role: "roles/viewer", Members: []string{"user:a@example.com"}}}}); err != nil {
		t.Errorf("version 3 without a condition: %v", err)
	}
	if _, err := set(t, nil, &iampb.Policy{Version: 2}); code(err) != codes.InvalidArgument {
		t.Errorf("version 2: %v, want INVALID_ARGUMENT", err)
	}
}

// TestIamPermissions answers with every requested permission: nothing is
// enforced, so nothing is denied.
func TestEveryRequestedPermissionIsReturned(t *testing.T) {
	want := []string{"secretmanager.secrets.get", "secretmanager.versions.access"}
	got := Permissions(&iampb.TestIamPermissionsRequest{Permissions: want}).GetPermissions()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("permissions = %v, want %v", got, want)
	}
}
