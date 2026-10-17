//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	crmv1 "google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/api/googleapi"
)

// runConstraint is a constraint name unique to this run. The organization
// is one per instance and shared by every test on it, so a test that sets
// a policy there uses a constraint no other test or run sets: any
// constraints/... name is accepted.
func runConstraint(h *Harness, kind string) string {
	return "constraints/custom.cb" + kind + strings.ReplaceAll(strings.TrimPrefix(h.Project(), "cb-test-"), "-", "")
}

// covers: cloudresourcemanager.folders.getOrgPolicy, cloudresourcemanager.folders.getEffectiveOrgPolicy, cloudresourcemanager.folders.setOrgPolicy, cloudresourcemanager.folders.clearOrgPolicy, cloudresourcemanager.folders.listOrgPolicies, cloudresourcemanager.folders.listAvailableOrgPolicyConstraints
// covers: cloudresourcemanager.organizations.getOrgPolicy, cloudresourcemanager.organizations.getEffectiveOrgPolicy, cloudresourcemanager.organizations.setOrgPolicy, cloudresourcemanager.organizations.clearOrgPolicy, cloudresourcemanager.organizations.listOrgPolicies, cloudresourcemanager.organizations.listAvailableOrgPolicyConstraints
// covers: cloudresourcemanager.projects.getEffectiveOrgPolicy, cloudresourcemanager.projects.setOrgPolicy, cloudresourcemanager.projects.getAncestry, cloudresourcemanager.projects.create, cloudresourcemanager.projects.get
//
// TestResourceManagerV1FolderAndOrganizationOrgPolicy (#1534): org policy
// on a folder and the organization through google.golang.org/api/
// cloudresourcemanager/v1, over the hierarchy v3 holds. A folder made by the
// v3 Folders API, and a project the v1 client creates in it, are the same
// folder and project to both (v3 reads the v1 project's parent, v1's
// getAncestry the v3 folder and the organization v3 gets). The
// organization's and the folder's policies are set, read, listed and
// cleared, each resource keeping its own; getEffectiveOrgPolicy evaluates
// them down organization, folder, project: a boolean policy below replaces
// the one above, a list policy with inheritFromParent adds to it, and the
// effective policy carries no etag. An unknown folder or organization is
// NOT_FOUND.
func TestResourceManagerV1FolderAndOrganizationOrgPolicy(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	c := rmV1(t, h)
	fc, oc, pc := foldersClients(t, h)
	boolC, listC := runConstraint(h, "bool"), runConstraint(h, "list")

	org, err := oc.GetOrganization(ctx, &rmpb.GetOrganizationRequest{Name: localOrg})
	if err != nil {
		t.Fatalf("v3 GetOrganization: %v", err)
	}
	if o, err := c.Organizations.Get(localOrg).Context(ctx).Do(); err != nil || o.DisplayName != org.GetDisplayName() || o.LifecycleState != "ACTIVE" {
		t.Errorf("v1 organizations.get = %+v, %v; want the organization v3 reads (%s)", o, err, org.GetDisplayName())
	}
	op, err := fc.CreateFolder(ctx, &rmpb.CreateFolderRequest{Folder: &rmpb.Folder{Parent: localOrg, DisplayName: folderLabel(h, "op-")}})
	if err != nil {
		t.Fatalf("v3 CreateFolder: %v", err)
	}
	folder, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("v3 CreateFolder: %v", err)
	}
	t.Cleanup(func() {
		_, _ = fc.DeleteFolder(context.Background(), &rmpb.DeleteFolderRequest{Name: folder.GetName()})
	})
	folderID := strings.TrimPrefix(folder.GetName(), "folders/")

	id := v1ID(h, "opf-")
	if _, err := c.Projects.Create(&crmv1.Project{ProjectId: id, Name: "In a folder",
		Parent: &crmv1.ResourceId{Type: "folder", Id: folderID}}).Context(ctx).Do(); err != nil {
		t.Fatalf("v1 projects.create in %s: %v", folder.GetName(), err)
	}
	t.Cleanup(func() { _, _ = c.Projects.Delete(id).Do() })
	if p, err := pc.GetProject(ctx, &rmpb.GetProjectRequest{Name: "projects/" + id}); err != nil || p.GetParent() != folder.GetName() {
		t.Errorf("v3 GetProject of the v1 project = %v, %v; want parent %s", p, err, folder.GetName())
	}
	if p, err := c.Projects.Get(id).Context(ctx).Do(); err != nil || p.Parent == nil || p.Parent.Type != "folder" || p.Parent.Id != folderID {
		t.Errorf("v1 projects.get = %+v, %v; want the v3 folder as parent", p, err)
	}
	anc, err := c.Projects.GetAncestry(id, &crmv1.GetAncestryRequest{}).Context(ctx).Do()
	if err != nil || len(anc.Ancestor) != 3 || anc.Ancestor[1].ResourceId.Id != folderID || "organizations/"+anc.Ancestor[2].ResourceId.Id != localOrg {
		t.Errorf("v1 getAncestry = %+v, %v; want the project, %s and %s", anc, err, folder.GetName(), localOrg)
	}

	// Set on the organization and the folder.
	if _, err := c.Organizations.SetOrgPolicy(localOrg, &crmv1.SetOrgPolicyRequest{Policy: &crmv1.OrgPolicy{
		Constraint: boolC, BooleanPolicy: &crmv1.BooleanPolicy{Enforced: true}}}).Context(ctx).Do(); err != nil {
		t.Fatalf("organizations.setOrgPolicy(%s): %v", boolC, err)
	}
	t.Cleanup(func() {
		for _, k := range []string{boolC, listC} {
			_, _ = c.Organizations.ClearOrgPolicy(localOrg, &crmv1.ClearOrgPolicyRequest{Constraint: k}).Do()
		}
	})
	orgList, err := c.Organizations.SetOrgPolicy(localOrg, &crmv1.SetOrgPolicyRequest{Policy: &crmv1.OrgPolicy{
		Constraint: listC, ListPolicy: &crmv1.ListPolicy{AllowedValues: []string{"in:eu-locations"}}}}).Context(ctx).Do()
	if err != nil || orgList.Etag == "" {
		t.Fatalf("organizations.setOrgPolicy(%s) = %+v, %v", listC, orgList, err)
	}
	var ge *googleapi.Error
	if _, err := c.Organizations.SetOrgPolicy(localOrg, &crmv1.SetOrgPolicyRequest{Policy: &crmv1.OrgPolicy{
		Constraint: listC, Etag: "c3RhbGU=", ListPolicy: &crmv1.ListPolicy{AllValues: "DENY"}}}).Context(ctx).Do(); !errors.As(err, &ge) || ge.Code != http.StatusConflict {
		t.Errorf("organizations.setOrgPolicy with a stale etag = %v; want 409 ABORTED", err)
	}
	if _, err := c.Folders.SetOrgPolicy(folder.GetName(), &crmv1.SetOrgPolicyRequest{Policy: &crmv1.OrgPolicy{
		Constraint: listC, ListPolicy: &crmv1.ListPolicy{AllowedValues: []string{"in:us-locations"}, InheritFromParent: true}}}).Context(ctx).Do(); err != nil {
		t.Fatalf("folders.setOrgPolicy(%s): %v", listC, err)
	}
	if _, err := c.Folders.SetOrgPolicy(folder.GetName(), &crmv1.SetOrgPolicyRequest{Policy: &crmv1.OrgPolicy{
		Constraint: boolC, BooleanPolicy: &crmv1.BooleanPolicy{Enforced: false}}}).Context(ctx).Do(); err != nil {
		t.Fatalf("folders.setOrgPolicy(%s): %v", boolC, err)
	}

	// Each reads its own back.
	if p, err := c.Organizations.GetOrgPolicy(localOrg, &crmv1.GetOrgPolicyRequest{Constraint: listC}).Context(ctx).Do(); err != nil || p.ListPolicy == nil || !slices.Equal(p.ListPolicy.AllowedValues, []string{"in:eu-locations"}) {
		t.Errorf("organizations.getOrgPolicy(%s) = %+v, %v", listC, p, err)
	}
	if p, err := c.Folders.GetOrgPolicy(folder.GetName(), &crmv1.GetOrgPolicyRequest{Constraint: listC}).Context(ctx).Do(); err != nil || p.ListPolicy == nil || !slices.Equal(p.ListPolicy.AllowedValues, []string{"in:us-locations"}) {
		t.Errorf("folders.getOrgPolicy(%s) = %+v, %v", listC, p, err)
	}
	if p, err := c.Projects.GetOrgPolicy("projects/"+id, &crmv1.GetOrgPolicyRequest{Constraint: listC}).Context(ctx).Do(); err != nil || p.ListPolicy != nil || p.BooleanPolicy != nil {
		t.Errorf("projects.getOrgPolicy(%s) on a project with none of its own = %+v, %v; want unset", listC, p, err)
	}
	fl, err := c.Folders.ListOrgPolicies(folder.GetName(), &crmv1.ListOrgPoliciesRequest{}).Context(ctx).Do()
	if err != nil || len(fl.Policies) != 2 {
		t.Errorf("folders.listOrgPolicies = %+v, %v; want its two", fl, err)
	}
	ol, err := c.Organizations.ListOrgPolicies(localOrg, &crmv1.ListOrgPoliciesRequest{}).Context(ctx).Do()
	var mine []string
	for _, p := range ol.Policies {
		if p.Constraint == boolC || p.Constraint == listC {
			mine = append(mine, p.Constraint)
		}
	}
	if err != nil || len(mine) != 2 {
		t.Errorf("organizations.listOrgPolicies = %+v, %v; want both of this test's", ol, err)
	}
	for _, res := range []string{folder.GetName(), localOrg} {
		var cons *crmv1.ListAvailableOrgPolicyConstraintsResponse
		if strings.HasPrefix(res, "folders/") {
			cons, err = c.Folders.ListAvailableOrgPolicyConstraints(res, &crmv1.ListAvailableOrgPolicyConstraintsRequest{}).Context(ctx).Do()
		} else {
			cons, err = c.Organizations.ListAvailableOrgPolicyConstraints(res, &crmv1.ListAvailableOrgPolicyConstraintsRequest{}).Context(ctx).Do()
		}
		if err != nil || len(cons.Constraints) == 0 {
			t.Errorf("%s listAvailableOrgPolicyConstraints = %v, %v", res, cons, err)
		}
	}

	// Effective, evaluated down organization, folder, project.
	if p, err := c.Organizations.GetEffectiveOrgPolicy(localOrg, &crmv1.GetEffectiveOrgPolicyRequest{Constraint: boolC}).Context(ctx).Do(); err != nil || p.BooleanPolicy == nil || !p.BooleanPolicy.Enforced {
		t.Errorf("organizations.getEffectiveOrgPolicy(%s) = %+v, %v; want enforced", boolC, p, err)
	}
	if p, err := c.Folders.GetEffectiveOrgPolicy(folder.GetName(), &crmv1.GetEffectiveOrgPolicyRequest{Constraint: boolC}).Context(ctx).Do(); err != nil || p.BooleanPolicy == nil || p.BooleanPolicy.Enforced {
		t.Errorf("folders.getEffectiveOrgPolicy(%s) = %+v, %v; want the folder's, not enforced", boolC, p, err)
	}
	eff, err := c.Projects.GetEffectiveOrgPolicy("projects/"+id, &crmv1.GetEffectiveOrgPolicyRequest{Constraint: listC}).Context(ctx).Do()
	if err != nil || eff.ListPolicy == nil || !slices.Equal(eff.ListPolicy.AllowedValues, []string{"in:eu-locations", "in:us-locations"}) || eff.Etag != "" {
		t.Errorf("projects.getEffectiveOrgPolicy(%s) = %+v, %v; want the organization's and the folder's values, no etag", listC, eff, err)
	}
	if _, err := c.Projects.SetOrgPolicy("projects/"+id, &crmv1.SetOrgPolicyRequest{Policy: &crmv1.OrgPolicy{
		Constraint: boolC, BooleanPolicy: &crmv1.BooleanPolicy{Enforced: true}}}).Context(ctx).Do(); err != nil {
		t.Fatalf("projects.setOrgPolicy(%s): %v", boolC, err)
	}
	if p, err := c.Projects.GetEffectiveOrgPolicy("projects/"+id, &crmv1.GetEffectiveOrgPolicyRequest{Constraint: boolC}).Context(ctx).Do(); err != nil || p.BooleanPolicy == nil || !p.BooleanPolicy.Enforced {
		t.Errorf("projects.getEffectiveOrgPolicy(%s) = %+v, %v; want the project's own, enforced", boolC, p, err)
	}

	// Clear the folder's; the project then inherits the organization's
	// list alone.
	if _, err := c.Folders.ClearOrgPolicy(folder.GetName(), &crmv1.ClearOrgPolicyRequest{Constraint: listC}).Context(ctx).Do(); err != nil {
		t.Fatalf("folders.clearOrgPolicy(%s): %v", listC, err)
	}
	if p, err := c.Projects.GetEffectiveOrgPolicy("projects/"+id, &crmv1.GetEffectiveOrgPolicyRequest{Constraint: listC}).Context(ctx).Do(); err != nil || p.ListPolicy == nil || !slices.Equal(p.ListPolicy.AllowedValues, []string{"in:eu-locations"}) {
		t.Errorf("projects.getEffectiveOrgPolicy(%s) after the folder's is cleared = %+v, %v", listC, p, err)
	}
	if _, err := c.Organizations.ClearOrgPolicy(localOrg, &crmv1.ClearOrgPolicyRequest{Constraint: listC}).Context(ctx).Do(); err != nil {
		t.Fatalf("organizations.clearOrgPolicy(%s): %v", listC, err)
	}
	if p, err := c.Organizations.GetOrgPolicy(localOrg, &crmv1.GetOrgPolicyRequest{Constraint: listC}).Context(ctx).Do(); err != nil || p.ListPolicy != nil || p.Etag == "" {
		t.Errorf("organizations.getOrgPolicy(%s) after clear = %+v, %v; want unset, with an etag", listC, p, err)
	}

	if _, err := c.Folders.GetOrgPolicy("folders/999999999999", &crmv1.GetOrgPolicyRequest{Constraint: boolC}).Context(ctx).Do(); !errors.As(err, &ge) || ge.Code != http.StatusNotFound {
		t.Errorf("folders.getOrgPolicy of an absent folder = %v; want 404", err)
	}
	if _, err := c.Organizations.GetOrgPolicy("organizations/1", &crmv1.GetOrgPolicyRequest{Constraint: boolC}).Context(ctx).Do(); !errors.As(err, &ge) || ge.Code != http.StatusNotFound {
		t.Errorf("organizations.getOrgPolicy of another organization = %v; want 404", err)
	}
}

