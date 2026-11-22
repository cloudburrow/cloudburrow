// Package authplatform is a local Google Auth Platform (#2030): each
// project's OAuth app (Branding, Audience, Data Access) and its OAuth
// clients with their secrets, and the OAuth 2.0 authorization code flow
// over them, which the instance's metadata server serves beside its token
// endpoint, so an application configured with a local client signs the
// instance's user in as it would sign a user in with Google.
//
// Google Auth Platform is console-only: Google documents no API for
// brands or clients now. The IAP OAuth Admin API that created them
// (iap.googleapis.com v1 projects.brands and identityAwareProxyClients,
// `gcloud iap oauth-brands` and `oauth-clients`) was deprecated on
// 2025-01-22 and shut down on 2026-03-19
// (https://docs.cloud.google.com/iap/docs/deprecations, read 2026-11-20),
// so it is not served. The layout and fields are the Help Center's
// (https://support.google.com/cloud/answer/15549257 and 15549049, read
// 2026-11-20); the flow is Google's OAuth 2.0 for web server and
// installed apps (https://developers.google.com/identity/protocols/oauth2).
//
// Client IDs and secrets are generated on this machine; a secret is shown
// once, at creation, and kept hashed, as Google keeps it.
package authplatform

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// Client types, as Create OAuth client ID names them.
const (
	TypeWeb     = "WEB"
	TypeAndroid = "ANDROID"
	TypeIOS     = "IOS"
	TypeChrome  = "CHROME_EXTENSION"
	TypeTV      = "TV_LIMITED_INPUT"
	TypeDesktop = "DESKTOP"
)

// TypeLabels are the console's words for the types.
var TypeLabels = map[string]string{
	TypeWeb: "Web application", TypeAndroid: "Android", TypeIOS: "iOS", TypeChrome: "Chrome Extension",
	TypeTV: "TVs and Limited Input devices", TypeDesktop: "Desktop app",
}

// MaxSecrets is "A client can have a maximum of two secrets".
const MaxSecrets = 2

// RestoreWindow is how long a deleted client can be restored.
const RestoreWindow = 30 * 24 * time.Hour

// Brand is the project's OAuth app: Branding, Audience and Data Access.
type Brand struct {
	AppName           string   `json:"appName"`
	SupportEmail      string   `json:"supportEmail"`
	HomePage          string   `json:"homePage,omitempty"`
	PrivacyPolicy     string   `json:"privacyPolicy,omitempty"`
	TermsOfService    string   `json:"termsOfService,omitempty"`
	AuthorizedDomains []string `json:"authorizedDomains,omitempty"`
	DeveloperEmails   []string `json:"developerEmails"`
	// UserType is INTERNAL or EXTERNAL.
	UserType string `json:"userType"`
	// PublishingStatus is TESTING or IN_PRODUCTION (an External app).
	PublishingStatus string   `json:"publishingStatus"`
	TestUsers        []string `json:"testUsers,omitempty"`
	// Scopes are Data Access's scopes.
	Scopes     []string  `json:"scopes,omitempty"`
	CreateTime time.Time `json:"createTime"`
}

// Secret is one of a client's secrets.
type Secret struct {
	ID         string    `json:"id"`
	Hash       string    `json:"hash"`
	Last4      string    `json:"last4"`
	Enabled    bool      `json:"enabled"`
	CreateTime time.Time `json:"createTime"`
}

// Client is an OAuth client.
type Client struct {
	ClientID          string     `json:"clientId"`
	Name              string     `json:"name"`
	Type              string     `json:"type"`
	JavaScriptOrigins []string   `json:"javascriptOrigins,omitempty"`
	RedirectURIs      []string   `json:"redirectUris,omitempty"`
	PackageName       string     `json:"packageName,omitempty"`
	SHA1              string     `json:"sha1,omitempty"`
	BundleID          string     `json:"bundleId,omitempty"`
	AppStoreID        string     `json:"appStoreId,omitempty"`
	TeamID            string     `json:"teamId,omitempty"`
	ExtensionID       string     `json:"extensionId,omitempty"`
	Secrets           []Secret   `json:"secrets,omitempty"`
	CreateTime        time.Time  `json:"createTime"`
	DeleteTime        *time.Time `json:"deleteTime,omitempty"`
}

