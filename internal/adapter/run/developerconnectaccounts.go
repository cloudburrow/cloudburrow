package run

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	dcpb "cloud.google.com/go/developerconnect/apiv1/developerconnectpb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/authplatform"
	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Developer Connect's account connectors (#2105), as Google's reference
// documents projects.locations.accountConnectors and their users
// (https://docs.cloud.google.com/developer-connect/docs/api/reference/rest/v1/projects.locations.accountConnectors
// and .users, read 2026-11-22) and "Configure account connectors"
// (https://docs.cloud.google.com/developer-connect/docs/configure-connectors):
// a connector names a provider's OAuth (providerOauthConfig: a
// systemProviderId and its scopes), and each user who connects their
// account through it (StartOAuth, the provider's consent, FinishOAuth) is
// a User, whose access token FetchAccessToken returns to that user.
//
// The one provider an instance can be is GOOGLE, which is modelled by the
// instance's own OAuth authorization server (internal/authplatform, served
// on the metadata port): StartOAuth's auth_uri is its authorization
// endpoint, the consent is its consent page signed in as the instance's
// user, and FinishOAuth exchanges the code with the PKCE verifier there.
// The access token FetchAccessToken returns is minted by the instance's
// credentials for the account that consented, as the metadata server's
// token endpoint mints one for a local OAuth client, so every service of
// the instance (Secure Source Manager's git server and APIs among them,
// through SetSourceManagerCaller's resolver) takes it as that account.
//
// GITHUB, GITLAB, SENTRY, ROVO, NEW_RELIC, DATASTAX and DYNATRACE are
// refused at CreateAccountConnector, UNIMPLEMENTED, as CreateConnection
// refuses a hosted provider: their OAuth is the hosted provider's, which no
// instance reaches, so a connector made for one could never be connected.
// Google documents no error for an unusable provider; refusing at create,
// rather than at StartOAuth, keeps a connector from existing that no user
// can ever complete (and Terraform from planning one clean).
//
// The OAuth client. Google's pre-configured clients are Developer
// Connect's own ("a Developer Connect pre-configured OAuth client"),
// registered with the provider and invisible to the project.
// UNVERIFIED (re-read 2026-11-22): Google documents neither that client's ID nor its redirect
// URI for a system provider (a custom client's is
// https://developerconnect.google.com/redirect/custom/projects/<number>/locations/<location>/accountConnectors/<id>),
// so CloudBurrow models it as one Web client of Developer Connect's in
// the authorization server's store, under a key no project has
// (dcSystemProject), whose only redirect URI is this port's
// /developerconnect/oauth/redirect, and whose secret lives in this
// process. The redirect page finishes the flow itself, as Google's
// developerconnect.google.com redirect does for its console; an API
// client that reads the code from the redirect calls FinishOAuth with
// oauth_params, and one that let the redirect page finish calls it with
// google_oauth_params (the ticket alone), as Google's Google-provider flow
// does. auth_uri is the full authorization URL (client_id, redirect_uri,
// scope, state = the ticket, the S256 code_challenge, access_type=offline)
// rather than the bare endpoint: the response carries no redirect URI a
// client could build one with (UNVERIFIED, re-read 2026-11-22).
//
// Other choices where Google documents none: a user's ID is the first 20
// hex digits of the SHA-256 of their lower-cased email; a user is the
// caller who started the flow (StartOAuth's credentials), and FinishOAuth
// from another caller is PERMISSION_DENIED; display_name is the caller's
// email; a ticket lives ten minutes in memory; etags are empty, as the
// connections' are (#1304); FetchAccessToken and FetchSelf for a caller
// who has not connected are FAILED_PRECONDITION and NOT_FOUND.

const (
	// dcSystemProject is the authorization server's key for Developer
	// Connect's own OAuth client: no project ID has dots.
	dcSystemProject = "developerconnect.googleapis.com"
	// dcSystemClientID is that client's ID.
	dcSystemClientID = "developerconnect-system.apps.googleusercontent.com"
	// DeveloperConnectOAuthStartPath and DeveloperConnectOAuthRedirectPath
	// are where this port serves an account connector's oauth_start_uri
	// and the OAuth client's redirect URI.
	DeveloperConnectOAuthStartPath    = dcJSONPrefix + "/oauth/start"
	DeveloperConnectOAuthRedirectPath = dcJSONPrefix + "/oauth/redirect"
	dcTicketTTL                       = 10 * time.Minute
	// dcTokenReuse is how long before its expiry a user's access token is
	// still handed out rather than refreshed.
	dcTokenReuse = 5 * time.Minute
)

