package resourcemanager

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

func wfScimCall(t *testing.T, r *Registry, method, path string, body any, out any) int {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	rec := httptest.NewRecorder()
	r.ServeWorkforceIdentity(rec, httptest.NewRequest(method, path, rd))
	if out != nil && rec.Code == 200 {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s: decode %q: %v", method, path, rec.Body.String(), err)
		}
	}
	return rec.Code
}

// SCIM tenants and tokens (#2065): create with Google's validation, one
// tenant per pool, immutable claim mapping, soft and hard delete, and a
// token's secret answered by create alone.
func TestWorkforceScimTenantsAndTokens(t *testing.T) {
	r := New(store.NewMemory())
	base := "/v1/locations/global/workforcePools"
	if code := wfScimCall(t, r, "POST", base+"?workforcePoolId=scim-pool", map[string]any{"parent": OrganizationName}, nil); code != 200 {
		t.Fatalf("create pool = %d", code)
	}
	prov := map[string]any{"attributeMapping": map[string]string{"google.subject": "assertion.sub"},
		"oidc": map[string]any{"issuerUri": "https://idp.example.test", "clientId": "c", "webSsoConfig": map[string]string{"responseType": "ID_TOKEN", "assertionClaimsBehavior": "ONLY_ID_TOKEN_CLAIMS"}}}
	for _, id := range []string{"prov-one", "prov-two"} {
		if code := wfScimCall(t, r, "POST", base+"/scim-pool/providers?workforcePoolProviderId="+id, prov, nil); code != 200 {
			t.Fatalf("create provider %s = %d", id, code)
		}
	}
	tenants := base + "/scim-pool/providers/prov-one/scimTenants"
	good := map[string]any{"displayName": "Okta", "claimMapping": map[string]string{"google.subject": "user.externalId", "google.group": "group.externalId"}}
	for what, c := range map[string]struct {
		id   string
		body map[string]any
		code int
	}{
		"a short ID":          {"ab", good, 400},
		"no claim mapping":    {"tenant-a", map[string]any{}, 400},
		"no google.subject":   {"tenant-a", map[string]any{"claimMapping": map[string]string{"google.group": "group.externalId"}}, 400},
		"an unsupported key":  {"tenant-a", map[string]any{"claimMapping": map[string]string{"google.subject": "user.id", "attribute.x": "user.x"}}, 400},
		"invalid CEL":         {"tenant-a", map[string]any{"claimMapping": map[string]string{"google.subject": "user.("}}, 400},
		"a long display name": {"tenant-a", map[string]any{"displayName": strings.Repeat("x", 33), "claimMapping": map[string]string{"google.subject": "user.id"}}, 400},
	} {
		if code := wfScimCall(t, r, "POST", tenants+"?workforcePoolProviderScimTenantId="+c.id, c.body, nil); code != c.code {
			t.Errorf("%s: create = %d; want %d", what, code, c.code)
		}
	}
	if code := wfScimCall(t, r, "POST", base+"/scim-pool/providers/nope/scimTenants?workforcePoolProviderScimTenantId=tenant-a", good, nil); code != 404 {
		t.Errorf("create under an unknown provider = %d; want 404", code)
	}
	var tenant WorkforcePoolProviderScimTenant
	if code := wfScimCall(t, r, "POST", tenants+"?workforcePoolProviderScimTenantId=tenant-a", good, &tenant); code != 200 {
		t.Fatalf("create tenant = %d", code)
	}
	name := "locations/global/workforcePools/scim-pool/providers/prov-one/scimTenants/tenant-a"
	if tenant.Name != name || tenant.State != "ACTIVE" || !strings.HasPrefix(tenant.BaseURI, wfScimBaseURIPrefix) ||
		!strings.HasSuffix(tenant.ServiceAgent, wfScimServiceAgentAt) || tenant.ClaimMapping["google.subject"] != "user.externalId" {
		t.Fatalf("created tenant = %+v", tenant)
	}
	if code := wfScimCall(t, r, "POST", tenants+"?workforcePoolProviderScimTenantId=tenant-a", good, nil); code != 409 {
		t.Errorf("duplicate tenant = %d; want 409", code)
	}
	if code := wfScimCall(t, r, "POST", base+"/scim-pool/providers/prov-two/scimTenants?workforcePoolProviderScimTenantId=tenant-b", good, nil); code != 400 {
		t.Errorf("a second tenant in the pool = %d; want 400 FAILED_PRECONDITION", code)
	}
	tname := "/v1/" + name
	if code := wfScimCall(t, r, "PATCH", tname+"?updateMask=claimMapping", good, nil); code != 400 {
		t.Errorf("patch claimMapping = %d; want 400", code)
	}
	var patched WorkforcePoolProviderScimTenant
	if code := wfScimCall(t, r, "PATCH", tname+"?updateMask=displayName,description", map[string]any{"displayName": "Okta 2", "description": "d"}, &patched); code != 200 ||
		patched.DisplayName != "Okta 2" || patched.Description != "d" {
		t.Fatalf("patch = %d, %+v", code, patched)
	}

	// Tokens.
	var tok WorkforcePoolProviderScimToken
	if code := wfScimCall(t, r, "POST", tname+"/tokens?workforcePoolProviderScimTokenId=1bad", map[string]any{}, nil); code != 400 {
		t.Errorf("a token ID starting with a digit = %d; want 400", code)
	}
	if code := wfScimCall(t, r, "POST", tname+"/tokens?workforcePoolProviderScimTokenId=tok-one", map[string]any{"displayName": "IdP"}, &tok); code != 200 || tok.SecurityToken == "" || tok.State != "ACTIVE" {
		t.Fatalf("create token = %d, %+v", code, tok)
	}
	secret := tok.SecurityToken
	if !r.ScimTokenLive("scim-pool", "prov-one", "tenant-a", secret) || r.ScimTokenLive("scim-pool", "prov-one", "tenant-a", "nope") {
		t.Fatal("ScimTokenLive does not match the token's secret")
	}
	var got WorkforcePoolProviderScimToken
	if code := wfScimCall(t, r, "GET", tname+"/tokens/tok-one", nil, &got); code != 200 || got.SecurityToken != "" || got.DisplayName != "IdP" {
		t.Fatalf("get token = %d, %+v", code, got)
	}
	if code := wfScimCall(t, r, "POST", tname+"/tokens?workforcePoolProviderScimTokenId=tok-two", map[string]any{}, nil); code != 200 {
		t.Fatalf("second token = %d", code)
	}
	if code := wfScimCall(t, r, "POST", tname+"/tokens?workforcePoolProviderScimTokenId=tok-three", map[string]any{}, nil); code != 400 {
		t.Errorf("a third token = %d; want 400", code)
	}
	var tl struct {
		Tokens []WorkforcePoolProviderScimToken `json:"workforcePoolProviderScimTokens"`
		Next   string                           `json:"nextPageToken"`
	}
	if code := wfScimCall(t, r, "GET", tname+"/tokens?pageSize=1", nil, &tl); code != 200 || len(tl.Tokens) != 1 || tl.Next == "" || tl.Tokens[0].SecurityToken != "" {
		t.Fatalf("list tokens = %d, %+v", code, tl)
	}
	if code := wfScimCall(t, r, "DELETE", tname+"/tokens/tok-two", nil, &got); code != 200 || got.State != "DELETED" {
		t.Fatalf("delete token = %d, %+v", code, got)
	}
	if code := wfScimCall(t, r, "PATCH", tname+"/tokens/tok-two?updateMask=displayName", map[string]any{"displayName": "x"}, nil); code != 400 {
		t.Errorf("patch a deleted token = %d; want 400", code)
	}
	tl.Tokens = nil
	if code := wfScimCall(t, r, "GET", tname+"/tokens", nil, &tl); code != 200 || len(tl.Tokens) != 1 {
		t.Fatalf("list live tokens = %d, %+v", code, tl)
	}
	tl.Tokens = nil
	if code := wfScimCall(t, r, "GET", tname+"/tokens?showDeleted=true", nil, &tl); code != 200 || len(tl.Tokens) != 2 {
		t.Fatalf("list with deleted tokens = %d, %+v", code, tl)
	}

	// Soft delete, list, undelete; hard delete frees the pool.
	var del WorkforcePoolProviderScimTenant
	if code := wfScimCall(t, r, "DELETE", tname, nil, &del); code != 200 || del.State != "DELETED" || del.PurgeTime == "" {
		t.Fatalf("delete tenant = %d, %+v", code, del)
	}
	if r.ScimTokenLive("scim-pool", "prov-one", "tenant-a", secret) {
		t.Error("a deleted tenant's token is live")
	}
	if code := wfScimCall(t, r, "PATCH", tname+"?updateMask=displayName", map[string]any{"displayName": "x"}, nil); code != 400 {
		t.Errorf("patch a deleted tenant = %d; want 400", code)
	}
	var list struct {
		Tenants []WorkforcePoolProviderScimTenant `json:"workforcePoolProviderScimTenants"`
	}
	if code := wfScimCall(t, r, "GET", tenants, nil, &list); code != 200 || len(list.Tenants) != 0 {
		t.Fatalf("list = %d, %+v", code, list)
	}
	if code := wfScimCall(t, r, "GET", tenants+"?showDeleted=true", nil, &list); code != 200 || len(list.Tenants) != 1 {
		t.Fatalf("list showDeleted = %d, %+v", code, list)
	}
	if code := wfScimCall(t, r, "POST", base+"/scim-pool/providers/prov-two/scimTenants?workforcePoolProviderScimTenantId=tenant-b", good, nil); code != 400 {
		t.Errorf("a new tenant while one is soft-deleted = %d; want 400", code)
	}
	if code := wfScimCall(t, r, "POST", tname+":undelete", map[string]any{}, &del); code != 200 || del.State != "ACTIVE" {
		t.Fatalf("undelete = %d, %+v", code, del)
	}
	if code := wfScimCall(t, r, "POST", tname+":undelete", map[string]any{}, nil); code != 400 {
		t.Errorf("undelete a live tenant = %d; want 400", code)
	}
	if code := wfScimCall(t, r, "DELETE", tname+"?hardDelete=true", nil, nil); code != 200 {
		t.Fatalf("hard delete = %d", code)
	}
	if code := wfScimCall(t, r, "GET", tname, nil, nil); code != 404 {
		t.Errorf("get after hard delete = %d; want 404", code)
	}
	if code := wfScimCall(t, r, "GET", tname+"/tokens/tok-one", nil, nil); code != 404 {
		t.Errorf("a hard-deleted tenant's token = %d; want 404", code)
	}
	if code := wfScimCall(t, r, "POST", base+"/scim-pool/providers/prov-two/scimTenants?workforcePoolProviderScimTenantId=tenant-b", good, nil); code != 200 {
		t.Errorf("a new tenant after the hard delete = %d; want 200", code)
	}
	// A deleted provider's tenants are still listed; no new ones.
	if code := wfScimCall(t, r, "DELETE", base+"/scim-pool/providers/prov-two", nil, nil); code != 200 {
		t.Fatalf("delete provider = %d", code)
	}
	list.Tenants = nil
	if code := wfScimCall(t, r, "GET", base+"/scim-pool/providers/prov-two/scimTenants", nil, &list); code != 200 || len(list.Tenants) != 1 {
		t.Fatalf("list under a deleted provider = %d, %+v", code, list)
	}
}