// HasSecret says whether the type is a confidential client: Web, Desktop
// and TV clients have secrets, Android, iOS and Chrome Extension clients
// none.
func HasSecret(typ string) bool { return typ == TypeWeb || typ == TypeDesktop || typ == TypeTV }

type refresh struct {
	ClientID string `json:"clientId"`
	Scope    string `json:"scope"`
	Email    string `json:"email"`
}

type state struct {
	Brand   *Brand              `json:"brand,omitempty"`
	Clients map[string]*Client  `json:"clients,omitempty"`
	Refresh map[string]*refresh `json:"refresh,omitempty"`
}

// Store keeps each project's app and clients.
type Store struct {
	st     store.Store
	now    func() time.Time
	number func(project string) int64

	mu    sync.Mutex
	codes map[string]*grant
	// managed are the Google-managed clients (managed.go).
	managed map[string]managedClient
}

// NewStore keeps the apps in st; number names a project's number, which
// begins its client IDs as Google's do.
func NewStore(st store.Store, number func(string) int64) *Store {
	return &Store{st: st, now: time.Now, number: number, codes: map[string]*grant{}}
}

const keyPrefix = "authplatform/"

// Error is a refusal, with the HTTP status the console or the OAuth
// endpoint answers.
type Error struct {
	Code   int
	OAuth  string // the OAuth error code, for the endpoints
	Detail string
}

func (e *Error) Error() string { return e.Detail }

func errorf(code int, oauth, format string, args ...any) *Error {
	return &Error{Code: code, OAuth: oauth, Detail: fmt.Sprintf(format, args...)}
}

func (s *Store) load(project string) (*state, error) {
	out := &state{Clients: map[string]*Client{}, Refresh: map[string]*refresh{}}
	if s.st == nil {
		return out, nil
	}
	b, err := s.st.Get(keyPrefix + project)
	if errors.Is(err, store.ErrNotFound) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, out); err != nil {
		return nil, fmt.Errorf("authplatform: read %s: %w", project, err)
	}
	if out.Clients == nil {
		out.Clients = map[string]*Client{}
	}
	if out.Refresh == nil {
		out.Refresh = map[string]*refresh{}
	}
	return out, nil
}