// DeveloperConnectOAuth is what account connectors need of the instance:
// its OAuth authorization server and its credentials.
type DeveloperConnectOAuth struct {
	// Store is the authorization server's clients and grants.
	Store *authplatform.Store
	// AuthBase is the authorization server's http://host:port (the
	// metadata server's), or "" before it listens.
	AuthBase func() string
	// Mint makes an access token for an account, valid for TTL.
	Mint func(email string) string
	TTL  time.Duration
}

var dcOAuth struct {
	mu     sync.Mutex
	o      DeveloperConnectOAuth
	secret string
}

// SetDeveloperConnectOAuth gives account connectors the instance's
// authorization server and credentials (#2105).
func SetDeveloperConnectOAuth(o DeveloperConnectOAuth) {
	raw := make([]byte, 24)
	_, _ = rand.Read(raw)
	dcOAuth.mu.Lock()
	defer dcOAuth.mu.Unlock()
	dcOAuth.o, dcOAuth.secret = o, "GOCSPX-"+base64.RawURLEncoding.EncodeToString(raw)
}

// dcRunBase is this port's http://host:port, or "".
func dcRunBase() string {
	if f, ok := ssmGitBase.Load().(func() string); ok && f != nil {
		return strings.TrimSuffix(f(), "/")
	}
	return ""
}

// dcRedirectURI is the system client's redirect URI.
func dcRedirectURI() string { return dcRunBase() + DeveloperConnectOAuthRedirectPath }

// dcOAuthReady is the authorization server, with the system client
// registered for this port's redirect URI.
func dcOAuthReady() (DeveloperConnectOAuth, string, error) {
	dcOAuth.mu.Lock()
	o, secret := dcOAuth.o, dcOAuth.secret
	dcOAuth.mu.Unlock()
	if o.Store == nil || o.Mint == nil || o.AuthBase == nil || o.AuthBase() == "" || dcRunBase() == "" {
		return o, "", apierror.FailedPrecondition("the instance's OAuth authorization server is not running, so no account can be connected")
	}
	err := o.Store.EnsureSystemClient(dcSystemProject,
		authplatform.Brand{AppName: "Developer Connect", SupportEmail: "developerconnect@cloudburrow.localhost", UserType: "INTERNAL", PublishingStatus: "IN_PRODUCTION"},
		authplatform.Client{ClientID: dcSystemClientID, Name: "Developer Connect", Type: authplatform.TypeWeb, RedirectURIs: []string{dcRedirectURI()}},
		secret)
	if err != nil {
		return o, "", apierror.Internal(err, "register Developer Connect's OAuth client")
	}
	return o, secret, nil
}

// dcUser is a connected user: the User, the caller it belongs to, and the
// grant its tokens are refreshed from.
type dcUser struct {
	User    *dcpb.User      `json:"-"`
	Raw     json.RawMessage `json:"user"`
	Caller  string          `json:"caller"`  // ssmCallerOf's account
	Account string          `json:"account"` // the account that consented
	Refresh string          `json:"refreshToken,omitempty"`
	Scopes  []string        `json:"scopes,omitempty"`

	access  string
	expires time.Time
}

// dcAccounts are the account connectors and their users, under
// ssmState.mu as the connections are.
var dcAccounts = struct {
	connectors map[string]*dcpb.AccountConnector
	users      map[string]*dcUser
}{connectors: map[string]*dcpb.AccountConnector{}, users: map[string]*dcUser{}}

// dcTicket is a started OAuth flow.
type dcTicket struct {
	connector, caller, verifier string
	scopes                      []string
	expires                     time.Time
	done                        bool
}

var dcTickets = struct {
	mu sync.Mutex
	m  map[string]*dcTicket
}{m: map[string]*dcTicket{}}

func loadDCAccountsLocked(connectors []json.RawMessage, users []json.RawMessage) {
	dcAccounts.connectors, dcAccounts.users = map[string]*dcpb.AccountConnector{}, map[string]*dcUser{}
	for _, raw := range connectors {
		c := &dcpb.AccountConnector{}
		if protojson.Unmarshal(raw, c) == nil {
			dcAccounts.connectors[c.GetName()] = c
		}
	}
	for _, raw := range users {
		u := &dcUser{}
		if json.Unmarshal(raw, u) != nil {
			continue
		}
		u.User = &dcpb.User{}
		if protojson.Unmarshal(u.Raw, u.User) == nil && dcAccounts.connectors[userConnector(u.User.GetName())] != nil {
			dcAccounts.users[u.User.GetName()] = u
		}
	}
}

