// Package awsprov is bring your own cloud's AWS (#2209): sign-in through IAM
// Identity Center's device flow (or an access key), and the VM CloudBurrow
// runs an instance's compute on, in the user's own account, with the AWS SDK
// for Go v2.
//
// It never reads ~/.aws: every aws.Config is built here from the keychain's
// credentials, so no shared config or credentials file, environment
// variable or instance profile is consulted.
package awsprov

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sso"
	"github.com/aws/aws-sdk-go-v2/service/ssooidc"
	oidctypes "github.com/aws/aws-sdk-go-v2/service/ssooidc/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/cloudburrow/cloudburrow/internal/byoc"
	"github.com/cloudburrow/cloudburrow/internal/sched"
)

// The services an Options.Endpoints map may name.
const (
	ServiceEC2     = "ec2"
	ServiceIAM     = "iam"
	ServiceSTS     = "sts"
	ServicePricing = "pricing"
	ServiceSSO     = "sso"
	ServiceOIDC    = "oidc"
)

// Options configure the driver. The zero value talks to AWS itself.
type Options struct {
	// Endpoint, when set, is the base URL of every service: a local fake.
	Endpoint string
	// Endpoints overrides Endpoint per service (ServiceEC2, ...).
	Endpoints map[string]string
	// HTTPClient is the client every SDK call uses; nil for the SDK's.
	HTTPClient aws.HTTPClient
	// Clock paces the device flow's polls and the waits on a VM's state.
	Clock sched.Clock
	// PollInterval is the wait between two reads of a VM's state; 5s by
	// default.
	PollInterval time.Duration
	// ClientName is the name IAM Identity Center shows for the sign-in.
	ClientName string
}

func (o Options) endpoint(service string) string {
	if e := o.Endpoints[service]; e != "" {
		return e
	}
	return o.Endpoint
}

func (o Options) clock() sched.Clock {
	if o.Clock == nil {
		return sched.RealClock{}
	}
	return o.Clock
}

func (o Options) poll() time.Duration {
	if o.PollInterval <= 0 {
		return 5 * time.Second
	}
	return o.PollInterval
}

// Driver signs in to AWS and opens connections.
type Driver struct{ opts Options }

// NewDriver is a driver with opts.
func NewDriver(opts Options) *Driver { return &Driver{opts: opts} }

var _ byoc.Driver = (*Driver)(nil)

// Cloud is AWS.
func (d *Driver) Cloud() byoc.Cloud { return byoc.AWS }

// config is an aws.Config built from nothing but its arguments.
func (d *Driver) config(region string, creds aws.CredentialsProvider) aws.Config {
	cfg := aws.Config{Region: region, Credentials: creds}
	if d.opts.HTTPClient != nil {
		cfg.HTTPClient = d.opts.HTTPClient
	}
	return cfg
}

func (d *Driver) base(service string) *string {
	if e := d.opts.endpoint(service); e != "" {
		return aws.String(e)
	}
	return nil
}

func (d *Driver) oidc(region string) *ssooidc.Client {
	return ssooidc.NewFromConfig(d.config(region, nil), func(o *ssooidc.Options) {
		o.BaseEndpoint = d.base(ServiceOIDC)
	})
}

func (d *Driver) portal(region string) *sso.Client {
	return sso.NewFromConfig(d.config(region, nil), func(o *sso.Options) {
		o.BaseEndpoint = d.base(ServiceSSO)
	})
}

func (d *Driver) sts(region string, creds aws.CredentialsProvider) *sts.Client {
	return sts.NewFromConfig(d.config(region, creds), func(o *sts.Options) {
		o.BaseEndpoint = d.base(ServiceSTS)
	})
}

// secret is the keychain item of one connection.
type secret struct {
	Method          string    `json:"method"`
	AccessKeyID     string    `json:"access_key_id"`
	SecretAccessKey string    `json:"secret_access_key"`
	SessionToken    string    `json:"session_token,omitempty"`
	Expires         time.Time `json:"expires,omitempty"`
	SSO             *ssoState `json:"sso,omitempty"`
}

