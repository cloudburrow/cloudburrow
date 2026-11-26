package azureprov

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/AzureAD/microsoft-authentication-library-for-go/apps/cache"
	"github.com/AzureAD/microsoft-authentication-library-for-go/apps/public"

	"github.com/cloudburrow/cloudburrow/internal/byoc"
)

// The sign-in is MSAL's interactive browser flow, the one azidentity's
// InteractiveBrowserCredential wraps: MSAL listens on a localhost port,
// opens Microsoft's sign-in page, and Microsoft's page posts the
// authorization code back to it (PKCE). azidentity keeps its token cache
// only through its own cache package, so this uses MSAL directly, with a
// cache accessor whose bytes the caller stores in the OS keychain.

// memoryCache is an MSAL cache accessor that holds the serialized cache in
// memory and reports each change.
type memoryCache struct {
	mu      sync.Mutex
	data    []byte
	changed func([]byte)
}

// Replace loads the held cache into MSAL.
func (c *memoryCache) Replace(_ context.Context, u cache.Unmarshaler, _ cache.ReplaceHints) error {
	c.mu.Lock()
	data := c.data
	c.mu.Unlock()
	if len(data) == 0 {
		return nil
	}
	return u.Unmarshal(data)
}

// Export keeps MSAL's cache, and reports it when it changed.
func (c *memoryCache) Export(_ context.Context, m cache.Marshaler, _ cache.ExportHints) error {
	data, err := m.Marshal()
	if err != nil {
		return err
	}
	c.mu.Lock()
	same := string(data) == string(c.data)
	c.data = data
	changed := c.changed
	c.mu.Unlock()
	if !same && changed != nil {
		changed(data)
	}
	return nil
}

func (c *memoryCache) bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.data...)
}

// secret is what a connection keeps in the keychain.
type secret struct {
	Cache         []byte `json:"msal_cache"`
	HomeAccountID string `json:"home_account_id"`
	Tenant        string `json:"tenant"`
	Subscription  string `json:"subscription"`
	Username      string `json:"username,omitempty"`
}

func (o Options) publicClient(tenant string, c *memoryCache) (public.Client, error) {
	opts := []public.Option{
		public.WithAuthority(o.authority(tenant)),
		public.WithCache(c),
		public.WithHTTPClient(o.HTTPClient),
	}
	if o.DisableInstanceDiscovery {
		opts = append(opts, public.WithInstanceDiscovery(false))
	}
	return public.New(ClientID, opts...)
}

// credential is an azcore.TokenCredential over a stored MSAL cache: every
// token is a silent acquisition, refreshed by MSAL with the refresh token
// when the access token is near expiry.
type credential struct {
	client  public.Client
	account public.Account
	cache   *memoryCache
}

func (o Options) newCredential(ctx context.Context, s secret, save func([]byte) error) (*credential, error) {
	c := &credential{cache: &memoryCache{data: s.Cache}}
	client, err := o.publicClient(s.Tenant, c.cache)
	if err != nil {
		return nil, fmt.Errorf("azure sign-in: %w", err)
	}
	c.client = client
	accounts, err := client.Accounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("azure sign-in: reading the token cache: %w", err)
	}
	for _, a := range accounts {
		if a.HomeAccountID == s.HomeAccountID {
			c.account = a
		}
	}
	if c.account.HomeAccountID == "" {
		return nil, &byoc.AuthError{Message: "the stored Microsoft sign-in holds no account: connect Azure again"}
	}
	if save != nil {
		c.cache.changed = func(data []byte) {
			s.Cache = data
			if b, err := json.Marshal(s); err == nil {
				_ = save(b)
			}
		}
	}
	return c, nil
}

