package resourcemanager

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	crmv1 "google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/api/option"
)

// TestV1OrgPolicyOnFoldersAndTheOrganization (#1534): the folder and
// organization org policy methods through the official v1 client, each
// resource's policies kept apart, and getEffectiveOrgPolicy evaluated down
// organization, folder, project.
func TestV1OrgPolicyOnFoldersAndTheOrganization(t *testing.T) {
	reg, srv, _ := startAPI(t)
	ctx := context.Background()
	c, err := crmv1.NewService(ctx, option.WithEndpoint("http://"+srv.Addr()+"/"),
		option.WithoutAuthentication(), option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	f, err := reg.CreateFolder(OrganizationName, "Apps")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Create(Project{ProjectID: "in-apps", Parent: f.Name}); err != nil {
		t.Fatal(err)
	}
	const boolC, listC = "constraints/compute.requireOsLogin", "constraints/gcp.resourceLocations"
	set := func(res string, p *crmv1.OrgPolicy) {
		t.Helper()
		req := &crmv1.SetOrgPolicyRequest{Policy: p}
		var err error
		switch res[:4] {
		case "fold":
			_, err = c.Folders.SetOrgPolicy(res, req).Context(ctx).Do()
		case "orga":
			_, err = c.Organizations.SetOrgPolicy(res, req).Context(ctx).Do()
		default:
			_, err = c.Projects.SetOrgPolicy(res, req).Context(ctx).Do()
		}
		if err != nil {
			t.Fatalf("setOrgPolicy(%s, %s): %v", res, p.Constraint, err)
		}
	}
	effective := func(constraint string) *crmv1.OrgPolicy {
		t.Helper()
		p, err := c.Projects.GetEffectiveOrgPolicy("projects/in-apps", &crmv1.GetEffectiveOrgPolicyRequest{Constraint: constraint}).Context(ctx).Do()
		if err != nil {
			t.Fatalf("getEffectiveOrgPolicy(%s): %v", constraint, err)
		}
		if p.Etag != "" {
			t.Errorf("an effective policy carries etag %q; v1 documents none", p.Etag)
		}
		return p
	}

	// The organization enforces; the project inherits it.
	set(OrganizationName, &crmv1.OrgPolicy{Constraint: boolC, BooleanPolicy: &crmv1.BooleanPolicy{Enforced: true}})
	if p := effective(boolC); p.BooleanPolicy == nil || !p.BooleanPolicy.Enforced {
		t.Errorf("effective %s below an enforcing organization = %+v", boolC, p)
	}
	// The folder turns it off; the project inherits the folder's.
	set(f.Name, &crmv1.OrgPolicy{Constraint: boolC, BooleanPolicy: &crmv1.BooleanPolicy{}})
	if p := effective(boolC); p.BooleanPolicy == nil || p.BooleanPolicy.Enforced {
		t.Errorf("effective %s below a folder that does not enforce = %+v", boolC, p)
	}
	if p, err := c.Folders.GetEffectiveOrgPolicy(f.Name, &crmv1.GetEffectiveOrgPolicyRequest{Constraint: boolC}).Context(ctx).Do(); err != nil || p.BooleanPolicy == nil || p.BooleanPolicy.Enforced {
		t.Errorf("folder effective %s = %+v, %v", boolC, p, err)
	}
	// restoreDefault on the project replaces both.
	set("projects/in-apps", &crmv1.OrgPolicy{Constraint: boolC, RestoreDefault: &crmv1.RestoreDefault{}})
	if p := effective(boolC); p.RestoreDefault == nil || p.BooleanPolicy != nil {
		t.Errorf("effective %s with restoreDefault on the project = %+v", boolC, p)
	}

	// Lists: an inheriting list adds to the one above; one that does not
	// replaces it.
	set(OrganizationName, &crmv1.OrgPolicy{Constraint: listC, ListPolicy: &crmv1.ListPolicy{AllowedValues: []string{"in:eu-locations"}}})
	set(f.Name, &crmv1.OrgPolicy{Constraint: listC, ListPolicy: &crmv1.ListPolicy{AllowedValues: []string{"in:us-locations"}, InheritFromParent: true}})
	if p := effective(listC); p.ListPolicy == nil || !reflect.DeepEqual(p.ListPolicy.AllowedValues, []string{"in:eu-locations", "in:us-locations"}) {
		t.Errorf("effective %s through an inheriting folder = %+v", listC, p.ListPolicy)
	}
	set("projects/in-apps", &crmv1.OrgPolicy{Constraint: listC, ListPolicy: &crmv1.ListPolicy{AllowedValues: []string{"in:asia-locations"}}})
	if p := effective(listC); p.ListPolicy == nil || !reflect.DeepEqual(p.ListPolicy.AllowedValues, []string{"in:asia-locations"}) {
		t.Errorf("effective %s with a project list that does not inherit = %+v", listC, p.ListPolicy)
	}
	// allValues ALLOW above an inheriting deny list: all but the denied.
	got := mergePolicy(&crmv1.OrgPolicy{ListPolicy: &crmv1.ListPolicy{AllValues: "ALLOW"}},
		&crmv1.OrgPolicy{Constraint: listC, ListPolicy: &crmv1.ListPolicy{DeniedValues: []string{"in:us-locations"}, InheritFromParent: true}})
	if got.ListPolicy.AllValues != "" || !reflect.DeepEqual(got.ListPolicy.DeniedValues, []string{"in:us-locations"}) {
		t.Errorf("ALLOW above an inheriting deny list = %+v", got.ListPolicy)
	}

	// Each resource lists its own.
	if l, err := c.Organizations.ListOrgPolicies(OrganizationName, &crmv1.ListOrgPoliciesRequest{}).Context(ctx).Do(); err != nil || len(l.Policies) != 2 {
		t.Errorf("organization listOrgPolicies = %+v, %v", l, err)
	}
	if _, err := c.Folders.ClearOrgPolicy(f.Name, &crmv1.ClearOrgPolicyRequest{Constraint: boolC}).Context(ctx).Do(); err != nil {
		t.Fatal(err)
	}
	if l, err := c.Folders.ListOrgPolicies(f.Name, &crmv1.ListOrgPoliciesRequest{}).Context(ctx).Do(); err != nil || len(l.Policies) != 1 || l.Policies[0].Constraint != listC {
		t.Errorf("folder listOrgPolicies after clear = %+v, %v", l, err)
	}
	if p, err := c.Folders.GetOrgPolicy(f.Name, &crmv1.GetOrgPolicyRequest{Constraint: boolC}).Context(ctx).Do(); err != nil || p.BooleanPolicy != nil || p.Etag == "" {
		t.Errorf("folder getOrgPolicy after clear = %+v, %v; want unset, with an etag", p, err)
	}
	if l, err := c.Folders.ListAvailableOrgPolicyConstraints(f.Name, &crmv1.ListAvailableOrgPolicyConstraintsRequest{}).Context(ctx).Do(); err != nil || len(l.Constraints) != len(Constraints) {
		t.Errorf("folder listAvailableOrgPolicyConstraints = %v, %v", l, err)
	}
	for _, err := range []error{
		func() error {
			_, err := c.Folders.GetOrgPolicy("folders/999999999999", &crmv1.GetOrgPolicyRequest{Constraint: boolC}).Context(ctx).Do()
			return err
		}(),
		func() error {
			_, err := c.Organizations.GetOrgPolicy("organizations/1", &crmv1.GetOrgPolicyRequest{Constraint: boolC}).Context(ctx).Do()
			return err
		}(),
	} {
		if code(err) != http.StatusNotFound {
			t.Errorf("an absent folder's or organization's policy = %v; want 404", err)
		}
	}
}
