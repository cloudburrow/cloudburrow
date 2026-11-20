package metadata

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/wif"
)

type fakeFederation struct {
	p       wif.Provider
	live    bool
	sets    map[string][]string
	revoked bool
}

func (f *fakeFederation) FederationProvider(aud string) (wif.Provider, bool) {
	return f.p, f.live && aud == f.p.Name()
}

func (f *fakeFederation) RecordFederatedPrincipal(p string, sets []string) error {
	f.sets[p] = sets
	return nil
}

func (f *fakeFederation) FederatedPrincipalActive(string) bool { return !f.revoked }

func testIdP(t *testing.T) (*rsa.PrivateKey, *httptest.Server) {
	t.Helper()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": srv.URL, "jwks_uri": srv.URL + "/jwks"})
		case "/jwks":
			b64 := func(i *big.Int) string { return base64.RawURLEncoding.EncodeToString(i.Bytes()) }
			_ = json.NewEncoder(w).Encode(wif.JWKS{Keys: []wif.JWK{{Kty: "RSA", Kid: "k1", Alg: "RS256", Use: "sig",
				N: b64(key.N), E: b64(big.NewInt(int64(key.E)))}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return key, srv
}

func signTestJWT(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	in := enc(map[string]any{"alg": "RS256", "kid": "k1", "typ": "JWT"}) + "." + enc(claims)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return in + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func exchange(t *testing.T, base string, form url.Values) (int, map[string]any) {
	t.Helper()
	resp, err := http.PostForm(base+STSPath, form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// The STS (#1941): an OIDC token from a local issuer, verified with the
// keys its discovery document names, exchanged for a token whose account
// is the subject's principal://; Google's OAuth errors otherwise.
func TestSTSExchangesALocalOIDCToken(t *testing.T) {
	c, _ := testCreds(t)
	key, idp := testIdP(t)
	f := &fakeFederation{live: true, sets: map[string][]string{}, p: wif.Provider{ProjectNumber: "42", Pool: "pool", ID: "prov",
		Kind: wif.KindOIDC, IssuerURI: idp.URL,
		Mapping:   map[string]string{"google.subject": "assertion.sub", "attribute.env": "assertion.env"},
		Condition: "attribute.env == 'ci'"}}
	c.SetFederation(f)
	srv := serve(t, c)
	now := time.Now()
	claims := map[string]any{"iss": idp.URL, "sub": "job-7", "aud": "https:" + f.p.Name(), "env": "ci",
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}
	form := url.Values{"grant_type": {stsGrantType}, "requested_token_type": {stsAccessTokenType},
		"subject_token_type": {stsJWTType}, "audience": {f.p.Name()}, "scope": {"https://www.googleapis.com/auth/cloud-platform"},
		"subject_token": {signTestJWT(t, key, claims)}}
	code, out := exchange(t, srv.URL, form)
	if code != 200 || out["token_type"] != "Bearer" || out["issued_token_type"] != stsAccessTokenType {
		t.Fatalf("exchange = %d %v", code, out)
	}
	principal := wif.Principal("42", "pool", "job-7")
	if who, ok := c.TokenAccount(out["access_token"].(string), time.Now()); !ok || who != principal {
		t.Fatalf("token account = %q %v; want %q", who, ok, principal)
	}
	if len(f.sets[principal]) != 2 {
		t.Fatalf("sets = %v", f.sets[principal])
	}
	f.revoked = true
	if _, ok := c.TokenAccount(out["access_token"].(string), time.Now()); ok {
		t.Fatal("a token of a disabled pool is still live")
	}

	bad := func(name string, mutate func(url.Values, map[string]any), wantCode int, wantErr, wantMsg string) {
		t.Helper()
		f2 := url.Values{}
		for k, v := range form {
			f2[k] = append([]string(nil), v...)
		}
		cl := map[string]any{}
		for k, v := range claims {
			cl[k] = v
		}
		mutate(f2, cl)
		if f2.Get("subject_token") == form.Get("subject_token") {
			f2.Set("subject_token", signTestJWT(t, key, cl))
		}
		code, out := exchange(t, srv.URL, f2)
		desc, _ := out["error_description"].(string)
		if code != wantCode || out["error"] != wantErr || !strings.Contains(desc, wantMsg) {
			t.Errorf("%s = %d %v; want %d %s %q", name, code, out, wantCode, wantErr, wantMsg)
		}
	}
	bad("wrong audience claim", func(_ url.Values, c map[string]any) { c["aud"] = "other" }, 400, "invalid_grant", "does not match the expected audience")
	bad("expired", func(_ url.Values, c map[string]any) { c["exp"] = now.Add(-time.Minute).Unix() }, 400, "invalid_grant", "expired")
	bad("condition", func(_ url.Values, c map[string]any) { c["env"] = "prod" }, 400, "invalid_grant", "rejected by the attribute condition")
	bad("issuer", func(_ url.Values, c map[string]any) { c["iss"] = "https://elsewhere" }, 400, "invalid_grant", "does not match the expected issuer")
	bad("unknown provider", func(v url.Values, _ map[string]any) {
		v.Set("audience", "//iam.googleapis.com/projects/42/locations/global/workloadIdentityPools/pool/providers/nope")
	},
		400, "invalid_target", "audience")
	bad("grant type", func(v url.Values, _ map[string]any) { v.Set("grant_type", "password") }, 400, "unsupported_grant_type", "grant_type")
	bad("token type", func(v url.Values, _ map[string]any) { v.Set("subject_token_type", "urn:x") }, 400, "invalid_request", "subject_token_type")
	bad("aws", func(v url.Values, _ map[string]any) { v.Set("subject_token_type", stsAWSType) }, 501, "invalid_request", "not implemented")
	bad("bad signature", func(v url.Values, _ map[string]any) {
		v.Set("subject_token", form.Get("subject_token")[:len(form.Get("subject_token"))-6]+"AAAAAA")
	},
		400, "invalid_grant", "signature")
}
