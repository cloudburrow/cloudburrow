package metadata

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// fakeDirectory is one account, "target", which "caller" is granted on.
type fakeDirectory struct {
	key      *rsa.PrivateKey
	disabled bool
	grants   map[string]bool // member + " " + permission
	userKey  *rsa.PrivateKey
	keyOK    bool
}

const (
	targetEmail = "target@demo.iam.gserviceaccount.com"
	callerEmail = "caller@demo.iam.gserviceaccount.com"
)

func (f *fakeDirectory) Account(name string) (DirectoryAccount, bool) {
	switch name {
	case targetEmail, "100000000000000000001":
		return DirectoryAccount{Email: targetEmail, UniqueID: "100000000000000000001", Disabled: f.disabled}, true
	case callerEmail:
		return DirectoryAccount{Email: callerEmail, UniqueID: "100000000000000000002"}, true
	}
	return DirectoryAccount{}, false
}

func (f *fakeDirectory) Granted(member, account, permission string) bool {
	return f.grants[member+" "+permission]
}

func (f *fakeDirectory) Signer(string) (*rsa.PrivateKey, string, error) {
	return f.key, "system-key", nil
}

func (f *fakeDirectory) Key(kid string, _ time.Time) (*rsa.PublicKey, string, bool, bool) {
	if kid != "user-key" {
		return nil, "", false, false
	}
	return &f.userKey.PublicKey, targetEmail, true, f.keyOK
}

func callIAM(t *testing.T, base, account, verb, bearer string, body map[string]any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/projects/-/serviceAccounts/"+account+":"+verb, bytes.NewReader(b))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func errStatus(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	s, _ := e["status"].(string)
	return s
}

