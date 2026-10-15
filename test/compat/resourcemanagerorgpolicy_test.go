//go:build compat

package compat

import (
	"errors"
	"net/http"
	"testing"

	crmv1 "google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/api/googleapi"
)

// TestResourceManagerV1OrgPolicy (#1296; v1 has no coverage page, as
// TestResourceManagerV1Projects), through google.golang.org/api/
// cloudresourcemanager/v1, as Terraform's google_project_organization_policy
// calls it: a project lists the available constraints; an unset policy
// reads back with only its constraint and an etag; a boolean and a list
// policy are set, read, listed and read as effective; a stale etag is
// ABORTED, a list policy on a boolean constraint INVALID_ARGUMENT; clearing
// a policy unsets it.
func TestResourceManagerV1OrgPolicy(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	c := rmV1(t, h)
	id := v1ID(h, "op-")
	if _, err := c.Projects.Create(&crmv1.Project{ProjectId: id, Name: "Org policy"}).Context(ctx).Do(); err != nil {
		t.Fatalf("v1 projects.create: %v", err)
	}
	t.Cleanup(func() { _, _ = c.Projects.Delete(id).Do() })

	cons, err := c.Projects.ListAvailableOrgPolicyConstraints("projects/"+id, &crmv1.ListAvailableOrgPolicyConstraintsRequest{}).Context(ctx).Do()
	if err != nil || len(cons.Constraints) == 0 {
		t.Fatalf("listAvailableOrgPolicyConstraints = %v, %v", cons, err)
	}
	const boolC, listC = "constraints/iam.disableServiceAccountKeyCreation", "constraints/gcp.resourceLocations"
	unset, err := c.Projects.GetOrgPolicy("projects/"+id, &crmv1.GetOrgPolicyRequest{Constraint: boolC}).Context(ctx).Do()
	if err != nil || unset.Constraint != boolC || unset.Etag == "" || unset.BooleanPolicy != nil {
		t.Fatalf("getOrgPolicy of an unset policy = %+v, %v", unset, err)
	}
	set, err := c.Projects.SetOrgPolicy("projects/"+id, &crmv1.SetOrgPolicyRequest{Policy: &crmv1.OrgPolicy{
		Constraint: boolC, BooleanPolicy: &crmv1.BooleanPolicy{Enforced: true}, Etag: unset.Etag}}).Context(ctx).Do()
	if err != nil || !set.BooleanPolicy.Enforced || set.Etag == "" || set.Etag == unset.Etag || set.UpdateTime == "" {
		t.Fatalf("setOrgPolicy = %+v, %v", set, err)
	}
	var ge *googleapi.Error
	if _, err := c.Projects.SetOrgPolicy("projects/"+id, &crmv1.SetOrgPolicyRequest{Policy: &crmv1.OrgPolicy{
		Constraint: boolC, BooleanPolicy: &crmv1.BooleanPolicy{}, Etag: unset.Etag}}).Context(ctx).Do(); !errors.As(err, &ge) || ge.Code != http.StatusConflict {
		t.Errorf("setOrgPolicy with a stale etag = %v; want 409 ABORTED", err)
	}
	if _, err := c.Projects.SetOrgPolicy("projects/"+id, &crmv1.SetOrgPolicyRequest{Policy: &crmv1.OrgPolicy{
		Constraint: boolC, ListPolicy: &crmv1.ListPolicy{AllValues: "DENY"}}}).Context(ctx).Do(); !errors.As(err, &ge) || ge.Code != http.StatusBadRequest {
		t.Errorf("setOrgPolicy of a list policy on a boolean constraint = %v; want 400", err)
	}
	if _, err := c.Projects.SetOrgPolicy("projects/"+id, &crmv1.SetOrgPolicyRequest{Policy: &crmv1.OrgPolicy{
		Constraint: listC, ListPolicy: &crmv1.ListPolicy{AllowedValues: []string{"in:eu-locations"}}}}).Context(ctx).Do(); err != nil {
		t.Fatalf("setOrgPolicy of a list policy: %v", err)
	}
	got, err := c.Projects.GetOrgPolicy("projects/"+id, &crmv1.GetOrgPolicyRequest{Constraint: listC}).Context(ctx).Do()
	if err != nil || got.ListPolicy == nil || len(got.ListPolicy.AllowedValues) != 1 {
		t.Errorf("getOrgPolicy = %+v, %v", got, err)
	}
	if eff, err := c.Projects.GetEffectiveOrgPolicy("projects/"+id, &crmv1.GetEffectiveOrgPolicyRequest{Constraint: boolC}).Context(ctx).Do(); err != nil || eff.BooleanPolicy == nil || !eff.BooleanPolicy.Enforced {
		t.Errorf("getEffectiveOrgPolicy = %+v, %v", eff, err)
	}
	list, err := c.Projects.ListOrgPolicies("projects/"+id, &crmv1.ListOrgPoliciesRequest{}).Context(ctx).Do()
	if err != nil || len(list.Policies) != 2 || list.Policies[0].Constraint != listC || list.Policies[1].Constraint != boolC {
		t.Errorf("listOrgPolicies = %+v, %v", list, err)
	}
	if _, err := c.Projects.ClearOrgPolicy("projects/"+id, &crmv1.ClearOrgPolicyRequest{Constraint: boolC}).Context(ctx).Do(); err != nil {
		t.Fatalf("clearOrgPolicy: %v", err)
	}
	if p, err := c.Projects.GetOrgPolicy("projects/"+id, &crmv1.GetOrgPolicyRequest{Constraint: boolC}).Context(ctx).Do(); err != nil || p.BooleanPolicy != nil {
		t.Errorf("getOrgPolicy after clearOrgPolicy = %+v, %v", p, err)
	}
	if _, err := c.Projects.GetOrgPolicy("projects/no-such-project-x", &crmv1.GetOrgPolicyRequest{Constraint: boolC}).Context(ctx).Do(); !errors.As(err, &ge) || ge.Code != http.StatusNotFound && ge.Code != http.StatusForbidden {
		t.Errorf("getOrgPolicy of a missing project = %v; want 404 or 403", err)
	}
}