// GetToken implements azcore.TokenCredential.
func (c *credential) GetToken(ctx context.Context, opts policy.TokenRequestOptions) (azcore.AccessToken, error) {
	res, err := c.client.AcquireTokenSilent(ctx, opts.Scopes, public.WithSilentAccount(c.account))
	if err != nil {
		return azcore.AccessToken{}, &byoc.AuthError{Message: "Microsoft no longer accepts the stored sign-in (" + err.Error() + "): connect Azure again"}
	}
	return azcore.AccessToken{Token: res.AccessToken, ExpiresOn: res.ExpiresOn}, nil
}

// signIn is one browser sign-in under way.
type signIn struct {
	d      *Driver
	opts   byoc.SignInOptions
	tenant string

	mu      sync.Mutex
	cache   *memoryCache
	account public.Account
	subs    map[string]string // subscription ID -> name
}

// Prompt implements byoc.SignIn.
func (s *signIn) Prompt() byoc.Prompt {
	return byoc.Prompt{Message: "Sign in on Microsoft's page in your browser"}
}

// Wait implements byoc.SignIn: the browser sign-in, then the subscriptions.
func (s *signIn) Wait(ctx context.Context) ([]byoc.Choice, error) {
	o := s.d.opts
	c := &memoryCache{}
	client, err := o.publicClient(s.tenant, c)
	if err != nil {
		return nil, fmt.Errorf("azure sign-in: %w", err)
	}
	open := s.opts.OpenBrowser
	if open == nil {
		open = o.OpenBrowser
	}
	var iopts []public.AcquireInteractiveOption
	if open != nil {
		iopts = append(iopts, public.WithOpenURL(open))
	}
	res, err := client.AcquireTokenInteractive(ctx, []string{o.scope()}, iopts...)
	if err != nil {
		return nil, fmt.Errorf("azure sign-in did not complete: %w", err)
	}
	// Later calls use the user's own tenant, where the token was issued.
	tenant := res.Account.Realm
	if tenant == "" || tenant == DefaultTenant || tenant == "common" {
		tenant = res.IDToken.TenantID
	}
	if tenant == "" {
		tenant = s.tenant
	}
	cred := &credential{client: client, account: res.Account, cache: c}
	subs, err := listSubscriptions(ctx, o, cred)
	if err != nil {
		return nil, err
	}
	if len(subs) == 0 {
		return nil, errors.New("the signed-in account can see no Azure subscription: ask the subscription's owner for access, or create one at portal.azure.com")
	}
	s.mu.Lock()
	s.cache, s.account, s.tenant, s.subs = c, res.Account, tenant, map[string]string{}
	s.mu.Unlock()
	var out []byoc.Choice
	for _, sub := range subs {
		s.subs[sub.id] = sub.name
		out = append(out, byoc.Choice{ID: sub.id, Label: sub.name + " (" + sub.id + ")"})
	}
	return out, nil
}

// Choose implements byoc.SignIn.
func (s *signIn) Choose(_ context.Context, id string) (byoc.Connection, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache == nil {
		return byoc.Connection{}, nil, errors.New("azure sign-in: not signed in yet")
	}
	name, ok := s.subs[id]
	if !ok {
		return byoc.Connection{}, nil, fmt.Errorf("subscription %q is not one the signed-in account can see: %w", id, byoc.ErrNotFound)
	}
	sec := secret{Cache: s.cache.bytes(), HomeAccountID: s.account.HomeAccountID, Tenant: s.tenant,
		Subscription: id, Username: s.account.PreferredUsername}
	b, err := json.Marshal(sec)
	if err != nil {
		return byoc.Connection{}, nil, err
	}
	conn := byoc.Connection{
		ID: byoc.ConnectionID(byoc.Azure, id), Cloud: byoc.Azure, Method: "browser",
		AccountID: id, AccountName: name, Principal: s.account.PreferredUsername,
		Detail:    map[string]string{"tenant": s.tenant, "home_account_id": s.account.HomeAccountID},
		CreatedAt: s.d.opts.Clock.Now().UTC(),
	}
	return conn, b, nil
}