// covers: cloudresourcemanager.folders.setOrgPolicy, cloudresourcemanager.folders.getOrgPolicy, cloudresourcemanager.folders.getEffectiveOrgPolicy, cloudresourcemanager.folders.clearOrgPolicy, cloudresourcemanager.organizations.setOrgPolicy, cloudresourcemanager.organizations.getOrgPolicy, cloudresourcemanager.organizations.clearOrgPolicy, cloudresourcemanager.organizations.listOrgPolicies
//
// TestGcloudFolderAndOrganizationOrgPolicies (#1534): `gcloud
// resource-manager org-policies`, which calls Resource Manager v1,
// configured by `cloudburrow gcloud-setup` alone: enable-enforce on the
// organization, allow on a folder the v3 API made, describe each, describe
// --effective on the folder, list on the organization, and delete both;
// the official v1 client reads each change. Every proxy variable points at
// the egress guard.
func TestGcloudFolderAndOrganizationOrgPolicies(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager)
	c := rmV1(t, h)
	fc, _, _ := foldersClients(t, h)
	g := newGcloudSession(t, h)
	ctx := h.Context()
	boolC, listC := runConstraint(h, "gbool"), runConstraint(h, "glist")
	org := strings.TrimPrefix(localOrg, "organizations/")
	op, err := fc.CreateFolder(ctx, &rmpb.CreateFolderRequest{Folder: &rmpb.Folder{Parent: localOrg, DisplayName: folderLabel(h, "gop-")}})
	if err != nil {
		t.Fatal(err)
	}
	folder, err := op.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = fc.DeleteFolder(context.Background(), &rmpb.DeleteFolderRequest{Name: folder.GetName()})
	})
	fid := strings.TrimPrefix(folder.GetName(), "folders/")
	t.Cleanup(func() {
		_, _ = c.Organizations.ClearOrgPolicy(localOrg, &crmv1.ClearOrgPolicyRequest{Constraint: boolC}).Do()
	})

	g.must("resource-manager", "org-policies", "enable-enforce", boolC, "--organization="+org)
	if p, err := c.Organizations.GetOrgPolicy(localOrg, &crmv1.GetOrgPolicyRequest{Constraint: boolC}).Context(ctx).Do(); err != nil || p.BooleanPolicy == nil || !p.BooleanPolicy.Enforced {
		t.Errorf("v1 getOrgPolicy after gcloud enable-enforce = %+v, %v", p, err)
	}
	if out := g.must("resource-manager", "org-policies", "describe", boolC, "--organization="+org, "--format=value(booleanPolicy.enforced)"); !strings.Contains(strings.ToLower(out), "true") {
		t.Errorf("gcloud org-policies describe on the organization = %q", out)
	}
	g.must("resource-manager", "org-policies", "allow", listC, "in:us-locations", "--folder="+fid)
	if p, err := c.Folders.GetOrgPolicy(folder.GetName(), &crmv1.GetOrgPolicyRequest{Constraint: listC}).Context(ctx).Do(); err != nil || p.ListPolicy == nil || !slices.Contains(p.ListPolicy.AllowedValues, "in:us-locations") {
		t.Errorf("v1 folders.getOrgPolicy after gcloud allow = %+v, %v", p, err)
	}
	if out := g.must("resource-manager", "org-policies", "describe", boolC, "--folder="+fid, "--effective", "--format=value(booleanPolicy.enforced)"); !strings.Contains(strings.ToLower(out), "true") {
		t.Errorf("gcloud org-policies describe --effective on the folder = %q; want the organization's, enforced", out)
	}
	if out := g.must("resource-manager", "org-policies", "list", "--organization="+org, "--format=value(constraint)"); !strings.Contains(out, boolC) {
		t.Errorf("gcloud org-policies list on the organization = %q; want %s", out, boolC)
	}
	g.must("resource-manager", "org-policies", "delete", listC, "--folder="+fid)
	g.must("resource-manager", "org-policies", "delete", boolC, "--organization="+org)
	for _, check := range []struct {
		what string
		get  func() (*crmv1.OrgPolicy, error)
	}{
		{"folder", func() (*crmv1.OrgPolicy, error) {
			return c.Folders.GetOrgPolicy(folder.GetName(), &crmv1.GetOrgPolicyRequest{Constraint: listC}).Context(ctx).Do()
		}},
		{"organization", func() (*crmv1.OrgPolicy, error) {
			return c.Organizations.GetOrgPolicy(localOrg, &crmv1.GetOrgPolicyRequest{Constraint: boolC}).Context(ctx).Do()
		}},
	} {
		if p, err := check.get(); err != nil || p.ListPolicy != nil || p.BooleanPolicy != nil {
			t.Errorf("the %s's policy after gcloud delete = %+v, %v; want unset", check.what, p, err)
		}
	}
}

