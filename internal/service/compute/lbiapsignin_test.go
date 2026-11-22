package compute

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSignIn is fakeIAP with a browser sign-in whose authorization
// endpoint is signin.test, and whose code "good" redeems as
// dev@example.com.
type fakeSignIn struct{ fakeIAP }

func (fakeSignIn) SignInURL(clientID, redirectURI, state string) string {
	return "http://signin.test/auth?" + url.Values{"client_id": {clientID}, "redirect_uri": {redirectURI}, "state": {state}}.Encode()
}

func (fakeSignIn) Redeem(clientID, secret, code, redirectURI string) (string, error) {
	if code != "good" || clientID != "managed-client" || redirectURI != IAPRedirectURI(clientID) {
		return "", io.EOF
	}
	return "dev@example.com", nil
}

func (fakeSignIn) ManagedClientID() string { return "managed-client" }

// TestIAPBrowserSignIn (#2160): a browser without a session is sent to the
// sign-in with IAP's redirect URI, comes back through handleRedirect and
// /_gcp_gatekeeper/authenticate with a session cookie, and reaches the
// app with IAP's headers; an AJAX request is 401; gcp-iap-mode IDENTITY,
// CLEAR_LOGIN_COOKIE and SECURE_TOKEN_TEST behave as documented.
func TestIAPBrowserSignIn(t *testing.T) {
	var seen atomic.Value
	s, _, base := originFixture(t, Options{IAP: fakeSignIn{fakeIAP{allow: map[string]bool{"dev@example.com": true}}}},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen.Store(r.Header.Clone())
			_, _ = io.WriteString(w, "ok "+r.URL.RequestURI())
		}))
	lbCall(t, s, "PATCH", "global/backendServices/app", obj{"iap": obj{"enabled": true}})
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(u string, hdr ...string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("GET", u, nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}
	// AJAX: 401, marked as IAP's.
	if r := get(base+"/page", "X-Requested-With", "XMLHttpRequest"); r.StatusCode != 401 || r.Header.Get("X-Goog-IAP-Generated-Response") == "" {
		t.Fatalf("XHR: %d %v", r.StatusCode, r.Header)
	}
	r := get(base + "/page?x=1")
	loc, _ := url.Parse(r.Header.Get("Location"))
	if r.StatusCode != 302 || loc.Host != "signin.test" || loc.Query().Get("redirect_uri") != "https://iap.googleapis.com/v1/oauth/clientIds/managed-client:handleRedirect" ||
		r.Header.Get("X-Goog-IAP-Generated-Response") == "" {
		t.Fatalf("browser: %d %s", r.StatusCode, loc)
	}
	state := loc.Query().Get("state")
	// A wrong code is refused.
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", IAPRedirectPrefix+"managed-client:handleRedirect?code=bad&state="+url.QueryEscape(state), nil))
	if rec.Code != 401 {
		t.Fatalf("bad code: %d %s", rec.Code, rec.Body)
	}
	// The state is used once: start again.
	r = get(base + "/page?x=1")
	loc, _ = url.Parse(r.Header.Get("Location"))
	state = loc.Query().Get("state")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest("GET", IAPRedirectPrefix+"managed-client:handleRedirect?code=good&state="+url.QueryEscape(state), nil))
	back := rec.Header().Get("Location")
	if rec.Code != 302 || !strings.HasPrefix(back, base+"/_gcp_gatekeeper/authenticate?gcp-iap-ticket=") {
		t.Fatalf("handleRedirect: %d %q", rec.Code, back)
	}
	r = get(back)
	if r.StatusCode != 302 || r.Header.Get("Location") != "/page?x=1" {
		t.Fatalf("authenticate: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	u, _ := url.Parse(base)
	names := map[string]bool{}
	for _, ck := range jar.Cookies(u) {
		names[ck.Name] = true
	}
	if !names[iapUIDCookie] || !names[iapCookieName("managed-client")] {
		t.Fatalf("cookies: %v", names)
	}
	// The ticket is used once.
	if r := get(back); r.StatusCode != 401 {
		t.Fatalf("ticket reused: %d", r.StatusCode)
	}
	r = get(base + "/page?x=1")
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 || string(b) != "ok /page?x=1" {
		t.Fatalf("signed in: %d %q", r.StatusCode, b)
	}
	if h := seen.Load().(http.Header); h.Get("X-Goog-Authenticated-User-Email") != "accounts.google.com:dev@example.com" || h.Get("X-Goog-Iap-Jwt-Assertion") == "" {
		t.Fatalf("headers: %v", h)
	}
	// IDENTITY.
	r = get(base + "/any?gcp-iap-mode=IDENTITY")
	var id map[string]string
	_ = json.NewDecoder(r.Body).Decode(&id)
	if r.StatusCode != 200 || id["email"] != "accounts.google.com:dev@example.com" || !strings.HasPrefix(id["sub"], "accounts.google.com:") {
		t.Fatalf("IDENTITY: %d %v", r.StatusCode, id)
	}
	// SECURE_TOKEN_TEST.
	get(base + "/t?gcp-iap-mode=SECURE_TOKEN_TEST&iap-secure-token-test-type=PAST_EXPIRATION")
	parts := strings.Split(seen.Load().(http.Header).Get("X-Goog-Iap-Jwt-Assertion"), ".")
	pb, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var cl map[string]any
	_ = json.Unmarshal(pb, &cl)
	if exp, _ := cl["exp"].(float64); int64(exp) > time.Now().Unix() {
		t.Fatalf("PAST_EXPIRATION: %v", cl)
	}
	// CLEAR_LOGIN_COOKIE: the cookies go, and the next visit signs in again.
	r = get(base + "/page?gcp-iap-mode=CLEAR_LOGIN_COOKIE")
	if r.StatusCode != 302 || r.Header.Get("Location") != "/page" {
		t.Fatalf("CLEAR_LOGIN_COOKIE: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	if r := get(base + "/page"); r.StatusCode != 302 {
		t.Fatalf("after CLEAR_LOGIN_COOKIE: %d", r.StatusCode)
	}
	// A forged cookie is no session.
	jar.SetCookies(u, []*http.Cookie{{Name: iapCookieName("managed-client"), Value: "a.b.c"}})
	if r := get(base + "/page"); r.StatusCode != 302 {
		t.Fatalf("forged cookie: %d", r.StatusCode)
	}
}