func (s *Store) save(project string, st *state) error {
	if s.st == nil {
		return nil
	}
	if !store.SafeKey(keyPrefix + project) {
		return errorf(400, "invalid_request", "project %q cannot be stored", project)
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return s.st.Put(keyPrefix+project, b)
}

// ResetProject forgets a project's app and clients, or every project's.
func (s *Store) ResetProject(project string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st == nil {
		return nil
	}
	keys := []string{keyPrefix + project}
	if project == "" {
		var err error
		if keys, err = s.st.List(keyPrefix); err != nil {
			return err
		}
	}
	for _, k := range keys {
		if err := s.st.Delete(k); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	return nil
}

// Brand is the project's app, nil before Get started.
func (s *Store) Brand(project string) (*Brand, error) {
	st, err := s.load(project)
	if err != nil {
		return nil, err
	}
	return st.Brand, nil
}

func validEmail(e string) bool {
	at := strings.Index(e, "@")
	return at > 0 && at < len(e)-1 && !strings.ContainsAny(e, " ,")
}

func cleanList(xs []string) []string {
	var out []string
	for _, x := range xs {
		if x = strings.TrimSpace(x); x != "" && !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

// SetBrand creates or changes the app (Get started, then Branding,
// Audience and Data Access's Save).
func (s *Store) SetBrand(project string, b Brand) (*Brand, error) {
	b.AppName = strings.TrimSpace(b.AppName)
	if b.AppName == "" {
		return nil, errorf(400, "invalid_request", "App name is required")
	}
	if !validEmail(b.SupportEmail) {
		return nil, errorf(400, "invalid_request", "User support email must be an email address")
	}
	b.DeveloperEmails = cleanList(b.DeveloperEmails)
	if len(b.DeveloperEmails) == 0 {
		return nil, errorf(400, "invalid_request", "Developer contact information needs at least one email address")
	}
	for _, e := range b.DeveloperEmails {
		if !validEmail(e) {
			return nil, errorf(400, "invalid_request", "%q is not an email address", e)
		}
	}
	switch b.UserType {
	case "INTERNAL", "EXTERNAL":
	default:
		return nil, errorf(400, "invalid_request", "User type must be Internal or External")
	}
	switch b.PublishingStatus {
	case "":
		b.PublishingStatus = "TESTING"
	case "TESTING", "IN_PRODUCTION":
	default:
		return nil, errorf(400, "invalid_request", "Publishing status must be Testing or In production")
	}
	if b.UserType == "INTERNAL" {
		b.PublishingStatus = "IN_PRODUCTION"
	}
	for _, link := range []string{b.HomePage, b.PrivacyPolicy, b.TermsOfService} {
		if link == "" {
			continue
		}
		if u, err := url.Parse(link); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, errorf(400, "invalid_request", "%q is not a URL", link)
		}
	}
	b.AuthorizedDomains = cleanList(b.AuthorizedDomains)
	b.TestUsers = cleanList(b.TestUsers)
	if len(b.TestUsers) > 100 {
		return nil, errorf(400, "invalid_request", "an app in testing has at most 100 test users")
	}
	b.Scopes = cleanList(b.Scopes)
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load(project)
	if err != nil {
		return nil, err
	}
	if st.Brand != nil {
		b.CreateTime = st.Brand.CreateTime
	} else {
		b.CreateTime = s.now().UTC()
	}
	st.Brand = &b
	if err := s.save(project, st); err != nil {
		return nil, err
	}
	return &b, nil
}

// Clients lists the project's clients, deleted ones (restorable) when
// deleted is set.
func (s *Store) Clients(project string, deleted bool) ([]*Client, error) {
	st, err := s.load(project)
	if err != nil {
		return nil, err
	}
	var out []*Client
	for _, c := range st.Clients {
		if (c.DeleteTime != nil) == deleted && (c.DeleteTime == nil || s.now().Sub(*c.DeleteTime) < RestoreWindow) {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreateTime.Before(out[j].CreateTime) })
	return out, nil
}

// Client returns one client.
func (s *Store) Client(project, id string) (*Client, error) {
	st, err := s.load(project)
	if err != nil {
		return nil, err
	}
	c := st.Clients[id]
	if c == nil {
		return nil, errorf(404, "invalid_client", "The OAuth client %s was not found.", id)
	}
	return c, nil
}

const alnum = "abcdefghijklmnopqrstuvwxyz0123456789"

func randomString(alphabet string, n int) string {
	b := make([]byte, n)
	for i := range b {
		k, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		b[i] = alphabet[k.Int64()]
	}
	return string(b)
}

func hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func (s *Store) newSecret() (Secret, string) {
	v := "GOCSPX-" + randomString("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_", 28)
	return Secret{ID: randomString(alnum, 12), Hash: hash(v), Last4: v[len(v)-4:], Enabled: true, CreateTime: s.now().UTC()}, v
}

func validRedirect(raw string, web bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Fragment != "" {
		return errorf(400, "invalid_request", "Invalid Redirect: %q must be an absolute URI without a fragment", raw)
	}
	if web && u.Scheme != "https" && !isLoopback(u.Hostname()) {
		return errorf(400, "invalid_request", "Invalid Redirect: %q must use https:// as the scheme, or be a localhost URI", raw)
	}
	return nil
}

func validOrigin(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return errorf(400, "invalid_request", "Invalid Origin: %q must be a scheme and host, with no path", raw)
	}
	if u.Scheme != "https" && !isLoopback(u.Hostname()) {
		return errorf(400, "invalid_request", "Invalid Origin: %q must use https://, or be localhost", raw)
	}
	return nil
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1" || strings.HasSuffix(host, ".localhost")
}

func (c *Client) validate() error {
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" {
		return errorf(400, "invalid_request", "Name is required")
	}
	if _, ok := TypeLabels[c.Type]; !ok {
		return errorf(400, "invalid_request", "Application type %q is not one of Google's", c.Type)
	}
	c.RedirectURIs, c.JavaScriptOrigins = cleanList(c.RedirectURIs), cleanList(c.JavaScriptOrigins)
	if c.Type != TypeWeb && (len(c.RedirectURIs) > 0 || len(c.JavaScriptOrigins) > 0) {
		return errorf(400, "invalid_request", "only a Web application has Authorized JavaScript origins and redirect URIs")
	}
	for _, r := range c.RedirectURIs {
		if err := validRedirect(r, true); err != nil {
			return err
		}
	}
	for _, o := range c.JavaScriptOrigins {
		if err := validOrigin(o); err != nil {
			return err
		}
	}
	switch c.Type {
	case TypeAndroid:
		if c.PackageName == "" || c.SHA1 == "" {
			return errorf(400, "invalid_request", "an Android client needs its Package name and SHA-1 certificate fingerprint")
		}
	case TypeIOS:
		if c.BundleID == "" {
			return errorf(400, "invalid_request", "an iOS client needs its Bundle ID")
		}
	case TypeChrome:
		if c.ExtensionID == "" {
			return errorf(400, "invalid_request", "a Chrome Extension client needs its Item ID")
		}
	}
	return nil
}

// CreateClient makes a client and returns it with its secret, which is
// not kept (only its hash and last four characters are). The app must be
// configured first, as Google's Create OAuth client ID asks.
func (s *Store) CreateClient(project string, c Client) (*Client, string, error) {
	if err := c.validate(); err != nil {
		return nil, "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load(project)
	if err != nil {
		return nil, "", err
	}
	if st.Brand == nil {
		return nil, "", errorf(400, "invalid_request", "To create an OAuth client ID, you must first configure your consent screen (Google Auth Platform: Get started)")
	}
	n := int64(0)
	if s.number != nil {
		n = s.number(project)
	}
	c.ClientID = strconv.FormatInt(n, 10) + "-" + randomString(alnum, 32) + ".apps.googleusercontent.com"
	c.CreateTime, c.DeleteTime, c.Secrets = s.now().UTC(), nil, nil
	secret := ""
	if HasSecret(c.Type) {
		var sec Secret
		sec, secret = s.newSecret()
		c.Secrets = []Secret{sec}
	}
	st.Clients[c.ClientID] = &c
	if err := s.save(project, st); err != nil {
		return nil, "", err
	}
	return &c, secret, nil
}

// UpdateClient changes a client's name and its type's fields.
func (s *Store) UpdateClient(project, id string, in Client) (*Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load(project)
	if err != nil {
		return nil, err
	}
	c := st.Clients[id]
	if c == nil || c.DeleteTime != nil {
		return nil, errorf(404, "invalid_client", "The OAuth client %s was not found.", id)
	}
	in.Type, in.ClientID, in.Secrets, in.CreateTime = c.Type, c.ClientID, c.Secrets, c.CreateTime
	if err := in.validate(); err != nil {
		return nil, err
	}
	st.Clients[id] = &in
	if err := s.save(project, st); err != nil {
		return nil, err
	}
	return &in, nil
}

// DeleteClient deletes a client, restorable for 30 days; RestoreClient
// brings it back.
func (s *Store) DeleteClient(project, id string, restore bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load(project)
	if err != nil {
		return err
	}
	c := st.Clients[id]
	if c == nil || (c.DeleteTime != nil) != restore {
		return errorf(404, "invalid_client", "The OAuth client %s was not found.", id)
	}
	if restore {
		if s.now().Sub(*c.DeleteTime) >= RestoreWindow {
			return errorf(404, "invalid_client", "The OAuth client %s was deleted more than 30 days ago.", id)
		}
		c.DeleteTime = nil
	} else {
		t := s.now().UTC()
		c.DeleteTime = &t
		for k, r := range st.Refresh {
			if r.ClientID == id {
				delete(st.Refresh, k)
			}
		}
	}
	return s.save(project, st)
}

// AddSecret adds a second secret (Add Secret) and returns its value.
func (s *Store) AddSecret(project, id string) (*Client, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load(project)
	if err != nil {
		return nil, "", err
	}
	c := st.Clients[id]
	if c == nil || c.DeleteTime != nil {
		return nil, "", errorf(404, "invalid_client", "The OAuth client %s was not found.", id)
	}
	if !HasSecret(c.Type) {
		return nil, "", errorf(400, "invalid_request", "a %s client has no secret", TypeLabels[c.Type])
	}
	if len(c.Secrets) >= MaxSecrets {
		return nil, "", errorf(400, "invalid_request", "A client can have a maximum of two secrets: disable and delete one first")
	}
	sec, v := s.newSecret()
	c.Secrets = append(c.Secrets, sec)
	if err := s.save(project, st); err != nil {
		return nil, "", err
	}
	return c, v, nil
}

// SetSecret enables or disables a secret, or deletes a disabled one.
func (s *Store) SetSecret(project, id, secretID, action string) (*Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load(project)
	if err != nil {
		return nil, err
	}
	c := st.Clients[id]
	if c == nil || c.DeleteTime != nil {
		return nil, errorf(404, "invalid_client", "The OAuth client %s was not found.", id)
	}
	i := slices.IndexFunc(c.Secrets, func(x Secret) bool { return x.ID == secretID })
	if i < 0 {
		return nil, errorf(404, "invalid_request", "secret %s not found", secretID)
	}
	switch action {
	case "enable":
		c.Secrets[i].Enabled = true
	case "disable":
		c.Secrets[i].Enabled = false
	case "delete":
		if c.Secrets[i].Enabled {
			return nil, errorf(400, "invalid_request", "disable the secret before you delete it")
		}
		c.Secrets = slices.Delete(c.Secrets, i, i+1)
	default:
		return nil, errorf(400, "invalid_request", "unknown action %q", action)
	}
	if err := s.save(project, st); err != nil {
		return nil, err
	}
	return c, nil
}

// find is the live client with a client ID, in whichever project.
func (s *Store) find(clientID string) (string, *state, *Client, error) {
	if m := s.managedOf(clientID); m != nil {
		return "", &state{Brand: m.b, Clients: map[string]*Client{clientID: m.c}, Refresh: map[string]*refresh{}}, m.c, nil
	}
	if s.st == nil || clientID == "" {
		return "", nil, nil, nil
	}
	keys, err := s.st.List(keyPrefix)
	if err != nil {
		return "", nil, nil, err
	}
	for _, k := range keys {
		project := strings.TrimPrefix(k, keyPrefix)
		st, err := s.load(project)
		if err != nil {
			continue
		}
		if c := st.Clients[clientID]; c != nil && c.DeleteTime == nil {
			return project, st, c, nil
		}
	}
	return "", nil, nil, nil
}

// Knows says whether clientID is a live local client, whose token
// requests this package answers.
func (s *Store) Knows(clientID string) bool {
	_, _, c, _ := s.find(clientID)
	return c != nil
}

func (c *Client) secretOK(v string) bool {
	h := hash(v)
	for _, sec := range c.Secrets {
		if sec.Enabled && sec.Hash == h {
			return true
		}
	}
	return false
}

// redirectOK is Google's redirect URI rule: a Web client's must be one it
// lists, exactly; a Desktop client's a loopback address on any port (or
// the out-of-band urn, no longer allowed by Google, refused here).
func (c *Client) redirectOK(raw string) bool {
	switch c.Type {
	case TypeWeb:
		return slices.Contains(c.RedirectURIs, raw)
	case TypeDesktop:
		u, err := url.Parse(raw)
		return err == nil && u.Scheme == "http" && isLoopback(u.Hostname())
	case TypeIOS, TypeAndroid:
		u, err := url.Parse(raw)
		return err == nil && (u.Scheme == strings.ToLower(c.BundleID) || u.Scheme == c.PackageName || strings.HasPrefix(u.Scheme, "com.googleusercontent.apps."))
	case TypeChrome:
		return raw == "https://"+c.ExtensionID+".chromiumapp.org/"
	}
	return false
}

// grant is an authorization code, kept in memory for ten minutes.
type grant struct {
	project, clientID, redirectURI, scope, email string
	challenge, method                            string
	offline                                      bool
	expires                                      time.Time
}

// AuthRequest is an authorization request, checked.
type AuthRequest struct {
	Project     string
	Client      *Client
	Brand       *Brand
	RedirectURI string
	Scope       string
	State       string
	Challenge   string
	Method      string
	Offline     bool
}

// CheckAuthorization validates an authorization request's parameters, as
// Google's authorization endpoint does before it shows the consent
// screen. A *Error with Redirect false is shown on the page (Google's
// "Error 400: redirect_uri_mismatch"); one with a redirect URI goes back
// to the client.
func (s *Store) CheckAuthorization(q url.Values, email string) (*AuthRequest, error) {
	project, st, c, err := s.find(q.Get("client_id"))
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, errorf(401, "invalid_client", "The OAuth client was not found.")
	}
	ru := q.Get("redirect_uri")
	if !c.redirectOK(ru) {
		return nil, errorf(400, "redirect_uri_mismatch", "You can't sign in to this app because it doesn't comply with Google's OAuth 2.0 policy. Request details: redirect_uri=%s", ru)
	}
	if q.Get("response_type") != "code" {
		return nil, errorf(400, "unsupported_response_type", "Required parameter is missing or unsupported: response_type (only code is served)")
	}
	scope := strings.Join(strings.Fields(q.Get("scope")), " ")
	if scope == "" {
		return nil, errorf(400, "invalid_request", "Missing required parameter: scope")
	}
	b := st.Brand
	if b == nil {
		return nil, errorf(400, "invalid_request", "The app has no OAuth consent screen configured.")
	}
	if b.UserType == "EXTERNAL" && b.PublishingStatus == "TESTING" && !slices.Contains(lower(b.TestUsers), strings.ToLower(email)) {
		return nil, errorf(403, "access_denied", "%s has not completed the Google verification process. The app is currently being tested, and can only be accessed by developer-approved testers (Audience: Test users). %s is not one.", b.AppName, email)
	}
	m := q.Get("code_challenge_method")
	if q.Get("code_challenge") != "" && m == "" {
		m = "plain"
	}
	if m != "" && m != "plain" && m != "S256" {
		return nil, errorf(400, "invalid_request", "Invalid code_challenge_method: %s", m)
	}
	return &AuthRequest{Project: project, Client: c, Brand: b, RedirectURI: ru, Scope: scope, State: q.Get("state"),
		Challenge: q.Get("code_challenge"), Method: m, Offline: q.Get("access_type") == "offline" || c.Type != TypeWeb}, nil
}

func lower(xs []string) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = strings.ToLower(x)
	}
	return out
}

