package run

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dcpb "cloud.google.com/go/developerconnect/apiv1/developerconnectpb"
	"github.com/cloudburrow/cloudburrow/internal/authplatform"
	"github.com/cloudburrow/cloudburrow/internal/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// dcAccountsFixture serves account connectors over a local authorization
// server: the Run port's pages on run, the consent page on auth.
type dcAccountsFixture struct {
	d         *DeveloperConnectServer
	run, auth *httptest.Server
	minted    atomic.Int64
}

func newDCAccountsFixture(t *testing.T) *dcAccountsFixture {
	t.Helper()
	if err := UseSourceManagerRoot(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	f := &dcAccountsFixture{}
	s := NewServer(&Knative{}, "t", time.Minute)
	m := s.SecureSourceManager(func() string { return f.run.URL })
	f.d = s.DeveloperConnect()
	f.run = httptest.NewServer(f.d.OAuthHandlerOr(SourceManagerHandlerOr(m, nil, func(g grpc.ServiceRegistrar) { m.Register(g) }, nil)))
	t.Cleanup(f.run.Close)
	st := authplatform.NewStore(store.NewMemory(), func(string) int64 { return 1 })
	f.auth = httptest.NewServer(&authplatform.Endpoints{Store: st, User: "owner@example.com"})
	t.Cleanup(f.auth.Close)
	SetDeveloperConnectOAuth(DeveloperConnectOAuth{Store: st, AuthBase: func() string { return f.auth.URL },
		Mint: func(email string) string { return fmt.Sprintf("tok%d-%s", f.minted.Add(1), email) }, TTL: time.Hour})
	t.Cleanup(func() { SetDeveloperConnectOAuth(DeveloperConnectOAuth{}) })
	SetSourceManagerCaller(func(b string) (string, bool) { return b + "@example.com", true }, "owner@example.com")
	t.Cleanup(func() { SetSourceManagerCaller(nil, "") })
	return f
}

func as(who string) context.Context {
	if who == "" {
		return context.Background()
	}
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+who))
}

func googleConnector(scopes ...string) *dcpb.AccountConnector {
	return &dcpb.AccountConnector{AccountConnectorConfig: &dcpb.AccountConnector_ProviderOauthConfig{ProviderOauthConfig: &dcpb.ProviderOAuthConfig{
		OauthProviderId: &dcpb.ProviderOAuthConfig_SystemProviderId{SystemProviderId: dcpb.SystemProvider_GOOGLE}, Scopes: scopes}}}
}

