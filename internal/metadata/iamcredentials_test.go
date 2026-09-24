package metadata

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"google.golang.org/api/impersonate"
	"google.golang.org/api/option"
)

// loopbackOnly fails any request that is not to loopback, so a test proves
// no Google endpoint was contacted rather than assuming it.
type loopbackOnly struct {
	t    *testing.T
	seen []string
}

func (l *loopbackOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	host := r.URL.Hostname()
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		l.t.Errorf("a request left the machine: %s %s", r.Method, r.URL)
		return nil, fmt.Errorf("blocked non-local request to %s", r.URL.Host)
	}
	l.seen = append(l.seen, r.URL.Path)
	return http.DefaultTransport.RoundTrip(r)
}

// toLocal sends iamcredentials.googleapis.com to the local server, and
// everything through the loopback guard. Go's impersonate package has the
// IAM Credentials endpoint compiled in and takes no override, so this is how
// Go code points it at CloudBurrow (docs/credentials.md).
type toLocal struct {
	local *url.URL
	next  http.RoundTripper
}

func (tl toLocal) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host == "iamcredentials.googleapis.com" {
		r = r.Clone(r.Context())
		r.URL.Scheme, r.URL.Host, r.Host = tl.local.Scheme, tl.local.Host, tl.local.Host
	}
	return tl.next.RoundTrip(r)
}

func localClient(t *testing.T, srvURL string, guard http.RoundTripper) *http.Client {
	u, err := url.Parse(srvURL)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: toLocal{local: u, next: guard}}
}

func TestImpersonatedAccessTokenThroughTheOfficialLibrary(t *testing.T) {
	c, _ := testCreds(t)
	srv := serve(t, c)
	guard := &loopbackOnly{t: t}
	ts, err := impersonate.CredentialsTokenSource(context.Background(), impersonate.CredentialsConfig{
		TargetPrincipal: "worker@demo-project.iam.gserviceaccount.com",
		Scopes:          []string{"https://www.googleapis.com/auth/cloud-platform"},
	}, option.WithHTTPClient(localClient(t, srv.URL, guard)))
	if err != nil {
		t.Fatalf("impersonate.CredentialsTokenSource: %v", err)
	}
	tok, err := ts.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if !strings.HasPrefix(tok.AccessToken, "cbl_") || tok.Expiry.IsZero() {
		t.Errorf("token = %+v", tok)
	}
	if len(guard.seen) == 0 || !strings.HasSuffix(guard.seen[0], ":generateAccessToken") {
		t.Errorf("requests = %v", guard.seen)
	}
}

func TestImpersonatedIDTokenVerifiesAgainstTheLocalCerts(t *testing.T) {
	c, _ := testCreds(t)
	srv := serve(t, c)
	guard := &loopbackOnly{t: t}
	const audience = "https://my-service.example.test"
	const account = "worker@demo-project.iam.gserviceaccount.com"
	ts, err := impersonate.IDTokenSource(context.Background(), impersonate.IDTokenConfig{
		TargetPrincipal: account, Audience: audience, IncludeEmail: true,
	}, option.WithHTTPClient(localClient(t, srv.URL, guard)))
	if err != nil {
		t.Fatalf("impersonate.IDTokenSource: %v", err)
	}
	tok, err := ts.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	parts := strings.Split(tok.AccessToken, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %q", tok.AccessToken)
	}
	var claims struct {
		Aud, Email string
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	_ = json.Unmarshal(raw, &claims)
	if claims.Aud != audience || claims.Email != account {
		t.Errorf("claims = %+v; want the requested audience and the impersonated account", claims)
	}

	// Verified against the key /certs publishes, fetched over the same guard.
	resp, err := (&http.Client{Transport: guard}).Get(srv.URL + "/certs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var jwks struct{ Keys []struct{ N, E, Kid string } }
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil || len(jwks.Keys) != 1 {
		t.Fatalf("certs = %v, %v", jwks, err)
	}
	nb, _ := base64.RawURLEncoding.DecodeString(jwks.Keys[0].N)
	eb, _ := base64.RawURLEncoding.DecodeString(jwks.Keys[0].E)
	pub := &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(new(big.Int).SetBytes(eb).Int64())}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		t.Errorf("the ID token does not verify against the local /certs: %v", err)
	}
}

func TestSignJwtAndSignBlob(t *testing.T) {
	c, _ := testCreds(t)
	srv := serve(t, c)
	post := func(verb, body string) (int, map[string]any) {
		resp, err := http.Post(srv.URL+"/v1/projects/-/serviceAccounts/a@b.iam.gserviceaccount.com:"+verb,
			"application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	code, out := post("signJwt", `{"payload":"{\"sub\":\"x\",\"aud\":\"y\"}"}`)
	if code != http.StatusOK || out["keyId"] != c.KeyID || strings.Count(fmt.Sprint(out["signedJwt"]), ".") != 2 {
		t.Errorf("signJwt = %d %v", code, out)
	}
	code, out = post("signBlob", `{"payload":"aGk="}`)
	if code != http.StatusNotImplemented || fmt.Sprint(out["error"].(map[string]any)["status"]) != "UNIMPLEMENTED" {
		t.Errorf("signBlob = %d %v, want 501 UNIMPLEMENTED", code, out)
	}
	if code, _ := post("generateIdToken", `{}`); code != http.StatusBadRequest {
		t.Errorf("generateIdToken without an audience = %d, want 400", code)
	}
}
