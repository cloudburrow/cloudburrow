//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	sv "google.golang.org/api/siteverification/v1"
)

func siteVerificationClient(t *testing.T, h *Harness) *sv.Service {
	t.Helper()
	hc := &http.Client{}
	t.Cleanup(hc.CloseIdleConnections)
	c, err := sv.NewService(h.Context(), option.WithEndpoint("http://"+h.Endpoint(EnvResourceManager)+"/siteVerification/v1/"),
		option.WithoutAuthentication(), option.WithHTTPClient(hc))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// localSite serves a site on loopback whose files the test sets.
func localSite(t *testing.T) (string, map[string]string) {
	t.Helper()
	files := map[string]string{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body, ok := files[r.URL.Path]; ok {
			_, _ = io.WriteString(w, body)
			return
		}
		http.NotFound(w, r)
	}), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String() + "/", files
}

// covers: siteVerification.webResource.getToken, siteVerification.webResource.insert, siteVerification.webResource.get, siteVerification.webResource.list, siteVerification.webResource.update, siteVerification.webResource.patch, siteVerification.webResource.delete
//
// TestSiteVerificationLifecycle (#2030), with
// google.golang.org/api/siteverification/v1: a FILE and a META token for a
// site served on this machine, insert refused until the token is in place,
// then the verified site read, listed, its owners updated and patched, and
// the verification deleted; a DNS method is refused, as the instance looks
// nothing up off the machine.
func TestSiteVerificationLifecycle(t *testing.T) {
	h := New(t)
	c := siteVerificationClient(t, h)
	ctx := h.Context()
	site, files := localSite(t)
	tok, err := c.WebResource.GetToken(&sv.SiteVerificationWebResourceGettokenRequest{VerificationMethod: "FILE",
		Site: &sv.SiteVerificationWebResourceGettokenRequestSite{Type: "SITE", Identifier: site}}).Context(ctx).Do()
	if err != nil {
		t.Fatal(err)
	}
	if tok.Method != "FILE" || !strings.HasPrefix(tok.Token, "google") || !strings.HasSuffix(tok.Token, ".html") {
		t.Fatalf("token = %+v", tok)
	}
	res := &sv.SiteVerificationWebResourceResource{Site: &sv.SiteVerificationWebResourceResourceSite{Type: "SITE", Identifier: site}}
	_, err = c.WebResource.Insert("FILE", res).Context(ctx).Do()
	var ge *googleapi.Error
	if !errorsAs(err, &ge) || ge.Code != 400 {
		t.Fatalf("insert before the token is placed = %v", err)
	}
	files["/"+tok.Token] = "google-site-verification: " + tok.Token
	got, err := c.WebResource.Insert("FILE", res).Context(ctx).Do()
	if err != nil {
		t.Fatal(err)
	}
	if got.Id == "" || got.Site.Identifier != site || len(got.Owners) == 0 {
		t.Fatalf("inserted = %+v", got)
	}
	t.Cleanup(func() { _ = c.WebResource.Delete(got.Id).Context(context.Background()).Do() })
	owner := got.Owners[0]
	// META on the same site, by its home page's tag.
	meta, err := c.WebResource.GetToken(&sv.SiteVerificationWebResourceGettokenRequest{VerificationMethod: "META",
		Site: &sv.SiteVerificationWebResourceGettokenRequestSite{Type: "SITE", Identifier: site}}).Context(ctx).Do()
	if err != nil || !strings.Contains(meta.Token, `name="google-site-verification"`) {
		t.Fatalf("META token = %+v, %v", meta, err)
	}
	files["/"] = "<html><head>" + meta.Token + "</head></html>"
	if _, err := c.WebResource.Insert("META", res).Context(ctx).Do(); err != nil {
		t.Errorf("META insert: %v", err)
	}
	if g, err := c.WebResource.Get(got.Id).Context(ctx).Do(); err != nil || g.Id != got.Id {
		t.Fatalf("get = %+v, %v", g, err)
	}
	list, err := c.WebResource.List().Context(ctx).Do()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range list.Items {
		found = found || it.Id == got.Id
	}
	if !found {
		t.Errorf("list lacks %s", got.Id)
	}
	up, err := c.WebResource.Update(got.Id, &sv.SiteVerificationWebResourceResource{Owners: []string{owner, "teammate@example.com"}}).Context(ctx).Do()
	if err != nil || len(up.Owners) != 2 {
		t.Fatalf("update = %+v, %v", up, err)
	}
	pa, err := c.WebResource.Patch(got.Id, &sv.SiteVerificationWebResourceResource{Owners: []string{owner}}).Context(ctx).Do()
	if err != nil || len(pa.Owners) != 1 {
		t.Fatalf("patch = %+v, %v", pa, err)
	}
	if err := c.WebResource.Delete(got.Id).Context(ctx).Do(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.WebResource.Get(got.Id).Context(ctx).Do(); !errorsAs(err, &ge) || ge.Code != 404 {
		t.Errorf("get after delete = %v", err)
	}
	_, err = c.WebResource.Insert("DNS_TXT", &sv.SiteVerificationWebResourceResource{Site: &sv.SiteVerificationWebResourceResourceSite{Type: "INET_DOMAIN", Identifier: "example.com"}}).Context(ctx).Do()
	if !errorsAs(err, &ge) || ge.Code != 400 || !strings.Contains(ge.Message, "public DNS") {
		t.Errorf("DNS_TXT = %v", err)
	}
}

// TestTerraformSiteVerification (#2030): data google_site_verification_token
// and google_site_verification_web_resource verify a site served on this
// machine through site_verification_custom_endpoint.
func TestTerraformSiteVerification(t *testing.T) {
	h := New(t)
	c := siteVerificationClient(t, h)
	site, files := localSite(t)
	tok, err := c.WebResource.GetToken(&sv.SiteVerificationWebResourceGettokenRequest{VerificationMethod: "FILE",
		Site: &sv.SiteVerificationWebResourceGettokenRequestSite{Type: "SITE", Identifier: site}}).Context(h.Context()).Do()
	if err != nil {
		t.Fatal(err)
	}
	files["/"+tok.Token] = "google-site-verification: " + tok.Token
	m := newTFModule(t)
	m.write(fmt.Sprintf(`
data "google_site_verification_token" "t" {
  type                = "SITE"
  identifier          = %q
  verification_method = "FILE"
}

resource "google_site_verification_web_resource" "r" {
  site {
    type       = data.google_site_verification_token.t.type
    identifier = data.google_site_verification_token.t.identifier
  }
  verification_method = data.google_site_verification_token.t.verification_method
}

output "token" {
  value = data.google_site_verification_token.t.token
}
`, site))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	if out := strings.TrimSpace(m.must("output", "-raw", "token")); out != tok.Token {
		t.Errorf("token output = %q, want %q", out, tok.Token)
	}
	m.planClean()
	list, err := c.WebResource.List().Context(h.Context()).Do()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range list.Items {
		found = found || it.Site.Identifier == site
	}
	if !found {
		t.Errorf("the API does not list %s", site)
	}
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
}

func errorsAs(err error, target **googleapi.Error) bool {
	ge, ok := err.(*googleapi.Error)
	if ok {
		*target = ge
	}
	return ok
}

// TestOAuthClientSignsInTheInstanceUser (#2030): an OAuth client made on
// the console's Google Auth Platform pages is usable with golang.org/x/oauth2
// against the instance's authorization and token endpoints: the consent
// screen, an authorization code with PKCE, its exchange for an access
// token, a refresh token and an ID token, and a refresh.
func TestOAuthClientSignsInTheInstanceUser(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	meta := h.Endpoint(EnvMetadata)
	project := h.Project()
	studio := func(call, body string) map[string]any {
		t.Helper()
		code, out := consoleDo(t, addr, http.MethodPost, "/api/studio/serviceusage?call="+call+"&project="+project, body)
		if code != http.StatusOK {
			t.Fatalf("%s = %d: %s", call, code, out)
		}
		var m map[string]any
		_ = json.Unmarshal([]byte(out), &m)
		return m
	}
	// An Internal app: the instance's user signs in, with no test users.
	studio("oauth-app-set", `{"brand":{"appName":"Compat app","supportEmail":"dev@example.com","developerEmails":["dev@example.com"],"userType":"INTERNAL"}}`)
	redirect := "http://127.0.0.1:47123/callback"
	created := studio("oauth-client-create", fmt.Sprintf(`{"client":{"name":"compat web","type":"WEB","redirectUris":[%q]}}`, redirect))
	client := created["client"].(map[string]any)
	id, secret := client["clientId"].(string), created["secret"].(string)
	if !strings.HasSuffix(id, ".apps.googleusercontent.com") || !strings.HasPrefix(secret, "GOCSPX-") {
		t.Fatalf("created client %q", id)
	}
	t.Cleanup(func() {
		_, _ = consoleDo(t, addr, http.MethodPost, "/api/studio/serviceusage?call=oauth-client-delete&project="+project, fmt.Sprintf(`{"id":%q}`, id))
	})
	conf := &oauth2.Config{ClientID: id, ClientSecret: secret, RedirectURL: redirect, Scopes: []string{"openid", "email"},
		Endpoint: oauth2.Endpoint{AuthURL: "http://" + meta + "/o/oauth2/v2/auth", TokenURL: "http://" + meta + "/token"}}
	verifier := oauth2.GenerateVerifier()
	authURL := conf.AuthCodeURL("state-1", oauth2.AccessTypeOffline, oauth2.S256ChallengeOption(verifier))
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Get(authURL)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(page), "Compat app wants to access your account") {
		t.Fatalf("consent page = %d:\n%s", resp.StatusCode, page)
	}
	body := string(page)
	i := strings.Index(body, `name="nonce" value="`) + len(`name="nonce" value="`)
	nonce := body[i : i+strings.Index(body[i:], `"`)]
	resp, err = noFollow.PostForm("http://"+meta+"/o/oauth2/v2/auth", url.Values{"nonce": {nonce}, "decision": {"allow"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || resp.StatusCode != http.StatusFound || loc.Query().Get("state") != "state-1" || loc.Query().Get("code") == "" {
		t.Fatalf("allow = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	ctx := context.WithValue(h.Context(), oauth2.HTTPClient, &http.Client{})
	if _, err := (&oauth2.Config{ClientID: id, ClientSecret: "GOCSPX-wrong", RedirectURL: redirect, Endpoint: conf.Endpoint}).
		Exchange(ctx, loc.Query().Get("code"), oauth2.VerifierOption(verifier)); err == nil {
		t.Fatal("a wrong client secret was accepted")
	}
	// The client is refused before its code is read, so the code is
	// still good.
	tok, err := conf.Exchange(ctx, loc.Query().Get("code"), oauth2.VerifierOption(verifier))
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken == "" || tok.RefreshToken == "" || tok.Extra("id_token") == nil {
		t.Fatalf("token: access %v, refresh %v, id_token %v", tok.AccessToken != "", tok.RefreshToken != "", tok.Extra("id_token") != nil)
	}
	tok.Expiry = time.Now().Add(-time.Minute)
	fresh, err := conf.TokenSource(ctx, tok).Token()
	if err != nil || fresh.AccessToken == "" || fresh.AccessToken == tok.AccessToken {
		t.Fatalf("refresh = %v", err)
	}
}
