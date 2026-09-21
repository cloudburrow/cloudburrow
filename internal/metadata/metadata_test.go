package metadata

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testCreds(t *testing.T) (*Credentials, string) {
	t.Helper()
	dir := t.TempDir()
	c, err := LoadOrCreate(dir, "demo", "http://127.0.0.1:9005/token")
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	return c, dir
}

func serve(t *testing.T, c *Credentials) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewServer(c, "demo", "127.0.0.1", 0).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func getMeta(t *testing.T, srv *httptest.Server, path string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(FlavorHeader, FlavorValue)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp.Header
}

// Without the header, any web page a developer visited could read tokens from
// the metadata server with a plain cross-origin request. A local server that
// accepted what a real one rejects would teach code to work here and fail in
// production.
func TestFlavorHeaderIsRequired(t *testing.T) {
	t.Parallel()
	c, _ := testCreds(t)
	srv := serve(t, c)

	resp, err := http.Get(srv.URL + "/computeMetadata/v1/instance/service-accounts/default/token")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a request without %s = %d, want 403", FlavorHeader, resp.StatusCode)
	}
}

// Clients decide they are on GCE by probing "/" and reading only the response
// header, so it must be set on every response including refusals.
func TestFlavorHeaderIsAlwaysSetOnResponses(t *testing.T) {
	t.Parallel()
	c, _ := testCreds(t)
	srv := serve(t, c)

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get(FlavorHeader); got != FlavorValue {
		t.Errorf("refused response carried %s = %q, want %q", FlavorHeader, got, FlavorValue)
	}

	_, _, hdr := getMeta(t, srv, "/")
	if got := hdr.Get(FlavorHeader); got != FlavorValue {
		t.Errorf("accepted response carried %s = %q", FlavorHeader, got)
	}
}

func TestMetadataTreeAnswersTheStandardKeys(t *testing.T) {
	t.Parallel()
	c, _ := testCreds(t)
	srv := serve(t, c)

	for _, tc := range []struct{ path, want string }{
		{"/", "computeMetadata/"},
		{"/computeMetadata/v1/", "instance/"},
		{"/computeMetadata/v1/project/project-id", "demo"},
		{"/computeMetadata/v1/instance/service-accounts/default/email", c.Email},
		{"/computeMetadata/v1/instance/service-accounts/default/aliases", "default"},
		{"/computeMetadata/v1/instance/service-accounts/default/scopes", "cloud-platform"},
		{"/computeMetadata/v1/instance/service-accounts/", "default/"},
	} {
		code, body, _ := getMeta(t, srv, tc.path)
		if code != http.StatusOK {
			t.Errorf("GET %s = %d", tc.path, code)
			continue
		}
		if !strings.Contains(body, tc.want) {
			t.Errorf("GET %s = %q, want it to contain %q", tc.path, body, tc.want)
		}
	}
}

// Clients parse the last path segment out of the zone, so a bare zone name
// breaks them.
func TestZoneIsFullyQualified(t *testing.T) {
	t.Parallel()
	c, _ := testCreds(t)
	srv := serve(t, c)

	_, body, _ := getMeta(t, srv, "/computeMetadata/v1/instance/zone")
	if !strings.HasPrefix(body, "projects/") || !strings.HasSuffix(body, "/"+DefaultZone) {
		t.Errorf("zone = %q, want projects/<n>/zones/<zone>", body)
	}
}

// A client configured with the account's own email rather than "default" must
// not get a 404, which it would read as "no such account".
func TestServiceAccountIsAddressableByEmailAndByDefault(t *testing.T) {
	t.Parallel()
	c, _ := testCreds(t)
	srv := serve(t, c)

	for _, account := range []string{"default", c.Email} {
		path := "/computeMetadata/v1/instance/service-accounts/" + account + "/email"
		code, body, _ := getMeta(t, srv, path)
		if code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, code)
		}
		if body != c.Email {
			t.Errorf("GET %s = %q, want %q", path, body, c.Email)
		}
	}
}

func TestUnknownServiceAccountIsNotFound(t *testing.T) {
	t.Parallel()
	c, _ := testCreds(t)
	srv := serve(t, c)

	code, _, _ := getMeta(t, srv, "/computeMetadata/v1/instance/service-accounts/someone-else/email")
	if code != http.StatusNotFound {
		t.Errorf("unknown account = %d, want 404", code)
	}
}

