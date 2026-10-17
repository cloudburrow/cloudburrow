package main

import (
	"context"
	"net/http/httptest"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestConsoleFolderPermissions (#1552): a folder's page has a Permissions
// tab, as Google's console offers IAM on a folder picked in the resource
// selector. Its grants and removals are the v3 Folders API's SetIamPolicy,
// which GetIamPolicy reads back; a stale etag is refused in the Folders
// API's words; Test permissions is its TestIamPermissions. A folder pending
// deletion has no tab, and the policy is the folder's, not a project's.
func TestConsoleFolderPermissions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := resourcemanager.New(store.NewMemory())
	h := hierarchyProvider{registry: reg}
	id, err := h.Create(ctx, "", map[string]string{"displayName": "Engineering", "parent": resourcemanager.OrganizationName})
	if err != nil {
		t.Fatal(err)
	}
	api := resourcemanager.NewFoldersServer(reg)
	resource := "folders/" + id
	var lastStale error
	c := permissionsCase{service: "hierarchy", project: "any-project", path: []string{id},
		readBack: func(t *testing.T) *iampb.Policy {
			pol, err := api.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: resource})
			if err != nil {
				t.Fatal(err)
			}
			return pol
		},
		write: func(t *testing.T, add *iampb.Binding) {
			pol, _ := api.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: resource})
			stale := pol.GetEtag()
			pol.Bindings = append(pol.Bindings, add)
			if _, err := api.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: resource, Policy: pol}); err != nil {
				t.Fatal(err)
			}
			pol.Etag = stale
			_, lastStale = api.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: resource, Policy: pol})
		},
		refusal: func() string { return grpcRefusal(lastStale) },
	}
	exercisePermissions(t, h, c)

	held, err := h.TestPermissions(ctx, "", []string{id}, []string{"resourcemanager.folders.get", "resourcemanager.folders.setIamPolicy"})
	if err != nil || len(held) != 2 {
		t.Errorf("Test permissions on the folder = %v, %v; want both held", held, err)
	}
	if pol, err := (projectIAMProvider{registry: reg}).GetPolicy(ctx, "", []string{"any-project"}); err == nil && len(pol.Bindings) != 0 {
		t.Errorf("the folder's grants reached a project's policy: %v", pol.Bindings)
	}

	srv := httptest.NewServer(console.New("127.0.0.1:0", nil, h).Handler())
	t.Cleanup(srv.Close)
	if h.PolicyOn(ctx, nil) != nil || h.PolicyOn(ctx, []string{id, "x"}) != nil {
		t.Error("PolicyOn draws a tab off a folder's own page")
	}
	if err := h.Delete(ctx, "", id); err != nil {
		t.Fatal(err)
	}
	if hasPermissionsTab(t, srv, "hierarchy", "any-project", id) {
		t.Error("a folder pending deletion has a Permissions tab")
	}
}

// TestConsoleOrganizationPage (#1554): the Resource hierarchy list links the
// organization to its own page, which shows what GetOrganization answers,
// the folders and projects directly under it and its organization policies,
// and has a Permissions tab, as Google's console offers IAM on the
// organization once it is picked in the resource selector. Its grants and
// removals are the v3 Organizations API's SetIamPolicy, which GetIamPolicy
// reads back; a stale etag is refused in that API's words; Test permissions
// is its TestIamPermissions. The page is not a folder's: it has no Rename.
func TestConsoleOrganizationPage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := resourcemanager.New(store.NewMemory())
	h := hierarchyProvider{registry: reg}
	top, err := h.Create(ctx, "", map[string]string{"displayName": "Top", "parent": resourcemanager.OrganizationName})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Create(ctx, "", map[string]string{"displayName": "Nested", "parent": "folders/" + top}); err != nil {
		t.Fatal(err)
	}
	l, err := h.List(ctx, "")
	if err != nil || len(l.Summary) != 1 || len(l.Summary[0].Properties) != 1 || l.Summary[0].Properties[0].Link != "/resource-hierarchy/organization" {
		t.Fatalf("the list's organization card = %+v, %v; want a link to the organization's page", l.Summary, err)
	}
	org := []string{organizationSegment}
	d, err := h.Detail(ctx, "", org)
	if err != nil || d.Unavailable != "" {
		t.Fatalf("the organization's page = %+v, %v", d, err)
	}
	if d.Edit != nil {
		t.Error("the organization's page offers Rename folder")
	}
	if len(d.Summary) < 2 || d.Summary[1].Value != resourcemanager.OrganizationName || d.Summary[0].Value != resourcemanager.OrganizationDomain {
		t.Errorf("the organization's summary = %+v", d.Summary)
	}
	ids := map[string]int{}
	for _, s := range d.Sections {
		ids[s.ID] = s.Listing.Total
	}
	if ids["folders"] != 1 {
		t.Errorf("the organization lists %d folders directly under it; want 1 (Top, not Nested)", ids["folders"])
	}
	if _, ok := ids["org-policies"]; !ok {
		t.Error("the organization's page has no Organization policies section")
	}
	if err := h.Edit(ctx, "", org, map[string]string{"displayName": "x"}); err == nil {
		t.Error("Edit on the organization's page succeeded")
	}

	api := resourcemanager.NewOrganizationsServer(reg)
	resource := resourcemanager.OrganizationName
	var lastStale error
	c := permissionsCase{service: "hierarchy", project: "any-project", path: org,
		readBack: func(t *testing.T) *iampb.Policy {
			pol, err := api.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: resource})
			if err != nil {
				t.Fatal(err)
			}
			return pol
		},
		write: func(t *testing.T, add *iampb.Binding) {
			pol, _ := api.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: resource})
			stale := pol.GetEtag()
			pol.Bindings = append(pol.Bindings, add)
			if _, err := api.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: resource, Policy: pol}); err != nil {
				t.Fatal(err)
			}
			pol.Etag = stale
			_, lastStale = api.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: resource, Policy: pol})
		},
		refusal: func() string { return grpcRefusal(lastStale) },
	}
	exercisePermissions(t, h, c)
	held, err := h.TestPermissions(ctx, "", org, []string{"resourcemanager.organizations.get", "resourcemanager.organizations.setIamPolicy"})
	if err != nil || len(held) != 2 {
		t.Errorf("Test permissions on the organization = %v, %v; want both held", held, err)
	}
	if pol, err := h.GetPolicy(ctx, "", []string{top}); err != nil || len(pol.Bindings) != 0 {
		t.Errorf("the organization's grants reached a folder's policy: %+v, %v", pol.Bindings, err)
	}
}