// Approve issues an authorization code for a checked request and returns
// the URL the browser is sent back to.
func (s *Store) Approve(a *AuthRequest, email string) string {
	code := "4/" + randomString(alnum+"ABCDEFGHIJKLMNOPQRSTUVWXYZ-_", 60)
	s.mu.Lock()
	for k, g := range s.codes {
		if s.now().After(g.expires) {
			delete(s.codes, k)
		}
	}
	s.codes[code] = &grant{project: a.Project, clientID: a.Client.ClientID, redirectURI: a.RedirectURI, scope: a.Scope, email: email,
		challenge: a.Challenge, method: a.Method, offline: a.Offline, expires: s.now().Add(10 * time.Minute)}
	s.mu.Unlock()
	return back(a.RedirectURI, url.Values{"code": {code}, "scope": {a.Scope}}, a.State)
}

// Deny returns the URL that tells the client the user refused.
func Deny(a *AuthRequest) string {
	return back(a.RedirectURI, url.Values{"error": {"access_denied"}}, a.State)
}

func back(redirect string, v url.Values, st string) string {
	if st != "" {
		v.Set("state", st)
	}
	sep := "?"
	if strings.Contains(redirect, "?") {
		sep = "&"
	}
	return redirect + sep + v.Encode()
}

// Grant is what a token request was granted.
type Grant struct {
	Email        string
	Scope        string
	ClientID     string
	RefreshToken string
}