// ssoState is what refreshing an IAM Identity Center sign-in takes.
type ssoState struct {
	StartURL            string    `json:"start_url"`
	Region              string    `json:"region"`
	AccountID           string    `json:"account_id"`
	RoleName            string    `json:"role_name"`
	AccessToken         string    `json:"access_token"`
	AccessTokenExpires  time.Time `json:"access_token_expires"`
	RefreshToken        string    `json:"refresh_token,omitempty"`
	ClientID            string    `json:"client_id"`
	ClientSecret        string    `json:"client_secret"`
	ClientSecretExpires time.Time `json:"client_secret_expires"`
}

const deviceGrant = "urn:ietf:params:oauth:grant-type:device_code"

// BeginSignIn starts IAM Identity Center's device flow: it registers a
// client, asks for a user code and opens the verification page.
func (d *Driver) BeginSignIn(ctx context.Context, opts byoc.SignInOptions) (byoc.SignIn, error) {
	startURL := strings.TrimSpace(opts.StartURL)
	region := strings.TrimSpace(opts.Region)
	if startURL == "" {
		return nil, errors.New("the AWS access portal URL is required (it looks like https://d-xxxxxxxxxx.awsapps.com/start); " +
			"for an account without IAM Identity Center, connect with an access key")
	}
	if u, err := url.Parse(startURL); err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("the AWS access portal URL %q is not an https URL", startURL)
	}
	if region == "" {
		return nil, errors.New("the IAM Identity Center region is required (the region its access portal is in, such as us-east-1)")
	}
	name := d.opts.ClientName
	if name == "" {
		name = "CloudBurrow"
	}
	oidc := d.oidc(region)
	reg, err := oidc.RegisterClient(ctx, &ssooidc.RegisterClientInput{
		ClientName: aws.String(name),
		ClientType: aws.String("public"),
		Scopes:     []string{"sso:account:access"},
		GrantTypes: []string{deviceGrant, "refresh_token"},
	})
	if err != nil {
		return nil, mapErr("sso-oauth:RegisterClient", err)
	}
	dev, err := oidc.StartDeviceAuthorization(ctx, &ssooidc.StartDeviceAuthorizationInput{
		ClientId:     reg.ClientId,
		ClientSecret: reg.ClientSecret,
		StartUrl:     aws.String(startURL),
	})
	if err != nil {
		return nil, mapErr("sso-oauth:StartDeviceAuthorization", err)
	}
	now := d.opts.clock().Now()
	s := &ssoSignIn{
		d:      d,
		region: region, startURL: startURL,
		clientID: aws.ToString(reg.ClientId), clientSecret: aws.ToString(reg.ClientSecret),
		clientExpires: time.Unix(reg.ClientSecretExpiresAt, 0).UTC(),
		deviceCode:    aws.ToString(dev.DeviceCode),
		interval:      time.Duration(dev.Interval) * time.Second,
		expires:       now.Add(time.Duration(dev.ExpiresIn) * time.Second),
	}
	if s.interval <= 0 {
		s.interval = 5 * time.Second
	}
	page := aws.ToString(dev.VerificationUriComplete)
	if page == "" {
		page = aws.ToString(dev.VerificationUri)
	}
	s.prompt = byoc.Prompt{
		URL:       page,
		UserCode:  aws.ToString(dev.UserCode),
		ExpiresAt: s.expires,
		Message:   "Sign in on the AWS page that opened, and check that it shows the code " + aws.ToString(dev.UserCode) + ".",
	}
	if opts.OpenBrowser != nil && page != "" {
		if err := opts.OpenBrowser(page); err != nil {
			s.prompt.Message = "Open " + page + " in your browser, sign in, and check that it shows the code " +
				aws.ToString(dev.UserCode) + "."
		}
	}
	return s, nil
}

type ssoSignIn struct {
	d                      *Driver
	region, startURL       string
	clientID, clientSecret string
	clientExpires          time.Time
	deviceCode             string
	interval               time.Duration
	expires                time.Time
	prompt                 byoc.Prompt

	mu           sync.Mutex
	accessToken  string
	tokenExpires time.Time
	refreshToken string
	choices      map[string]ssoChoice
}