// IAM Credentials with the service accounts (#1747): authentication, the
// permission, self-impersonation, disabled accounts, the lifetime, and
// signBlob and signJwt with the account's key.
func TestIAMCredentialsAuthorisesAgainstTheDirectory(t *testing.T) {
	c, _ := testCreds(t)
	sysKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	d := &fakeDirectory{key: sysKey, grants: map[string]bool{}}
	c.SetServiceAccounts(d)
	srv := serve(t, c)
	scope := map[string]any{"scope": []string{"https://www.googleapis.com/auth/cloud-platform"}}
	caller := c.MintAccessTokenFor(callerEmail, time.Hour)

	if code, out := callIAM(t, srv.URL, targetEmail, "generateAccessToken", "", scope); code != 401 || errStatus(out) != "UNAUTHENTICATED" {
		t.Errorf("no bearer = %d %v", code, errStatus(out))
	}
	if code, _ := callIAM(t, srv.URL, targetEmail, "generateAccessToken", "not-a-token", scope); code != 401 {
		t.Errorf("an unknown bearer = %d", code)
	}
	code, out := callIAM(t, srv.URL, targetEmail, "generateAccessToken", caller, scope)
	if e, _ := out["error"].(map[string]any); code != 403 || !strings.Contains(e["message"].(string), "iam.serviceAccounts.getAccessToken") {
		t.Errorf("without the permission = %d %v", code, out)
	}
	if code, _ := callIAM(t, srv.URL, "nobody@demo.iam.gserviceaccount.com", "generateAccessToken", caller, scope); code != 403 {
		t.Errorf("an unknown account = %d, want 403", code)
	}
	for _, p := range []string{"getAccessToken", "getOpenIdToken", "signBlob", "signJwt"} {
		d.grants["serviceAccount:"+callerEmail+" iam.serviceAccounts."+p] = true
	}
	code, out = callIAM(t, srv.URL, targetEmail, "generateAccessToken", caller, scope)
	tok, _ := out["accessToken"].(string)
	if code != 200 || tok == "" {
		t.Fatalf("granted = %d %v", code, errStatus(out))
	}
	if who, ok := c.tokenAccount(tok, time.Now()); !ok || who != targetEmail {
		t.Errorf("the minted token is for %q", who)
	}
	if code, _ := callIAM(t, srv.URL, "100000000000000000001", "generateAccessToken", caller, scope); code != 200 {
		t.Errorf("by unique ID = %d", code)
	}
	if code, out := callIAM(t, srv.URL, targetEmail, "generateAccessToken", caller, map[string]any{}); code != 400 {
		t.Errorf("no scope = %d %v", code, errStatus(out))
	}
	if code, _ := callIAM(t, srv.URL, targetEmail, "generateAccessToken", caller, map[string]any{"scope": []string{"x"}, "lifetime": "3601s"}); code != 400 {
		t.Errorf("a lifetime over 3600s = %d", code)
	}
	// The target may not mint for itself.
	if code, out := callIAM(t, srv.URL, targetEmail, "generateAccessToken", tok, scope); code != 400 || errStatus(out) != "FAILED_PRECONDITION" {
		t.Errorf("self-impersonation = %d %v", code, errStatus(out))
	}
	// ID token: the account's unique ID is its subject.
	code, out = callIAM(t, srv.URL, targetEmail, "generateIdToken", caller, map[string]any{"audience": "https://svc.example", "includeEmail": true})
	idt, _ := out["token"].(string)
	if code != 200 || len(strings.Split(idt, ".")) != 3 {
		t.Fatalf("generateIdToken = %d", code)
	}
	payload, _ := base64.RawURLEncoding.DecodeString(strings.Split(idt, ".")[1])
	var claims map[string]any
	_ = json.Unmarshal(payload, &claims)
	if claims["sub"] != "100000000000000000001" || claims["email"] != targetEmail || claims["aud"] != "https://svc.example" {
		t.Errorf("ID token claims: sub %v, email %v, aud %v", claims["sub"], claims["email"], claims["aud"])
	}
	// signBlob with the system-managed key.
	code, out = callIAM(t, srv.URL, targetEmail, "signBlob", caller, map[string]any{"payload": base64.StdEncoding.EncodeToString([]byte("blob"))})
	sig, _ := base64.StdEncoding.DecodeString(func() string { s, _ := out["signedBlob"].(string); return s }())
	sum := sha256.Sum256([]byte("blob"))
	if code != 200 || out["keyId"] != "system-key" || rsa.VerifyPKCS1v15(&sysKey.PublicKey, cryptoSHA256, sum[:], sig) != nil {
		t.Errorf("signBlob = %d, keyId %v", code, out["keyId"])
	}
	// signJwt too, with the key ID in its header.
	code, out = callIAM(t, srv.URL, targetEmail, "signJwt", caller, map[string]any{"payload": `{"iss":"` + targetEmail + `","aud":"x"}`})
	jwt, _ := out["signedJwt"].(string)
	parts := strings.Split(jwt, ".")
	if code != 200 || len(parts) != 3 {
		t.Fatalf("signJwt = %d", code)
	}
	h, _ := base64.RawURLEncoding.DecodeString(parts[0])
	jsig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	jsum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !strings.Contains(string(h), `"kid":"system-key"`) || rsa.VerifyPKCS1v15(&sysKey.PublicKey, cryptoSHA256, jsum[:], jsig) != nil {
		t.Errorf("signJwt header %s, or its signature does not verify", h)
	}
	if code, _ := callIAM(t, srv.URL, targetEmail, "signJwt", caller, map[string]any{"payload": `{"exp":` + itoa64(time.Now().Add(13*time.Hour).Unix()) + `}`}); code != 400 {
		t.Errorf("an exp over 12 hours = %d", code)
	}
	// A disabled account is refused.
	d.disabled = true
	if code, out := callIAM(t, srv.URL, targetEmail, "generateAccessToken", caller, scope); code != 400 || errStatus(out) != "FAILED_PRECONDITION" {
		t.Errorf("a disabled account = %d %v", code, errStatus(out))
	}
	d.disabled = false
	// The instance's own account keeps the earlier behaviour.
	if code, _ := callIAM(t, srv.URL, "-", "generateAccessToken", "", scope); code != 200 {
		t.Errorf("the instance's own account = %d", code)
	}
	// Delegation: caller -> delegate needs implicitDelegation.
	if code, _ := callIAM(t, srv.URL, targetEmail, "generateAccessToken", caller, map[string]any{"scope": []string{"x"},
		"delegates": []string{"projects/-/serviceAccounts/" + callerEmail}}); code != 403 {
		t.Errorf("an ungranted delegate = %d, want 403", code)
	}
}

func itoa64(n int64) string { b, _ := json.Marshal(n); return string(b) }

// The token endpoint and a service account key (#1747): accepted while the
// key is usable, refused with invalid_grant once it is not.
func TestTokenEndpointAcceptsServiceAccountKeys(t *testing.T) {
	c, _ := testCreds(t)
	uk, _ := rsa.GenerateKey(rand.Reader, 2048)
	d := &fakeDirectory{userKey: uk, keyOK: true, grants: map[string]bool{}}
	c.SetServiceAccounts(d)
	srv := serve(t, c)
	now := time.Now()
	assertion, err := signWith(uk, map[string]any{"alg": "RS256", "typ": "JWT", "kid": "user-key"},
		map[string]any{"iss": targetEmail, "aud": srv.URL + "/token", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "scope": "x"})
	if err != nil {
		t.Fatal(err)
	}
	exchange := func() (int, map[string]any) {
		resp, err := http.PostForm(srv.URL+"/token", url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {assertion}})
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	code, out := exchange()
	tok, _ := out["access_token"].(string)
	if code != 200 || tok == "" {
		t.Fatalf("exchange = %d", code)
	}
	if who, ok := c.tokenAccount(tok, time.Now()); !ok || who != targetEmail {
		t.Errorf("the token is for %q", who)
	}
	d.keyOK = false
	if code, out := exchange(); code != 400 || out["error"] != "invalid_grant" {
		t.Errorf("a disabled key = %d %v", code, out["error"])
	}
}