func saveDCAccountsLocked() (connectors, users []json.RawMessage) {
	for _, name := range sortedKeys(dcAccounts.connectors) {
		b, _ := protojson.Marshal(dcAccounts.connectors[name])
		connectors = append(connectors, b)
	}
	for _, name := range sortedKeys(dcAccounts.users) {
		u := dcAccounts.users[name]
		u.Raw, _ = protojson.Marshal(u.User)
		b, _ := json.Marshal(u)
		users = append(users, b)
	}
	return connectors, users
}

// userConnector is a user's connector, by its name.
func userConnector(user string) string {
	c, _, _ := strings.Cut(user, "/users/")
	return c
}

// parseAccountConnectorName checks
// projects/*/locations/*/accountConnectors/*.
func parseAccountConnectorName(name string) (parent string, err error) {
	parent, _, err = parseSSMName(name, "accountConnectors")
	return parent, err
}

// parseUserName checks projects/*/locations/*/accountConnectors/*/users/*.
func parseUserName(name string) (connector string, err error) {
	connector, id, ok := strings.Cut(name, "/users/")
	if _, perr := parseAccountConnectorName(connector); !ok || perr != nil || id == "" || strings.Contains(id, "/") {
		return "", apierror.InvalidArgument("invalid name %q: want projects/*/locations/*/accountConnectors/*/users/*", name)
	}
	return connector, nil
}

// dcUserID is the ID of a caller's user.
func dcUserID(caller string) string {
	h := sha256.Sum256([]byte(strings.ToLower(ssmCallerEmail(caller))))
	return hex.EncodeToString(h[:10])
}

// dcStartURI is a connector's oauth_start_uri.
func dcStartURI(name string) string {
	base := dcRunBase()
	if base == "" {
		return ""
	}
	return base + DeveloperConnectOAuthStartPath + "?" + url.Values{"accountConnector": {name}}.Encode()
}

// connectorOut is a connector as it is answered.
func connectorOut(c *dcpb.AccountConnector) *dcpb.AccountConnector {
	out := proto.Clone(c).(*dcpb.AccountConnector)
	out.OauthStartUri = dcStartURI(c.GetName())
	return out
}

// checkProviderConfig refuses a connector config no instance can serve.
func checkProviderConfig(c *dcpb.AccountConnector) error {
	cfg := c.GetProviderOauthConfig()
	if cfg == nil {
		return apierror.InvalidArgument("account_connector needs a provider_oauth_config")
	}
	switch p := cfg.GetSystemProviderId(); p {
	case dcpb.SystemProvider_GOOGLE:
	case dcpb.SystemProvider_SYSTEM_PROVIDER_UNSPECIFIED:
		return apierror.InvalidArgument("provider_oauth_config.system_provider_id is required: GOOGLE is the one served")
	default:
		return apierror.Unimplemented("system provider %s is not supported: its OAuth is the hosted provider's, which no instance reaches; "+
			"GOOGLE, the instance's own authorization server, is the one served", p)
	}
	if len(cfg.GetScopes()) == 0 {
		return apierror.InvalidArgument("provider_oauth_config.scopes is required: the GOOGLE provider needs at least one scope")
	}
	for _, s := range cfg.GetScopes() {
		if strings.TrimSpace(s) == "" || strings.ContainsAny(s, " \t\n") {
			return apierror.InvalidArgument("invalid scope %q in provider_oauth_config.scopes", s)
		}
	}
	return nil
}

// doneAt is an LRO for an account connector or a user, of its location.
func (d *DeveloperConnectServer) doneAt(target, verb string, resp proto.Message) (*longrunningpb.Operation, error) {
	loc, _, _ := strings.Cut(target, "/accountConnectors/")
	return d.done(loc, target, verb, resp)
}

// revokeUser forgets a user's grant at the authorization server.
func revokeUser(u *dcUser) {
	dcOAuth.mu.Lock()
	st := dcOAuth.o.Store
	dcOAuth.mu.Unlock()
	if st != nil && u.Refresh != "" {
		st.Revoke(u.Refresh)
	}
}

// deleteUsersLocked deletes a connector's users.
func deleteUsersLocked(connector string) {
	for name, u := range dcAccounts.users {
		if userConnector(name) == connector {
			revokeUser(u)
			delete(dcAccounts.users, name)
		}
	}
}

