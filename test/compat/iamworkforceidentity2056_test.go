//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	iam "cloud.google.com/go/iam/apiv2"
	iamv2 "cloud.google.com/go/iam/apiv2/iampb"
	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	iamv1 "google.golang.org/api/iam/v1"
	"google.golang.org/api/option"
	stsv1 "google.golang.org/api/sts/v1"
)

// wfWebSSO is the web sign-in configuration every OIDC provider needs.
var wfWebSSO = &iamv1.GoogleIamAdminV1WorkforcePoolProviderOidcWebSsoConfig{ResponseType: "ID_TOKEN", AssertionClaimsBehavior: "ONLY_ID_TOKEN_CLAIMS"}

// wfPoolID is a pool ID for this run: workforce pool IDs are global, and a
// deleted one is not reusable for 30 days.
func wfPoolID(h *Harness, prefix string) string {
	id := strings.ToLower(v1ID(h, prefix))
	if len(id) < 6 {
		id += "-pool"
	}
	return id
}

// covers: iam.locations.workforcePools.create, iam.locations.workforcePools.get, iam.locations.workforcePools.list, iam.locations.workforcePools.patch, iam.locations.workforcePools.delete, iam.locations.workforcePools.undelete, iam.locations.workforcePools.operations.get
// covers: iam.locations.workforcePools.providers.create, iam.locations.workforcePools.providers.get, iam.locations.workforcePools.providers.list, iam.locations.workforcePools.providers.patch, iam.locations.workforcePools.providers.delete, iam.locations.workforcePools.providers.undelete, iam.locations.workforcePools.providers.operations.get
// covers: iam.locations.workforcePools.providers.keys.create, iam.locations.workforcePools.providers.keys.get, iam.locations.workforcePools.providers.keys.list, iam.locations.workforcePools.providers.keys.delete, iam.locations.workforcePools.providers.keys.undelete, iam.locations.workforcePools.providers.keys.operations.get
//
// TestWorkforcePools (#2056), with google.golang.org/api/iam/v1: a pool is
// created under the organization (a done Operation, named
// locations/global/workforcePools/ID, sessionDuration 3600s by default),
// read, listed by the organization, patched, soft-deleted (DELETED with an
// expireTime, listed only with showDeleted, its ID not reusable) and
// undeleted, with Google's validation (an ID shorter than 6 or not
// starting with a letter, gcp-, no parent, another organization, a
// session over 12 hours); OIDC and SAML providers likewise (no clientId,
// no webSsoConfig, ID_TOKEN with merged claims, no google.subject, an
// invalid CEL condition; a client secret is never returned); a SAML
// provider's encryption key is created, listed, deleted and undeleted.
func TestWorkforcePools(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager)
	ctx := h.Context()
	svc := iamREST(t, h)
	pools := svc.Locations.WorkforcePools
	id := wfPoolID(h, "wfp-")
	name := "locations/global/workforcePools/" + id

	for what, c := range map[string]struct {
		id   string
		pool *iamv1.WorkforcePool
		code int
	}{
		"a short ID":        {"wf-1", &iamv1.WorkforcePool{Parent: localOrgRoles}, 400},
		"a digit first":     {"1wf-pool", &iamv1.WorkforcePool{Parent: localOrgRoles}, 400},
		"gcp-":              {"gcp-pool", &iamv1.WorkforcePool{Parent: localOrgRoles}, 400},
		"no parent":         {id, &iamv1.WorkforcePool{}, 400},
		"another org":       {id, &iamv1.WorkforcePool{Parent: "organizations/42"}, 404},
		"a 13-hour session": {id, &iamv1.WorkforcePool{Parent: localOrgRoles, SessionDuration: "46800s"}, 400},
	} {
		_, err := pools.Create("locations/global", c.pool).WorkforcePoolId(c.id).Context(ctx).Do()
		wantWIFError(t, what, err, c.code)
	}
	op, err := pools.Create("locations/global", &iamv1.WorkforcePool{Parent: localOrgRoles, DisplayName: "Staff", Description: "employees"}).
		WorkforcePoolId(id).Context(ctx).Do()
	if err != nil || !op.Done {
		t.Fatalf("create pool = %v, %v", op, err)
	}
	var created iamv1.WorkforcePool
	if err := json.Unmarshal(op.Response, &created); err != nil || created.Name != name || created.State != "ACTIVE" ||
		created.Parent != localOrgRoles || created.SessionDuration != "3600s" {
		t.Fatalf("created pool = %+v, %v", created, err)
	}
	t.Cleanup(func() { _, _ = pools.Delete(name).Context(context.Background()).Do() })
	if got, err := pools.Operations.Get(op.Name).Context(ctx).Do(); err != nil || !got.Done {
		t.Fatalf("operations.get = %v, %v", got, err)
	}
	_, err = pools.Create("locations/global", &iamv1.WorkforcePool{Parent: localOrgRoles}).WorkforcePoolId(id).Context(ctx).Do()
	wantWIFError(t, "a duplicate pool", err, 409)
	if _, err := pools.Patch(name, &iamv1.WorkforcePool{DisplayName: "Staff pool", SessionDuration: "7200s"}).
		UpdateMask("displayName,sessionDuration").Context(ctx).Do(); err != nil {
		t.Fatalf("patch pool: %v", err)
	}
	if got, err := pools.Get(name).Context(ctx).Do(); err != nil || got.DisplayName != "Staff pool" || got.SessionDuration != "7200s" || got.Description != "employees" {
		t.Fatalf("get pool = %+v, %v", got, err)
	}
	_, err = pools.Get("locations/global/workforcePools/nope-pool-x").Context(ctx).Do()
	wantWIFError(t, "a missing pool", err, 404)
	_, err = pools.List("locations/global").Context(ctx).Do()
	wantWIFError(t, "list without a parent", err, 400)
	listed := func(showDeleted bool) bool {
		t.Helper()
		l, err := pools.List("locations/global").Parent(localOrgRoles).ShowDeleted(showDeleted).Context(ctx).Do()
		if err != nil {
			t.Fatalf("list pools: %v", err)
		}
		for _, p := range l.WorkforcePools {
			if p.Name == name {
				return true
			}
		}
		return false
	}
	if !listed(false) {
		t.Fatal("the pool is not listed")
	}

	provs := pools.Providers
	mapping := map[string]string{"google.subject": "assertion.sub", "google.groups": "assertion.groups", "attribute.dept": "assertion.dept"}
	oidc := func(o *iamv1.GoogleIamAdminV1WorkforcePoolProviderOidc) *iamv1.WorkforcePoolProvider {
		return &iamv1.WorkforcePoolProvider{Oidc: o, AttributeMapping: mapping}
	}
	for what, c := range map[string]*iamv1.WorkforcePoolProvider{
		"no clientId":     oidc(&iamv1.GoogleIamAdminV1WorkforcePoolProviderOidc{IssuerUri: "https://idp.example.test", WebSsoConfig: wfWebSSO}),
		"no webSsoConfig": oidc(&iamv1.GoogleIamAdminV1WorkforcePoolProviderOidc{IssuerUri: "https://idp.example.test", ClientId: "staff"}),
		"ID_TOKEN with merged claims": oidc(&iamv1.GoogleIamAdminV1WorkforcePoolProviderOidc{IssuerUri: "https://idp.example.test", ClientId: "staff",
			WebSsoConfig: &iamv1.GoogleIamAdminV1WorkforcePoolProviderOidcWebSsoConfig{ResponseType: "ID_TOKEN", AssertionClaimsBehavior: "MERGE_USER_INFO_OVER_ID_TOKEN_CLAIMS"}}),
		"an http issuer": oidc(&iamv1.GoogleIamAdminV1WorkforcePoolProviderOidc{IssuerUri: "http://idp.example.test", ClientId: "staff", WebSsoConfig: wfWebSSO}),
		"no google.subject": {Oidc: &iamv1.GoogleIamAdminV1WorkforcePoolProviderOidc{IssuerUri: "https://idp.example.test", ClientId: "staff", WebSsoConfig: wfWebSSO},
			AttributeMapping: map[string]string{"attribute.dept": "assertion.dept"}},
		"an invalid condition": {Oidc: &iamv1.GoogleIamAdminV1WorkforcePoolProviderOidc{IssuerUri: "https://idp.example.test", ClientId: "staff", WebSsoConfig: wfWebSSO},
			AttributeMapping: mapping, AttributeCondition: "attribute.dept =="},
	} {
		_, err := provs.Create(name, c).WorkforcePoolProviderId("bad-prov").Context(ctx).Do()
		wantWIFError(t, what, err, 400)
	}
	pop, err := provs.Create(name, &iamv1.WorkforcePoolProvider{DisplayName: "Staff IdP", AttributeMapping: mapping,
		AttributeCondition: "'staff' in google.groups",
		Oidc: &iamv1.GoogleIamAdminV1WorkforcePoolProviderOidc{IssuerUri: "https://idp.example.test", ClientId: "staff",
			ClientSecret: &iamv1.GoogleIamAdminV1WorkforcePoolProviderOidcClientSecret{Value: &iamv1.GoogleIamAdminV1WorkforcePoolProviderOidcClientSecretValue{PlainText: "local-test-secret"}},
			WebSsoConfig: &iamv1.GoogleIamAdminV1WorkforcePoolProviderOidcWebSsoConfig{ResponseType: "CODE", AssertionClaimsBehavior: "MERGE_USER_INFO_OVER_ID_TOKEN_CLAIMS"}}}).
		WorkforcePoolProviderId("oidc-prov").Context(ctx).Do()
	if err != nil {
		t.Fatalf("create OIDC provider: %v", err)
	}
	if got, err := provs.Operations.Get(pop.Name).Context(ctx).Do(); err != nil || !got.Done {
		t.Fatalf("providers.operations.get = %v, %v", got, err)
	}
	got, err := provs.Get(name + "/providers/oidc-prov").Context(ctx).Do()
	if err != nil || got.Oidc == nil || got.Oidc.ClientSecret == nil || got.Oidc.ClientSecret.Value == nil ||
		got.Oidc.ClientSecret.Value.PlainText != "" || got.Oidc.ClientSecret.Value.Thumbprint == "" || got.State != "ACTIVE" {
		t.Fatalf("get OIDC provider = %+v, %v; want the secret's thumbprint only", got, err)
	}
	if _, err := provs.Create(name, &iamv1.WorkforcePoolProvider{Saml: &iamv1.GoogleIamAdminV1WorkforcePoolProviderSaml{IdpMetadataXml: samlMetadata(t)},
		AttributeMapping: map[string]string{"google.subject": "assertion.subject"}}).WorkforcePoolProviderId("saml-prov").Context(ctx).Do(); err != nil {
		t.Fatalf("create SAML provider: %v", err)
	}
	if _, err := provs.Patch(name+"/providers/oidc-prov", &iamv1.WorkforcePoolProvider{Disabled: true, Description: "off"}).
		UpdateMask("disabled,description").Context(ctx).Do(); err != nil {
		t.Fatalf("patch provider: %v", err)
	}
	if l, err := provs.List(name).Context(ctx).Do(); err != nil || len(l.WorkforcePoolProviders) != 2 {
		t.Fatalf("list providers = %v, %v", l, err)
	}

	keys := provs.Keys
	saml := name + "/providers/saml-prov"
	kop, err := keys.Create(saml, &iamv1.WorkforcePoolProviderKey{Use: "ENCRYPTION", KeyData: &iamv1.KeyData{KeySpec: "RSA_2048"}}).
		WorkforcePoolProviderKeyId("enc-key1").Context(ctx).Do()
	if err != nil || !kop.Done {
		t.Fatalf("create key = %v, %v", kop, err)
	}
	if got, err := keys.Operations.Get(kop.Name).Context(ctx).Do(); err != nil || !got.Done {
		t.Fatalf("keys.operations.get = %v, %v", got, err)
	}
	if k, err := keys.Get(saml + "/keys/enc-key1").Context(ctx).Do(); err != nil || k.KeyData == nil || !strings.Contains(k.KeyData.Key, "BEGIN CERTIFICATE") {
		t.Fatalf("get key = %+v, %v", k, err)
	}
	if _, err := keys.Delete(saml + "/keys/enc-key1").Context(ctx).Do(); err != nil {
		t.Fatalf("delete key: %v", err)
	}
	if kl, err := keys.List(saml).Context(ctx).Do(); err != nil || len(kl.WorkforcePoolProviderKeys) != 0 {
		t.Fatalf("list keys after delete = %v, %v", kl, err)
	}
	if _, err := keys.Undelete(saml+"/keys/enc-key1", &iamv1.UndeleteWorkforcePoolProviderKeyRequest{}).Context(ctx).Do(); err != nil {
		t.Fatalf("undelete key: %v", err)
	}
	if _, err := provs.Delete(saml).Context(ctx).Do(); err != nil {
		t.Fatalf("delete provider: %v", err)
	}
	if l, err := provs.List(name).ShowDeleted(true).Context(ctx).Do(); err != nil || len(l.WorkforcePoolProviders) != 2 {
		t.Fatalf("list providers with showDeleted = %v, %v", l, err)
	}
	if _, err := provs.Undelete(saml, &iamv1.UndeleteWorkforcePoolProviderRequest{}).Context(ctx).Do(); err != nil {
		t.Fatalf("undelete provider: %v", err)
	}

	if _, err := pools.Delete(name).Context(ctx).Do(); err != nil {
		t.Fatalf("delete pool: %v", err)
	}
	if d, err := pools.Get(name).Context(ctx).Do(); err != nil || d.State != "DELETED" || d.ExpireTime == "" {
		t.Fatalf("get deleted pool = %+v, %v", d, err)
	}
	if listed(false) || !listed(true) {
		t.Fatal("a deleted pool is listed without showDeleted, or not with it")
	}
	_, err = pools.Create("locations/global", &iamv1.WorkforcePool{Parent: localOrgRoles}).WorkforcePoolId(id).Context(ctx).Do()
	wantWIFError(t, "a deleted pool's ID", err, 409)
	if _, err := pools.Undelete(name, &iamv1.UndeleteWorkforcePoolRequest{}).Context(ctx).Do(); err != nil {
		t.Fatalf("undelete pool: %v", err)
	}
}

