package authplatform

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// consent signs in through e's consent page and is where the browser is
// sent back to.
func consent(t *testing.T, e *Endpoints, q url.Values) string {
	t.Helper()
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodGet, AuthorizePath+"?"+q.Encode(), nil))
	body := w.Body.String()
	i := strings.Index(body, `name="nonce" value="`)
	if w.Code != 200 || i < 0 {
		t.Fatalf("consent page = %d %s", w.Code, body)
	}
	i += len(`name="nonce" value="`)
	nonce := body[i : i+strings.Index(body[i:], `"`)]
	w = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, AuthorizePath, strings.NewReader(url.Values{"nonce": {nonce}, "decision": {"allow"}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	e.ServeHTTP(w, r)
	return w.Header().Get("Location")
}

// TestManagedClientAndRedeem (#2160): a Google-managed client signs in
// with no project's app, its code is redeemed without a secret by its
// owner, a project's Web client's with its secret's SHA-256, and the
// redirect to a Google endpoint is mapped onto this machine.
func TestManagedClientAndRedeem(t *testing.T) {
	s := app(t)
	const ru = "https://iap.googleapis.com/v1/oauth/clientIds/m:handleRedirect"
	s.RegisterManaged(Client{ClientID: "m", Type: TypeWeb, RedirectURIs: []string{ru}}, Brand{AppName: "Google Cloud", UserType: "INTERNAL"})
	e := &Endpoints{Store: s, User: "user@example.com", LocalRedirect: func(u string) string {
		return strings.Replace(u, "https://iap.googleapis.com/", "http://127.0.0.1:1/", 1)
	}}
	loc, _ := url.Parse(consent(t, e, url.Values{"client_id": {"m"}, "redirect_uri": {ru}, "response_type": {"code"}, "scope": {"openid email"}, "state": {"s1"}}))
	if loc.Host != "127.0.0.1:1" || loc.Path != "/v1/oauth/clientIds/m:handleRedirect" || loc.Query().Get("state") != "s1" {
		t.Fatalf("redirect = %s", loc)
	}
	if _, err := s.Redeem("m", "", loc.Query().Get("code"), "https://other/"); err == nil {
		t.Fatal("redeemed with another redirect URI")
	}
	loc, _ = url.Parse(consent(t, e, url.Values{"client_id": {"m"}, "redirect_uri": {ru}, "response_type": {"code"}, "scope": {"openid email"}}))
	g, err := s.Redeem("m", "", loc.Query().Get("code"), ru)
	if err != nil || g.Email != "user@example.com" {
		t.Fatalf("redeem = %+v %v", g, err)
	}
	if cs, _ := s.Clients("p", false); len(cs) != 0 {
		t.Fatalf("the managed client is listed: %v", cs)
	}
	c, secret, cerr := s.CreateClient("p", Client{Name: "iap", Type: TypeWeb, RedirectURIs: []string{"https://iap.googleapis.com/v1/oauth/clientIds/x:handleRedirect"}})
	if cerr != nil {
		t.Fatal(cerr)
	}
	q := url.Values{"client_id": {c.ClientID}, "redirect_uri": c.RedirectURIs, "response_type": {"code"}, "scope": {"openid email"}}
	loc, _ = url.Parse(consent(t, e, q))
	if _, err := s.Redeem(c.ClientID, hash("wrong"), loc.Query().Get("code"), c.RedirectURIs[0]); err == nil || err.OAuth != "invalid_client" {
		t.Fatalf("wrong secret = %v", err)
	}
	loc, _ = url.Parse(consent(t, e, q))
	if g, err := s.Redeem(c.ClientID, hash(secret), loc.Query().Get("code"), c.RedirectURIs[0]); err != nil || g.Email != "user@example.com" {
		t.Fatalf("redeem = %+v %v", g, err)
	}
}
