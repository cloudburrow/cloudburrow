package resourcemanager

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// scimFixture is a pool with a provider, its SCIM tenant and a token.
type scimFixture struct {
	t      *testing.T
	r      *Registry
	base   string // the tenant's endpoint path
	secret string
}

func newScimFixture(t *testing.T, usage string) *scimFixture {
	t.Helper()
	r := New(store.NewMemory())
	pools := "/v1/locations/global/workforcePools"
	if code := wfScimCall(t, r, "POST", pools+"?workforcePoolId=sppool", map[string]any{"parent": OrganizationName}, nil); code != 200 {
		t.Fatalf("create pool = %d", code)
	}
	prov := map[string]any{"attributeMapping": map[string]string{"google.subject": "assertion.sub"}, "scimUsage": usage,
		"oidc": map[string]any{"issuerUri": "https://idp.example.test", "clientId": "c", "webSsoConfig": map[string]string{"responseType": "ID_TOKEN", "assertionClaimsBehavior": "ONLY_ID_TOKEN_CLAIMS"}}}
	if code := wfScimCall(t, r, "POST", pools+"/sppool/providers?workforcePoolProviderId=staff-idp", prov, nil); code != 200 {
		t.Fatalf("create provider = %d", code)
	}
	var tenant WorkforcePoolProviderScimTenant
	tn := pools + "/sppool/providers/staff-idp/scimTenants"
	if code := wfScimCall(t, r, "POST", tn+"?workforcePoolProviderScimTenantId=okta", map[string]any{"claimMapping": map[string]string{
		"google.subject": "user.externalId", "google.group": "group.displayName", "attribute.dept": "user.title"}}, &tenant); code != 200 {
		t.Fatalf("create tenant = %d", code)
	}
	var tok WorkforcePoolProviderScimToken
	if code := wfScimCall(t, r, "POST", tn+"/okta/tokens?workforcePoolProviderScimTokenId=tok-one", map[string]any{}, &tok); code != 200 {
		t.Fatalf("create token = %d", code)
	}
	return &scimFixture{t: t, r: r, base: strings.TrimPrefix(tenant.BaseURI, "https://iamscim.googleapis.com"), secret: tok.SecurityToken}
}

func (f *scimFixture) call(method, path string, body any, out any) int {
	f.t.Helper()
	return f.callAs(f.secret, method, path, body, out)
}

func (f *scimFixture) callAs(secret, method, path string, body any, out any) int {
	f.t.Helper()
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, f.base+path, bytes.NewReader(b))
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	rec := httptest.NewRecorder()
	f.r.ServeScim(rec, req)
	if out != nil && rec.Code < 300 {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			f.t.Fatalf("%s %s: decode %q: %v", method, path, rec.Body.String(), err)
		}
	}
	if out != nil && rec.Code >= 400 {
		_ = json.Unmarshal(rec.Body.Bytes(), out)
	}
	return rec.Code
}

func scimUser(ext, userName string) map[string]any {
	return map[string]any{"schemas": []string{scimUserSchema}, "externalId": ext, "userName": userName, "title": "eng",
		"emails": []any{map[string]any{"value": userName, "type": "work", "primary": true}}}
}

type scimList struct {
	TotalResults int              `json:"totalResults"`
	Resources    []map[string]any `json:"Resources"`
}