var noRedirects = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// consent opens an authorization URL and allows it, returning where the
// consent page sends the browser.
func consent(t *testing.T, authURI string) *url.URL {
	t.Helper()
	resp, err := noRedirects.Get(authURI)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	m := regexp.MustCompile(`name="nonce" value="([^"]+)"`).FindSubmatch(body)
	if resp.StatusCode != http.StatusOK || m == nil {
		t.Fatalf("consent page: %d\n%s", resp.StatusCode, body)
	}
	u, _ := url.Parse(authURI)
	resp, err = noRedirects.PostForm(u.Scheme+"://"+u.Host+u.Path, url.Values{"nonce": {string(m[1])}, "decision": {"allow"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc, err := resp.Location()
	if err != nil {
		t.Fatalf("consent's allow: %d, no redirect: %v", resp.StatusCode, err)
	}
	return loc
}

func TestAccountConnectorProvidersAndCRUD(t *testing.T) {
	f := newDCAccountsFixture(t)
	ctx := context.Background()
	parent := "projects/dc-proj/locations/us-central1"
	for _, tc := range []struct {
		c    *dcpb.AccountConnector
		want codes.Code
	}{
		{&dcpb.AccountConnector{}, codes.InvalidArgument},
		{googleConnector(), codes.InvalidArgument},
		{&dcpb.AccountConnector{AccountConnectorConfig: &dcpb.AccountConnector_ProviderOauthConfig{ProviderOauthConfig: &dcpb.ProviderOAuthConfig{Scopes: []string{"repo"}}}}, codes.InvalidArgument},
		{&dcpb.AccountConnector{AccountConnectorConfig: &dcpb.AccountConnector_ProviderOauthConfig{ProviderOauthConfig: &dcpb.ProviderOAuthConfig{
			OauthProviderId: &dcpb.ProviderOAuthConfig_SystemProviderId{SystemProviderId: dcpb.SystemProvider_GITHUB}, Scopes: []string{"repo"}}}}, codes.Unimplemented},
		{&dcpb.AccountConnector{AccountConnectorConfig: &dcpb.AccountConnector_ProviderOauthConfig{ProviderOauthConfig: &dcpb.ProviderOAuthConfig{
			OauthProviderId: &dcpb.ProviderOAuthConfig_SystemProviderId{SystemProviderId: dcpb.SystemProvider_DYNATRACE}}}}, codes.Unimplemented},
	} {
		if _, err := f.d.CreateAccountConnector(ctx, &dcpb.CreateAccountConnectorRequest{Parent: parent, AccountConnectorId: "x", AccountConnector: tc.c}); status.Code(err) != tc.want {
			t.Errorf("create %v: %v; want %v", tc.c, err, tc.want)
		}
	}
	if _, err := f.d.CreateAccountConnector(ctx, &dcpb.CreateAccountConnectorRequest{Parent: parent, AccountConnectorId: "c1",
		AccountConnector: googleConnector("openid", "email")}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.CreateAccountConnector(ctx, &dcpb.CreateAccountConnectorRequest{Parent: parent, AccountConnectorId: "c1",
		AccountConnector: googleConnector("openid")}); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("second create: %v", err)
	}
	c, err := f.d.GetAccountConnector(ctx, &dcpb.GetAccountConnectorRequest{Name: parent + "/accountConnectors/c1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(c.GetOauthStartUri(), f.run.URL+DeveloperConnectOAuthStartPath+"?") {
		t.Errorf("oauth_start_uri = %q", c.GetOauthStartUri())
	}
	// allow_missing creates; the labels then filter.
	if _, err := f.d.UpdateAccountConnector(ctx, &dcpb.UpdateAccountConnectorRequest{AllowMissing: true, AccountConnector: func() *dcpb.AccountConnector {
		ac := googleConnector("openid")
		ac.Name, ac.Labels = parent+"/accountConnectors/c2", map[string]string{"team": "a"}
		return ac
	}()}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.UpdateAccountConnector(ctx, &dcpb.UpdateAccountConnectorRequest{AccountConnector: &dcpb.AccountConnector{Name: parent + "/accountConnectors/nope"}}); status.Code(err) != codes.NotFound {
		t.Fatalf("update missing: %v", err)
	}
	list, err := f.d.ListAccountConnectors(ctx, &dcpb.ListAccountConnectorsRequest{Parent: parent, Filter: `labels.team = "a"`})
	if err != nil || len(list.GetAccountConnectors()) != 1 || !strings.HasSuffix(list.GetAccountConnectors()[0].GetName(), "/c2") {
		t.Fatalf("filtered list: %v %v", list, err)
	}
	list, err = f.d.ListAccountConnectors(ctx, &dcpb.ListAccountConnectorsRequest{Parent: parent, PageSize: 1, OrderBy: "name desc"})
	if err != nil || len(list.GetAccountConnectors()) != 1 || list.GetNextPageToken() == "" || !strings.HasSuffix(list.GetAccountConnectors()[0].GetName(), "/c2") {
		t.Fatalf("paged list: %v %v", list, err)
	}
	if _, err := f.d.UpdateAccountConnector(ctx, &dcpb.UpdateAccountConnectorRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"oauthStartUri"}},
		AccountConnector: &dcpb.AccountConnector{Name: parent + "/accountConnectors/c1"}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("an output-only path: %v", err)
	}
	if _, err := f.d.UpdateAccountConnector(ctx, &dcpb.UpdateAccountConnectorRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}},
		AccountConnector: &dcpb.AccountConnector{Name: parent + "/accountConnectors/c1", Etag: "stale"}}); status.Code(err) != codes.Aborted {
		t.Fatalf("a stale etag: %v", err)
	}
	if _, err := f.d.DeleteAccountConnector(ctx, &dcpb.DeleteAccountConnectorRequest{Name: parent + "/accountConnectors/c2"}); err != nil {
		t.Fatal(err)
	}
	// Saved beside the repositories, and read back.
	ssmState.mu.Lock()
	dcState.loaded = ""
	loadDCLocked()
	n := len(dcAccounts.connectors)
	ssmState.mu.Unlock()
	if n != 1 {
		t.Fatalf("reloaded %d connectors; want 1", n)
	}
	// A project's reset forgets them.
	resetDeveloperConnectProject(projectPrefixes("dc-proj"))
	if _, err := f.d.GetAccountConnector(ctx, &dcpb.GetAccountConnectorRequest{Name: parent + "/accountConnectors/c1"}); status.Code(err) != codes.NotFound {
		t.Fatalf("after the project's reset: %v", err)
	}
}