type ssoChoice struct {
	account, accountName, role string
}

func (s *ssoSignIn) Prompt() byoc.Prompt { return s.prompt }

// Wait polls CreateToken until the user approves the code, then lists the
// accounts and roles the sign-in may use.
func (s *ssoSignIn) Wait(ctx context.Context) ([]byoc.Choice, error) {
	clock := s.d.opts.clock()
	oidc := s.d.oidc(s.region)
	interval := s.interval
	for {
		tok, err := oidc.CreateToken(ctx, &ssooidc.CreateTokenInput{
			ClientId:     aws.String(s.clientID),
			ClientSecret: aws.String(s.clientSecret),
			GrantType:    aws.String(deviceGrant),
			DeviceCode:   aws.String(s.deviceCode),
		})
		if err == nil {
			s.mu.Lock()
			s.accessToken = aws.ToString(tok.AccessToken)
			s.tokenExpires = clock.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
			s.refreshToken = aws.ToString(tok.RefreshToken)
			s.mu.Unlock()
			return s.list(ctx)
		}
		var pending *oidctypes.AuthorizationPendingException
		var slow *oidctypes.SlowDownException
		var expired *oidctypes.ExpiredTokenException
		var denied *oidctypes.AccessDeniedException
		switch {
		case errors.As(err, &pending):
		case errors.As(err, &slow):
			interval += 5 * time.Second
		case errors.As(err, &expired):
			return nil, errors.New("the sign-in code expired before it was approved: start the sign-in again")
		case errors.As(err, &denied):
			return nil, errors.New("the sign-in was declined on the AWS page")
		default:
			return nil, mapErr("sso-oauth:CreateToken", err)
		}
		if !s.expires.IsZero() && !clock.Now().Before(s.expires) {
			return nil, errors.New("the sign-in code expired before it was approved: start the sign-in again")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-clock.After(interval):
		}
	}
}

