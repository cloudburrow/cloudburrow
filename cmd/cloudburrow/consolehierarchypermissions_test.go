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
