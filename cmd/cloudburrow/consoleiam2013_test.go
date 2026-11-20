package main

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/iamroles"
)

// TestConsoleIAMRolePickerProducts (#2013): Select a role's roles carry
// their group: Basic for a basic role, Custom for a custom role, and the
// product Google's role index lists a predefined role under.
func TestConsoleIAMRolePickerProducts(t *testing.T) {
	reg := iamPageRegistry(t)
	ctx := context.Background()
	if _, err := (iamRolesProvider{registry: reg}).Create(ctx, "iam-page", map[string]string{"title": "Mine", "roleId": "mine", "stage": "GA",
		"permissions": "storage.objects.get"}); err != nil {
		t.Fatal(err)
	}
	out, err := projectIAMProvider{registry: reg}.StudioCall(ctx, "iam-page", "roles", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"roles/viewer": iamroles.BasicProduct, "projects/iam-page/roles/mine": customRoleProduct,
		"roles/storage.objectViewer": "Cloud Storage", "roles/pubsub.viewer": "Pub/Sub", "roles/compute.admin": "Compute Engine"}
	for _, r := range out.([]iamPageRole) {
		if r.Product == "" {
			t.Errorf("%s has no product", r.Name)
		}
		if w, ok := want[r.Name]; ok {
			if r.Product != w {
				t.Errorf("%s's product is %q, want %q", r.Name, r.Product, w)
			}
			delete(want, r.Name)
		}
	}
	if len(want) > 0 {
		t.Errorf("the picker lacks %v", want)
	}
}

// TestConsoleOrgPoliciesPolicySource (#2013): the list's Policy source is
// which of Google's three sources the project's own policy is.
func TestConsoleOrgPoliciesPolicySource(t *testing.T) {
	reg := iamPageRegistry(t)
	p := iamOrgPoliciesProvider{registry: reg}
	ctx := context.Background()
	const c = "constraints/gcp.resourceLocations"
	source := func() string {
		l, err := p.List(ctx, "iam-page")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(l.Columns, ",") != "Name,Policy source,Effective,Last edited" || l.NameColumn != "Constraint" {
			t.Errorf("the columns are %s / %v", l.NameColumn, l.Columns)
		}
		for _, r := range l.Items {
			if r.Name == c {
				return r.Fields["Policy source"]
			}
		}
		t.Fatalf("%s is not listed", c)
		return ""
	}
	if got := source(); got != "Inherit parent's policy" {
		t.Errorf("an unset constraint's source is %q", got)
	}
	if _, err := orgPolicyEditCall(t, p, "policy-set", orgPolicyEditChange{Constraint: c,
		orgPolicyEditSpec: orgPolicyEditSpec{Source: "override", Rules: []orgPolicyEditRule{{Values: "denyAll"}}}}); err != nil {
		t.Fatal(err)
	}
	if got := source(); got != "Override parent's policy" {
		t.Errorf("an overriding policy's source is %q", got)
	}
	v := orgPolicyEditRead(t, p, c)
	if _, err := orgPolicyEditCall(t, p, "policy-set", orgPolicyEditChange{Constraint: c, Etag: v.Etag,
		orgPolicyEditSpec: orgPolicyEditSpec{Source: "default"}}); err != nil {
		t.Fatal(err)
	}
	if got := source(); got != "Google-managed default" {
		t.Errorf("a reset policy's source is %q", got)
	}
}

// TestConsoleAuditLogsPanel (#2013): the Audit Logs actions are Google's
// information panel: Log Types and Exempted Principals tabs, Save, the
// selection's services carried hidden.
func TestConsoleAuditLogsPanel(t *testing.T) {
	reg := iamPageRegistry(t)
	p := iamAuditProvider{registry: reg}
	acts := p.ListActions(context.Background(), "iam-page")
	if len(acts) != 2 {
		t.Fatalf("ListActions = %v", acts)
	}
	for _, a := range acts {
		if !a.Panel || a.Submit != "Save" {
			t.Errorf("%s is not a panel ending in Save: %+v", a.Label, a)
		}
		var tabs []string
		for _, f := range a.Fields {
			if f.Section != "" && (len(tabs) == 0 || tabs[len(tabs)-1] != f.Section) {
				tabs = append(tabs, f.Section)
			}
			if f.Name == "services" && f.Type != "hidden" {
				t.Errorf("%s's services field is %+v", a.Label, f)
			}
		}
		if strings.Join(tabs, ",") != "Log Types,Exempted Principals" {
			t.Errorf("%s's tabs are %v", a.Label, tabs)
		}
	}
	if _, err := auditLogConfigsOf(map[string]string{"exempted": `[{"dataRead":"true"}]`}); err == nil {
		t.Error("an exemption naming no principal was accepted")
	}
	got, err := auditLogConfigsOf(map[string]string{"adminRead": "true",
		"exempted": `[{"principal":"user:a@example.com","dataRead":"true","dataWrite":"true"}]`})
	if err != nil || len(got) != 3 || len(got[0].GetExemptedMembers()) != 0 || got[1].GetExemptedMembers()[0] != "user:a@example.com" {
		t.Errorf("auditLogConfigsOf = %v, %v", got, err)
	}
}
