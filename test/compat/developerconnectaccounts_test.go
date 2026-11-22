//go:build compat

package compat

import (
	b1pb "cloud.google.com/go/dataform/apiv1beta1/dataformpb"
	"context"
	"google.golang.org/protobuf/proto"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	developerconnect "cloud.google.com/go/developerconnect/apiv1"
	dcpb "cloud.google.com/go/developerconnect/apiv1/developerconnectpb"
	securesourcemanager "cloud.google.com/go/securesourcemanager/apiv1"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"github.com/google/uuid"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// Not yet run: once it passes on an instance, the Methods line below becomes "// covers:" and the rows Verified.
// covers: google.cloud.developerconnect.v1.DeveloperConnect/CreateAccountConnector, google.cloud.developerconnect.v1.DeveloperConnect/GetAccountConnector, google.cloud.developerconnect.v1.DeveloperConnect/ListAccountConnectors, google.cloud.developerconnect.v1.DeveloperConnect/UpdateAccountConnector, google.cloud.developerconnect.v1.DeveloperConnect/DeleteAccountConnector, google.cloud.developerconnect.v1.DeveloperConnect/ListUsers, google.cloud.developerconnect.v1.DeveloperConnect/DeleteUser, google.cloud.developerconnect.v1.DeveloperConnect/FetchSelf, google.cloud.developerconnect.v1.DeveloperConnect/DeleteSelf, google.cloud.developerconnect.v1.DeveloperConnect/FetchAccessToken, google.cloud.developerconnect.v1.DeveloperConnect/StartOAuth, google.cloud.developerconnect.v1.DeveloperConnect/FinishOAuth
//
// TestDeveloperConnectAccountConnectors (#2105), with the official
// Developer Connect and Secure Source Manager clients over gRPC on the Run
// port, as Google's projects.locations.accountConnectors and .users
// reference documents them: a GOOGLE connector (the instance's own OAuth
// authorization server) is made, read with its output-only oauthStartUri,
// listed with a filter and paging, and updated (labels; allow_missing
// creates); a GITHUB one is UNIMPLEMENTED, one with no scopes
// INVALID_ARGUMENT. StartOAuth returns a ticket, an S256 PKCE challenge,
// the client ID, the scopes and the authorization server's URL; the test
// drives the local consent page (GET, then Allow), and FinishOAuth with
// the code and ticket records the caller's user, whom FetchSelf and
// ListUsers show (display name the instance's user's email).
// FetchAccessToken's token is accepted by a Secure Source Manager
// repository's git server: git ls-remote with it as a bearer token. A
// second flow lets the redirect page finish (the browser follows the
// consent's redirect) and FinishOAuth with google_oauth_params answers it;
// DeleteSelf and DeleteUser remove the user; a connector with a user is not
// deleted without force, and is with it.
func TestDeveloperConnectAccountConnectors(t *testing.T) {
	h := New(t)
	runAddr := h.Endpoint(EnvRun)
	meta := h.Endpoint(EnvMetadata)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)
	dc, err := developerconnect.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dc.Close() })
	ssm, err := securesourcemanager.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ssm.Close() })
	parent := runParent(h)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	google := func(scopes ...string) *dcpb.AccountConnector {
		return &dcpb.AccountConnector{AccountConnectorConfig: &dcpb.AccountConnector_ProviderOauthConfig{ProviderOauthConfig: &dcpb.ProviderOAuthConfig{
			OauthProviderId: &dcpb.ProviderOAuthConfig_SystemProviderId{SystemProviderId: dcpb.SystemProvider_GOOGLE}, Scopes: scopes}}}
	}

	if _, err := dc.CreateAccountConnector(ctx, &dcpb.CreateAccountConnectorRequest{Parent: parent, AccountConnectorId: "gh-" + suffix,
		AccountConnector: &dcpb.AccountConnector{AccountConnectorConfig: &dcpb.AccountConnector_ProviderOauthConfig{ProviderOauthConfig: &dcpb.ProviderOAuthConfig{
			OauthProviderId: &dcpb.ProviderOAuthConfig_SystemProviderId{SystemProviderId: dcpb.SystemProvider_GITHUB}, Scopes: []string{"repo"}}}}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("a GITHUB connector: %v; want Unimplemented", err)
	}
	if _, err := dc.CreateAccountConnector(ctx, &dcpb.CreateAccountConnectorRequest{Parent: parent, AccountConnectorId: "none-" + suffix,
		AccountConnector: google()}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a connector with no scopes: %v; want InvalidArgument", err)
	}
	op, err := dc.CreateAccountConnector(ctx, &dcpb.CreateAccountConnectorRequest{Parent: parent, AccountConnectorId: "ac-" + suffix,
		AccountConnector: google("https://www.googleapis.com/auth/drive.readonly")})
	if err != nil {
		t.Fatalf("CreateAccountConnector: %v", err)
	}
	conn, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateAccountConnector: %v", err)
	}
	name := conn.GetName()
	t.Cleanup(func() {
		if op, err := dc.DeleteAccountConnector(context.Background(), &dcpb.DeleteAccountConnectorRequest{Name: name, Force: true}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	got, err := dc.GetAccountConnector(ctx, &dcpb.GetAccountConnectorRequest{Name: name})
	if err != nil || got.GetProviderOauthConfig().GetSystemProviderId() != dcpb.SystemProvider_GOOGLE || got.GetOauthStartUri() == "" {
		t.Fatalf("GetAccountConnector = %v, %v", got, err)
	}
	uop, err := dc.UpdateAccountConnector(ctx, &dcpb.UpdateAccountConnectorRequest{UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}},
		AccountConnector: &dcpb.AccountConnector{Name: name, Labels: map[string]string{"team": suffix}}})
	if err != nil {
		t.Fatalf("UpdateAccountConnector: %v", err)
	}
	if up, err := uop.Wait(ctx); err != nil || up.GetLabels()["team"] != suffix {
		t.Fatalf("UpdateAccountConnector = %v, %v", up, err)
	}
	missing := google("openid")
	missing.Name = parent + "/accountConnectors/am-" + suffix
	if mop, err := dc.UpdateAccountConnector(ctx, &dcpb.UpdateAccountConnectorRequest{AllowMissing: true, AccountConnector: missing}); err != nil {
		t.Fatalf("UpdateAccountConnector allow_missing: %v", err)
	} else if made, err := mop.Wait(ctx); err != nil || made.GetName() != missing.Name {
		t.Fatalf("allow_missing made %v, %v", made, err)
	}
	if dop, err := dc.DeleteAccountConnector(ctx, &dcpb.DeleteAccountConnectorRequest{Name: missing.Name}); err != nil {
		t.Fatalf("DeleteAccountConnector: %v", err)
	} else if err := dop.Wait(ctx); err != nil {
		t.Fatalf("DeleteAccountConnector: %v", err)
	}
	var found []string
	for it := dc.ListAccountConnectors(ctx, &dcpb.ListAccountConnectorsRequest{Parent: parent, Filter: `labels.team = "` + suffix + `"`, PageSize: 1}); ; {
		c, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListAccountConnectors: %v", err)
		}
		found = append(found, c.GetName())
	}
	if len(found) != 1 || found[0] != name {
		t.Fatalf("ListAccountConnectors by label = %v; want [%s]", found, name)
	}

	// The flow. Every URL the test opens must be this instance's.
	local := func(raw, hostPort string) *url.URL {
		t.Helper()
		u, err := url.Parse(raw)
		_, port, _ := net.SplitHostPort(stripScheme(hostPort))
		loopback := u != nil && (u.Hostname() == "localhost" || net.ParseIP(u.Hostname()).IsLoopback())
		if err != nil || u.Scheme != "http" || !loopback || u.Port() != port {
			t.Fatalf("%q is not on this instance's %s", raw, hostPort)
		}
		return u
	}
	noFollow := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	nonceRE := regexp.MustCompile(`name="nonce" value="([^"]+)"`)
	allow := func(authURI string) *url.URL {
		t.Helper()
		u := local(authURI, meta)
		resp, err := noFollow.Get(u.String())
		if err != nil {
			t.Fatal(err)
		}
		page, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		m := nonceRE.FindSubmatch(page)
		if resp.StatusCode != http.StatusOK || m == nil || !strings.Contains(string(page), "Developer Connect wants to access your account") {
			t.Fatalf("the consent page: %d\n%s", resp.StatusCode, page)
		}
		resp, err = noFollow.PostForm(u.Scheme+"://"+u.Host+u.Path, url.Values{"nonce": {string(m[1])}, "decision": {"allow"}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		loc, err := resp.Location()
		if err != nil {
			t.Fatalf("Allow: %d, no redirect", resp.StatusCode)
		}
		return local(loc.String(), runAddr)
	}
	connect := func() {
		t.Helper()
		start, err := dc.StartOAuth(ctx, &dcpb.StartOAuthRequest{AccountConnector: name})
		if err != nil {
			t.Fatalf("StartOAuth: %v", err)
		}
		if start.GetTicket() == "" || start.GetCodeChallengeMethod() != "S256" || start.GetCodeChallenge() == "" || start.GetClientId() == "" ||
			start.GetSystemProviderId() != dcpb.SystemProvider_GOOGLE || len(start.GetScopes()) != 1 {
			t.Fatalf("StartOAuth = %v", start)
		}
		back := allow(start.GetAuthUri())
		if back.Query().Get("state") != start.GetTicket() || back.Query().Get("code") == "" {
			t.Fatalf("the redirect %v carries no code for the ticket", back)
		}
		fin, err := dc.FinishOAuth(ctx, &dcpb.FinishOAuthRequest{AccountConnector: name, Params: &dcpb.FinishOAuthRequest_OauthParams{
			OauthParams: &dcpb.FinishOAuthRequest_OAuthParams{Code: back.Query().Get("code"), Ticket: start.GetTicket()}}})
		if err != nil || fin.GetExchangeError() != nil {
			t.Fatalf("FinishOAuth = %v, %v", fin, err)
		}
	}
	if _, err := dc.FetchAccessToken(ctx, &dcpb.FetchAccessTokenRequest{AccountConnector: name}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("FetchAccessToken before connecting: %v; want FailedPrecondition", err)
	}
	connect()
	self, err := dc.FetchSelf(ctx, &dcpb.FetchSelfRequest{Name: name})
	if err != nil || !strings.HasPrefix(self.GetName(), name+"/users/") || !strings.Contains(self.GetDisplayName(), "@") {
		t.Fatalf("FetchSelf = %v, %v", self, err)
	}
	tok, err := dc.FetchAccessToken(ctx, &dcpb.FetchAccessTokenRequest{AccountConnector: name})
	if err != nil || tok.GetToken() == "" || tok.GetExpirationTime() == nil || tok.GetExchangeError() != nil {
		t.Fatalf("FetchAccessToken: %v (exchange error %v)", err, tok.GetExchangeError())
	}

	// The token is the user's at Secure Source Manager's git server.
	iop, err := ssm.CreateInstance(ctx, &ssmpb.CreateInstanceRequest{Parent: parent, InstanceId: "acc-" + suffix})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	inst, err := iop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	t.Cleanup(func() {
		if op, err := ssm.DeleteInstance(context.Background(), &ssmpb.DeleteInstanceRequest{Name: inst.GetName(), Force: true}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	rop, err := ssm.CreateRepository(ctx, &ssmpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "r-" + suffix,
		Repository: &ssmpb.Repository{Instance: inst.GetName(), InitialConfig: &ssmpb.Repository_InitialConfig{Readme: "default"}}})
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	repo, err := rop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	gitURL := local(repo.GetUris().GetGitHttps(), runAddr).String()
	cmd := exec.CommandContext(ctx, "git", "-c", "http.extraHeader=Authorization: Bearer "+tok.GetToken(), "ls-remote", gitURL)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git ls-remote with the connector's token: %v\n%s", err, out)
	} else if !strings.Contains(string(out), "refs/heads/") {
		t.Errorf("git ls-remote listed no branch:\n%s", out)
	}

	// Dataform v1beta1's git_remote_settings.account_connector (#2105): a
	// repository whose remote is that Secure Source Manager repository
	// authenticates with the caller's connected account, over gRPC and REST.
	if os.Getenv(EnvDataform) != "" {
		for transport, c := range dataformBetaClients(t, h) {
			df, err := c.CreateRepository(ctx, &b1pb.CreateRepositoryRequest{Parent: parent, RepositoryId: "acc" + transport + suffix,
				Repository: &b1pb.Repository{GitRemoteSettings: &b1pb.Repository_GitRemoteSettings{Url: repo.GetUris().GetGitHttps(), DefaultBranch: "main",
					AccountConnector: proto.String(name)}}})
			if err != nil {
				t.Fatalf("Dataform %s CreateRepository with account_connector: %v", transport, err)
			}
			t.Cleanup(func() {
				_ = c.DeleteRepository(context.Background(), &b1pb.DeleteRepositoryRequest{Name: df.GetName(), Force: true})
			})
			if df.GetGitRemoteSettings().GetAccountConnector() != name {
				t.Errorf("Dataform %s account_connector = %q", transport, df.GetGitRemoteSettings().GetAccountConnector())
			}
			if st, err := c.ComputeRepositoryAccessTokenStatus(ctx, &b1pb.ComputeRepositoryAccessTokenStatusRequest{Name: df.GetName()}); err != nil ||
				st.GetTokenStatus() != b1pb.ComputeRepositoryAccessTokenStatusResponse_VALID {
				t.Errorf("Dataform %s ComputeRepositoryAccessTokenStatus = %v, %v", transport, st, err)
			}
			if br, err := c.FetchRemoteBranches(ctx, &b1pb.FetchRemoteBranchesRequest{Name: df.GetName()}); err != nil || len(br.GetBranches()) == 0 {
				t.Errorf("Dataform %s FetchRemoteBranches through the account connector = %v, %v", transport, br, err)
			}
		}
	}

	// DeleteSelf, then a flow the redirect page finishes.
	if dop, err := dc.DeleteSelf(ctx, &dcpb.DeleteSelfRequest{Name: name}); err != nil {
		t.Fatalf("DeleteSelf: %v", err)
	} else if err := dop.Wait(ctx); err != nil {
		t.Fatalf("DeleteSelf: %v", err)
	}
	if _, err := dc.FetchSelf(ctx, &dcpb.FetchSelfRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Fatalf("FetchSelf after DeleteSelf: %v; want NotFound", err)
	}
	start, err := dc.StartOAuth(ctx, &dcpb.StartOAuthRequest{AccountConnector: name})
	if err != nil {
		t.Fatalf("StartOAuth: %v", err)
	}
	back := allow(start.GetAuthUri())
	resp, err := noFollow.Get(back.String())
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(page), "Account connected") {
		t.Fatalf("the redirect page: %d\n%s", resp.StatusCode, page)
	}
	if _, err := dc.FinishOAuth(ctx, &dcpb.FinishOAuthRequest{AccountConnector: name, Params: &dcpb.FinishOAuthRequest_GoogleOauthParams{
		GoogleOauthParams: &dcpb.FinishOAuthRequest_GoogleOAuthParams{Ticket: start.GetTicket(), Scopes: start.GetScopes()}}}); err != nil {
		t.Fatalf("FinishOAuth google_oauth_params: %v", err)
	}
	var users []*dcpb.User
	for it := dc.ListUsers(ctx, &dcpb.ListUsersRequest{Parent: name}); ; {
		u, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListUsers: %v", err)
		}
		users = append(users, u)
	}
	if len(users) != 1 || users[0].GetName() != self.GetName() {
		t.Fatalf("ListUsers = %v; want the one user %s", users, self.GetName())
	}
	if dop, err := dc.DeleteUser(ctx, &dcpb.DeleteUserRequest{Name: users[0].GetName()}); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	} else if err := dop.Wait(ctx); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if _, err := dc.FetchAccessToken(ctx, &dcpb.FetchAccessTokenRequest{AccountConnector: name}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("FetchAccessToken after DeleteUser: %v; want FailedPrecondition", err)
	}

	// A connector with a user is deleted only with force.
	connect()
	if _, err := dc.DeleteAccountConnector(ctx, &dcpb.DeleteAccountConnectorRequest{Name: name}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("DeleteAccountConnector with a user, no force: %v; want FailedPrecondition", err)
	}
	dop, err := dc.DeleteAccountConnector(ctx, &dcpb.DeleteAccountConnectorRequest{Name: name, Force: true})
	if err != nil {
		t.Fatalf("DeleteAccountConnector force: %v", err)
	}
	if err := dop.Wait(ctx); err != nil {
		t.Fatalf("DeleteAccountConnector force: %v", err)
	}
	if _, err := dc.GetAccountConnector(ctx, &dcpb.GetAccountConnectorRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetAccountConnector after delete: %v; want NotFound", err)
	}
}
