//go:build compat

package compat

import (
	"context"
	"strings"
	"testing"

	iamv1 "google.golang.org/api/iam/v1"
)

// covers: iam.locations.workforcePools.providers.scimTenants.create, iam.locations.workforcePools.providers.scimTenants.get, iam.locations.workforcePools.providers.scimTenants.list, iam.locations.workforcePools.providers.scimTenants.patch, iam.locations.workforcePools.providers.scimTenants.delete, iam.locations.workforcePools.providers.scimTenants.undelete
// covers: iam.locations.workforcePools.providers.scimTenants.getIamPolicy, iam.locations.workforcePools.providers.scimTenants.setIamPolicy, iam.locations.workforcePools.providers.scimTenants.testIamPermissions
// covers: iam.locations.workforcePools.providers.scimTenants.tokens.create, iam.locations.workforcePools.providers.scimTenants.tokens.get, iam.locations.workforcePools.providers.scimTenants.tokens.list, iam.locations.workforcePools.providers.scimTenants.tokens.patch, iam.locations.workforcePools.providers.scimTenants.tokens.delete
//
// TestWorkforceScimTenants (#2065), with google.golang.org/api/iam/v1: a
// provider's SCIM tenant is created (its baseUri and service agent in
// Google's forms), read, listed, patched (claimMapping immutable),
// soft-deleted (DELETED with a purgeTime, listed only with showDeleted,
// not updatable) and undeleted, then hard-deleted; a pool has one tenant;
// a token's securityToken is answered by create only, at most two tokens
// per tenant, a deleted token is not updatable. The tenant's own IAM
// policy is set and read back, and testIamPermissions answers its
// permissions.
func TestWorkforceScimTenants(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager)
	ctx := h.Context()
	svc := iamREST(t, h)
	pools := svc.Locations.WorkforcePools
	id := wfPoolID(h, "wfscim-")
	pool := "locations/global/workforcePools/" + id
	if _, err := pools.Create("locations/global", &iamv1.WorkforcePool{Parent: localOrgRoles}).WorkforcePoolId(id).Context(ctx).Do(); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	t.Cleanup(func() { _, _ = pools.Delete(pool).Context(context.Background()).Do() })
	for _, p := range []string{"scim-one", "scim-two"} {
		if _, err := pools.Providers.Create(pool, &iamv1.WorkforcePoolProvider{
			Oidc:             &iamv1.GoogleIamAdminV1WorkforcePoolProviderOidc{IssuerUri: "https://idp.example.test", ClientId: "staff", WebSsoConfig: wfWebSSO},
			AttributeMapping: map[string]string{"google.subject": "assertion.sub"}}).WorkforcePoolProviderId(p).Context(ctx).Do(); err != nil {
			t.Fatalf("create provider %s: %v", p, err)
		}
	}
	prov := pool + "/providers/scim-one"
	tenants := pools.Providers.ScimTenants
	mapping := map[string]string{"google.subject": "user.externalId", "google.group": "group.externalId"}

	for what, c := range map[string]struct {
		id     string
		tenant *iamv1.WorkforcePoolProviderScimTenant
	}{
		"a short ID":        {"ab", &iamv1.WorkforcePoolProviderScimTenant{ClaimMapping: mapping}},
		"no claim mapping":  {"tenant-x", &iamv1.WorkforcePoolProviderScimTenant{}},
		"no google.subject": {"tenant-x", &iamv1.WorkforcePoolProviderScimTenant{ClaimMapping: map[string]string{"google.group": "group.externalId"}}},
		"invalid CEL":       {"tenant-x", &iamv1.WorkforcePoolProviderScimTenant{ClaimMapping: map[string]string{"google.subject": "user.("}}},
	} {
		_, err := tenants.Create(prov, c.tenant).WorkforcePoolProviderScimTenantId(c.id).Context(ctx).Do()
		wantWIFError(t, what, err, 400)
	}
	created, err := tenants.Create(prov, &iamv1.WorkforcePoolProviderScimTenant{DisplayName: "Okta", ClaimMapping: mapping}).
		WorkforcePoolProviderScimTenantId("tenant-a").Context(ctx).Do()
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	name := prov + "/scimTenants/tenant-a"
	uid := strings.TrimPrefix(created.BaseUri, "https://iamscim.googleapis.com/v1alpha1/tenants/")
	if created.Name != name || created.State != "ACTIVE" || uid == "" || uid == created.BaseUri ||
		created.ServiceAgent != "o-"+strings.TrimPrefix(localOrgRoles, "organizations/")+"-"+uid+"@gcp-sa-iamscim.iam.gserviceaccount.com" ||
		created.ClaimMapping["google.group"] != "group.externalId" {
		t.Fatalf("created tenant = %+v", created)
	}
	_, err = tenants.Create(prov, &iamv1.WorkforcePoolProviderScimTenant{ClaimMapping: mapping}).WorkforcePoolProviderScimTenantId("tenant-a").Context(ctx).Do()
	wantWIFError(t, "a duplicate tenant", err, 409)
	_, err = tenants.Create(pool+"/providers/scim-two", &iamv1.WorkforcePoolProviderScimTenant{ClaimMapping: mapping}).WorkforcePoolProviderScimTenantId("tenant-b").Context(ctx).Do()
	wantWIFError(t, "a second tenant in the pool", err, 400)
	_, err = tenants.Patch(name, &iamv1.WorkforcePoolProviderScimTenant{ClaimMapping: map[string]string{"google.subject": "user.id"}}).UpdateMask("claimMapping").Context(ctx).Do()
	wantWIFError(t, "patch claimMapping", err, 400)
	if got, err := tenants.Patch(name, &iamv1.WorkforcePoolProviderScimTenant{DisplayName: "Okta prod", Description: "staff"}).
		UpdateMask("displayName,description").Context(ctx).Do(); err != nil || got.DisplayName != "Okta prod" || got.Description != "staff" {
		t.Fatalf("patch tenant = %+v, %v", got, err)
	}
	if got, err := tenants.Get(name).Context(ctx).Do(); err != nil || got.DisplayName != "Okta prod" || got.BaseUri != created.BaseUri {
		t.Fatalf("get tenant = %+v, %v", got, err)
	}

	member := "principal://iam.googleapis.com/" + pool + "/subject/scim-admin"
	if got, err := tenants.SetIamPolicy(name, &iamv1.SetIamPolicyRequest{Policy: &iamv1.Policy{Bindings: []*iamv1.Binding{
		{Role: "roles/iam.workforcePoolViewer", Members: []string{member}}}}}).Context(ctx).Do(); err != nil || got.Etag == "" {
		t.Fatalf("tenant setIamPolicy = %+v, %v", got, err)
	}
	if got, err := tenants.GetIamPolicy(name, &iamv1.GetIamPolicyRequest{}).Context(ctx).Do(); err != nil || len(got.Bindings) != 1 || got.Bindings[0].Members[0] != member {
		t.Fatalf("tenant getIamPolicy = %+v, %v", got, err)
	}
	if got, err := tenants.TestIamPermissions(name, &iamv1.TestIamPermissionsRequest{Permissions: []string{"iam.workforcePoolProviderScimTenants.get", "storage.buckets.get"}}).
		Context(ctx).Do(); err != nil || len(got.Permissions) != 1 {
		t.Fatalf("tenant testIamPermissions = %+v, %v", got, err)
	}

	tokens := tenants.Tokens
	_, err = tokens.Create(name, &iamv1.WorkforcePoolProviderScimToken{}).WorkforcePoolProviderScimTokenId("1-bad").Context(ctx).Do()
	wantWIFError(t, "a token ID starting with a digit", err, 400)
	tok, err := tokens.Create(name, &iamv1.WorkforcePoolProviderScimToken{DisplayName: "Okta"}).WorkforcePoolProviderScimTokenId("tok-one").Context(ctx).Do()
	if err != nil || tok.SecurityToken == "" || tok.State != "ACTIVE" || tok.Name != name+"/tokens/tok-one" {
		t.Fatalf("create token = %+v, %v", tok, err)
	}
	if got, err := tokens.Get(tok.Name).Context(ctx).Do(); err != nil || got.SecurityToken != "" || got.DisplayName != "Okta" {
		t.Fatalf("get token = %+v, %v", got, err)
	}
	if _, err := tokens.Create(name, &iamv1.WorkforcePoolProviderScimToken{}).WorkforcePoolProviderScimTokenId("tok-two").Context(ctx).Do(); err != nil {
		t.Fatalf("second token: %v", err)
	}
	_, err = tokens.Create(name, &iamv1.WorkforcePoolProviderScimToken{}).WorkforcePoolProviderScimTokenId("tok-three").Context(ctx).Do()
	wantWIFError(t, "a third token", err, 400)
	page, err := tokens.List(name).Context(ctx).Do()
	if err != nil || len(page.WorkforcePoolProviderScimTokens) != 2 {
		t.Fatalf("list tokens = %+v, %v", page, err)
	}
	for _, x := range page.WorkforcePoolProviderScimTokens {
		if x.SecurityToken != "" {
			t.Fatal("list answered a security token")
		}
	}
	if got, err := tokens.Patch(tok.Name, &iamv1.WorkforcePoolProviderScimToken{DisplayName: "Okta 2"}).UpdateMask("displayName").Context(ctx).Do(); err != nil || got.DisplayName != "Okta 2" {
		t.Fatalf("patch token = %+v, %v", got, err)
	}
	if got, err := tokens.Delete(name + "/tokens/tok-two").Context(ctx).Do(); err != nil || got.State != "DELETED" {
		t.Fatalf("delete token = %+v, %v", got, err)
	}
	_, err = tokens.Patch(name+"/tokens/tok-two", &iamv1.WorkforcePoolProviderScimToken{DisplayName: "x"}).UpdateMask("displayName").Context(ctx).Do()
	wantWIFError(t, "patch a deleted token", err, 400)
	if l, err := tokens.List(name).ShowDeleted(true).Context(ctx).Do(); err != nil || len(l.WorkforcePoolProviderScimTokens) != 2 {
		t.Fatalf("list tokens showDeleted = %+v, %v", l, err)
	}

	listed := func(showDeleted bool) int {
		t.Helper()
		l, err := tenants.List(prov).ShowDeleted(showDeleted).PageSize(1).Context(ctx).Do()
		if err != nil {
			t.Fatalf("list tenants: %v", err)
		}
		return len(l.WorkforcePoolProviderScimTenants)
	}
	if listed(false) != 1 {
		t.Fatal("the tenant is not listed")
	}
	if got, err := tenants.Delete(name).Context(ctx).Do(); err != nil || got.State != "DELETED" || got.PurgeTime == "" {
		t.Fatalf("delete tenant = %+v, %v", got, err)
	}
	if listed(false) != 0 || listed(true) != 1 {
		t.Fatal("a deleted tenant is listed without showDeleted, or not with it")
	}
	_, err = tenants.Patch(name, &iamv1.WorkforcePoolProviderScimTenant{DisplayName: "x"}).UpdateMask("displayName").Context(ctx).Do()
	wantWIFError(t, "patch a deleted tenant", err, 400)
	if got, err := tenants.Undelete(name, &iamv1.UndeleteWorkforcePoolProviderScimTenantRequest{}).Context(ctx).Do(); err != nil || got.State != "ACTIVE" {
		t.Fatalf("undelete tenant = %+v, %v", got, err)
	}
	if _, err := tenants.Delete(name).HardDelete(true).Context(ctx).Do(); err != nil {
		t.Fatalf("hard delete: %v", err)
	}
	_, err = tenants.Get(name).Context(ctx).Do()
	wantWIFError(t, "a hard-deleted tenant", err, 404)
	if _, err := tenants.Create(pool+"/providers/scim-two", &iamv1.WorkforcePoolProviderScimTenant{ClaimMapping: mapping}).
		WorkforcePoolProviderScimTenantId("tenant-b").Context(ctx).Do(); err != nil {
		t.Fatalf("a tenant after the hard delete: %v", err)
	}
	t.Cleanup(func() {
		_, _ = tenants.Delete(pool + "/providers/scim-two/scimTenants/tenant-b").HardDelete(true).Context(context.Background()).Do()
	})
}
