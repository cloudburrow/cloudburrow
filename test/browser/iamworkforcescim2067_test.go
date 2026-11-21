//go:build browser

package browser

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	iam "google.golang.org/api/iam/v1"
	"google.golang.org/api/option"
)

// TestIAMWorkforcePoolPermissionsAndScimPage (#2067): IAM & Admin >
// Workforce Identity Federation. A pool's Permissions tab grants a role
// the official google.golang.org/api/iam/v1 client's getIamPolicy reads
// back; a provider's SCIM tab offers Create SCIM tenant with the claim
// mapping, which makes the tenant; the tenant's page shows its base URI
// and service agent, Create token answers the security token once (a get
// answers none), and its own Permissions tab grants a role on the tenant.
// The pool's, the provider's and the tenant's pages pass the console's
// WCAG 2.1 AA audit.
func TestIAMWorkforcePoolPermissionsAndScimPage(t *testing.T) {
	needService(t, "iam-workforce-identity")
	ctx := context.Background()
	svc, err := iam.NewService(ctx, option.WithEndpoint("http://"+endpoint(t, envResourceManager)+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	project := uniqueProject(t)
	id := fmt.Sprintf("scimui-%d", time.Now().UnixNano()%1e9)
	name := "locations/global/workforcePools/" + id
	pools := svc.Locations.WorkforcePools
	if _, err := pools.Create("locations/global", &iam.WorkforcePool{Parent: "organizations/100000000001"}).WorkforcePoolId(id).Context(ctx).Do(); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	t.Cleanup(func() { _, _ = pools.Delete(name).Context(context.Background()).Do() })
	if _, err := pools.Providers.Create(name, &iam.WorkforcePoolProvider{
		Oidc: &iam.GoogleIamAdminV1WorkforcePoolProviderOidc{IssuerUri: "https://idp.example.test", ClientId: "staff",
			WebSsoConfig: &iam.GoogleIamAdminV1WorkforcePoolProviderOidcWebSsoConfig{ResponseType: "ID_TOKEN", AssertionClaimsBehavior: "ONLY_ID_TOKEN_CLAIMS"}},
		AttributeMapping: map[string]string{"google.subject": "assertion.sub"}}).WorkforcePoolProviderId("staff-idp").Context(ctx).Do(); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	q := "?project=" + project
	hasMember := func(pol *iam.Policy, role, member string) bool {
		for _, b := range pol.Bindings {
			for _, m := range b.Members {
				if b.Role == role && m == member {
					return true
				}
			}
		}
		return false
	}

	// The pool's Permissions tab.
	p := open(t)
	p.navigate("/iam-admin/workforce-identity-pools/" + id + q)
	p.waitFor(`document.querySelector("#view [role=tab]") !== null`)
	p.clickText("#view [role=tab]", "Permissions")
	p.waitFor(`document.querySelector("#permissions-note") !== null`)
	var note string
	p.eval(`document.querySelector("#permissions-note").textContent`, &note)
	if !strings.Contains(note, "iam-enforcement") {
		t.Errorf("the pool's Permissions note does not say how the policy is enforced: %s", note)
	}
	member := "principal://iam.googleapis.com/" + name + "/subject/ada"
	p.grantOnTab(member, "roles/iam.workforcePoolViewer")
	p.waitFor(`document.querySelector("#view").textContent.includes("roles/iam.workforcePoolViewer")`)
	if pol, err := pools.GetIamPolicy(name, &iam.GetIamPolicyRequest{}).Context(ctx).Do(); err != nil || !hasMember(pol, "roles/iam.workforcePoolViewer", member) {
		t.Fatalf("after Grant access the pool's policy = %+v, %v", pol, err)
	}

	// The provider's SCIM tab: Create SCIM tenant.
	provPath := "/iam-admin/workforce-identity-pools/" + id + "/staff-idp"
	p.navigate(provPath + q)
	p.waitFor(`document.querySelector("#view [role=tab]") !== null`)
	p.clickText("#view [role=tab]", "SCIM")
	p.pageAction("Create SCIM tenant", map[string]string{"tenantDisplayName": "Okta", "tenantId": "okta-scim",
		"claimMapping": "google.subject=user.externalId\ngoogle.group=group.displayName"}, "")
	tenantName := name + "/providers/staff-idp/scimTenants/okta-scim"
	tenant, err := pools.Providers.ScimTenants.Get(tenantName).Context(ctx).Do()
	if err != nil || tenant.DisplayName != "Okta" || tenant.ClaimMapping["google.group"] != "group.displayName" {
		t.Fatalf("the tenant Create SCIM tenant made = %+v, %v", tenant, err)
	}
	t.Cleanup(func() {
		_, _ = pools.Providers.ScimTenants.Delete(tenantName).HardDelete(true).Context(context.Background()).Do()
	})

	// The tenant's page: its base URI, Create token (the security token
	// once), and its own Permissions tab.
	tenantPath := provPath + "/scim:okta-scim"
	p.navigate(tenantPath + q)
	p.waitFor(`document.querySelector("#view").textContent.includes("gcp-sa-iamscim")`)
	var text string
	p.eval(`document.querySelector("#view").textContent`, &text)
	if !strings.Contains(text, tenant.BaseUri) || !strings.Contains(text, tenant.ServiceAgent) {
		t.Errorf("the tenant's page does not show its base URI and service agent: %.600s", text)
	}
	rows := p.pageAction("Create token", map[string]string{"tokenDisplayName": "Okta", "tokenId": "okta-token"}, "")
	tok, err := pools.Providers.ScimTenants.Tokens.Get(tenantName + "/tokens/okta-token").Context(ctx).Do()
	if err != nil || tok.State != "ACTIVE" || tok.SecurityToken != "" {
		t.Fatalf("the token Create token made = %+v, %v", tok, err)
	}
	if !strings.Contains(rows, "okta-token") || len(rows) < len("okta-token")+20 {
		t.Errorf("Create token did not answer the security token: %q", rows)
	}
	p.navigate(tenantPath + q)
	p.waitFor(`document.querySelector("#view [role=tab]") !== null`)
	p.clickText("#view [role=tab]", "Tokens")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("okta-token"))`)
	p.clickText("#view [role=tab]", "Permissions")
	p.waitFor(`document.querySelector("#permissions-note") !== null`)
	p.grantOnTab(member, "roles/iam.workforcePoolAdmin")
	p.waitFor(`document.querySelector("#view").textContent.includes("roles/iam.workforcePoolAdmin")`)
	if pol, err := pools.Providers.ScimTenants.GetIamPolicy(tenantName, &iam.GetIamPolicyRequest{}).Context(ctx).Do(); err != nil ||
		!hasMember(pol, "roles/iam.workforcePoolAdmin", member) {
		t.Fatalf("after Grant access the tenant's policy = %+v, %v", pol, err)
	}

	auditScreens(t, []a11yPage{
		screen("a workforce pool's Permissions", "/iam-admin/workforce-identity-pools/"+id, project),
		screen("a workforce provider's SCIM tab", provPath, project),
		screen("a SCIM tenant", tenantPath, project),
	})
}