func TestAccountConnectorOAuthFlow(t *testing.T) {
	f := newDCAccountsFixture(t)
	ctx := context.Background()
	name := "projects/dc-proj/locations/us-central1/accountConnectors/c1"
	if _, err := f.d.CreateAccountConnector(ctx, &dcpb.CreateAccountConnectorRequest{Parent: "projects/dc-proj/locations/us-central1", AccountConnectorId: "c1",
		AccountConnector: googleConnector("https://www.googleapis.com/auth/drive.readonly")}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.FetchAccessToken(ctx, &dcpb.FetchAccessTokenRequest{AccountConnector: name}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("a token before connecting: %v", err)
	}
	if _, err := f.d.FetchSelf(ctx, &dcpb.FetchSelfRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Fatalf("self before connecting: %v", err)
	}
	if _, err := f.d.UserAccessToken(ctx, name, "owner@example.com"); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), name) {
		t.Fatalf("UserAccessToken before connecting: %v", err)
	}

	start, err := f.d.StartOAuth(ctx, &dcpb.StartOAuthRequest{AccountConnector: name})
	if err != nil {
		t.Fatal(err)
	}
	if start.GetCodeChallengeMethod() != "S256" || start.GetCodeChallenge() == "" || start.GetClientId() != dcSystemClientID ||
		start.GetSystemProviderId() != dcpb.SystemProvider_GOOGLE || !strings.HasPrefix(start.GetAuthUri(), f.auth.URL+authplatform.AuthorizePath) {
		t.Fatalf("StartOAuth = %v", start)
	}
	back := consent(t, start.GetAuthUri())
	if back.Path != DeveloperConnectOAuthRedirectPath || back.Query().Get("state") != start.GetTicket() {
		t.Fatalf("consent sent the browser to %v", back)
	}
	code := back.Query().Get("code")
	// Another caller cannot finish the owner's flow.
	if _, err := f.d.FinishOAuth(as("alice"), &dcpb.FinishOAuthRequest{AccountConnector: name,
		Params: &dcpb.FinishOAuthRequest_OauthParams{OauthParams: &dcpb.FinishOAuthRequest_OAuthParams{Code: code, Ticket: start.GetTicket()}}}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("FinishOAuth by another caller: %v", err)
	}
	fin, err := f.d.FinishOAuth(ctx, &dcpb.FinishOAuthRequest{AccountConnector: name,
		Params: &dcpb.FinishOAuthRequest_OauthParams{OauthParams: &dcpb.FinishOAuthRequest_OAuthParams{Code: code, Ticket: start.GetTicket()}}})
	if err != nil || fin.GetExchangeError() != nil {
		t.Fatalf("FinishOAuth: %v %v", fin, err)
	}
	// The code is spent: a second exchange is the server's invalid_grant.
	if again, err := f.d.FinishOAuth(ctx, &dcpb.FinishOAuthRequest{AccountConnector: name,
		Params: &dcpb.FinishOAuthRequest_OauthParams{OauthParams: &dcpb.FinishOAuthRequest_OAuthParams{Code: code, Ticket: start.GetTicket()}}}); err != nil ||
		again.GetExchangeError().GetCode() != "invalid_grant" {
		t.Fatalf("a spent code: %v %v", again, err)
	}
	self, err := f.d.FetchSelf(ctx, &dcpb.FetchSelfRequest{Name: name})
	if err != nil || self.GetDisplayName() != "owner@example.com" || !strings.HasPrefix(self.GetName(), name+"/users/") {
		t.Fatalf("FetchSelf = %v %v", self, err)
	}
	tok, err := f.d.FetchAccessToken(ctx, &dcpb.FetchAccessTokenRequest{AccountConnector: name})
	if err != nil || !strings.HasSuffix(tok.GetToken(), "-owner@example.com") || tok.GetExpirationTime() == nil ||
		len(tok.GetScopes()) != 1 {
		t.Fatalf("FetchAccessToken = %v %v", tok, err)
	}
	again, err := f.d.UserAccessToken(ctx, name, "Owner@Example.com")
	if err != nil || again != tok.GetToken() {
		t.Fatalf("UserAccessToken = %q %v; want the current token %q", again, err, tok.GetToken())
	}
	// Near its expiry, the token is refreshed from the grant.
	ssmState.mu.Lock()
	dcAccounts.users[self.GetName()].expires = time.Now()
	ssmState.mu.Unlock()
	fresh, err := f.d.FetchAccessToken(ctx, &dcpb.FetchAccessTokenRequest{AccountConnector: name})
	if err != nil || fresh.GetToken() == tok.GetToken() || !strings.HasSuffix(fresh.GetToken(), "-owner@example.com") {
		t.Fatalf("refreshed = %v %v", fresh, err)
	}
	if u, _ := f.d.FetchSelf(ctx, &dcpb.FetchSelfRequest{Name: name}); u.GetLastTokenRequestTime() == nil {
		t.Fatal("last_token_request_time is not set")
	}
	if _, err := f.d.UserAccessToken(ctx, name, "bob@example.com"); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("UserAccessToken of a user who did not connect: %v", err)
	}

	// Alice connects through the start URI and the redirect page, which
	// finishes the flow; google_oauth_params then answers it again.
	aliceStart, err := f.d.StartOAuth(as("alice"), &dcpb.StartOAuthRequest{AccountConnector: name})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.FinishOAuth(as("alice"), &dcpb.FinishOAuthRequest{AccountConnector: name,
		Params: &dcpb.FinishOAuthRequest_GoogleOauthParams{GoogleOauthParams: &dcpb.FinishOAuthRequest_GoogleOAuthParams{Ticket: aliceStart.GetTicket()}}}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("google_oauth_params before consent: %v", err)
	}
	cb := consent(t, aliceStart.GetAuthUri())
	resp, err := http.Get(cb.String())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "Account connected") {
		t.Fatalf("redirect page: %d\n%s", resp.StatusCode, body)
	}
	if _, err := f.d.FinishOAuth(as("alice"), &dcpb.FinishOAuthRequest{AccountConnector: name,
		Params: &dcpb.FinishOAuthRequest_GoogleOauthParams{GoogleOauthParams: &dcpb.FinishOAuthRequest_GoogleOAuthParams{Ticket: aliceStart.GetTicket()}}}); err != nil {
		t.Fatalf("google_oauth_params after the redirect page: %v", err)
	}
	users, err := f.d.ListUsers(ctx, &dcpb.ListUsersRequest{Parent: name})
	if err != nil || len(users.GetUsers()) != 2 {
		t.Fatalf("ListUsers = %v %v", users, err)
	}
	// The consent page signs in as the instance's user, so alice's token
	// is that account's.
	if at, err := f.d.FetchAccessToken(as("alice"), &dcpb.FetchAccessTokenRequest{AccountConnector: name}); err != nil || !strings.HasSuffix(at.GetToken(), "-owner@example.com") {
		t.Fatalf("alice's token = %v %v", at, err)
	}

	// The start URI starts a flow and sends the browser to consent.
	c, _ := f.d.GetAccountConnector(ctx, &dcpb.GetAccountConnectorRequest{Name: name})
	resp, err = noRedirects.Get(c.GetOauthStartUri())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if loc, _ := resp.Location(); resp.StatusCode != http.StatusFound || loc == nil || !strings.HasPrefix(loc.String(), f.auth.URL+authplatform.AuthorizePath) {
		t.Fatalf("start URI: %d %v", resp.StatusCode, loc)
	}

	if _, err := f.d.DeleteSelf(as("alice"), &dcpb.DeleteSelfRequest{Name: name}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.FetchSelf(as("alice"), &dcpb.FetchSelfRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Fatalf("alice after DeleteSelf: %v", err)
	}
	if _, err := f.d.DeleteAccountConnector(ctx, &dcpb.DeleteAccountConnectorRequest{Name: name}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("delete with users, no force: %v", err)
	}
	// A change of scopes removes the users.
	if _, err := f.d.UpdateAccountConnector(ctx, &dcpb.UpdateAccountConnectorRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"provider_oauth_config.scopes"}},
		AccountConnector: googleConnectorNamed(name, "openid")}); err != nil {
		t.Fatal(err)
	}
	if users, _ := f.d.ListUsers(ctx, &dcpb.ListUsersRequest{Parent: name}); len(users.GetUsers()) != 0 {
		t.Fatalf("users after a change of scopes: %v", users)
	}
	if _, err := f.d.DeleteAccountConnector(ctx, &dcpb.DeleteAccountConnectorRequest{Name: name, Force: true}); err != nil {
		t.Fatal(err)
	}
}