// Exchange answers a token request (grant_type authorization_code or
// refresh_token) whose client is one of this store's.
func (s *Store) Exchange(form url.Values, basicID, basicSecret string) (*Grant, *Error) {
	id, secret := form.Get("client_id"), form.Get("client_secret")
	if basicID != "" {
		id, secret = basicID, basicSecret
	}
	project, _, c, err := s.find(id)
	if err != nil {
		return nil, errorf(500, "server_error", "%v", err)
	}
	if c == nil {
		return nil, errorf(401, "invalid_client", "The OAuth client was not found.")
	}
	if HasSecret(c.Type) && !c.secretOK(secret) {
		return nil, errorf(401, "invalid_client", "Unauthorized")
	}
	switch form.Get("grant_type") {
	case "authorization_code":
		code := form.Get("code")
		s.mu.Lock()
		g := s.codes[code]
		delete(s.codes, code) // a code is used once
		s.mu.Unlock()
		if g == nil || g.clientID != c.ClientID || s.now().After(g.expires) {
			return nil, errorf(400, "invalid_grant", "Malformed auth code.")
		}
		if form.Get("redirect_uri") != g.redirectURI {
			return nil, errorf(400, "redirect_uri_mismatch", "Bad Request")
		}
		if g.challenge != "" {
			v := form.Get("code_verifier")
			if v == "" {
				return nil, errorf(400, "invalid_grant", "Missing code verifier.")
			}
			want := v
			if g.method == "S256" {
				h := sha256.Sum256([]byte(v))
				want = base64.RawURLEncoding.EncodeToString(h[:])
			}
			if want != g.challenge {
				return nil, errorf(400, "invalid_grant", "Invalid code verifier.")
			}
		} else if !HasSecret(c.Type) {
			return nil, errorf(400, "invalid_request", "client_secret is missing.")
		}
		out := &Grant{Email: g.email, Scope: g.scope, ClientID: c.ClientID}
		if g.offline {
			rt := "1//" + randomString(alnum+"ABCDEFGHIJKLMNOPQRSTUVWXYZ-_", 64)
			s.mu.Lock()
			st, err := s.load(project)
			if err == nil {
				st.Refresh[hash(rt)] = &refresh{ClientID: c.ClientID, Scope: g.scope, Email: g.email}
				err = s.save(project, st)
			}
			s.mu.Unlock()
			if err != nil {
				return nil, errorf(500, "server_error", "%v", err)
			}
			out.RefreshToken = rt
		}
		return out, nil
	case "refresh_token":
		st, err := s.load(project)
		if err != nil {
			return nil, errorf(500, "server_error", "%v", err)
		}
		r := st.Refresh[hash(form.Get("refresh_token"))]
		if r == nil || r.ClientID != c.ClientID {
			return nil, errorf(400, "invalid_grant", "Token has been expired or revoked.")
		}
		return &Grant{Email: r.Email, Scope: r.Scope, ClientID: c.ClientID}, nil
	case "":
		return nil, errorf(400, "invalid_request", "Missing required parameter: grant_type")
	}
	return nil, errorf(400, "unsupported_grant_type", "Invalid grant_type: %s", form.Get("grant_type"))
}

// Revoke forgets a refresh token (https://oauth2.googleapis.com/revoke).
func (s *Store) Revoke(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st == nil {
		return false
	}
	keys, err := s.st.List(keyPrefix)
	if err != nil {
		return false
	}
	h := hash(token)
	for _, k := range keys {
		project := strings.TrimPrefix(k, keyPrefix)
		st, err := s.load(project)
		if err != nil {
			continue
		}
		if _, ok := st.Refresh[h]; ok {
			delete(st.Refresh, h)
			return s.save(project, st) == nil
		}
	}
	return false
}
