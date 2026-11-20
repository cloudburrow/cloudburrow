package authplatform

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

func app(t *testing.T) *Store {
	t.Helper()
	s := NewStore(store.NewMemory(), func(string) int64 { return 123 })
	if _, _, err := s.CreateClient("p", Client{Name: "web", Type: TypeWeb}); err == nil {
		t.Fatal("a client was created before the consent screen")
	}
	if _, err := s.SetBrand("p", Brand{AppName: "Demo", SupportEmail: "dev@example.com", DeveloperEmails: []string{"dev@example.com"},
		UserType: "EXTERNAL", TestUsers: []string{"user@example.com"}}); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAuthorizationCodeFlow(t *testing.T) {
	s := app(t)
	c, secret, err := s.CreateClient("p", Client{Name: "web", Type: TypeWeb, RedirectURIs: []string{"http://localhost:9999/cb"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(c.ClientID, "123-") || !strings.HasSuffix(c.ClientID, ".apps.googleusercontent.com") || !strings.HasPrefix(secret, "GOCSPX-") {
		t.Fatalf("client %s", c.ClientID)
	}
	if c.Secrets[0].Hash == secret || c.Secrets[0].Last4 != secret[len(secret)-4:] {
		t.Fatal("the secret is kept in the clear")
	}
	verifier := "a-verifier-that-is-long-enough-for-pkce-0123456789"
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{"client_id": {c.ClientID}, "redirect_uri": {"http://localhost:9999/cb"}, "response_type": {"code"},
		"scope": {"openid email"}, "state": {"xyz"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"}, "access_type": {"offline"}}
	if _, err := s.CheckAuthorization(q, "stranger@example.com"); err == nil || err.(*Error).OAuth != "access_denied" {
		t.Fatalf("a non-tester of an app in testing = %v", err)
	}
	bad := url.Values{}
	for k, v := range q {
		bad[k] = v
	}
	bad.Set("redirect_uri", "http://localhost:9999/other")
	if _, err := s.CheckAuthorization(bad, "user@example.com"); err == nil || err.(*Error).OAuth != "redirect_uri_mismatch" {
		t.Fatalf("an unlisted redirect = %v", err)
	}
	e := &Endpoints{Store: s, User: "user@example.com"}
	w := httptest.NewRecorder()
	e.ServeHTTP(w, httptest.NewRequest(http.MethodGet, AuthorizePath+"?"+q.Encode(), nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Demo wants to access your account") {
		t.Fatalf("consent page = %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	i := strings.Index(body, `name="nonce" value="`) + len(`name="nonce" value="`)
	nonce := body[i : i+strings.Index(body[i:], `"`)]
	w = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, AuthorizePath, strings.NewReader(url.Values{"nonce": {nonce}, "decision": {"allow"}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	e.ServeHTTP(w, r)
	loc, _ := url.Parse(w.Header().Get("Location"))
	code := loc.Query().Get("code")
	if w.Code != http.StatusFound || code == "" || loc.Query().Get("state") != "xyz" {
		t.Fatalf("allow = %d %s", w.Code, w.Header().Get("Location"))
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {"http://localhost:9999/cb"},
		"client_id": {c.ClientID}, "client_secret": {"wrong"}, "code_verifier": {verifier}}
	if _, err := s.Exchange(form, "", ""); err == nil || err.OAuth != "invalid_client" {
		t.Fatalf("wrong secret = %v", err)
	}
	form.Set("client_secret", secret)
	g, oerr := s.Exchange(form, "", "")
	if oerr != nil || g.Email != "user@example.com" || g.RefreshToken == "" {
		t.Fatalf("exchange = %+v, %v", g, oerr)
	}
	if _, err := s.Exchange(form, "", ""); err == nil || err.OAuth != "invalid_grant" {
		t.Errorf("a code used twice = %v", err)
	}
	g2, oerr := s.Exchange(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {g.RefreshToken}}, c.ClientID, secret)
	if oerr != nil || g2.Email != "user@example.com" || g2.Scope != "openid email" {
		t.Fatalf("refresh = %+v, %v", g2, oerr)
	}
	// A second secret, then the first disabled: only the second works.
	_, second, err := s.AddSecret("p", c.ClientID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AddSecret("p", c.ClientID); err == nil {
		t.Error("a third secret was added")
	}
	if _, err := s.SetSecret("p", c.ClientID, c.Secrets[0].ID, "delete"); err == nil {
		t.Error("an enabled secret was deleted")
	}
	if _, err := s.SetSecret("p", c.ClientID, c.Secrets[0].ID, "disable"); err != nil {
		t.Fatal(err)
	}
	refresh := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {g.RefreshToken}}
	if _, oerr := s.Exchange(refresh, c.ClientID, secret); oerr == nil {
		t.Error("a disabled secret still works")
	}
	if _, oerr := s.Exchange(refresh, c.ClientID, second); oerr != nil {
		t.Errorf("the second secret = %v", oerr)
	}
	if !s.Revoke(g.RefreshToken) {
		t.Error("revoke")
	}
	if _, oerr := s.Exchange(refresh, c.ClientID, second); oerr == nil || oerr.OAuth != "invalid_grant" {
		t.Errorf("a revoked refresh token = %v", oerr)
	}
	// Deleted, then restored.
	if err := s.DeleteClient("p", c.ClientID, false); err != nil {
		t.Fatal(err)
	}
	if s.Knows(c.ClientID) {
		t.Error("a deleted client is still known")
	}
	if err := s.DeleteClient("p", c.ClientID, true); err != nil || !s.Knows(c.ClientID) {
		t.Errorf("restore = %v", err)
	}
}

func TestDesktopClientsTakeAnyLoopbackPort(t *testing.T) {
	s := app(t)
	c, _, err := s.CreateClient("p", Client{Name: "cli", Type: TypeDesktop})
	if err != nil {
		t.Fatal(err)
	}
	q := url.Values{"client_id": {c.ClientID}, "redirect_uri": {"http://127.0.0.1:53111/"}, "response_type": {"code"}, "scope": {"email"}}
	if _, err := s.CheckAuthorization(q, "user@example.com"); err != nil {
		t.Errorf("loopback redirect = %v", err)
	}
	q.Set("redirect_uri", "https://example.com/cb")
	if _, err := s.CheckAuthorization(q, "user@example.com"); err == nil {
		t.Error("a desktop client redirected off the machine")
	}
	if _, _, err := s.CreateClient("p", Client{Name: "bad", Type: TypeWeb, RedirectURIs: []string{"http://example.com/cb"}}); err == nil {
		t.Error("a web client's http redirect off localhost was accepted")
	}
}