// wfSetup is a workforce pool with an OIDC provider trusting a local IdP.
type wfSetup struct {
	idp                      *wifIdP
	project, pool, audience  string
	poolName, issuer, client string
}

func newWFSetup(t *testing.T, h *Harness, prefix string, withJWKS bool) *wfSetup {
	t.Helper()
	ctx := h.Context()
	s := &wfSetup{idp: newWIFIdP(t), project: iamProject(t, h, prefix), pool: wfPoolID(h, prefix), client: "staff-console"}
	pools := iamREST(t, h).Locations.WorkforcePools
	if _, err := pools.Create("locations/global", &iamv1.WorkforcePool{Parent: localOrgRoles, SessionDuration: "1800s"}).
		WorkforcePoolId(s.pool).Context(ctx).Do(); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	s.poolName = "locations/global/workforcePools/" + s.pool
	t.Cleanup(func() { _, _ = pools.Delete(s.poolName).Context(context.Background()).Do() })
	s.issuer = s.idp.srv.URL
	o := &iamv1.GoogleIamAdminV1WorkforcePoolProviderOidc{IssuerUri: s.issuer, ClientId: s.client, WebSsoConfig: wfWebSSO}
	if withJWKS {
		// The keys inline: the issuer is never fetched.
		s.issuer = "https://idp.example.test"
		o.IssuerUri, o.JwksJson = s.issuer, s.idp.jwks()
	}
	if _, err := pools.Providers.Create(s.poolName, &iamv1.WorkforcePoolProvider{Oidc: o,
		AttributeMapping:   map[string]string{"google.subject": "assertion.sub", "google.groups": "assertion.groups", "attribute.dept": "assertion.dept"},
		AttributeCondition: "'staff' in google.groups",
	}).WorkforcePoolProviderId("staff-idp").Context(ctx).Do(); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	s.audience = "//iam.googleapis.com/" + s.poolName + "/providers/staff-idp"
	return s
}

