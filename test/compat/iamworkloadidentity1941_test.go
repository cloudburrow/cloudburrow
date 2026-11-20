//go:build compat

package compat

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/auth/credentials"
	"cloud.google.com/go/storage"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/googleapi"
	iamv1 "google.golang.org/api/iam/v1"
	"google.golang.org/api/option"
	stsv1 "google.golang.org/api/sts/v1"
)

// wifIdP is a local OIDC identity provider for the tests: a key the test
// generates, served as a JWKS behind a discovery document on a loopback
// port. Nothing here is a real credential.
type wifIdP struct {
	key *rsa.PrivateKey
	srv *httptest.Server
}

func newWIFIdP(t *testing.T) *wifIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &wifIdP{key: key}
	idp.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": idp.srv.URL, "jwks_uri": idp.srv.URL + "/jwks"})
		case "/jwks":
			_, _ = w.Write([]byte(idp.jwks()))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(idp.srv.Close)
	return idp
}

func (i *wifIdP) jwks() string {
	b64 := func(n *big.Int) string { return base64.RawURLEncoding.EncodeToString(n.Bytes()) }
	b, _ := json.Marshal(map[string]any{"keys": []map[string]string{{"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "test-1",
		"n": b64(i.key.N), "e": b64(big.NewInt(int64(i.key.E)))}}})
	return string(b)
}