// CreateAccountConnector makes a connector for the GOOGLE provider.
func (d *DeveloperConnectServer) CreateAccountConnector(_ context.Context, req *dcpb.CreateAccountConnectorRequest) (*longrunningpb.Operation, error) {
	if _, _, err := resource.ParseLocation(req.GetParent()); err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	if !dcIDRE.MatchString(req.GetAccountConnectorId()) {
		return nil, apierror.InvalidArgument("invalid account_connector_id %q: lower case letters, digits and hyphens, starting with a letter", req.GetAccountConnectorId())
	}
	if req.GetAccountConnector() == nil {
		return nil, apierror.InvalidArgument("account_connector is required")
	}
	c := proto.Clone(req.GetAccountConnector()).(*dcpb.AccountConnector)
	if err := checkProviderConfig(c); err != nil {
		return nil, err
	}
	name := req.GetParent() + "/accountConnectors/" + req.GetAccountConnectorId()
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	loadDCLocked()
	if _, ok := dcAccounts.connectors[name]; ok {
		return nil, apierror.AlreadyExists("account connector %q already exists", name)
	}
	now := timestamppb.Now()
	c.Name, c.CreateTime, c.UpdateTime, c.Etag, c.OauthStartUri = name, now, now, "", ""
	if !req.GetValidateOnly() {
		dcAccounts.connectors[name] = c
		saveDCLocked()
	}
	return d.doneAt(name, "create", connectorOut(c))
}

// GetAccountConnector reads a connector.
func (d *DeveloperConnectServer) GetAccountConnector(_ context.Context, req *dcpb.GetAccountConnectorRequest) (*dcpb.AccountConnector, error) {
	if _, err := parseAccountConnectorName(req.GetName()); err != nil {
		return nil, err
	}
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	loadDCLocked()
	c, ok := dcAccounts.connectors[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("account connector %q not found", req.GetName())
	}
	return connectorOut(c), nil
}

// ListAccountConnectors lists a location's connectors, by name.
func (d *DeveloperConnectServer) ListAccountConnectors(_ context.Context, req *dcpb.ListAccountConnectorsRequest) (*dcpb.ListAccountConnectorsResponse, error) {
	if _, _, err := resource.ParseLocation(req.GetParent()); err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	ssmState.mu.Lock()
	loadDCLocked()
	var all []*dcpb.AccountConnector
	for _, name := range sortedKeys(dcAccounts.connectors) {
		if strings.HasPrefix(name, req.GetParent()+"/accountConnectors/") {
			all = append(all, connectorOut(dcAccounts.connectors[name]))
		}
	}
	ssmState.mu.Unlock()
	all, err := aipfilter.List(req.GetFilter(), req.GetOrderBy(), all)
	if err != nil {
		return nil, err
	}
	items, next, err := page(all, req.GetPageSize(), req.GetPageToken(), req.GetParent()+"/accountConnectors|"+req.GetFilter()+"|"+req.GetOrderBy())
	if err != nil {
		return nil, err
	}
	return &dcpb.ListAccountConnectorsResponse{AccountConnectors: items, NextPageToken: next}, nil
}

// dcMaskPath is an update_mask path in one spelling, REST's or gRPC's.
func dcMaskPath(p string) string { return strings.ToLower(strings.ReplaceAll(p, "_", "")) }