func (s *ssoSignIn) list(ctx context.Context) ([]byoc.Choice, error) {
	portal := s.d.portal(s.region)
	token := aws.String(s.accessToken)
	choices := map[string]ssoChoice{}
	var out []byoc.Choice
	accounts := sso.NewListAccountsPaginator(portal, &sso.ListAccountsInput{AccessToken: token})
	for accounts.HasMorePages() {
		page, err := accounts.NextPage(ctx)
		if err != nil {
			return nil, mapErr("sso:ListAccounts", err)
		}
		for _, a := range page.AccountList {
			roles := sso.NewListAccountRolesPaginator(portal, &sso.ListAccountRolesInput{AccessToken: token, AccountId: a.AccountId})
			for roles.HasMorePages() {
				rp, err := roles.NextPage(ctx)
				if err != nil {
					return nil, mapErr("sso:ListAccountRoles", err)
				}
				for _, r := range rp.RoleList {
					c := ssoChoice{account: aws.ToString(a.AccountId), accountName: aws.ToString(a.AccountName), role: aws.ToString(r.RoleName)}
					id := c.account + "/" + c.role
					choices[id] = c
					label := c.account
					if c.accountName != "" {
						label = c.accountName + " (" + c.account + ")"
					}
					out = append(out, byoc.Choice{ID: id, Label: label + " - " + c.role})
				}
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("this IAM Identity Center sign-in has no AWS account and role assigned to it: " +
			"ask your administrator to assign you a permission set on an account")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	s.mu.Lock()
	s.choices = choices
	s.mu.Unlock()
	return out, nil
}

// Choose connects one account and role.
func (s *ssoSignIn) Choose(ctx context.Context, id string) (byoc.Connection, []byte, error) {
	s.mu.Lock()
	c, ok := s.choices[id]
	st := &ssoState{
		StartURL: s.startURL, Region: s.region, AccountID: c.account, RoleName: c.role,
		AccessToken: s.accessToken, AccessTokenExpires: s.tokenExpires, RefreshToken: s.refreshToken,
		ClientID: s.clientID, ClientSecret: s.clientSecret, ClientSecretExpires: s.clientExpires,
	}
	s.mu.Unlock()
	if !ok {
		return byoc.Connection{}, nil, fmt.Errorf("%q is not one of the sign-in's accounts and roles", id)
	}
	sec := &secret{Method: "sso", SSO: st}
	if err := s.d.roleCredentials(ctx, sec); err != nil {
		return byoc.Connection{}, nil, err
	}
	who, err := s.d.sts(s.region, staticCreds(sec)).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return byoc.Connection{}, nil, mapErr("sts:GetCallerIdentity", err)
	}
	raw, err := json.Marshal(sec)
	if err != nil {
		return byoc.Connection{}, nil, err
	}
	conn := byoc.Connection{
		ID: byoc.ConnectionID(byoc.AWS, c.account), Cloud: byoc.AWS, Method: "sso",
		AccountID: c.account, AccountName: c.accountName, Principal: aws.ToString(who.Arn),
		Detail:    map[string]string{"start_url": s.startURL, "sso_region": s.region, "role": c.role},
		CreatedAt: s.d.opts.clock().Now().UTC(),
	}
	return conn, raw, nil
}

// roleCredentials fills sec's credentials from GetRoleCredentials.
func (d *Driver) roleCredentials(ctx context.Context, sec *secret) error {
	st := sec.SSO
	out, err := d.portal(st.Region).GetRoleCredentials(ctx, &sso.GetRoleCredentialsInput{
		AccessToken: aws.String(st.AccessToken), AccountId: aws.String(st.AccountID), RoleName: aws.String(st.RoleName),
	})
	if err != nil {
		return mapErr("sso:GetRoleCredentials", err)
	}
	rc := out.RoleCredentials
	if rc == nil {
		return errors.New("IAM Identity Center returned no role credentials")
	}
	sec.AccessKeyID = aws.ToString(rc.AccessKeyId)
	sec.SecretAccessKey = aws.ToString(rc.SecretAccessKey)
	sec.SessionToken = aws.ToString(rc.SessionToken)
	sec.Expires = time.UnixMilli(rc.Expiration).UTC()
	return nil
}

// refreshToken renews the IAM Identity Center access token with the
// refresh token, while the client registration is still valid.
func (d *Driver) refreshToken(ctx context.Context, st *ssoState) error {
	now := d.opts.clock().Now()
	if st.RefreshToken == "" || (!st.ClientSecretExpires.IsZero() && !now.Before(st.ClientSecretExpires)) {
		return &byoc.AuthError{Message: "the IAM Identity Center sign-in has expired"}
	}
	tok, err := d.oidc(st.Region).CreateToken(ctx, &ssooidc.CreateTokenInput{
		ClientId: aws.String(st.ClientID), ClientSecret: aws.String(st.ClientSecret),
		GrantType: aws.String("refresh_token"), RefreshToken: aws.String(st.RefreshToken),
	})
	if err != nil {
		return &byoc.AuthError{Message: "the IAM Identity Center sign-in could not be renewed: " + err.Error()}
	}
	st.AccessToken = aws.ToString(tok.AccessToken)
	st.AccessTokenExpires = now.Add(time.Duration(tok.ExpiresIn) * time.Second)
	if r := aws.ToString(tok.RefreshToken); r != "" {
		st.RefreshToken = r
	}
	return nil
}

func staticCreds(sec *secret) aws.CredentialsProvider {
	c := aws.Credentials{AccessKeyID: sec.AccessKeyID, SecretAccessKey: sec.SecretAccessKey, SessionToken: sec.SessionToken, Source: "cloudburrow-keychain"}
	return aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) { return c, nil })
}