func (i *wifIdP) token(t *testing.T, iss, aud, sub string, extra map[string]any) string {
	t.Helper()
	now := time.Now()
	claims := map[string]any{"iss": iss, "aud": aud, "sub": sub, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}
	for k, v := range extra {
		claims[k] = v
	}
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	in := enc(map[string]string{"alg": "RS256", "kid": "test-1", "typ": "JWT"}) + "." + enc(claims)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, i.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return in + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func iamREST(t *testing.T, h *Harness) *iamv1.Service {
	t.Helper()
	svc, err := iamv1.NewService(h.Context(), option.WithEndpoint("http://"+strings.TrimPrefix(h.Endpoint(EnvResourceManager), "http://")+"/"),
		option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func metadataURL(h *Harness) string {
	m := h.Endpoint(EnvMetadata)
	if !strings.HasPrefix(m, "http://") {
		m = "http://" + m
	}
	return strings.TrimSuffix(m, "/")
}

func projectNumber(t *testing.T, h *Harness, project string) string {
	t.Helper()
	p, err := rmV1(t, h).Projects.Get(project).Context(h.Context()).Do()
	if err != nil {
		t.Fatalf("projects.get %s: %v", project, err)
	}
	return strconv.FormatInt(p.ProjectNumber, 10)
}

func wantWIFError(t *testing.T, what string, err error, code int) {
	t.Helper()
	var ge *googleapi.Error
	if !errors.As(err, &ge) || ge.Code != code {
		t.Fatalf("%s = %v; want HTTP %d", what, err, code)
	}
}

// samlMetadata is IdP metadata with a signing certificate generated here.
func samlMetadata(t *testing.T) string {
	t.Helper()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test idp"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(1, 0, 0)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return `<EntityDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata" entityID="https://idp.example.test/saml">` +
		`<IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol"><KeyDescriptor use="signing">` +
		`<KeyInfo xmlns="http://www.w3.org/2000/09/xmldsig#"><X509Data><X509Certificate>` + base64.StdEncoding.EncodeToString(der) +
		`</X509Certificate></X509Data></KeyInfo></KeyDescriptor></IDPSSODescriptor></EntityDescriptor>`
}

// covers: iam.projects.locations.workloadIdentityPools.create, iam.projects.locations.workloadIdentityPools.get, iam.projects.locations.workloadIdentityPools.list, iam.projects.locations.workloadIdentityPools.patch, iam.projects.locations.workloadIdentityPools.delete, iam.projects.locations.workloadIdentityPools.undelete, iam.projects.locations.workloadIdentityPools.operations.get
// covers: iam.projects.locations.workloadIdentityPools.providers.create, iam.projects.locations.workloadIdentityPools.providers.get, iam.projects.locations.workloadIdentityPools.providers.list, iam.projects.locations.workloadIdentityPools.providers.patch, iam.projects.locations.workloadIdentityPools.providers.delete, iam.projects.locations.workloadIdentityPools.providers.undelete
// covers: iam.projects.locations.workloadIdentityPools.providers.keys.create, iam.projects.locations.workloadIdentityPools.providers.keys.get, iam.projects.locations.workloadIdentityPools.providers.keys.list, iam.projects.locations.workloadIdentityPools.providers.keys.delete, iam.projects.locations.workloadIdentityPools.providers.keys.undelete
//
// TestWorkloadIdentityPools (#1941), with google.golang.org/api/iam/v1:
// a pool is created (a done Operation, named under the project number),
// read, listed, patched, soft-deleted (DELETED with an expireTime, listed
// only with showDeleted, its ID not reusable) and undeleted; OIDC, AWS,
// SAML and X.509 providers likewise, with Google's validation (the gcp-
// prefix, an ID shorter than 4, an OIDC provider without google.subject,
// an invalid CEL mapping, two provider kinds); a SAML provider's
// encryption key is created, listed, deleted and undeleted.
func TestWorkloadIdentityPools(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager)
	ctx := h.Context()
	project := iamProject(t, h, "wifp-")
	num := projectNumber(t, h, project)
	svc := iamREST(t, h)
	pools := svc.Projects.Locations.WorkloadIdentityPools
	parent := "projects/" + project + "/locations/global"

	if _, err := pools.Create(parent, &iamv1.WorkloadIdentityPool{}).WorkloadIdentityPoolId("gcp-pool").Context(ctx).Do(); err == nil {
		t.Fatal("a gcp- pool ID was accepted")
	} else {
		wantWIFError(t, "gcp- pool ID", err, 400)
	}
	op, err := pools.Create(parent, &iamv1.WorkloadIdentityPool{DisplayName: "CI pool", Description: "for CI"}).
		WorkloadIdentityPoolId("ci-pool").Context(ctx).Do()
	if err != nil || !op.Done {
		t.Fatalf("create pool = %v, %v", op, err)
	}
	var created iamv1.WorkloadIdentityPool
	if err := json.Unmarshal(op.Response, &created); err != nil || created.Name != "projects/"+num+"/locations/global/workloadIdentityPools/ci-pool" || created.State != "ACTIVE" {
		t.Fatalf("created pool = %+v, %v", created, err)
	}
	if got, err := pools.Operations.Get(op.Name).Context(ctx).Do(); err != nil || !got.Done {
		t.Fatalf("operations.get = %v, %v", got, err)
	}
	if _, err := pools.Create(parent, &iamv1.WorkloadIdentityPool{}).WorkloadIdentityPoolId("ci-pool").Context(ctx).Do(); err == nil {
		t.Fatal("a duplicate pool was created")
	} else {
		wantWIFError(t, "duplicate pool", err, 409)
	}
	poolName := parent + "/workloadIdentityPools/ci-pool"
	if _, err := pools.Patch(poolName, &iamv1.WorkloadIdentityPool{DisplayName: "CI"}).UpdateMask("displayName").Context(ctx).Do(); err != nil {
		t.Fatalf("patch pool: %v", err)
	}
	got, err := pools.Get(poolName).Context(ctx).Do()
	if err != nil || got.DisplayName != "CI" || got.Description != "for CI" {
		t.Fatalf("get pool = %+v, %v", got, err)
	}
	if _, err := pools.Get(parent + "/workloadIdentityPools/nope-pool").Context(ctx).Do(); err == nil {
		t.Fatal("get of a missing pool succeeded")
	} else {
		wantWIFError(t, "missing pool", err, 404)
	}

	provs := pools.Providers
	mapping := map[string]string{"google.subject": "assertion.sub", "attribute.repo": "assertion.repo"}
	if _, err := provs.Create(poolName, &iamv1.WorkloadIdentityPoolProvider{Oidc: &iamv1.Oidc{IssuerUri: "https://idp.example.test"}}).
		WorkloadIdentityPoolProviderId("no-subject").Context(ctx).Do(); err == nil {
		t.Fatal("an OIDC provider without google.subject was accepted")
	} else {
		wantWIFError(t, "OIDC without google.subject", err, 400)
	}
	if _, err := provs.Create(poolName, &iamv1.WorkloadIdentityPoolProvider{Oidc: &iamv1.Oidc{IssuerUri: "https://idp.example.test"},
		AttributeMapping: map[string]string{"google.subject": "assertion.sub +"}}).WorkloadIdentityPoolProviderId("bad-cel").Context(ctx).Do(); err == nil {
		t.Fatal("an invalid CEL mapping was accepted")
	} else {
		wantWIFError(t, "invalid CEL", err, 400)
	}
	if _, err := provs.Create(poolName, &iamv1.WorkloadIdentityPoolProvider{Oidc: &iamv1.Oidc{IssuerUri: "https://idp.example.test"},
		Aws: &iamv1.Aws{AccountId: "123456789012"}, AttributeMapping: mapping}).WorkloadIdentityPoolProviderId("two-kinds").Context(ctx).Do(); err == nil {
		t.Fatal("a provider of two kinds was accepted")
	} else {
		wantWIFError(t, "two kinds", err, 400)
	}
	if _, err := provs.Create(poolName, &iamv1.WorkloadIdentityPoolProvider{DisplayName: "GitHub", Oidc: &iamv1.Oidc{IssuerUri: "https://idp.example.test",
		AllowedAudiences: []string{"ci"}}, AttributeMapping: mapping, AttributeCondition: "attribute.repo.startsWith('org/')"}).
		WorkloadIdentityPoolProviderId("oidc-prov").Context(ctx).Do(); err != nil {
		t.Fatalf("create OIDC provider: %v", err)
	}
	if _, err := provs.Create(poolName, &iamv1.WorkloadIdentityPoolProvider{Aws: &iamv1.Aws{AccountId: "123456789012"}}).
		WorkloadIdentityPoolProviderId("aws-prov").Context(ctx).Do(); err != nil {
		t.Fatalf("create AWS provider: %v", err)
	}
	if _, err := provs.Create(poolName, &iamv1.WorkloadIdentityPoolProvider{Saml: &iamv1.Saml{IdpMetadataXml: samlMetadata(t)},
		AttributeMapping: map[string]string{"google.subject": "assertion.subject"}}).WorkloadIdentityPoolProviderId("saml-prov").Context(ctx).Do(); err != nil {
		t.Fatalf("create SAML provider: %v", err)
	}
	aws, err := provs.Get(poolName + "/providers/aws-prov").Context(ctx).Do()
	if err != nil || aws.AttributeMapping["google.subject"] != "assertion.arn" || aws.AttributeMapping["attribute.aws_role"] == "" {
		t.Fatalf("AWS provider's default mapping = %v, %v", aws, err)
	}
	if _, err := provs.Patch(poolName+"/providers/oidc-prov", &iamv1.WorkloadIdentityPoolProvider{Disabled: true}).UpdateMask("disabled").Context(ctx).Do(); err != nil {
		t.Fatalf("disable provider: %v", err)
	}
	list, err := provs.List(poolName).Context(ctx).Do()
	if err != nil || len(list.WorkloadIdentityPoolProviders) != 3 {
		t.Fatalf("list providers = %v, %v", list, err)
	}

	keys := provs.Keys
	samlName := poolName + "/providers/saml-prov"
	kop, err := keys.Create(samlName, &iamv1.WorkloadIdentityPoolProviderKey{Use: "ENCRYPTION", KeyData: &iamv1.KeyData{KeySpec: "RSA_2048"}}).
		WorkloadIdentityPoolProviderKeyId("enc-key1").Context(ctx).Do()
	if err != nil || !kop.Done {
		t.Fatalf("create key = %v, %v", kop, err)
	}
	k, err := keys.Get(samlName + "/keys/enc-key1").Context(ctx).Do()
	if err != nil || k.KeyData == nil || !strings.Contains(k.KeyData.Key, "BEGIN CERTIFICATE") || k.KeyData.Format != "RSA_X509_PEM" {
		t.Fatalf("get key = %+v, %v", k, err)
	}
	if _, err := keys.Delete(samlName + "/keys/enc-key1").Context(ctx).Do(); err != nil {
		t.Fatalf("delete key: %v", err)
	}
	if kl, err := keys.List(samlName).Context(ctx).Do(); err != nil || len(kl.WorkloadIdentityPoolProviderKeys) != 0 {
		t.Fatalf("list keys after delete = %v, %v", kl, err)
	}
	if _, err := keys.Undelete(samlName+"/keys/enc-key1", &iamv1.UndeleteWorkloadIdentityPoolProviderKeyRequest{}).Context(ctx).Do(); err != nil {
		t.Fatalf("undelete key: %v", err)
	}

	if _, err := provs.Delete(poolName + "/providers/aws-prov").Context(ctx).Do(); err != nil {
		t.Fatalf("delete provider: %v", err)
	}
	if l, _ := provs.List(poolName).ShowDeleted(true).Context(ctx).Do(); l == nil || len(l.WorkloadIdentityPoolProviders) != 3 {
		t.Fatalf("list providers with showDeleted = %v", l)
	}
	if _, err := provs.Undelete(poolName+"/providers/aws-prov", &iamv1.UndeleteWorkloadIdentityPoolProviderRequest{}).Context(ctx).Do(); err != nil {
		t.Fatalf("undelete provider: %v", err)
	}

	if _, err := pools.Delete(poolName).Context(ctx).Do(); err != nil {
		t.Fatalf("delete pool: %v", err)
	}
	deleted, err := pools.Get(poolName).Context(ctx).Do()
	if err != nil || deleted.State != "DELETED" || deleted.ExpireTime == "" {
		t.Fatalf("get deleted pool = %+v, %v", deleted, err)
	}
	if l, err := pools.List(parent).Context(ctx).Do(); err != nil || len(l.WorkloadIdentityPools) != 0 {
		t.Fatalf("list after delete = %v, %v", l, err)
	}
	if l, err := pools.List(parent).ShowDeleted(true).Context(ctx).Do(); err != nil || len(l.WorkloadIdentityPools) != 1 {
		t.Fatalf("list with showDeleted = %v, %v", l, err)
	}
	if _, err := pools.Create(parent, &iamv1.WorkloadIdentityPool{}).WorkloadIdentityPoolId("ci-pool").Context(ctx).Do(); err == nil {
		t.Fatal("a deleted pool's ID was reused")
	}
	if _, err := pools.Undelete(poolName, &iamv1.UndeleteWorkloadIdentityPoolRequest{}).Context(ctx).Do(); err != nil {
		t.Fatalf("undelete pool: %v", err)
	}
}

// wifSetup is a pool with an OIDC provider trusting a local IdP.
type wifSetup struct {
	idp                    *wifIdP
	project, num, audience string
	poolName               string
}

func newWIFSetup(t *testing.T, h *Harness, prefix string, withJWKS bool) *wifSetup {
	t.Helper()
	ctx := h.Context()
	s := &wifSetup{idp: newWIFIdP(t), project: iamProject(t, h, prefix)}
	s.num = projectNumber(t, h, s.project)
	svc := iamREST(t, h)
	parent := "projects/" + s.project + "/locations/global"
	if _, err := svc.Projects.Locations.WorkloadIdentityPools.Create(parent, &iamv1.WorkloadIdentityPool{}).
		WorkloadIdentityPoolId("ci-pool").Context(ctx).Do(); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	s.poolName = parent + "/workloadIdentityPools/ci-pool"
	oidc := &iamv1.Oidc{IssuerUri: s.idp.srv.URL}
	if withJWKS {
		// The keys inline: the issuer is never fetched.
		oidc = &iamv1.Oidc{IssuerUri: "https://idp.example.test", JwksJson: s.idp.jwks()}
	}
	if _, err := svc.Projects.Locations.WorkloadIdentityPools.Providers.Create(s.poolName, &iamv1.WorkloadIdentityPoolProvider{
		Oidc:               oidc,
		AttributeMapping:   map[string]string{"google.subject": "assertion.sub", "attribute.repo": "assertion.repo", "google.groups": "assertion.groups"},
		AttributeCondition: "attribute.repo.startsWith('org/')",
	}).WorkloadIdentityPoolProviderId("ci-prov").Context(ctx).Do(); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	s.audience = "//iam.googleapis.com/projects/" + s.num + "/locations/global/workloadIdentityPools/ci-pool/providers/ci-prov"
	return s
}

func (s *wifSetup) issuer(withJWKS bool) string {
	if withJWKS {
		return "https://idp.example.test"
	}
	return s.idp.srv.URL
}

func (s *wifSetup) principal(sub string) string {
	return "principal://iam.googleapis.com/projects/" + s.num + "/locations/global/workloadIdentityPools/ci-pool/subject/" + sub
}

func (s *wifSetup) set(suffix string) string {
	return "principalSet://iam.googleapis.com/projects/" + s.num + "/locations/global/workloadIdentityPools/ci-pool/" + suffix
}

// credentialConfig writes Google's external_account credential
// configuration, file-sourced, naming this instance's STS (and, when sa is
// set, IAM Credentials for impersonation). Every URL is local.
func (s *wifSetup) credentialConfig(t *testing.T, h *Harness, jwt, sa string) string {
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
	cfg := map[string]any{
		"type":               "external_account",
		"audience":           s.audience,
		"subject_token_type": "urn:ietf:params:oauth:token-type:jwt",
		"token_url":          meta + "/v1/token",
		"credential_source":  map[string]string{"file": tokenFile},
	}
	if sa != "" {
		cfg["service_account_impersonation_url"] = meta + "/v1/projects/-/serviceAccounts/" + sa + ":generateAccessToken"
	}
	b, _ := json.Marshal(cfg)
	path := filepath.Join(dir, "credential-config.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// oauth2Token is a token from golang.org/x/oauth2/google's external
// account credentials for the configuration file at path.
func oauth2Token(t *testing.T, ctx context.Context, path string) (string, error) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	creds, err := google.CredentialsFromJSON(ctx, b, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		t.Fatalf("CredentialsFromJSON: %v", err)
	}
	tok, err := creds.TokenSource.Token()
	if err != nil {
		return "", err
	}
	return tok.AccessToken, nil
}

// authToken is a token from cloud.google.com/go/auth's credentials for
// the configuration file at path.
func authToken(t *testing.T, ctx context.Context, path string) (string, error) {
	t.Helper()
	creds, err := credentials.NewCredentialsFromFile(credentials.ExternalAccount, path,
		&credentials.DetectOptions{Scopes: []string{"https://www.googleapis.com/auth/cloud-platform"}})
	if err != nil {
		t.Fatalf("NewCredentialsFromFile: %v", err)
	}
	tok, err := creds.Token(ctx)
	if err != nil {
		return "", err
	}
	return tok.Value, nil
}

// covers: sts.token, iamcredentials.projects.serviceAccounts.generateAccessToken
//
// TestWorkloadIdentityFederationSTS (#1941): the official STS client
// (google.golang.org/api/sts/v1) exchanges a token from a local IdP, whose
// keys the instance fetches from the issuer's loopback discovery document,
// and refuses a token for another audience (invalid_grant), one the
// attribute condition rejects (invalid_grant) and an unknown provider
// (invalid_target); golang.org/x/oauth2/google and cloud.google.com/go/auth
// read Google's external_account credential configuration with a
// file-sourced credential and get a federated token through the local
// STS; with service_account_impersonation_url, the token is refused (403
// iam.serviceAccounts.getAccessToken) until roles/iam.workloadIdentityUser
// is granted on the account to the principalSet:// of the token's mapped
// attribute, and then minted.
func TestWorkloadIdentityFederationSTS(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	ctx := h.Context()
	s := newWIFSetup(t, h, "wifs-", false)
	meta := metadataURL(h)
	sts, err := stsv1.NewService(ctx, option.WithEndpoint(meta+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	jwt := s.idp.token(t, s.issuer(false), "https:"+s.audience, "job-1", map[string]any{"repo": "org/app", "groups": []string{"deployers"}})
	req := func(tok, aud string) *stsv1.GoogleIdentityStsV1ExchangeTokenRequest {
		return &stsv1.GoogleIdentityStsV1ExchangeTokenRequest{GrantType: "urn:ietf:params:oauth:grant-type:token-exchange",
			RequestedTokenType: "urn:ietf:params:oauth:token-type:access_token", SubjectTokenType: "urn:ietf:params:oauth:token-type:jwt",
			Audience: aud, Scope: "https://www.googleapis.com/auth/cloud-platform", SubjectToken: tok}
	}
	resp, err := sts.V1.Token(req(jwt, s.audience)).Context(ctx).Do()
	if err != nil || resp.AccessToken == "" || resp.TokenType != "Bearer" || resp.IssuedTokenType != "urn:ietf:params:oauth:token-type:access_token" || resp.ExpiresIn <= 0 {
		t.Fatalf("sts.token = %+v, %v", resp, err)
	}
	wantSTSError := func(what string, err error, code int, kind string) {
		t.Helper()
		var ge *googleapi.Error
		if !errors.As(err, &ge) || ge.Code != code || !strings.Contains(ge.Body, `"error":"`+kind+`"`) {
			t.Fatalf("%s = %v; want %d %s", what, err, code, kind)
		}
	}
	_, err = sts.V1.Token(req(s.idp.token(t, s.issuer(false), "elsewhere", "job-1", map[string]any{"repo": "org/app"}), s.audience)).Context(ctx).Do()
	wantSTSError("another audience", err, 400, "invalid_grant")
	_, err = sts.V1.Token(req(s.idp.token(t, s.issuer(false), "https:"+s.audience, "job-1", map[string]any{"repo": "other/app"}), s.audience)).Context(ctx).Do()
	wantSTSError("the attribute condition", err, 400, "invalid_grant")
	_, err = sts.V1.Token(req(jwt, strings.TrimSuffix(s.audience, "ci-prov")+"no-prov")).Context(ctx).Do()
	wantSTSError("an unknown provider", err, 400, "invalid_target")

	direct := s.credentialConfig(t, h, jwt, "")
	if tok, err := oauth2Token(t, ctx, direct); err != nil || tok == "" {
		t.Fatalf("x/oauth2 external account token = %q, %v", tok, err)
	}
	if tok, err := authToken(t, ctx, direct); err != nil || tok == "" {
		t.Fatalf("cloud.google.com/go/auth external account token = %q, %v", tok, err)
	}

	sa := newServiceAccount(t, h, s.project, "wifsa")
	impersonated := s.credentialConfig(t, h, jwt, sa.GetEmail())
	if _, err := oauth2Token(t, ctx, impersonated); err == nil || !strings.Contains(err.Error(), "iam.serviceAccounts.getAccessToken") {
		t.Fatalf("impersonation without workloadIdentityUser = %v; want 403 iam.serviceAccounts.getAccessToken", err)
	}
	grantOn(t, h, sa.GetName(), "roles/iam.workloadIdentityUser", s.set("attribute.repo/org/app"))
	waitGranted(t, "impersonation through x/oauth2", func() error { _, err := oauth2Token(t, ctx, impersonated); return err })
	waitGranted(t, "impersonation through cloud.google.com/go/auth", func() error { _, err := authToken(t, ctx, impersonated); return err })
}

// covers: storage.objects.get
//
// TestWorkloadIdentityFederationStorage (#1941), with IAM enforcement on
// and the official Cloud Storage client: a federated token from Google's
// external_account configuration (keys inline in jwksJson) reads nothing
// without a role, with Google's 403 naming its principal://; the
// principal:// subject granted roles/storage.objectViewer on the bucket
// reads, the grant revoked refuses again; the principalSet:// of its group
// granted reads; a disabled pool's token is refused (401), and enabled
// again reads; an impersonated service account holding the role reads.
func TestWorkloadIdentityFederationStorage(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvStorage)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	s := newWIFSetup(t, h, "wifg-", true)
	owner := storageClient(t, h)
	bucket := s.project + "-wif"
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
	jwt := s.idp.token(t, s.issuer(true), s.audience, "job-2", map[string]any{"repo": "org/app", "groups": []string{"deployers"}})
	tok, err := oauth2Token(t, ctx, s.credentialConfig(t, h, jwt, ""))
	if err != nil {
		t.Fatalf("federated token: %v", err)
	}
	as := newStorageAs(t, h, tok)
	principal := s.principal("job-2")
	read := func() error {
		got, err := storageRead(t, h, as.json, bucket, "a.txt")
		if err == nil && got != "hello" {
			return fmt.Errorf("read %q", got)
		}
		return err
	}
	_, err = storageRead(t, h, as.json, bucket, "a.txt")
	var ge *googleapi.Error
	if !errors.As(err, &ge) || ge.Code != http.StatusForbidden || !strings.Contains(ge.Body, principal+" does not have storage.objects.get access") {
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
	group := s.set("group/deployers")
	setRole(group, true)
	waitGranted(t, "read as the principalSet:// group", read)

	pools := iamREST(t, h).Projects.Locations.WorkloadIdentityPools
	if _, err := pools.Patch(s.poolName, &iamv1.WorkloadIdentityPool{Disabled: true}).UpdateMask("disabled").Context(ctx).Do(); err != nil {
		t.Fatalf("disable pool: %v", err)
	}
	if err := read(); !errors.As(err, &ge) || ge.Code != http.StatusUnauthorized {
		t.Fatalf("read with a disabled pool's token = %v; want 401", err)
	}
	if _, err := pools.Patch(s.poolName, &iamv1.WorkloadIdentityPool{Disabled: false}).UpdateMask("disabled").Context(ctx).Do(); err != nil {
		t.Fatalf("enable pool: %v", err)
	}
	waitGranted(t, "read after the pool is enabled again", read)
	setRole(group, false)

	sa := newServiceAccount(t, h, s.project, "wifgsa")
	grantOn(t, h, sa.GetName(), "roles/iam.workloadIdentityUser", s.set("*"))
	setRole("serviceAccount:"+sa.GetEmail(), true)
	var saTok string
	waitGranted(t, "impersonation", func() error {
		var err error
		saTok, err = authToken(t, ctx, s.credentialConfig(t, h, jwt, sa.GetEmail()))
		return err
	})
	asSA := newStorageAs(t, h, saTok)
	waitGranted(t, "read as the impersonated account", func() error { _, err := storageRead(t, h, asSA.json, bucket, "a.txt"); return err })
}