// UpdateAccountConnector changes a connector's labels, annotations and
// scopes; a change of scopes deletes its users, who connect again, as
// ProviderOAuthConfig.scopes documents. Its provider is immutable.
// allow_missing creates a connector that does not exist, the mask then
// ignored, as the request documents.
func (d *DeveloperConnectServer) UpdateAccountConnector(ctx context.Context, req *dcpb.UpdateAccountConnectorRequest) (*longrunningpb.Operation, error) {
	in := req.GetAccountConnector()
	parent, err := parseAccountConnectorName(in.GetName())
	if err != nil {
		return nil, err
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		paths = []string{"labels", "annotations", "providerOauthConfig.scopes"}
	}
	for _, p := range paths {
		switch dcMaskPath(p) {
		case "labels", "annotations", "provideroauthconfig", "provideroauthconfig.scopes", "etag":
		default:
			return nil, apierror.InvalidArgument("update_mask path %q cannot change: labels, annotations and providerOauthConfig.scopes can", p)
		}
	}
	ssmState.mu.Lock()
	loadDCLocked()
	cur, ok := dcAccounts.connectors[in.GetName()]
	if !ok {
		ssmState.mu.Unlock()
		if !req.GetAllowMissing() {
			return nil, apierror.NotFound("account connector %q not found", in.GetName())
		}
		_, id, _ := parseSSMName(in.GetName(), "accountConnectors")
		return d.CreateAccountConnector(ctx, &dcpb.CreateAccountConnectorRequest{Parent: parent, AccountConnectorId: id,
			AccountConnector: in, ValidateOnly: req.GetValidateOnly()})
	}
	defer ssmState.mu.Unlock()
	if err := dcEtagMatches("account connector", in.GetName(), in.GetEtag(), cur.GetEtag()); err != nil {
		return nil, err
	}
	next := proto.Clone(cur).(*dcpb.AccountConnector)
	scopesChanged := false
	for _, p := range paths {
		switch dcMaskPath(p) {
		case "labels":
			next.Labels = in.GetLabels()
		case "annotations":
			next.Annotations = in.GetAnnotations()
		case "provideroauthconfig", "provideroauthconfig.scopes":
			if in.GetProviderOauthConfig() == nil {
				return nil, apierror.InvalidArgument("update_mask names %q but account_connector has no provider_oauth_config", p)
			}
			if sp := in.GetProviderOauthConfig().GetSystemProviderId(); sp != dcpb.SystemProvider_SYSTEM_PROVIDER_UNSPECIFIED &&
				sp != cur.GetProviderOauthConfig().GetSystemProviderId() {
				return nil, apierror.InvalidArgument("provider_oauth_config.system_provider_id is immutable")
			}
			cfg := proto.Clone(cur.GetProviderOauthConfig()).(*dcpb.ProviderOAuthConfig)
			cfg.Scopes = in.GetProviderOauthConfig().GetScopes()
			scopesChanged = !slices.Equal(cfg.Scopes, cur.GetProviderOauthConfig().GetScopes())
			next.AccountConnectorConfig = &dcpb.AccountConnector_ProviderOauthConfig{ProviderOauthConfig: cfg}
		}
	}
	if err := checkProviderConfig(next); err != nil {
		return nil, err
	}
	if !req.GetValidateOnly() {
		next.UpdateTime = timestamppb.Now()
		dcAccounts.connectors[in.GetName()] = next
		if scopesChanged {
			deleteUsersLocked(in.GetName())
		}
		saveDCLocked()
	}
	return d.doneAt(in.GetName(), "update", connectorOut(next))
}

// DeleteAccountConnector removes a connector; one with users only with
// force, which deletes them.
func (d *DeveloperConnectServer) DeleteAccountConnector(_ context.Context, req *dcpb.DeleteAccountConnectorRequest) (*longrunningpb.Operation, error) {
	if _, err := parseAccountConnectorName(req.GetName()); err != nil {
		return nil, err
	}
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	loadDCLocked()
	c, ok := dcAccounts.connectors[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("account connector %q not found", req.GetName())
	}
	if err := dcEtagMatches("account connector", req.GetName(), req.GetEtag(), c.GetEtag()); err != nil {
		return nil, err
	}
	if !req.GetForce() {
		for name := range dcAccounts.users {
			if userConnector(name) == req.GetName() {
				return nil, apierror.FailedPrecondition("account connector %q has users: delete them first, or set force", req.GetName())
			}
		}
	}
	if !req.GetValidateOnly() {
		deleteUsersLocked(req.GetName())
		delete(dcAccounts.connectors, req.GetName())
		saveDCLocked()
	}
	return d.doneAt(req.GetName(), "delete", &emptypb.Empty{})
}

// connectorLocked is a connector, or NOT_FOUND.
func connectorLocked(name string) (*dcpb.AccountConnector, error) {
	if _, err := parseAccountConnectorName(name); err != nil {
		return nil, err
	}
	c, ok := dcAccounts.connectors[name]
	if !ok {
		return nil, apierror.NotFound("account connector %q not found", name)
	}
	return c, nil
}

// ListUsers lists a connector's users, by name.
func (d *DeveloperConnectServer) ListUsers(_ context.Context, req *dcpb.ListUsersRequest) (*dcpb.ListUsersResponse, error) {
	ssmState.mu.Lock()
	loadDCLocked()
	if _, err := connectorLocked(req.GetParent()); err != nil {
		ssmState.mu.Unlock()
		return nil, err
	}
	var all []*dcpb.User
	for _, name := range sortedKeys(dcAccounts.users) {
		if userConnector(name) == req.GetParent() {
			all = append(all, proto.Clone(dcAccounts.users[name].User).(*dcpb.User))
		}
	}
	ssmState.mu.Unlock()
	all, err := aipfilter.List(req.GetFilter(), req.GetOrderBy(), all)
	if err != nil {
		return nil, err
	}
	items, next, err := page(all, req.GetPageSize(), req.GetPageToken(), req.GetParent()+"/users|"+req.GetFilter()+"|"+req.GetOrderBy())
	if err != nil {
		return nil, err
	}
	return &dcpb.ListUsersResponse{Users: items, NextPageToken: next}, nil
}

