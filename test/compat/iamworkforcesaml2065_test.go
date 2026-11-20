//go:build compat

package compat

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	iamv1 "google.golang.org/api/iam/v1"
	"google.golang.org/api/option"
	stsv1 "google.golang.org/api/sts/v1"

	"github.com/cloudburrow/cloudburrow/internal/wif/samltest"
)

// covers: sts.token, storage.objects.get
//
// TestWorkforceIdentityFederationSAML (#2065), with IAM enforcement on: a
// workforce pool's SAML provider trusts a locally generated IdP's metadata
// (a throwaway key made by the test). The official STS client
// (google.golang.org/api/sts/v1) exchanges a signed SAML response, base64
// as Google documents, for a token whose principal:// reads an object
// through the official Cloud Storage client once granted
// roles/storage.objectViewer, its lifetime bounded by the assertion's
// SessionNotOnOrAfter; golang.org/x/oauth2/google reads an
// external_account configuration with subject_token_type saml2 and a
// file-sourced assertion. Refused with invalid_grant: a response whose
// NameID was changed after signing, an expired assertion, an unsigned
// one, one signed by another key, one for another audience and one the
// attribute condition rejects. An assertion encrypted to the provider's
// encryption key (iam.locations.workforcePools.providers.keys) is
// decrypted and exchanged.
func TestWorkforceIdentityFederationSAML(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvStorage)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	idp, err := samltest.NewIdP("https://idp.example.test/saml/metadata")
	if err != nil {
		t.Fatal(err)
	}
	project := iamProject(t, h, "wfsaml-")
	pool := wfPoolID(h, "wfsaml-")
	pools := iamREST(t, h).Locations.WorkforcePools
	if _, err := pools.Create("locations/global", &iamv1.WorkforcePool{Parent: localOrgRoles, SessionDuration: "1800s"}).
		WorkforcePoolId(pool).Context(ctx).Do(); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	poolName := "locations/global/workforcePools/" + pool
	t.Cleanup(func() { _, _ = pools.Delete(poolName).Context(h.Context()).Do() })
	if _, err := pools.Providers.Create(poolName, &iamv1.WorkforcePoolProvider{
		Saml: &iamv1.GoogleIamAdminV1WorkforcePoolProviderSaml{IdpMetadataXml: idp.Metadata()},
		AttributeMapping: map[string]string{"google.subject": "assertion.subject", "google.groups": "assertion.attributes['groups']",
			"attribute.dept": "assertion.attributes['dept'][0]"},
		AttributeCondition: "'staff' in google.groups",
	}).WorkforcePoolProviderId("saml-idp").Context(ctx).Do(); err != nil {
		t.Fatalf("create SAML provider: %v", err)
	}
	provName := poolName + "/providers/saml-idp"
	audience := "//iam.googleapis.com/" + provName
	samlAudience := "https://iam.googleapis.com/" + provName
	acs := "https://auth.cloud.google/signin-callback/" + provName
	opts := func(nameID string, groups ...string) samltest.Options {
		return samltest.Options{Audience: samlAudience, Recipient: acs, NameID: nameID, SignAssertion: true,
			Attributes: map[string][]string{"groups": groups, "dept": {"eng"}}}
	}
	response := func(o samltest.Options, from *samltest.IdP) string {
		t.Helper()
		s, err := from.Response(o)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	sts, err := stsv1.NewService(ctx, option.WithEndpoint(metadataURL(h)+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	exchange := func(token string) (*stsv1.GoogleIdentityStsV1ExchangeTokenResponse, error) {
		return sts.V1.Token(&stsv1.GoogleIdentityStsV1ExchangeTokenRequest{GrantType: "urn:ietf:params:oauth:grant-type:token-exchange",
			RequestedTokenType: "urn:ietf:params:oauth:token-type:access_token", SubjectTokenType: "urn:ietf:params:oauth:token-type:saml2",
			Audience: audience, Scope: "https://www.googleapis.com/auth/cloud-platform", SubjectToken: token,
			Options: `{"userProject":"` + project + `"}`}).Context(ctx).Do()
	}
	o := opts("erin@example.test", "staff")
	o.SignResponse = true
	resp, err := exchange(response(o, idp))
	if err != nil || resp.AccessToken == "" || resp.TokenType != "Bearer" || resp.ExpiresIn != 1800 {
		t.Fatalf("sts.token (SAML) = %+v, %v; want a Bearer token for the pool's 1800s session", resp, err)
	}
	o = opts("erin@example.test", "staff")
	o.SessionNotOnOrAfter = time.Now().Add(10 * time.Minute)
	if r, err := exchange(response(o, idp)); err != nil || r.ExpiresIn > 600 || r.ExpiresIn < 590 {
		t.Fatalf("sts.token with SessionNotOnOrAfter in 10 minutes = %+v, %v; want expires_in of about 600", r, err)
	}

	wantGrant := func(what, token string) {
		t.Helper()
		_, err := exchange(token)
		var ge *googleapi.Error
		if !errors.As(err, &ge) || ge.Code != http.StatusBadRequest || !strings.Contains(ge.Body, `"error":"invalid_grant"`) {
			t.Fatalf("%s = %v; want 400 invalid_grant", what, err)
		}
	}
	x, err := idp.ResponseXML(opts("erin@example.test", "staff"))
	if err != nil {
		t.Fatal(err)
	}
	wantGrant("a tampered assertion", base64.StdEncoding.EncodeToString([]byte(strings.Replace(x, ">erin@example.test<", ">mallory@example.test<", 1))))
	o = opts("erin@example.test", "staff")
	o.IssueInstant, o.NotOnOrAfter = time.Now().Add(-30*time.Minute), time.Now().Add(-time.Minute)
	wantGrant("an expired assertion", response(o, idp))
	o = opts("erin@example.test", "staff")
	o.SignAssertion = false
	wantGrant("an unsigned assertion", response(o, idp))
	other, err := samltest.NewIdP(idp.EntityID)
	if err != nil {
		t.Fatal(err)
	}
	wantGrant("an assertion signed by another key", response(opts("erin@example.test", "staff"), other))
	o = opts("erin@example.test", "staff")
	o.Audience = "https://iam.googleapis.com/" + poolName + "/providers/other-idp"
	wantGrant("an assertion for another audience", response(o, idp))
	wantGrant("an assertion the condition rejects", response(opts("erin@example.test", "contractors"), idp))

	// Encrypted to the provider's key.
	kop, err := pools.Providers.Keys.Create(provName, &iamv1.WorkforcePoolProviderKey{Use: "ENCRYPTION", KeyData: &iamv1.KeyData{KeySpec: "RSA_2048"}}).
		WorkforcePoolProviderKeyId("enc-key").Context(ctx).Do()
	if err != nil || !kop.Done {
		t.Fatalf("create key = %v, %v", kop, err)
	}
	key, err := pools.Providers.Keys.Get(provName + "/keys/enc-key").Context(ctx).Do()
	if err != nil {
		t.Fatalf("get key: %v", err)
	}
	block, _ := pem.Decode([]byte(key.KeyData.Key))
	if block == nil {
		t.Fatalf("the key's certificate is not PEM: %q", key.KeyData.Key)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	o = opts("erin@example.test", "staff")
	o.EncryptTo = cert
	if _, err := exchange(response(o, idp)); err != nil {
		t.Fatalf("sts.token with an encrypted assertion: %v", err)
	}

	// Storage, through Google's auth library reading a file-sourced SAML
	// credential configuration.
	owner := storageClient(t, h)
	bucket := project + "-saml"
	bh := owner.Bucket(bucket)
	if err := bh.Create(ctx, project, &storage.BucketAttrs{UniformBucketLevelAccess: storage.UniformBucketLevelAccess{Enabled: true}}); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { _ = bh.Object("s.txt").Delete(h.Context()); _ = bh.Delete(h.Context()) })
	w := bh.Object("s.txt").NewWriter(ctx)
	_, _ = w.Write([]byte("saml"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := samlCredentialConfig(t, h, audience, response(opts("erin@example.test", "staff"), idp), project)
	tok, err := oauth2Token(t, ctx, cfg)
	if err != nil {
		t.Fatalf("x/oauth2 SAML workforce token: %v", err)
	}
	as := newStorageAs(t, h, tok)
	principal := "principal://iam.googleapis.com/" + poolName + "/subject/erin@example.test"
	read := func() error {
		got, err := storageRead(t, h, as.json, bucket, "s.txt")
		if err == nil && got != "saml" {
			return errors.New("read " + got)
		}
		return err
	}
	var ge *googleapi.Error
	if err := read(); !errors.As(err, &ge) || ge.Code != http.StatusForbidden || !strings.Contains(ge.Body, principal) {
		t.Fatalf("read without a role = %v; want 403 naming %s", err, principal)
	}
	pol, err := bh.IAM().Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pol.Add(principal, "roles/storage.objectViewer")
	if err := bh.IAM().SetPolicy(ctx, pol); err != nil {
		t.Fatalf("setIamPolicy: %v", err)
	}
	waitGranted(t, "read as the SAML subject's principal://", read)
}

// samlCredentialConfig writes Google's external_account configuration for
// a workforce SAML provider, with a file-sourced base64 assertion, naming
// this instance's STS. Every URL is local.
func samlCredentialConfig(t *testing.T, h *Harness, audience, assertion, userProject string) string {
	t.Helper()
	meta := metadataURL(h)
	if !strings.HasPrefix(meta, "http://127.0.0.1") && !strings.HasPrefix(meta, "http://localhost") {
		t.Fatalf("the metadata endpoint %q is not local", meta)
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "assertion.b64")
	if err := os.WriteFile(file, []byte(assertion), 0o600); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{
		"type":                        "external_account",
		"audience":                    audience,
		"subject_token_type":          "urn:ietf:params:oauth:token-type:saml2",
		"token_url":                   meta + "/v1/token",
		"workforce_pool_user_project": userProject,
		"credential_source":           map[string]string{"file": file},
	})
	path := filepath.Join(dir, "saml-config.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// covers: iam.locations.workforcePools.getIamPolicy, iam.locations.workforcePools.setIamPolicy, iam.locations.workforcePools.testIamPermissions
//
// TestWorkforcePoolIAMPolicy (#2065), with IAM enforcement on and the
// official google.golang.org/api/iam/v1 client: the instance's account
// sets a pool's own policy, granting a minted service account IAM
// Workforce Pool Viewer on the pool alone; it then reads the pool, its
// testIamPermissions answers iam.workforcePools.get but not .update, and
// its getIamPolicy and setIamPolicy are refused (iam.workforcePools.getIamPolicy,
// .setIamPolicy) with the IAM API's words, which IAM Workforce Pool Admin
// on the pool grants. testIamPermissions of a pool that does not exist
// answers no permissions, not NOT_FOUND.
func TestWorkforcePoolIAMPolicy(t *testing.T) {
	h := New(t)
	rm := h.Endpoint(EnvResourceManager)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "wfiam-")
	pool := wfPoolID(h, "wfiam-")
	owner := iamREST(t, h).Locations.WorkforcePools
	if _, err := owner.Create("locations/global", &iamv1.WorkforcePool{Parent: localOrgRoles}).WorkforcePoolId(pool).Context(ctx).Do(); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	poolName := "locations/global/workforcePools/" + pool
	t.Cleanup(func() { _, _ = owner.Delete(poolName).Context(h.Context()).Do() })
	p := newIAMPrincipal(t, h, project, "wfiam")
	asSvc, err := iamv1.NewService(ctx, restAs(rm, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	as := asSvc.Locations.WorkforcePools
	get := func() error { _, err := as.Get(poolName).Context(ctx).Do(); return err }
	wantAPIDenied(t, "pools.get without a role", get(), iamDenied("iam.workforcePools.get"))

	member := "serviceAccount:" + p.email
	setRole := func(role string) {
		t.Helper()
		cur, err := owner.GetIamPolicy(poolName, &iamv1.GetIamPolicyRequest{}).Context(ctx).Do()
		if err != nil {
			t.Fatalf("getIamPolicy: %v", err)
		}
		cur.Bindings = append(cur.Bindings, &iamv1.Binding{Role: role, Members: []string{member}})
		got, err := owner.SetIamPolicy(poolName, &iamv1.SetIamPolicyRequest{Policy: cur}).Context(ctx).Do()
		if err != nil || got.Etag == "" {
			t.Fatalf("setIamPolicy = %v, %v", got, err)
		}
	}
	setRole("roles/iam.workforcePoolViewer")
	if got, err := owner.GetIamPolicy(poolName, &iamv1.GetIamPolicyRequest{}).Context(ctx).Do(); err != nil || len(got.Bindings) != 1 ||
		got.Bindings[0].Members[0] != member {
		t.Fatalf("getIamPolicy after set = %+v, %v", got, err)
	}
	waitGranted(t, "pools.get as a viewer on the pool", get)
	tr, err := as.TestIamPermissions(poolName, &iamv1.TestIamPermissionsRequest{Permissions: []string{"iam.workforcePools.get", "iam.workforcePools.update"}}).Context(ctx).Do()
	if err != nil || len(tr.Permissions) != 1 || tr.Permissions[0] != "iam.workforcePools.get" {
		t.Fatalf("testIamPermissions as a viewer = %+v, %v; want iam.workforcePools.get alone", tr, err)
	}
	_, err = as.GetIamPolicy(poolName, &iamv1.GetIamPolicyRequest{}).Context(ctx).Do()
	wantAPIDenied(t, "getIamPolicy as a viewer", err, iamDenied("iam.workforcePools.getIamPolicy"))
	_, err = as.SetIamPolicy(poolName, &iamv1.SetIamPolicyRequest{Policy: &iamv1.Policy{}}).Context(ctx).Do()
	wantAPIDenied(t, "setIamPolicy as a viewer", err, iamDenied("iam.workforcePools.setIamPolicy"))
	setRole("roles/iam.workforcePoolAdmin")
	waitGranted(t, "getIamPolicy as an admin on the pool", func() error {
		_, err := as.GetIamPolicy(poolName, &iamv1.GetIamPolicyRequest{}).Context(ctx).Do()
		return err
	})
	if tr, err := owner.TestIamPermissions("locations/global/workforcePools/"+pool+"-none", &iamv1.TestIamPermissionsRequest{Permissions: []string{"iam.workforcePools.get"}}).
		Context(ctx).Do(); err != nil || len(tr.Permissions) != 0 {
		t.Fatalf("testIamPermissions of an unknown pool = %+v, %v; want none", tr, err)
	}
}

// covers: iam.locations.workforcePools.getIamPolicy, iam.locations.workforcePools.setIamPolicy
// covers: iam.locations.workforcePools.providers.scimTenants.create, iam.locations.workforcePools.providers.scimTenants.get, iam.locations.workforcePools.providers.scimTenants.list, iam.locations.workforcePools.providers.scimTenants.delete
// covers: iam.locations.workforcePools.providers.scimTenants.tokens.create, iam.locations.workforcePools.providers.scimTenants.tokens.list
//
// TestGcloudIAMWorkforcePoolIAMPolicy (#2065): gcloud iam workforce-pools
// set-iam-policy and get-iam-policy on a pool, and providers scim-tenants
// create, describe, list, tokens create and list, and delete (GA gcloud;
// a tenant's get-iam-policy is alpha only), through gcloud-setup's iam override alone.
func TestGcloudIAMWorkforcePoolIAMPolicy(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager)
	g := newGcloudSession(t, h)
	id := wfPoolID(h, "wfgi-")
	org := strings.TrimPrefix(localOrgRoles, "organizations/")
	loc := "--location=global"
	g.must("iam", "workforce-pools", "create", id, "--organization="+org, loc)
	t.Cleanup(func() { _, _ = g.run(nil, "iam", "workforce-pools", "delete", id, loc, "--quiet") })
	member := "principal://iam.googleapis.com/locations/global/workforcePools/" + id + "/subject/frank"
	file := filepath.Join(t.TempDir(), "policy.json")
	b, _ := json.Marshal(map[string]any{"bindings": []any{map[string]any{"role": "roles/iam.workforcePoolViewer", "members": []string{member}}}})
	if err := os.WriteFile(file, b, 0o600); err != nil {
		t.Fatal(err)
	}
	g.must("iam", "workforce-pools", "set-iam-policy", id, file, loc)
	if out := g.must("iam", "workforce-pools", "get-iam-policy", id, loc, "--format=json"); !strings.Contains(out, member) || !strings.Contains(out, "roles/iam.workforcePoolViewer") {
		t.Fatalf("get-iam-policy:\n%s", out)
	}

	g.must("iam", "workforce-pools", "providers", "create-oidc", "scim-idp", "--workforce-pool="+id, loc, "--issuer-uri=https://idp.example.test",
		"--client-id=staff", "--attribute-mapping=google.subject=assertion.sub", "--web-sso-response-type=id-token",
		"--web-sso-assertion-claims-behavior=only-id-token-claims")
	tenant := []string{"--workforce-pool=" + id, "--provider=scim-idp", loc}
	g.must(append([]string{"iam", "workforce-pools", "providers", "scim-tenants", "create", "gc-tenant", "--display-name=gcloud",
		"--claim-mapping=google.subject=user.externalId,google.group=group.externalId"}, tenant...)...)
	if out := g.must(append([]string{"iam", "workforce-pools", "providers", "scim-tenants", "describe", "gc-tenant", "--format=value(state,baseUri)"}, tenant...)...); !strings.Contains(out, "ACTIVE") ||
		!strings.Contains(out, "iamscim.googleapis.com") {
		t.Errorf("scim-tenants describe:\n%s", out)
	}
	if out := g.must(append([]string{"iam", "workforce-pools", "providers", "scim-tenants", "list", "--format=value(name)"}, tenant...)...); !strings.Contains(out, "gc-tenant") {
		t.Errorf("scim-tenants list:\n%s", out)
	}
	if _, err := g.run(nil, append([]string{"iam", "workforce-pools", "providers", "scim-tenants", "create", "gcp-tenant",
		"--claim-mapping=google.subject=user.externalId"}, tenant...)...); err == nil {
		t.Error("a gcp- SCIM tenant ID was accepted")
	}
	tok := append([]string{"--scim-tenant=gc-tenant"}, tenant...)
	if out := g.must(append([]string{"iam", "workforce-pools", "providers", "scim-tenants", "tokens", "create", "gc-token", "--format=value(securityToken)"}, tok...)...); strings.TrimSpace(out) == "" {
		t.Error("tokens create answered no securityToken")
	}
	if out := g.must(append([]string{"iam", "workforce-pools", "providers", "scim-tenants", "tokens", "list", "--format=value(name,securityToken)"}, tok...)...); !strings.Contains(out, "gc-token") {
		t.Errorf("tokens list:\n%s", out)
	}
	g.must(append([]string{"iam", "workforce-pools", "providers", "scim-tenants", "delete", "gc-tenant", "--quiet"}, tenant...)...)
}