func (s *wfSetup) token(t *testing.T, sub, dept string, groups ...string) string {
	return s.idp.token(t, s.issuer, s.client, sub, map[string]any{"groups": groups, "dept": dept})
}

func (s *wfSetup) principal(sub string) string {
	return "principal://iam.googleapis.com/" + s.poolName + "/subject/" + sub
}

func (s *wfSetup) set(suffix string) string {
	return "principalSet://iam.googleapis.com/" + s.poolName + "/" + suffix
}

// credentialConfig writes Google's external_account configuration for a
// workforce pool, file-sourced, with workforce_pool_user_project, naming
// this instance's STS. Every URL is local.
func (s *wfSetup) credentialConfig(t *testing.T, h *Harness, jwt, userProject string) string {
	t.Helper()
	meta := metadataURL(h)
	if !strings.HasPrefix(meta, "http://127.0.0.1") && !strings.HasPrefix(meta, "http://localhost") {
		t.Fatalf("the metadata endpoint %q is not local", meta)
	}
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token.jwt")
	if err := os.WriteFile(tokenFile, []byte(jwt), 0o600); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{
		"type":                        "external_account",
		"audience":                    s.audience,
		"subject_token_type":          "urn:ietf:params:oauth:token-type:id_token",
		"token_url":                   meta + "/v1/token",
		"workforce_pool_user_project": userProject,
		"credential_source":           map[string]string{"file": tokenFile},
	})
	path := filepath.Join(dir, "credential-config.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// covers: sts.token
//
// TestWorkforceIdentityFederationSTS (#2056): the official STS client
// (google.golang.org/api/sts/v1) exchanges a token from a local IdP for a
// workforce provider's audience, the keys fetched from the issuer's
// loopback discovery document, for the pool's session duration; it refuses
// a token whose aud is not the provider's client ID (invalid_grant), one
// the attribute condition rejects (invalid_grant), an unknown provider
// (invalid_target), an options.userProject that names no project
// (invalid_request), and every exchange while the pool's
// accessRestrictions.disableProgrammaticSignin is set;
// golang.org/x/oauth2/google and cloud.google.com/go/auth read Google's
// external_account configuration with workforce_pool_user_project and get
// a token through the local STS.
func TestWorkforceIdentityFederationSTS(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	ctx := h.Context()
	s := newWFSetup(t, h, "wfs-", false)
	sts, err := stsv1.NewService(ctx, option.WithEndpoint(metadataURL(h)+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	req := func(tok, aud, options string) *stsv1.GoogleIdentityStsV1ExchangeTokenRequest {
		return &stsv1.GoogleIdentityStsV1ExchangeTokenRequest{GrantType: "urn:ietf:params:oauth:grant-type:token-exchange",
			RequestedTokenType: "urn:ietf:params:oauth:token-type:access_token", SubjectTokenType: "urn:ietf:params:oauth:token-type:id_token",
			Audience: aud, Scope: "https://www.googleapis.com/auth/cloud-platform", SubjectToken: tok, Options: options}
	}
	jwt := s.token(t, "alice", "eng", "staff")
	userProject := `{"userProject":"` + s.project + `"}`
	resp, err := sts.V1.Token(req(jwt, s.audience, userProject)).Context(ctx).Do()
	if err != nil || resp.AccessToken == "" || resp.TokenType != "Bearer" || resp.ExpiresIn != 1800 {
		t.Fatalf("sts.token = %+v, %v; want a Bearer token for the pool's 1800s session", resp, err)
	}
	wantSTSError := func(what string, err error, code int, kind string) {
		t.Helper()
		var ge *googleapi.Error
		if !errors.As(err, &ge) || ge.Code != code || !strings.Contains(ge.Body, `"error":"`+kind+`"`) {
			t.Fatalf("%s = %v; want %d %s", what, err, code, kind)
		}
	}
	_, err = sts.V1.Token(req(s.idp.token(t, s.issuer, "another-client", "alice", map[string]any{"groups": []string{"staff"}}), s.audience, "")).Context(ctx).Do()
	wantSTSError("another client ID", err, 400, "invalid_grant")
	_, err = sts.V1.Token(req(s.token(t, "bob", "eng", "contractors"), s.audience, "")).Context(ctx).Do()
	wantSTSError("the attribute condition", err, 400, "invalid_grant")
	_, err = sts.V1.Token(req(jwt, strings.TrimSuffix(s.audience, "staff-idp")+"no-idp", "")).Context(ctx).Do()
	wantSTSError("an unknown provider", err, 400, "invalid_target")
	_, err = sts.V1.Token(req(jwt, s.audience, `{"userProject":"no-such-project-x"}`)).Context(ctx).Do()
	wantSTSError("an unknown userProject", err, 400, "invalid_request")

	cfg := s.credentialConfig(t, h, jwt, s.project)
	if tok, err := oauth2Token(t, ctx, cfg); err != nil || tok == "" {
		t.Fatalf("x/oauth2 workforce token = %q, %v", tok, err)
	}
	if tok, err := authToken(t, ctx, cfg); err != nil || tok == "" {
		t.Fatalf("cloud.google.com/go/auth workforce token = %q, %v", tok, err)
	}

	pools := iamREST(t, h).Locations.WorkforcePools
	if _, err := pools.Patch(s.poolName, &iamv1.WorkforcePool{AccessRestrictions: &iamv1.AccessRestrictions{DisableProgrammaticSignin: true}}).
		UpdateMask("accessRestrictions.disableProgrammaticSignin").Context(ctx).Do(); err != nil {
		t.Fatalf("disable programmatic sign-in: %v", err)
	}
	_, err = sts.V1.Token(req(jwt, s.audience, "")).Context(ctx).Do()
	wantSTSError("programmatic sign-in disabled", err, 400, "invalid_request")
}

// covers: iam.locations.workforcePools.subjects.delete, iam.locations.workforcePools.subjects.undelete, iam.locations.workforcePools.subjects.revokeSessions, iam.locations.workforcePools.subjects.operations.get, storage.objects.get
//
// TestWorkforceIdentityFederationStorage (#2056), with IAM enforcement on
// and the official Cloud Storage client: a workforce token from Google's
// external_account configuration (keys inline in jwksJson) reads nothing
// without a role, with Google's 403 naming its principal://; the subject's
// principal:// granted roles/storage.objectViewer on the bucket reads, and
// revoked refuses again; the principalSet:// of its group granted reads; a
// deny policy on the project denying storage.googleapis.com/objects.get to
// the principalSet:// of its attribute.dept refuses it, and removed reads
// again; revokeSessions makes the token grant nothing (401) and a new
// exchange reads; a deleted subject exchanges no token (invalid_grant)
// until undeleted; a disabled pool's token is refused (401).
func TestWorkforceIdentityFederationStorage(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvStorage)
	rmEndpoint := h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	s := newWFSetup(t, h, "wfg-", true)
	owner := storageClient(t, h)
	bucket := s.project + "-wf"
	bh := owner.Bucket(bucket)
	if err := bh.Create(ctx, s.project, &storage.BucketAttrs{UniformBucketLevelAccess: storage.UniformBucketLevelAccess{Enabled: true}}); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { _ = bh.Object("a.txt").Delete(ctx); _ = bh.Delete(ctx) })
	w := bh.Object("a.txt").NewWriter(ctx)
	_, _ = w.Write([]byte("hello"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	jwt := s.token(t, "carol@example.test", "eng", "staff", "auditors")
	cfg := s.credentialConfig(t, h, jwt, s.project)
	exchange := func() (string, error) { return oauth2Token(t, ctx, cfg) }
	tok, err := exchange()
	if err != nil {
		t.Fatalf("workforce token: %v", err)
	}
	as := newStorageAs(t, h, tok)
	principal := s.principal("carol@example.test")
	readWith := func(c *storage.Client) func() error {
		return func() error {
			got, err := storageRead(t, h, c, bucket, "a.txt")
			if err == nil && got != "hello" {
				return fmt.Errorf("read %q", got)
			}
			return err
		}
	}
	read := readWith(as.json)
	var ge *googleapi.Error
	if err := read(); !errors.As(err, &ge) || ge.Code != http.StatusForbidden || !strings.Contains(ge.Body, principal+" does not have storage.objects.get access") {
		t.Fatalf("read without a role = %v; want 403 naming %s", err, principal)
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
	setRole(principal, true)
	waitGranted(t, "read as the principal://", read)
	setRole(principal, false)
	if read() == nil {
		t.Fatal("read after the revoke succeeded")
	}
	group := s.set("group/auditors")
	setRole(group, true)
	waitGranted(t, "read as the principalSet:// group", read)

	pc, err := iam.NewPoliciesClient(ctx, rmOptionsAt(rmEndpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	dop, err := pc.CreatePolicy(ctx, &iamv2.CreatePolicyRequest{Parent: denyParent("projects/" + s.project), PolicyId: "deny-eng",
		Policy: &iamv2.Policy{Rules: denyRuleOf([]string{s.set("attribute.dept/eng")}, []string{"storage.googleapis.com/objects.get"}, nil)}})
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}
	deny, err := dop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}
	if err := read(); !errors.As(err, &ge) || ge.Code != http.StatusForbidden {
		t.Fatalf("read under the deny policy = %v; want 403", err)
	}
	if op, err := pc.DeletePolicy(ctx, &iamv2.DeletePolicyRequest{Name: deny.GetName()}); err != nil {
		t.Fatalf("DeletePolicy: %v", err)
	} else if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("DeletePolicy: %v", err)
	}
	waitGranted(t, "read after the deny policy is deleted", read)

	subjects := iamREST(t, h).Locations.WorkforcePools.Subjects
	subject := s.poolName + "/subjects/carol@example.test"
	rop, err := subjects.RevokeSessions(subject, &iamv1.RevokeWorkforcePoolSubjectSessionsRequest{}).Context(ctx).Do()
	if err != nil || !rop.Done {
		t.Fatalf("revokeSessions = %v, %v", rop, err)
	}
	if got, err := subjects.Operations.Get(rop.Name).Context(ctx).Do(); err != nil || !got.Done {
		t.Fatalf("subjects.operations.get = %v, %v", got, err)
	}
	if err := read(); !errors.As(err, &ge) || ge.Code != http.StatusUnauthorized {
		t.Fatalf("read with a revoked session's token = %v; want 401", err)
	}
	tok, err = exchange()
	if err != nil {
		t.Fatalf("exchange after revokeSessions: %v", err)
	}
	waitGranted(t, "read with a new session", readWith(newStorageAs(t, h, tok).json))

	if _, err := subjects.Delete(subject).Context(ctx).Do(); err != nil {
		t.Fatalf("delete subject: %v", err)
	}
	_, err = subjects.Delete(subject).Context(ctx).Do()
	wantWIFError(t, "a second delete", err, 400)
	if _, err := exchange(); err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("exchange for a deleted subject = %v; want invalid_grant", err)
	}
	if _, err := subjects.Undelete(subject, &iamv1.UndeleteWorkforcePoolSubjectRequest{}).Context(ctx).Do(); err != nil {
		t.Fatalf("undelete subject: %v", err)
	}
	if tok, err = exchange(); err != nil {
		t.Fatalf("exchange after undelete: %v", err)
	}
	as = newStorageAs(t, h, tok)
	waitGranted(t, "read after the subject is undeleted", readWith(as.json))

	pools := iamREST(t, h).Locations.WorkforcePools
	if _, err := pools.Patch(s.poolName, &iamv1.WorkforcePool{Disabled: true}).UpdateMask("disabled").Context(ctx).Do(); err != nil {
		t.Fatalf("disable pool: %v", err)
	}
	if err := readWith(as.json)(); !errors.As(err, &ge) || ge.Code != http.StatusUnauthorized {
		t.Fatalf("read with a disabled pool's token = %v; want 401", err)
	}
	if _, err := pools.Patch(s.poolName, &iamv1.WorkforcePool{Disabled: false}).UpdateMask("disabled").Context(ctx).Do(); err != nil {
		t.Fatalf("enable pool: %v", err)
	}
	waitGranted(t, "read after the pool is enabled again", readWith(as.json))
	setRole(group, false)
}

// covers: iam.locations.workforcePools.create, iam.locations.workforcePools.get, iam.locations.workforcePools.list, iam.locations.workforcePools.patch, iam.locations.workforcePools.delete, iam.locations.workforcePools.undelete
// covers: iam.locations.workforcePools.providers.create, iam.locations.workforcePools.providers.get, iam.locations.workforcePools.providers.list, iam.locations.workforcePools.providers.patch, iam.locations.workforcePools.providers.delete, iam.locations.workforcePools.providers.undelete
//
// TestGcloudIAMWorkforcePools (#2056): gcloud iam workforce-pools create,
// describe, list, update, delete and undelete, and providers create-oidc,
// describe, list, update-oidc, delete and undelete, through gcloud-setup's
// iam override alone.
func TestGcloudIAMWorkforcePools(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager)
	g := newGcloudSession(t, h)
	id := wfPoolID(h, "wfgc-")
	org := strings.TrimPrefix(localOrgRoles, "organizations/")
	loc := "--location=global"
	g.must("iam", "workforce-pools", "create", id, "--organization="+org, loc, "--display-name=gcloud pool", "--session-duration=7200s")
	t.Cleanup(func() { _, _ = g.run(nil, "iam", "workforce-pools", "delete", id, loc, "--quiet") })
	if out := g.must("iam", "workforce-pools", "describe", id, loc, "--format=value(displayName,state,sessionDuration,parent)"); !strings.Contains(out, "gcloud pool") ||
		!strings.Contains(out, "ACTIVE") || !strings.Contains(out, localOrgRoles) {
		t.Errorf("describe pool:\n%s", out)
	}
	g.must("iam", "workforce-pools", "update", id, loc, "--description=updated")
	if out := g.must("iam", "workforce-pools", "list", "--organization="+org, loc, "--format=value(name,description)"); !strings.Contains(out, id) || !strings.Contains(out, "updated") {
		t.Errorf("list pools:\n%s", out)
	}
	prov := []string{"--workforce-pool=" + id, loc}
	g.must(append([]string{"iam", "workforce-pools", "providers", "create-oidc", "gc-idp", "--issuer-uri=https://idp.example.test",
		"--client-id=staff", "--attribute-mapping=google.subject=assertion.sub,google.groups=assertion.groups",
		"--attribute-condition='staff' in google.groups", "--web-sso-response-type=id-token",
		"--web-sso-assertion-claims-behavior=only-id-token-claims"}, prov...)...)
	if out := g.must(append([]string{"iam", "workforce-pools", "providers", "describe", "gc-idp", "--format=value(oidc.issuerUri,oidc.clientId)"}, prov...)...); !strings.Contains(out, "https://idp.example.test") ||
		!strings.Contains(out, "staff") {
		t.Errorf("describe provider:\n%s", out)
	}
	g.must(append([]string{"iam", "workforce-pools", "providers", "update-oidc", "gc-idp", "--client-id=staff2"}, prov...)...)
	if out := g.must(append([]string{"iam", "workforce-pools", "providers", "list", "--format=value(name,oidc.clientId)"}, prov...)...); !strings.Contains(out, "staff2") {
		t.Errorf("list providers:\n%s", out)
	}
	if _, err := g.run(nil, append([]string{"iam", "workforce-pools", "providers", "create-oidc", "gcp-idp", "--issuer-uri=https://idp.example.test",
		"--client-id=staff", "--attribute-mapping=google.subject=assertion.sub", "--web-sso-response-type=id-token",
		"--web-sso-assertion-claims-behavior=only-id-token-claims"}, prov...)...); err == nil {
		t.Error("a gcp- provider ID was accepted")
	}
	g.must(append([]string{"iam", "workforce-pools", "providers", "delete", "gc-idp", "--quiet"}, prov...)...)
	g.must(append([]string{"iam", "workforce-pools", "providers", "undelete", "gc-idp"}, prov...)...)
	g.must("iam", "workforce-pools", "delete", id, loc, "--quiet")
	if out := g.must("iam", "workforce-pools", "describe", id, loc, "--format=value(state)"); !strings.Contains(out, "DELETED") {
		t.Errorf("describe after delete:\n%s", out)
	}
	g.must("iam", "workforce-pools", "undelete", id, loc)
}

// covers: iam.locations.workforcePools.get, iam.locations.workforcePools.list, iam.locations.workforcePools.patch, iam.locations.workforcePools.providers.list
//
// TestIAMEnforcementWorkforceIdentity (#2056), with the official
// google.golang.org/api/iam/v1 client: a pool's methods are checked on
// the pool under its organization. A minted service account's token is
// refused a pool's get (iam.workforcePools.get), the organization's list
// (iam.workforcePools.list) and the providers' list
// (iam.workforcePoolProviders.list) with the IAM API's words; IAM
// Workforce Pool Viewer on the organization lets them through, not a patch
// (iam.workforcePools.update), which IAM Workforce Pool Admin grants. A
// workforce identity's own token is refused the pool's get until its
// principal:// holds the viewer role on the organization. The instance's
// own account is never refused.
func TestIAMEnforcementWorkforceIdentity(t *testing.T) {
	h := New(t)
	rm := h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	s := newWFSetup(t, h, "iowf-", true)
	p := newIAMPrincipal(t, h, s.project, "iowf")
	_, oc, _ := foldersClientsAt(t, h, rmOptionsAt(rm))
	as, err := iamv1.NewService(ctx, restAs(rm, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	pools := as.Locations.WorkforcePools
	get := func(svc *iamv1.Service) func() error {
		return func() error { _, err := svc.Locations.WorkforcePools.Get(s.poolName).Context(ctx).Do(); return err }
	}
	list := func() error {
		_, err := pools.List("locations/global").Parent(localOrgRoles).Context(ctx).Do()
		return err
	}
	providers := func() error { _, err := pools.Providers.List(s.poolName).Context(ctx).Do(); return err }
	patch := func() error {
		_, err := pools.Patch(s.poolName, &iamv1.WorkforcePool{Description: "patched"}).UpdateMask("description").Context(ctx).Do()
		return err
	}
	wantAPIDenied(t, "pools.get without a role", get(as)(), iamDenied("iam.workforcePools.get"))
	wantAPIDenied(t, "pools.list without a role", list(), iamDenied("iam.workforcePools.list"))
	wantAPIDenied(t, "providers.list without a role", providers(), iamDenied("iam.workforcePoolProviders.list"))
	if err := get(iamREST(t, h))(); err != nil {
		t.Fatalf("the instance's own account: pools.get = %v", err)
	}

	member := "serviceAccount:" + p.email
	unbind := bindOn(t, oc.GetIamPolicy, oc.SetIamPolicy, localOrgRoles, &iampb.Binding{Role: "roles/iam.workforcePoolViewer", Members: []string{member}})
	t.Cleanup(unbind)
	waitGranted(t, "pools.get as an IAM Workforce Pool Viewer", get(as))
	for what, call := range map[string]func() error{"pools.list": list, "providers.list": providers} {
		if err := call(); err != nil {
			t.Errorf("%s as an IAM Workforce Pool Viewer: %v", what, err)
		}
	}
	wantAPIDenied(t, "pools.patch as a viewer", patch(), iamDenied("iam.workforcePools.update"))
	unbindAdmin := bindOn(t, oc.GetIamPolicy, oc.SetIamPolicy, localOrgRoles, &iampb.Binding{Role: "roles/iam.workforcePoolAdmin", Members: []string{member}})
	t.Cleanup(unbindAdmin)
	waitGranted(t, "pools.patch as an IAM Workforce Pool Admin", patch)

	tok, err := oauth2Token(t, ctx, s.credentialConfig(t, h, s.token(t, "dave", "ops", "staff"), s.project))
	if err != nil {
		t.Fatalf("workforce token: %v", err)
	}
	asUser, err := iamv1.NewService(ctx, restAs(rm, tok)...)
	if err != nil {
		t.Fatal(err)
	}
	wantAPIDenied(t, "pools.get as the workforce identity without a role", get(asUser)(), iamDenied("iam.workforcePools.get"))
	unbindUser := bindOn(t, oc.GetIamPolicy, oc.SetIamPolicy, localOrgRoles, &iampb.Binding{Role: "roles/iam.workforcePoolViewer", Members: []string{s.principal("dave")}})
	t.Cleanup(unbindUser)
	waitGranted(t, "pools.get as the workforce identity's principal://", get(asUser))
}