// DeleteUser removes a user of a connector; their grant is revoked.
func (d *DeveloperConnectServer) DeleteUser(_ context.Context, req *dcpb.DeleteUserRequest) (*longrunningpb.Operation, error) {
	if _, err := parseUserName(req.GetName()); err != nil {
		return nil, err
	}
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	loadDCLocked()
	u, ok := dcAccounts.users[req.GetName()]
	if !ok {
		return nil, apierror.NotFound("user %q not found", req.GetName())
	}
	if err := dcEtagMatches("user", req.GetName(), req.GetEtag(), ""); err != nil {
		return nil, err
	}
	if !req.GetValidateOnly() {
		revokeUser(u)
		delete(dcAccounts.users, req.GetName())
		saveDCLocked()
	}
	return d.doneAt(req.GetName(), "delete", &emptypb.Empty{})
}

// selfLocked is the caller's user of a connector.
func selfLocked(connector, caller string) (*dcUser, error) {
	if _, err := connectorLocked(connector); err != nil {
		return nil, err
	}
	u, ok := dcAccounts.users[connector+"/users/"+dcUserID(caller)]
	if !ok {
		return nil, apierror.NotFound("%s has not connected an account to account connector %q", ssmCallerEmail(caller), connector)
	}
	return u, nil
}

// FetchSelf is the caller's user.
func (d *DeveloperConnectServer) FetchSelf(ctx context.Context, req *dcpb.FetchSelfRequest) (*dcpb.User, error) {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	loadDCLocked()
	u, err := selfLocked(req.GetName(), ssmCallerOf(ctx))
	if err != nil {
		return nil, err
	}
	return proto.Clone(u.User).(*dcpb.User), nil
}

// DeleteSelf removes the caller's user.
func (d *DeveloperConnectServer) DeleteSelf(ctx context.Context, req *dcpb.DeleteSelfRequest) (*longrunningpb.Operation, error) {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	loadDCLocked()
	u, err := selfLocked(req.GetName(), ssmCallerOf(ctx))
	if err != nil {
		return nil, err
	}
	revokeUser(u)
	delete(dcAccounts.users, u.User.GetName())
	saveDCLocked()
	return d.doneAt(u.User.GetName(), "delete", &emptypb.Empty{})
}

// tokenLocked is a user's current access token, refreshed from their
// grant when it is near its expiry; ee is the authorization server's
// refusal of the refresh.
func tokenLocked(u *dcUser) (tok string, exp time.Time, ee *dcpb.ExchangeError, err error) {
	now := time.Now()
	if u.access != "" && now.Add(dcTokenReuse).Before(u.expires) {
		return u.access, u.expires, nil, nil
	}
	o, secret, err := dcOAuthReady()
	if err != nil {
		return "", time.Time{}, nil, err
	}
	g, oerr := o.Store.Exchange(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {u.Refresh},
		"client_id": {dcSystemClientID}, "client_secret": {secret}}, "", "")
	if oerr != nil {
		return "", time.Time{}, &dcpb.ExchangeError{Code: oerr.OAuth, Description: oerr.Detail}, nil
	}
	u.access, u.expires = o.Mint(g.Email), now.Add(o.TTL).Truncate(time.Second)
	return u.access, u.expires, nil, nil
}

// FetchAccessToken is the caller's access token from their connected
// account.
func (d *DeveloperConnectServer) FetchAccessToken(ctx context.Context, req *dcpb.FetchAccessTokenRequest) (*dcpb.FetchAccessTokenResponse, error) {
	caller := ssmCallerOf(ctx)
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	loadDCLocked()
	u, err := selfLocked(req.GetAccountConnector(), caller)
	if err != nil && apierror.From(err).Code == codes.NotFound && dcAccounts.connectors[req.GetAccountConnector()] != nil {
		return nil, apierror.FailedPrecondition("%s has not connected an account to account connector %q: start the OAuth flow (StartOAuth) first",
			ssmCallerEmail(caller), req.GetAccountConnector())
	}
	if err != nil {
		return nil, err
	}
	tok, exp, ee, err := tokenLocked(u)
	if err != nil {
		return nil, err
	}
	if ee != nil {
		return &dcpb.FetchAccessTokenResponse{ExchangeError: ee}, nil
	}
	u.User.LastTokenRequestTime = timestamppb.Now()
	saveDCLocked()
	return &dcpb.FetchAccessTokenResponse{Token: tok, ExpirationTime: timestamppb.New(exp), Scopes: slices.Clone(u.Scopes)}, nil
}