// The SCIM endpoint (#2068): bearer-token auth, Users and Groups with
// Google's claim-mapping rules, eq/and filters, PATCH, group members only
// in member-filtered lists, ServiceProviderConfig and Schemas, and the
// endpoints Google does not support.
func TestWorkforceScimEndpoint(t *testing.T) {
	f := newScimFixture(t, scimUsageGroups)
	for what, secret := range map[string]string{"no token": "", "a wrong token": "nope"} {
		req := httptest.NewRequest("GET", f.base+"/Users", nil)
		if secret != "" {
			req.Header.Set("Authorization", "Bearer "+secret)
		}
		rec := httptest.NewRecorder()
		f.r.ServeScim(rec, req)
		if rec.Code != 401 || strings.Contains(rec.Header().Get("Content-Type"), "json") {
			t.Errorf("%s = %d %q; want 401, not JSON", what, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
	other := httptest.NewRequest("GET", scimPathPrefix+"0123456789abcdef/Users", nil)
	other.Header.Set("Authorization", "Bearer "+f.secret)
	orec := httptest.NewRecorder()
	f.r.ServeScim(orec, other)
	if orec.Code != 401 {
		t.Errorf("the token at another tenant's base = %d; want 401", orec.Code)
	}

	var alice map[string]any
	if code := f.call("POST", "/Users", scimUser("u-alice", "alice@example.test"), &alice); code != 201 {
		t.Fatalf("create user = %d %v", code, alice)
	}
	aliceID, _ := alice["id"].(string)
	meta, _ := alice["meta"].(map[string]any)
	if aliceID == "" || meta["resourceType"] != "User" || !strings.HasSuffix(meta["location"].(string), "/Users/"+aliceID) || alice["active"] != true {
		t.Fatalf("created user = %v", alice)
	}
	var e map[string]any
	if code := f.call("POST", "/Users", scimUser("u-alice", "other@example.test"), &e); code != 409 || e["scimType"] != "uniqueness" {
		t.Errorf("a duplicate google.subject = %d %v; want 409 uniqueness", code, e)
	}
	noExt := scimUser("", "bob@example.test")
	delete(noExt, "externalId")
	if code := f.call("POST", "/Users", noExt, nil); code != 400 {
		t.Errorf("an empty google.subject = %d; want 400", code)
	}
	two := scimUser("u-two", "two@example.test")
	two["emails"] = []any{map[string]any{"value": "a@x", "type": "work"}, map[string]any{"value": "b@x", "type": "work"}}
	if code := f.call("POST", "/Users", two, nil); code != 400 {
		t.Errorf("two emails = %d; want 400", code)
	}
	home := scimUser("u-home", "home@example.test")
	home["emails"] = []any{map[string]any{"value": "a@x", "type": "home"}}
	if code := f.call("POST", "/Users", home, nil); code != 400 {
		t.Errorf("a home email = %d; want 400", code)
	}
	pw := scimUser("u-bob", "Bob@Example.test")
	pw["password"] = "hunter2"
	var bob map[string]any
	if code := f.call("POST", "/Users", pw, &bob); code != 201 || bob["password"] != nil {
		t.Fatalf("create bob = %d %v; want no password", code, bob)
	}
	bobID := bob["id"].(string)

	var l scimList
	q := func(filter string) string { return "/Users?filter=" + url.QueryEscape(filter) }
	if code := f.call("GET", q(`userName eq "bob@example.test"`), nil, &l); code != 200 || l.TotalResults != 1 || l.Resources[0]["id"] != bobID {
		t.Errorf("userName eq (case-insensitive) = %d %+v", code, l)
	}
	if code := f.call("GET", q(`externalId eq "U-ALICE"`), nil, &l); code != 200 || l.TotalResults != 0 {
		t.Errorf("externalId eq compares case-exactly = %d %+v", code, l)
	}
	if code := f.call("GET", q(`emails.value eq "alice@example.test" and active eq true`), nil, &l); code != 200 || l.TotalResults != 1 {
		t.Errorf("eq and eq = %d %+v", code, l)
	}
	for _, bad := range []string{`userName co "a"`, `userName sw "a"`, `userName eq "a" or userName eq "b"`, `userName pr`} {
		if code := f.call("GET", q(bad), nil, &e); code != 400 || e["scimType"] != "invalidFilter" {
			t.Errorf("filter %s = %d %v; want 400 invalidFilter", bad, code, e)
		}
	}
	if code := f.call("GET", "/Users?startIndex=2&count=1", nil, &l); code != 200 || l.TotalResults != 1 || len(l.Resources) != 1 {
		t.Errorf("startIndex ignored, count honoured = %d %+v", code, l)
	}

	// PATCH.
	patch := func(id string, ops ...map[string]any) (int, map[string]any) {
		var out map[string]any
		code := f.call("PATCH", "/Users/"+id, map[string]any{"schemas": []string{scimPatchSchema}, "Operations": ops}, &out)
		return code, out
	}
	code, got := patch(bobID, map[string]any{"op": "Replace", "path": "active", "value": "False"},
		map[string]any{"op": "replace", "path": `emails[type eq "work"].value`, "value": "bob2@example.test"},
		map[string]any{"op": "add", "path": "name.givenName", "value": "Bob"},
		map[string]any{"op": "add", "path": scimEntSchema + ":department", "value": "R&D"})
	if code != 200 || got["active"] != false || got["name"].(map[string]any)["givenName"] != "Bob" ||
		got["emails"].([]any)[0].(map[string]any)["value"] != "bob2@example.test" || got[scimEntSchema].(map[string]any)["department"] != "R&D" {
		t.Fatalf("patch = %d %v", code, got)
	}
	if code, got = patch(bobID, map[string]any{"op": "replace", "value": map[string]any{"active": true, "displayName": "Bob B"}}); code != 200 || got["active"] != true || got["displayName"] != "Bob B" {
		t.Errorf("patch without a path = %d %v", code, got)
	}
	if code, got = patch(bobID, map[string]any{"op": "remove", "path": "displayName"}); code != 200 || got["displayName"] != nil {
		t.Errorf("patch remove = %d %v", code, got)
	}
	if code, got = patch(bobID, map[string]any{"op": "replace", "path": "externalId", "value": "u-robert"}); code != 400 || got["scimType"] != "mutability" {
		t.Errorf("patch google.subject = %d %v; want 400 mutability", code, got)
	}
	if code, _ = patch(bobID, map[string]any{"op": "remove"}); code != 400 {
		t.Errorf("remove without a path = %d; want 400", code)
	}
	var put map[string]any
	repl := scimUser("u-bob", "bob@example.test")
	repl["displayName"] = "Robert"
	if code := f.call("PUT", "/Users/"+bobID, repl, &put); code != 200 || put["displayName"] != "Robert" || put["id"] != bobID {
		t.Errorf("PUT = %d %v", code, put)
	}

	// Groups.
	var g map[string]any
	group := map[string]any{"schemas": []string{scimGroupSchema}, "displayName": "auditors", "members": []any{map[string]any{"value": aliceID}}}
	if code := f.call("POST", "/Groups", group, &g); code != 201 {
		t.Fatalf("create group = %d %v", code, g)
	}
	gid := g["id"].(string)
	if code := f.call("POST", "/Groups", group, nil); code != 409 {
		t.Errorf("a duplicate google.group = %d; want 409", code)
	}
	bad := map[string]any{"schemas": []string{scimGroupSchema}, "displayName": "x", "members": []any{map[string]any{"value": "nobody"}}}
	if code := f.call("POST", "/Groups", bad, nil); code != 400 {
		t.Errorf("an unknown member = %d; want 400", code)
	}
	if code := f.call("GET", "/Groups/"+gid, nil, &g); code != 200 || len(g["members"].([]any)) != 0 {
		t.Errorf("get group = %d %v; want empty members", code, g)
	}
	if code := f.call("GET", "/Groups", nil, &l); code != 200 || len(l.Resources) != 1 || len(l.Resources[0]["members"].([]any)) != 0 {
		t.Errorf("list groups = %d %+v; want empty members", code, l)
	}
	mf := "/Groups?filter=" + url.QueryEscape(`id eq "`+gid+`" and members.value eq "`+aliceID+`"`)
	if code := f.call("GET", mf, nil, &l); code != 200 || len(l.Resources) != 1 || len(l.Resources[0]["members"].([]any)) != 1 {
		t.Errorf("list groups by member = %d %+v; want the member", code, l)
	}
	if code := f.call("PUT", "/Groups/"+gid, group, nil); code != 501 {
		t.Errorf("PUT a group = %d; want 501", code)
	}
	var gp map[string]any
	if code := f.call("PATCH", "/Groups/"+gid, map[string]any{"schemas": []string{scimPatchSchema}, "Operations": []any{
		map[string]any{"op": "Add", "path": "members", "value": []any{map[string]any{"value": bobID}}}}}, &gp); code != 200 {
		t.Fatalf("patch group add member = %d %v", code, gp)
	}
	if code := f.call("GET", "/Groups?filter="+url.QueryEscape(`members.value eq "`+bobID+`"`), nil, &l); code != 200 || len(l.Resources) != 1 ||
		len(l.Resources[0]["members"].([]any)) != 2 {
		t.Errorf("after adding bob = %d %+v", code, l)
	}

	// Membership in IAM: ENABLED_FOR_GROUPS reads SCIM's groups, not the token's.
	principal := "principal://iam.googleapis.com/locations/global/workforcePools/sppool/subject/u-alice"
	tokenGroup := "principalSet://iam.googleapis.com/locations/global/workforcePools/sppool/group/from-token"
	scimGroup := "principalSet://iam.googleapis.com/locations/global/workforcePools/sppool/group/auditors"
	if err := f.r.RecordFederatedPrincipal(principal, []string{tokenGroup}); err != nil {
		t.Fatal(err)
	}
	if got := f.r.federatedPrincipals(principal); !slices.Contains(got, scimGroup) || slices.Contains(got, tokenGroup) {
		t.Errorf("principals = %v; want the SCIM group only", got)
	}
	if subs, _ := f.r.WorkforcePoolSubjects("sppool"); len(subs) == 0 {
		t.Error("the provisioned user is not a pool subject")
	}
	if code := f.call("DELETE", "/Users/"+aliceID, nil, nil); code != 204 {
		t.Fatalf("delete user = %d", code)
	}
	if got := f.r.federatedPrincipals(principal); slices.Contains(got, scimGroup) {
		t.Errorf("principals after deprovisioning = %v; want no SCIM group", got)
	}
	if code := f.call("GET", "/Users/"+aliceID, nil, nil); code != 404 {
		t.Errorf("get a deleted user = %d; want 404", code)
	}

	// The rest of the protocol.
	var spc map[string]any
	if code := f.call("GET", "/ServiceProviderConfig", nil, &spc); code != 200 || spc["patch"].(map[string]any)["supported"] != true ||
		spc["bulk"].(map[string]any)["supported"] != false || spc["filter"].(map[string]any)["maxResults"].(float64) != 100 {
		t.Errorf("ServiceProviderConfig = %d %v", code, spc)
	}
	if code := f.call("GET", "/Schemas", nil, &l); code != 200 || l.TotalResults != 3 {
		t.Errorf("Schemas = %d %+v", code, l)
	}
	var sch map[string]any
	if code := f.call("GET", "/Schemas/"+scimUserSchema, nil, &sch); code != 200 || sch["name"] != "User" {
		t.Errorf("Schemas/User = %d %v", code, sch)
	}
	for _, p := range []string{"/ResourceTypes", "/Me", "/Bulk"} {
		if code := f.call("GET", p, nil, nil); code != 501 {
			t.Errorf("%s = %d; want 501", p, code)
		}
	}

	// A deleted token, then a deleted tenant, authenticate nothing.
	if code := wfScimCall(t, f.r, "DELETE", "/v1/locations/global/workforcePools/sppool/providers/staff-idp/scimTenants/okta/tokens/tok-one", nil, nil); code != 200 {
		t.Fatalf("delete token = %d", code)
	}
	if code := f.call("GET", "/Users", nil, nil); code != 401 {
		t.Errorf("a deleted token = %d; want 401", code)
	}
}

// ENABLED_FOR_USERS_GROUPS: a subject without an active SCIM user signs in
// to nothing; its custom attributes are SCIM's.
func TestWorkforceScimUsersGroups(t *testing.T) {
	f := newScimFixture(t, scimUsageUsersGroups)
	principal := "principal://iam.googleapis.com/locations/global/workforcePools/sppool/subject/u-carol"
	if err := f.r.WorkforceScimSignIn("sppool", "staff-idp", "u-carol"); err == nil {
		t.Error("sign-in without a SCIM user succeeded")
	}
	var carol map[string]any
	if code := f.call("POST", "/Users", scimUser("u-carol", "carol@example.test"), &carol); code != 201 {
		t.Fatalf("create = %d", code)
	}
	if err := f.r.WorkforceScimSignIn("sppool", "staff-idp", "u-carol"); err != nil {
		t.Fatalf("sign-in: %v", err)
	}
	if err := f.r.RecordFederatedPrincipal(principal, []string{"principalSet://iam.googleapis.com/locations/global/workforcePools/sppool/attribute.dept/token"}); err != nil {
		t.Fatal(err)
	}
	want := "principalSet://iam.googleapis.com/locations/global/workforcePools/sppool/attribute.dept/eng"
	if got := f.r.federatedPrincipals(principal); !slices.Contains(got, want) || len(got) != 2 {
		t.Errorf("principals = %v; want %s", got, want)
	}
	if code := f.call("PATCH", "/Users/"+carol["id"].(string), map[string]any{"schemas": []string{scimPatchSchema},
		"Operations": []any{map[string]any{"op": "replace", "path": "active", "value": false}}}, nil); code != 200 {
		t.Fatalf("deactivate = %d", code)
	}
	if got := f.r.federatedPrincipals(principal); got != nil {
		t.Errorf("principals of a deactivated user = %v; want none", got)
	}
}

// scimUsage on a provider other than the tenant's is refused, and a
// provider using SCIM without a tenant fails sign-in.
func TestWorkforceScimUsageProvider(t *testing.T) {
	f := newScimFixture(t, "")
	pools := "/v1/locations/global/workforcePools"
	prov := map[string]any{"attributeMapping": map[string]string{"google.subject": "assertion.sub"}, "scimUsage": scimUsageGroups,
		"oidc": map[string]any{"issuerUri": "https://idp2.example.test", "clientId": "c", "webSsoConfig": map[string]string{"responseType": "ID_TOKEN", "assertionClaimsBehavior": "ONLY_ID_TOKEN_CLAIMS"}}}
	if code := wfScimCall(t, f.r, "POST", pools+"/sppool/providers?workforcePoolProviderId=idp-two", prov, nil); code != 400 {
		t.Errorf("scimUsage on another provider = %d; want 400", code)
	}
	if code := wfScimCall(t, f.r, "PATCH", pools+"/sppool/providers/staff-idp?updateMask=scimUsage", map[string]any{"scimUsage": "BOGUS"}, nil); code != 400 {
		t.Errorf("an unknown scimUsage = %d; want 400", code)
	}
	if code := wfScimCall(t, f.r, "DELETE", pools+"/sppool/providers/staff-idp/scimTenants/okta?hardDelete=true", nil, nil); code != 200 {
		t.Fatalf("hard delete = %d", code)
	}
	if code := wfScimCall(t, f.r, "PATCH", pools+"/sppool/providers/staff-idp?updateMask=scimUsage", map[string]any{"scimUsage": scimUsageGroups}, nil); code != 200 {
		t.Fatalf("enable scimUsage = %d", code)
	}
	if err := f.r.WorkforceScimSignIn("sppool", "staff-idp", "anyone"); err == nil {
		t.Error("sign-in through a SCIM provider without a tenant succeeded")
	}
}
