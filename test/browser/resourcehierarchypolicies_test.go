//go:build browser

package browser

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	crmv1 "google.golang.org/api/cloudresourcemanager/v1"
	crmv3 "google.golang.org/api/cloudresourcemanager/v3"
	"google.golang.org/api/option"
)

// TestResourceHierarchyAppManagementAndOrgPoliciesThroughThePage (#1591), in
// the storage shard. On a folder's page, Enable app management makes the
// folder's app-management capability true, as the v3 REST client's
// folders.capabilities.get reads it, and Set organization policy enforcing
// constraints/compute.requireOsLogin is the policy the v1 client's
// folders.getOrgPolicy reads. On the organization's page, Set organization
// policy with an allowed value for constraints/gcp.resourceLocations is the
// one organizations.getOrgPolicy reads; Clear organization policy removes
// it.
func TestResourceHierarchyAppManagementAndOrgPoliciesThroughThePage(t *testing.T) {
	needService(t, "hierarchy")
	ctx := context.Background()
	folders, err := resourcemanager.NewFoldersClient(ctx, rmClientOptions(t)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = folders.Close() })
	rest := []option.ClientOption{option.WithEndpoint("http://" + endpoint(t, envResourceManager) + "/"),
		option.WithoutAuthentication(), option.WithHTTPClient(http.DefaultClient)}
	v1, err := crmv1.NewService(ctx, rest...)
	if err != nil {
		t.Fatal(err)
	}
	v3, err := crmv3.NewService(ctx, rest...)
	if err != nil {
		t.Fatal(err)
	}
	orgs, err := resourcemanager.NewOrganizationsClient(ctx, rmClientOptions(t)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = orgs.Close() })
	org, err := orgs.SearchOrganizations(ctx, &rmpb.SearchOrganizationsRequest{}).Next()
	if err != nil {
		t.Fatalf("SearchOrganizations: %v", err)
	}
	op, err := folders.CreateFolder(ctx, &rmpb.CreateFolderRequest{Folder: &rmpb.Folder{
		Parent: org.GetName(), DisplayName: fmt.Sprintf("Apps %d", time.Now().UnixNano()%1e8)}})
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	f, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	const boolC, listC = "constraints/compute.requireOsLogin", "constraints/gcp.resourceLocations"
	t.Cleanup(func() {
		_, _ = v1.Organizations.ClearOrgPolicy(org.GetName(), &crmv1.ClearOrgPolicyRequest{Constraint: listC}).Do()
		if op, err := folders.DeleteFolder(context.Background(), &rmpb.DeleteFolderRequest{Name: f.GetName()}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})

	p := open(t)
	p.navigate("/resource-hierarchy/" + strings.TrimPrefix(f.GetName(), "folders/"))
	p.waitFor(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent === "Enable app management")`)
	p.clickText("#view .page-actions button", "Enable app management")
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		c, err := v3.Folders.Capabilities.Get(f.GetName() + "/capabilities/app-management").Do()
		if err == nil && c.Value {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after Enable app management, folders.capabilities.get = %+v, %v; want true", c, err)
		}
	}
	p.waitFor(`document.querySelector(".modal") === null`)
	p.navigate("/resource-hierarchy/" + strings.TrimPrefix(f.GetName(), "folders/"))
	p.waitFor(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent === "Set organization policy")`)
	p.pageAction("Set organization policy", map[string]string{"constraint": boolC, "enforced": "true"}, "")
	if pol, err := v1.Folders.GetOrgPolicy(f.GetName(), &crmv1.GetOrgPolicyRequest{Constraint: boolC}).Do(); err != nil ||
		pol.BooleanPolicy == nil || !pol.BooleanPolicy.Enforced {
		t.Errorf("after Set organization policy on the folder, folders.getOrgPolicy = %+v, %v; want %s enforced", pol, err, boolC)
	}

	p.navigate("/resource-hierarchy/organization")
	p.waitFor(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent === "Set organization policy")`)
	p.pageAction("Set organization policy", map[string]string{"constraint": listC, "allowed": "in:eu-locations"}, "")
	pol, err := v1.Organizations.GetOrgPolicy(org.GetName(), &crmv1.GetOrgPolicyRequest{Constraint: listC}).Do()
	if err != nil || pol.ListPolicy == nil || !slices.Equal(pol.ListPolicy.AllowedValues, []string{"in:eu-locations"}) {
		t.Fatalf("after Set organization policy on the organization, organizations.getOrgPolicy = %+v, %v", pol, err)
	}
	p.waitFor(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent === "Clear organization policy")`)
	p.pageAction("Clear organization policy", map[string]string{"constraint": listC}, "")
	if pol, err := v1.Organizations.GetOrgPolicy(org.GetName(), &crmv1.GetOrgPolicyRequest{Constraint: listC}).Do(); err != nil || pol.ListPolicy != nil {
		t.Errorf("after Clear organization policy, organizations.getOrgPolicy = %+v, %v; want no list policy", pol, err)
	}
}