// UserAccessToken is the current access token of a connector's user,
// named by email, refreshed as FetchAccessToken refreshes it, for a
// service of this instance that acts for that user (Dataform's workspaces
// on a Developer Connect account connector). It is FAILED_PRECONDITION,
// naming the connector, when that user has not connected an account.
func (d *DeveloperConnectServer) UserAccessToken(_ context.Context, connector, userEmail string) (string, error) {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	loadDCLocked()
	if _, err := connectorLocked(connector); err != nil {
		return "", err
	}
	want := strings.ToLower(strings.TrimSpace(userEmail))
	var u *dcUser
	for name, cand := range dcAccounts.users {
		if userConnector(name) == connector && strings.ToLower(ssmCallerEmail(cand.Caller)) == want {
			u = cand
		}
	}
	if u == nil {
		return "", apierror.FailedPrecondition("%s has not connected an account to account connector %q: open its oauth start URI to connect", userEmail, connector)
	}
	tok, _, ee, err := tokenLocked(u)
	if err != nil {
		return "", err
	}
	if ee != nil {
		return "", apierror.FailedPrecondition("the account %s connected to account connector %q can no longer be used (%s: %s): connect it again",
			userEmail, connector, ee.GetCode(), ee.GetDescription())
	}
	u.User.LastTokenRequestTime = timestamppb.Now()
	saveDCLocked()
	return tok, nil
}

// StartOAuth begins connecting the caller's account: a ticket, the PKCE
// challenge, and the authorization server's URL to send them to.
func (d *DeveloperConnectServer) StartOAuth(ctx context.Context, req *dcpb.StartOAuthRequest) (*dcpb.StartOAuthResponse, error) {
	ssmState.mu.Lock()
	loadDCLocked()
	c, err := connectorLocked(req.GetAccountConnector())
	var scopes []string
	if err == nil {
		scopes = slices.Clone(c.GetProviderOauthConfig().GetScopes())
	}
	ssmState.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := checkProviderConfig(c); err != nil {
		return nil, err
	}
	o, _, err := dcOAuthReady()
	if err != nil {
		return nil, err
	}
	raw := make([]byte, 48)
	if _, err := rand.Read(raw); err != nil {
		return nil, apierror.Internal(err, "a code verifier")
	}
	verifier := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	ticket := uuid.NewString()
	dcTickets.mu.Lock()
	now := time.Now()
	for k, t := range dcTickets.m {
		if now.After(t.expires) {
			delete(dcTickets.m, k)
		}
	}
	dcTickets.m[ticket] = &dcTicket{connector: req.GetAccountConnector(), caller: ssmCallerOf(ctx), verifier: verifier,
		scopes: scopes, expires: now.Add(dcTicketTTL)}
	dcTickets.mu.Unlock()
	q := url.Values{"client_id": {dcSystemClientID}, "redirect_uri": {dcRedirectURI()}, "response_type": {"code"},
		"scope": {strings.Join(scopes, " ")}, "state": {ticket}, "code_challenge": {challenge},
		"code_challenge_method": {"S256"}, "access_type": {"offline"}, "prompt": {"consent"}}
	return &dcpb.StartOAuthResponse{Id: &dcpb.StartOAuthResponse_SystemProviderId{SystemProviderId: dcpb.SystemProvider_GOOGLE},
		Ticket: ticket, CodeChallenge: challenge, CodeChallengeMethod: "S256", ClientId: dcSystemClientID, Scopes: scopes,
		AuthUri: strings.TrimSuffix(o.AuthBase(), "/") + authplatform.AuthorizePath + "?" + q.Encode()}, nil
}

// FinishOAuth completes connecting the caller's account: with
// oauth_params, by exchanging the code the redirect carried; with
// google_oauth_params, from a ticket the redirect page already finished.
func (d *DeveloperConnectServer) FinishOAuth(ctx context.Context, req *dcpb.FinishOAuthRequest) (*dcpb.FinishOAuthResponse, error) {
	switch {
	case req.GetOauthParams() != nil:
		p := req.GetOauthParams()
		if p.GetCode() == "" || p.GetTicket() == "" {
			return nil, apierror.InvalidArgument("oauth_params needs code and ticket")
		}
		return finishTicket(req.GetAccountConnector(), p.GetTicket(), p.GetCode(), ssmCallerOf(ctx), true)
	case req.GetGoogleOauthParams() != nil:
		p := req.GetGoogleOauthParams()
		if p.GetTicket() == "" {
			return nil, apierror.InvalidArgument("google_oauth_params needs ticket")
		}
		return finishTicket(req.GetAccountConnector(), p.GetTicket(), "", ssmCallerOf(ctx), true)
	}
	return nil, apierror.InvalidArgument("FinishOAuth needs oauth_params or google_oauth_params")
}