func TestAccessTokenHasTheOAuthShape(t *testing.T) {
	t.Parallel()
	c, _ := testCreds(t)
	srv := serve(t, c)

	code, body, _ := getMeta(t, srv, "/computeMetadata/v1/instance/service-accounts/default/token")
	if code != http.StatusOK {
		t.Fatalf("token = %d: %s", code, body)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal([]byte(body), &tok); err != nil {
		t.Fatalf("decode token: %v\n%s", err, body)
	}
	if tok.TokenType != "Bearer" || tok.AccessToken == "" || tok.ExpiresIn <= 0 {
		t.Errorf("token = %+v", tok)
	}
	// An access token is opaque on Google too. Making it look like a JWT
	// would invite someone to try to verify it.
	if strings.Count(tok.AccessToken, ".") == 2 {
		t.Errorf("the access token looks like a JWT: %q", tok.AccessToken)
	}
}

// A client that forgot the audience needs to be told, not handed a token for
// nothing.
func TestIdentityRequiresAnAudience(t *testing.T) {
	t.Parallel()
	c, _ := testCreds(t)
	srv := serve(t, c)

	code, _, _ := getMeta(t, srv, "/computeMetadata/v1/instance/service-accounts/default/identity")
	if code != http.StatusBadRequest {
		t.Errorf("identity without an audience = %d, want 400", code)
	}
}

// The ID token must be a structurally valid RS256 JWT that verifies against
// the key this instance publishes — self-consistent, even though it is not
// Google-issued.
func TestIdentityTokenVerifiesAgainstThePublishedKey(t *testing.T) {
	t.Parallel()
	c, _ := testCreds(t)
	srv := serve(t, c)

	const audience = "https://example.test/callback"
	code, token, _ := getMeta(t, srv,
		"/computeMetadata/v1/instance/service-accounts/default/identity?audience="+url.QueryEscape(audience))
	if code != http.StatusOK {
		t.Fatalf("identity = %d: %s", code, token)
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments, want 3: %q", len(parts), token)
	}

	var claims struct {
		Aud string `json:"aud"`
		Iss string `json:"iss"`
		Exp int64  `json:"exp"`
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("parse claims: %v", err)
	}
	if claims.Aud != audience {
		t.Errorf("aud = %q, want %q", claims.Aud, audience)
	}
	if claims.Exp <= time.Now().Unix() {
		t.Error("token is already expired")
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&c.PrivateKey.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Errorf("signature does not verify against the instance key: %v", err)
	}
}

func TestJWKSPublishesTheSigningKey(t *testing.T) {
	t.Parallel()
	c, _ := testCreds(t)
	srv := serve(t, c)

	resp, err := http.Get(srv.URL + "/certs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var jwks struct {
		Keys []struct {
			Kid, Kty, Alg, N, E string
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		t.Fatalf("decode JWKS: %v", err)
	}
	if len(jwks.Keys) != 1 {
		t.Fatalf("JWKS has %d keys, want 1", len(jwks.Keys))
	}
	k := jwks.Keys[0]
	if k.Kid != c.KeyID {
		t.Errorf("kid = %q, want %q so a token's kid can be matched", k.Kid, c.KeyID)
	}
	if k.Kty != "RSA" || k.Alg != "RS256" || k.N == "" || k.E == "" {
		t.Errorf("JWKS entry is incomplete: %+v", k)
	}
}

// The token endpoint does not verify the assertion — CloudBurrow
// authenticates nothing — but a client that is silently sending none should
// find out rather than receive a token.
func TestTokenExchangeRequiresAnAssertion(t *testing.T) {
	t.Parallel()
	c, _ := testCreds(t)
	srv := serve(t, c)

	resp, err := http.PostForm(srv.URL+"/token", url.Values{"grant_type": {"x"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty token request = %d, want 400", resp.StatusCode)
	}

	resp, err = http.PostForm(srv.URL+"/token", url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {"anything"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("token exchange = %d, want 200", resp.StatusCode)
	}
}

func TestTokenExchangeRefusesGET(t *testing.T) {
	t.Parallel()
	c, _ := testCreds(t)
	srv := serve(t, c)

	resp, err := http.Get(srv.URL + "/token")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /token = %d, want 405", resp.StatusCode)
	}
}

// A key that changed on every run would invalidate any client that had
// already read the fixture, failing with an opaque signature error.
func TestCredentialsKeyIsStableAcrossRuns(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	first, err := LoadOrCreate(dir, "demo", "http://127.0.0.1:9005/token")
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreate(dir, "demo", "http://127.0.0.1:9005/token")
	if err != nil {
		t.Fatal(err)
	}
	if !first.PrivateKey.Equal(second.PrivateKey) {
		t.Error("the key was regenerated on the second run")
	}
	if first.KeyID != second.KeyID {
		t.Errorf("key id changed: %q then %q", first.KeyID, second.KeyID)
	}
}

func TestCredentialsKeyIsNotWorldReadable(t *testing.T) {
	t.Parallel()
	_, dir := testCreds(t)
	for _, name := range []string{KeyFileName, CredentialsFileName} {
		path := filepath.Join(dir, name)
		if name == CredentialsFileName {
			c, _ := LoadOrCreate(dir, "demo", "http://127.0.0.1:9005/token")
			if _, err := c.WriteADC(dir); err != nil {
				t.Fatal(err)
			}
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if mode := info.Mode().Perm(); mode&0o077 != 0 {
			t.Errorf("%s mode = %o, want no group or other access", name, mode)
		}
	}
}

// The fixture must point at CloudBurrow, or a client reading it would talk to
// Google.
func TestADCPointsEntirelyAtTheLocalInstance(t *testing.T) {
	t.Parallel()
	c, dir := testCreds(t)
	path, err := c.WriteADC(dir)
	if err != nil {
		t.Fatalf("WriteADC: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var adc map[string]any
	if err := json.Unmarshal(body, &adc); err != nil {
		t.Fatalf("the fixture is not valid JSON: %v", err)
	}
	if adc["type"] != "service_account" {
		t.Errorf("type = %v, want service_account", adc["type"])
	}
	if adc["project_id"] != "demo" {
		t.Errorf("project_id = %v", adc["project_id"])
	}
	for _, key := range []string{"token_uri", "auth_uri", "auth_provider_x509_cert_url", "client_x509_cert_url"} {
		v, _ := adc[key].(string)
		if !strings.Contains(v, "127.0.0.1") {
			t.Errorf("%s = %q, which is not the local instance", key, v)
		}
		if strings.Contains(v, "googleapis.com") || strings.Contains(v, "google.com") {
			t.Errorf("%s = %q reaches out to Google", key, v)
		}
	}
	// Anyone who opens the file, or finds it by accident, should see at once
	// that it grants nothing.
	if note, _ := adc["_cloudburrow"].(string); !strings.Contains(note, "authorises nothing") {
		t.Errorf("the fixture does not say what it is: %q", note)
	}
	if !strings.Contains(string(body), "BEGIN PRIVATE KEY") {
		t.Error("the fixture carries no private key, so no client can use it")
	}
}

func TestLoadOrCreateRejectsACorruptKey(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, KeyFileName), []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(dir, "demo", "http://127.0.0.1:9005/token"); err == nil {
		t.Fatal("a corrupt key file was accepted")
	}
}

func TestServiceAccountEmailNamesItsOrigin(t *testing.T) {
	t.Parallel()
	got := ServiceAccountEmail("myproj")
	if !strings.HasPrefix(got, "cloudburrow-local@") {
		t.Errorf("email = %q, want a name that says where it came from", got)
	}
	if !strings.HasSuffix(got, "@myproj.iam.gserviceaccount.com") {
		t.Errorf("email = %q, want the real service-account domain shape", got)
	}
}

func TestServerStartsStopsAndServes(t *testing.T) {
	t.Parallel()
	c, _ := testCreds(t)
	srv := NewServer(c, "demo", "127.0.0.1", 0)

	if srv.Addr() != "" {
		t.Error("Addr reported an address before Start")
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	addr := srv.Addr()
	if addr == "" {
		t.Fatal("Addr is empty after Start")
	}

	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/computeMetadata/v1/project/project-id", nil)
	req.Header.Set(FlavorHeader, FlavorValue)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "demo" {
		t.Errorf("project-id = %q", body)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// Stop must be idempotent: the coordinator may stop a component that
	// never started, or stop twice during a failed startup.
	if err := srv.Stop(ctx); err != nil {
		t.Errorf("second Stop: %v", err)
	}
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Error("the server still served a request after Stop")
	}
}

func TestStartReportsABindFailure(t *testing.T) {
	t.Parallel()
	c, _ := testCreds(t)

	busy := NewServer(c, "demo", "127.0.0.1", 0)
	if err := busy.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Stop(context.Background()) }()

	_, portStr, _ := net.SplitHostPort(busy.Addr())
	port, _ := strconv.Atoi(portStr)

	clash := NewServer(c, "demo", "127.0.0.1", port)
	if err := clash.Start(context.Background()); err == nil {
		_ = clash.Stop(context.Background())
		t.Fatal("binding a port already in use reported success")
	}
}

func TestRemainingInstanceKeysAnswer(t *testing.T) {
	t.Parallel()
	c, _ := testCreds(t)
	srv := serve(t, c)

	for _, path := range []string{
		"/computeMetadata/v1/instance/id",
		"/computeMetadata/v1/instance/name",
		"/computeMetadata/v1/instance/machine-type",
		"/computeMetadata/v1/project/numeric-project-id",
	} {
		if code, body, _ := getMeta(t, srv, path); code != http.StatusOK || body == "" {
			t.Errorf("GET %s = %d %q", path, code, body)
		}
	}
}

// An unknown key must be a 404, not an empty 200: a client reading a blank
// value would treat it as a configured empty string.
func TestUnknownKeysAreNotFound(t *testing.T) {
	t.Parallel()
	c, _ := testCreds(t)
	srv := serve(t, c)

	for _, path := range []string{
		"/computeMetadata/v1/instance/service-accounts/default/nosuchkey",
		"/nosuchpath",
	} {
		if code, _, _ := getMeta(t, srv, path); code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, code)
		}
	}
}