// covers: cloudresourcemanager.folders.setOrgPolicy, cloudresourcemanager.folders.getOrgPolicy, cloudresourcemanager.folders.clearOrgPolicy, cloudresourcemanager.organizations.setOrgPolicy, cloudresourcemanager.organizations.getOrgPolicy, cloudresourcemanager.organizations.clearOrgPolicy
//
// TestTerraformFolderAndOrganizationPolicy (#1534): a
// google_folder_organization_policy (list) on a folder the v3 API made and a
// google_organization_policy (boolean) on the local organization; the
// official v1 client reads both back; the plan is clean; a change applies in
// place; destroy clears both.
func TestTerraformFolderAndOrganizationPolicy(t *testing.T) {
	h := New(t)
	c := rmV1(t, h) // asks for the Resource Manager endpoint first
	fc, _, _ := foldersClients(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	boolC, listC := runConstraint(h, "tbool"), runConstraint(h, "tlist")
	op, err := fc.CreateFolder(ctx, &rmpb.CreateFolderRequest{Folder: &rmpb.Folder{Parent: localOrg, DisplayName: folderLabel(h, "tfop-")}})
	if err != nil {
		t.Fatal(err)
	}
	folder, err := op.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = fc.DeleteFolder(context.Background(), &rmpb.DeleteFolderRequest{Name: folder.GetName()})
	})
	t.Cleanup(func() {
		_, _ = c.Organizations.ClearOrgPolicy(localOrg, &crmv1.ClearOrgPolicyRequest{Constraint: boolC}).Do()
	})
	module := func(enforced bool, location string) string {
		return fmt.Sprintf(`
resource "google_organization_policy" "b" {
  org_id     = %[1]q
  constraint = %[2]q
  boolean_policy {
    enforced = %[3]t
  }
}
resource "google_folder_organization_policy" "l" {
  folder     = %[4]q
  constraint = %[5]q
  list_policy {
    allow {
      values = [%[6]q]
    }
  }
}
`, strings.TrimPrefix(localOrg, "organizations/"), boolC, enforced, folder.GetName(), listC, location)
	}
	check := func(when string, enforced bool, location string) {
		t.Helper()
		b, err := c.Organizations.GetOrgPolicy(localOrg, &crmv1.GetOrgPolicyRequest{Constraint: boolC}).Context(ctx).Do()
		if err != nil || b.BooleanPolicy == nil || b.BooleanPolicy.Enforced != enforced {
			t.Errorf("%s: the organization's %s = %+v, %v; want enforced = %t", when, boolC, b, err, enforced)
		}
		l, err := c.Folders.GetOrgPolicy(folder.GetName(), &crmv1.GetOrgPolicyRequest{Constraint: listC}).Context(ctx).Do()
		if err != nil || l.ListPolicy == nil || !slices.Equal(l.ListPolicy.AllowedValues, []string{location}) {
			t.Errorf("%s: the folder's %s = %+v, %v; want %s allowed", when, listC, l, err, location)
		}
	}
	m.write(module(true, "in:us-locations"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	check("after apply", true, "in:us-locations")
	m.planClean()

	m.write(module(false, "in:eu-locations"))
	out := m.must("apply", "-auto-approve", "-input=false", "-no-color")
	if !strings.Contains(out, "0 added, 2 changed, 0 destroyed") {
		t.Errorf("the change was not applied in place:\n%s", lastLines(out, 20))
	}
	check("after the change", false, "in:eu-locations")
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if p, err := c.Organizations.GetOrgPolicy(localOrg, &crmv1.GetOrgPolicyRequest{Constraint: boolC}).Context(ctx).Do(); err != nil || p.BooleanPolicy != nil {
		t.Errorf("after destroy, the organization's %s = %+v, %v; want it unset", boolC, p, err)
	}
	if p, err := c.Folders.GetOrgPolicy(folder.GetName(), &crmv1.GetOrgPolicyRequest{Constraint: listC}).Context(ctx).Do(); err != nil || p.ListPolicy != nil {
		t.Errorf("after destroy, the folder's %s = %+v, %v; want it unset", listC, p, err)
	}
}