// finishTicket exchanges a ticket's code, when code is set, and records
// its caller's user; one already finished is answered again. checkCaller
// holds the caller to the ticket's, which the redirect page (a browser,
// with no credentials) is not.
func finishTicket(connector, ticket, code, caller string, checkCaller bool) (*dcpb.FinishOAuthResponse, error) {
	dcTickets.mu.Lock()
	t, ok := dcTickets.m[ticket]
	if ok && time.Now().After(t.expires) {
		delete(dcTickets.m, ticket)
		ok = false
	}
	dcTickets.mu.Unlock()
	switch {
	case !ok:
		return nil, apierror.InvalidArgument("ticket %q is unknown or expired: start the OAuth flow again", ticket)
	case connector != "" && t.connector != connector:
		return nil, apierror.InvalidArgument("ticket %q is for account connector %q, not %q", ticket, t.connector, connector)
	case checkCaller && t.caller != caller:
		return nil, &apierror.Error{Code: codes.PermissionDenied, Reason: "forbidden",
			Message: "the OAuth flow of ticket " + ticket + " was started by another user"}
	}
	if code == "" {
		if t.done {
			return &dcpb.FinishOAuthResponse{}, nil
		}
		return nil, apierror.FailedPrecondition("the OAuth flow of ticket %q has not been authorized: open its auth_uri and allow access first", ticket)
	}
	o, secret, err := dcOAuthReady()
	if err != nil {
		return nil, err
	}
	g, oerr := o.Store.Exchange(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {dcRedirectURI()},
		"client_id": {dcSystemClientID}, "client_secret": {secret}, "code_verifier": {t.verifier}}, "", "")
	if oerr != nil {
		return &dcpb.FinishOAuthResponse{ExchangeError: &dcpb.ExchangeError{Code: oerr.OAuth, Description: oerr.Detail}}, nil
	}
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	loadDCLocked()
	if _, err := connectorLocked(t.connector); err != nil {
		o.Store.Revoke(g.RefreshToken)
		return nil, err
	}
	name := t.connector + "/users/" + dcUserID(t.caller)
	created := timestamppb.Now()
	if old, ok := dcAccounts.users[name]; ok {
		created = old.User.GetCreateTime()
		revokeUser(old)
	}
	u := &dcUser{User: &dcpb.User{Name: name, DisplayName: ssmCallerEmail(t.caller), CreateTime: created},
		Caller: t.caller, Account: g.Email, Refresh: g.RefreshToken, Scopes: strings.Fields(g.Scope)}
	u.access, u.expires = o.Mint(g.Email), time.Now().Add(o.TTL).Truncate(time.Second)
	dcAccounts.users[name] = u
	saveDCLocked()
	dcTickets.mu.Lock()
	t.done = true
	dcTickets.mu.Unlock()
	return &dcpb.FinishOAuthResponse{}, nil
}

// resetDeveloperConnectProject forgets a project's account connectors and
// their users, whose grants are revoked.
func resetDeveloperConnectProject(prefixes []string) {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	loadDCLocked()
	changed := false
	for name := range dcAccounts.connectors {
		if ofProject(name, prefixes) {
			deleteUsersLocked(name)
			delete(dcAccounts.connectors, name)
			changed = true
		}
	}
	if changed {
		saveDCLocked()
	}
	dcTickets.mu.Lock()
	for k, t := range dcTickets.m {
		if ofProject(t.connector, prefixes) {
			delete(dcTickets.m, k)
		}
	}
	dcTickets.mu.Unlock()
}

// DeveloperConnectAccountConnectors lists a project's account connectors
// in every location, for the console.
func DeveloperConnectAccountConnectors(project string) []*dcpb.AccountConnector {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	loadDCLocked()
	var out []*dcpb.AccountConnector
	for _, name := range sortedKeys(dcAccounts.connectors) {
		if strings.HasPrefix(name, "projects/"+project+"/") {
			out = append(out, connectorOut(dcAccounts.connectors[name]))
		}
	}
	return out
}

// DeveloperConnectAccountLocations are the locations a project has account
// connectors in, sorted, for the console to list each through the API.
func DeveloperConnectAccountLocations(project string) []string {
	seen := map[string]bool{}
	for _, c := range DeveloperConnectAccountConnectors(project) {
		seen[strings.Split(c.GetName(), "/")[3]] = true
	}
	return sortedKeys(seen)
}
