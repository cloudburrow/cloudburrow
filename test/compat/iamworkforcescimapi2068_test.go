//go:build compat

package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	iamv1 "google.golang.org/api/iam/v1"
)

// scimClient is a SCIM 2.0 client of a tenant's local endpoint: Google
// ships no SCIM SDK (identity providers such as Okta and Entra ID are the
// clients), so this speaks RFC 7644 over net/http, and refuses any base
// that is not this instance's loopback address.
type scimClient struct {
	t     *testing.T
	base  string
	token string
}

func newScimClient(t *testing.T, base, token string) *scimClient {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "http" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") {
		t.Fatalf("the SCIM base %q is not a local endpoint", base)
	}
	return &scimClient{t: t, base: base, token: token}
}

func (c *scimClient) do(method, path string, body any, out any) int {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		c.t.Fatal(err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Content-Type", "application/scim+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil && len(b) > 0 && resp.StatusCode < 300 {
		if err := json.Unmarshal(b, out); err != nil {
			c.t.Fatalf("%s %s: decode %q: %v", method, path, b, err)
		}
	}
	return resp.StatusCode
}

// covers: iam.locations.workforcePools.providers.patch
//
// TestWorkforceScimProvisioning (#2068), on an enforcing instance: a
// workforce pool's OIDC provider gets a SCIM tenant and a token through
// google.golang.org/api/iam/v1, and scimUsage ENABLED_FOR_GROUPS; over the
// tenant's SCIM endpoint (RFC 7644, authenticated by the token) a user and
// a group holding it are provisioned; a bad or missing token is 401. The
// official Cloud Storage client, with a token golang.org/x/oauth2/google
// exchanges at the local STS from that user's OIDC token (which names no
// such group), reads an object granted roles/storage.objectViewer through
// the group's principalSet://.../group/G binding; once the user is
// deprovisioned (DELETE /Users/ID) the same token is refused (403), and a
// group the token names but SCIM does not grants nothing.
func TestWorkforceScimProvisioning(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvStorage)
	rm := h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	s := newWFSetup(t, h, "wfsc-", true)
	pools := iamREST(t, h).Locations.WorkforcePools
	prov := s.poolName + "/providers/staff-idp"
	tenants := pools.Providers.ScimTenants
	tenant, err := tenants.Create(prov, &iamv1.WorkforcePoolProviderScimTenant{DisplayName: "IdP", ClaimMapping: map[string]string{
		"google.subject": "user.externalId", "google.group": "group.displayName"}}).WorkforcePoolProviderScimTenantId("scim-idp").Context(ctx).Do()
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _, _ = tenants.Delete(tenant.Name).HardDelete(true).Context(context.Background()).Do() })
	tok, err := tenants.Tokens.Create(tenant.Name, &iamv1.WorkforcePoolProviderScimToken{DisplayName: "IdP"}).
		WorkforcePoolProviderScimTokenId("idp-token").Context(ctx).Do()
	if err != nil || tok.SecurityToken == "" {
		t.Fatalf("create token: %v", err)
	}
	if got, err := pools.Providers.Patch(prov, &iamv1.WorkforcePoolProvider{ScimUsage: "ENABLED_FOR_GROUPS"}).UpdateMask("scimUsage").Context(ctx).Do(); err != nil {
		t.Fatalf("enable scimUsage: %v", err)
	} else if got.Response != nil {
		var p iamv1.WorkforcePoolProvider
		if json.Unmarshal(got.Response, &p) == nil && p.ScimUsage != "ENABLED_FOR_GROUPS" {
			t.Fatalf("provider scimUsage = %q", p.ScimUsage)
		}
	}

	uid := strings.TrimPrefix(tenant.BaseUri, "https://iamscim.googleapis.com/v1alpha1/tenants/")
	base := "http://" + strings.TrimPrefix(rm, "http://") + "/v1alpha1/tenants/" + uid
	scim := newScimClient(t, base, tok.SecurityToken)
	for what, c := range map[string]*scimClient{"no token": newScimClient(t, base, ""), "a wrong token": newScimClient(t, base, "not-a-scim-token")} {
		if code := c.do("GET", "/Users", nil, nil); code != http.StatusUnauthorized {
			t.Errorf("%s = %d; want 401", what, code)
		}
	}
	var spc map[string]any
	if code := scim.do("GET", "/ServiceProviderConfig", nil, &spc); code != 200 || spc["patch"] == nil {
		t.Fatalf("ServiceProviderConfig = %d %v", code, spc)
	}

	sub := "erin-" + strings.TrimPrefix(s.pool, "wfsc-")
	var user map[string]any
	if code := scim.do("POST", "/Users", map[string]any{"schemas": []string{"urn:ietf:params:scim:schemas:core:2.0:User"},
		"externalId": sub, "userName": "erin@example.test", "active": true,
		"emails": []any{map[string]any{"value": "erin@example.test", "type": "work", "primary": true}}}, &user); code != http.StatusCreated {
		t.Fatalf("provision user = %d", code)
	}
	userID, _ := user["id"].(string)
	var group map[string]any
	if code := scim.do("POST", "/Groups", map[string]any{"schemas": []string{"urn:ietf:params:scim:schemas:core:2.0:Group"},
		"displayName": "scim-readers", "members": []any{map[string]any{"value": userID}}}, &group); code != http.StatusCreated {
		t.Fatalf("provision group = %d", code)
	}
	var list struct {
		TotalResults int              `json:"totalResults"`
		Resources    []map[string]any `json:"Resources"`
	}
	if code := scim.do("GET", "/Users?filter="+url.QueryEscape(`externalId eq "`+sub+`"`), nil, &list); code != 200 || list.TotalResults != 1 {
		t.Fatalf("filter users = %d %+v", code, list)
	}

	owner := storageClient(t, h)
	bucket := s.project + "-scim"
	bh := owner.Bucket(bucket)
	if err := bh.Create(ctx, s.project, &storage.BucketAttrs{UniformBucketLevelAccess: storage.UniformBucketLevelAccess{Enabled: true}}); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { _ = bh.Object("a.txt").Delete(context.Background()); _ = bh.Delete(context.Background()) })
	w := bh.Object("a.txt").NewWriter(ctx)
	_, _ = w.Write([]byte("hello"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	setRole := func(member string, add bool) {
		t.Helper()
		pol, err := bh.IAM().Policy(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if add {
			pol.Add(member, "roles/storage.objectViewer")
		} else {
			pol.Remove(member, "roles/storage.objectViewer")
		}
		if err := bh.IAM().SetPolicy(ctx, pol); err != nil {
			t.Fatalf("setIamPolicy %s: %v", member, err)
		}
	}
	// The sign-in token names the group "token-only", which SCIM does not
	// have: with ENABLED_FOR_GROUPS it grants nothing.
	setRole(s.set("group/token-only"), true)
	setRole(s.set("group/scim-readers"), true)
	t.Cleanup(func() { setRole(s.set("group/scim-readers"), false); setRole(s.set("group/token-only"), false) })

	cfg := s.credentialConfig(t, h, s.token(t, sub, "eng", "staff", "token-only"), s.project)
	access, err := oauth2Token(t, ctx, cfg)
	if err != nil {
		t.Fatalf("workforce token for the provisioned user: %v", err)
	}
	as := newStorageAs(t, h, access)
	read := func() error {
		got, err := storageRead(t, h, as.json, bucket, "a.txt")
		if err == nil && got != "hello" {
			return errors.New("read " + got)
		}
		return err
	}
	waitGranted(t, "read through the SCIM group's principalSet://", read)

	if code := scim.do("DELETE", "/Users/"+userID, nil, nil); code != http.StatusNoContent {
		t.Fatalf("deprovision user = %d", code)
	}
	var ge *googleapi.Error
	refused := false
	for i := 0; i < 10 && !refused; i++ {
		err := read()
		refused = errors.As(err, &ge) && ge.Code == http.StatusForbidden
		if !refused {
			time.Sleep(200 * time.Millisecond)
		}
	}
	if !refused {
		t.Fatalf("read after deprovisioning = %v; want 403", read())
	}
	if code := scim.do("GET", "/Users/"+userID, nil, nil); code != http.StatusNotFound {
		t.Errorf("get the deprovisioned user = %d; want 404", code)
	}
}