// ConnectKey connects with an access key, checked with GetCallerIdentity.
func (d *Driver) ConnectKey(ctx context.Context, key byoc.AccessKey) (byoc.Connection, []byte, error) {
	key.AccessKeyID = strings.TrimSpace(key.AccessKeyID)
	key.SecretAccessKey = strings.TrimSpace(key.SecretAccessKey)
	key.SessionToken = strings.TrimSpace(key.SessionToken)
	if key.AccessKeyID == "" || key.SecretAccessKey == "" {
		return byoc.Connection{}, nil, errors.New("both the access key ID and the secret access key are required")
	}
	sec := &secret{Method: "access-key", AccessKeyID: key.AccessKeyID, SecretAccessKey: key.SecretAccessKey, SessionToken: key.SessionToken}
	who, err := d.sts(defaultRegion, staticCreds(sec)).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return byoc.Connection{}, nil, mapErr("sts:GetCallerIdentity", err)
	}
	raw, err := json.Marshal(sec)
	if err != nil {
		return byoc.Connection{}, nil, err
	}
	account := aws.ToString(who.Account)
	return byoc.Connection{
		ID: byoc.ConnectionID(byoc.AWS, account), Cloud: byoc.AWS, Method: "access-key",
		AccountID: account, Principal: aws.ToString(who.Arn),
		CreatedAt: d.opts.clock().Now().UTC(),
	}, raw, nil
}

// defaultRegion is where the account-wide services (STS, IAM) are called
// for an access-key connection, and the Pricing API's region.
const defaultRegion = "us-east-1"

// Open is the connection's Provider.
func (d *Driver) Open(_ context.Context, conn byoc.Connection, raw []byte, save func([]byte) error) (byoc.Provider, error) {
	if conn.Cloud != byoc.AWS {
		return nil, fmt.Errorf("connection %s is not an AWS connection", conn.ID)
	}
	var sec secret
	if err := json.Unmarshal(raw, &sec); err != nil {
		return nil, fmt.Errorf("the keychain item of connection %s is unreadable: %w", conn.ID, err)
	}
	if sec.Method == "sso" && sec.SSO == nil {
		return nil, fmt.Errorf("the keychain item of connection %s has no IAM Identity Center sign-in", conn.ID)
	}
	p := &Provider{d: d, conn: conn, sec: sec, save: save, home: defaultRegion}
	if sec.SSO != nil && sec.SSO.Region != "" {
		p.home = sec.SSO.Region
	}
	p.creds = aws.NewCredentialsCache(aws.CredentialsProviderFunc(p.retrieve), func(o *aws.CredentialsCacheOptions) {
		o.ExpiryWindow = 5 * time.Minute
	})
	return p, nil
}

// retrieve is the provider's credentials: the stored ones while they last,
// then new role credentials from IAM Identity Center.
func (p *Provider) retrieve(ctx context.Context) (aws.Credentials, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.d.opts.clock().Now()
	if p.sec.Method != "sso" {
		return aws.Credentials{AccessKeyID: p.sec.AccessKeyID, SecretAccessKey: p.sec.SecretAccessKey,
			SessionToken: p.sec.SessionToken, Source: "cloudburrow-keychain"}, nil
	}
	if p.sec.AccessKeyID == "" || !now.Add(5*time.Minute).Before(p.sec.Expires) {
		st := p.sec.SSO
		if !now.Add(time.Minute).Before(st.AccessTokenExpires) {
			if err := p.d.refreshToken(ctx, st); err != nil {
				return aws.Credentials{}, err
			}
		}
		if err := p.d.roleCredentials(ctx, &p.sec); err != nil {
			var auth *byoc.AuthError
			if !errors.As(err, &auth) {
				return aws.Credentials{}, err
			}
			// The token was revoked early: renew it once.
			if rerr := p.d.refreshToken(ctx, st); rerr != nil {
				return aws.Credentials{}, rerr
			}
			if err := p.d.roleCredentials(ctx, &p.sec); err != nil {
				return aws.Credentials{}, err
			}
		}
		if p.save != nil {
			raw, err := json.Marshal(p.sec)
			if err == nil {
				err = p.save(raw)
			}
			if err != nil {
				return aws.Credentials{}, fmt.Errorf("the renewed credentials could not be saved to the keychain: %w", err)
			}
		}
	}
	return aws.Credentials{AccessKeyID: p.sec.AccessKeyID, SecretAccessKey: p.sec.SecretAccessKey,
		SessionToken: p.sec.SessionToken, CanExpire: true, Expires: p.sec.Expires, Source: "cloudburrow-iam-identity-center"}, nil
}