func googleConnectorNamed(name string, scopes ...string) *dcpb.AccountConnector {
	c := googleConnector(scopes...)
	c.Name = name
	return c
}

func TestAccountConnectorDeleteUser(t *testing.T) {
	f := newDCAccountsFixture(t)
	ctx := context.Background()
	name := "projects/dc-proj/locations/us-central1/accountConnectors/c1"
	if _, err := f.d.CreateAccountConnector(ctx, &dcpb.CreateAccountConnectorRequest{Parent: "projects/dc-proj/locations/us-central1", AccountConnectorId: "c1",
		AccountConnector: googleConnector("openid")}); err != nil {
		t.Fatal(err)
	}
	start, err := f.d.StartOAuth(ctx, &dcpb.StartOAuthRequest{AccountConnector: name})
	if err != nil {
		t.Fatal(err)
	}
	back := consent(t, start.GetAuthUri())
	if _, err := f.d.FinishOAuth(ctx, &dcpb.FinishOAuthRequest{AccountConnector: name, Params: &dcpb.FinishOAuthRequest_OauthParams{
		OauthParams: &dcpb.FinishOAuthRequest_OAuthParams{Code: back.Query().Get("code"), Ticket: start.GetTicket()}}}); err != nil {
		t.Fatal(err)
	}
	self, err := f.d.FetchSelf(ctx, &dcpb.FetchSelfRequest{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.DeleteUser(ctx, &dcpb.DeleteUserRequest{Name: self.GetName(), Etag: "stale"}); status.Code(err) != codes.Aborted {
		t.Fatalf("a stale etag: %v", err)
	}
	if _, err := f.d.DeleteUser(ctx, &dcpb.DeleteUserRequest{Name: name + "/users"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("a bad name: %v", err)
	}
	if _, err := f.d.DeleteUser(ctx, &dcpb.DeleteUserRequest{Name: self.GetName()}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.UserAccessToken(ctx, name, "owner@example.com"); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("UserAccessToken after DeleteUser: %v", err)
	}
}
