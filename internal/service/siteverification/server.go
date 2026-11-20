// Package siteverification serves the Google Site Verification API v1
// (www.googleapis.com/siteVerification/v1, #2030): verification tokens,
// and the sites and domains the caller has verified, which the console's
// APIs & Services > Domain verification lists.
//
// Read from Google's public docs on 2026-11-20:
// https://developers.google.com/site-verification/v1 and its
// webResource reference, with the v1 discovery document
// (google.golang.org/api/siteverification/v1).
//
// A local instance makes no calls off the machine, so verification is
// checked only where it can be without one: FILE and META fetch the site
// when its host is this machine (loopback) or a private address, as a
// site served locally is; DNS_TXT and DNS_CNAME (a lookup in the public
// DNS) and ANALYTICS and TAG_MANAGER (Google's own products) are refused
// with the reason.
package siteverification

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// Prefix is the API's path on the port that serves it.
const Prefix = "/siteVerification/v1/"

// Site is a webResource's site.
type Site struct {
	Type       string `json:"type,omitempty"`
	Identifier string `json:"identifier,omitempty"`
}

// Resource is a verified site or domain.
type Resource struct {
	ID     string   `json:"id,omitempty"`
	Site   Site     `json:"site"`
	Owners []string `json:"owners,omitempty"`
}

// Server is the Site Verification API.
type Server struct {
	st store.Store
	// Caller names the account a bearer token was minted for.
	Caller func(bearer string) (string, bool)
	// Owner is the account a call with no known token is made as: the
	// instance's own user.
	Owner string
	// Client fetches a site for FILE and META; its dialer refuses any
	// address that is not loopback or private.
	Client *http.Client

	mu sync.Mutex
}

// NewServer keeps verified resources in st.
func NewServer(st store.Store, owner string) *Server {
	return &Server{st: st, Owner: owner, Client: localClient()}
}

func localClient() *http.Client {
	d := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			for _, ip := range ips {
				if ip.IP.IsLoopback() || ip.IP.IsPrivate() {
					return d.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
				}
			}
			return nil, errOffMachine
		},
	}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

var errOffMachine = errors.New("the site is not on this machine or a private network, and a local instance makes no calls off the machine")

const storeKey = "siteverification/resources"

func (s *Server) load() (map[string]*Resource, error) {
	out := map[string]*Resource{}
	if s.st == nil {
		return out, nil
	}
	b, err := s.st.Get(storeKey)
	if errors.Is(err, store.ErrNotFound) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("siteverification: %w", err)
	}
	return out, nil
}

func (s *Server) save(all map[string]*Resource) error {
	if s.st == nil {
		return nil
	}
	b, err := json.Marshal(all)
	if err != nil {
		return err
	}
	return s.st.Put(storeKey, b)
}

// Reset forgets every verified resource.
func (s *Server) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st == nil {
		return nil
	}
	if err := s.st.Delete(storeKey); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}

type apiError struct {
	code int
	msg  string
}

func (e *apiError) Error() string { return e.msg }

func fail(code int, format string, args ...any) error {
	return &apiError{code: code, msg: fmt.Sprintf(format, args...)}
}

func (s *Server) caller(r *http.Request) string {
	if s.Caller != nil {
		if b, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			if email, ok := s.Caller(b); ok && email != "" {
				return strings.ToLower(email)
			}
		}
	}
	return strings.ToLower(s.Owner)
}

// ServeHTTP serves the API's paths under Prefix.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	out, err := s.route(r)
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	if err != nil {
		var ae *apiError
		if !errors.As(err, &ae) {
			ae = &apiError{code: http.StatusInternalServerError, msg: err.Error()}
		}
		w.WriteHeader(ae.code)
		reason := map[int]string{400: "invalid", 401: "required", 403: "forbidden", 404: "notFound", 501: "notImplemented"}[ae.code]
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": ae.code, "message": ae.msg,
			"errors": []map[string]any{{"domain": "global", "reason": reason, "message": ae.msg}}}})
		return
	}
	if out == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) route(r *http.Request) (any, error) {
	rest := strings.TrimPrefix(r.URL.EscapedPath(), Prefix)
	who := s.caller(r)
	switch {
	case rest == "token" && r.Method == http.MethodPost:
		var req struct {
			Site               Site   `json:"site"`
			VerificationMethod string `json:"verificationMethod"`
		}
		if err := decode(r, &req); err != nil {
			return nil, err
		}
		return s.GetToken(who, req.Site, req.VerificationMethod)
	case rest == "webResource" && r.Method == http.MethodGet:
		return s.List(who)
	case rest == "webResource" && r.Method == http.MethodPost:
		var in Resource
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return s.Insert(r.Context(), who, in, r.URL.Query().Get("verificationMethod"))
	case strings.HasPrefix(rest, "webResource/"):
		id, err := url.PathUnescape(strings.TrimPrefix(rest, "webResource/"))
		if err != nil || id == "" {
			return nil, fail(http.StatusBadRequest, "Invalid id")
		}
		switch r.Method {
		case http.MethodGet:
			return s.Get(who, id)
		case http.MethodDelete:
			return nil, s.Delete(who, id)
		case http.MethodPut, http.MethodPatch:
			var in Resource
			if err := decode(r, &in); err != nil {
				return nil, err
			}
			return s.Update(who, id, in, r.Method == http.MethodPatch)
		}
	}
	return nil, fail(http.StatusNotFound, "%s %s is not a Site Verification API v1 method", r.Method, r.URL.Path)
}

func decode(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return fail(http.StatusBadRequest, "Invalid JSON payload received: %v", err)
	}
	return nil
}

// ID is a site's webResource ID: its identifier, escaped, with dns:// for
// a domain, as Google's IDs are.
func ID(site Site) string {
	if site.Type == "INET_DOMAIN" {
		return url.QueryEscape("dns://" + strings.ToLower(site.Identifier))
	}
	return url.QueryEscape(site.Identifier)
}

func validSite(site Site) error {
	switch site.Type {
	case "SITE":
		u, err := url.Parse(site.Identifier)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || !strings.HasSuffix(site.Identifier, "/") {
			return fail(http.StatusBadRequest, "Invalid site identifier %q: a SITE is a URL of a site's root, ending in a slash", site.Identifier)
		}
	case "INET_DOMAIN":
		if site.Identifier == "" || strings.ContainsAny(site.Identifier, "/: ") {
			return fail(http.StatusBadRequest, "Invalid domain %q", site.Identifier)
		}
	case "ANDROID_APP":
		if !strings.HasPrefix(site.Identifier, "android-app://") {
			return fail(http.StatusBadRequest, "Invalid Android app identifier %q", site.Identifier)
		}
	case "":
		return fail(http.StatusBadRequest, "Required parameter: site.type")
	default:
		return fail(http.StatusBadRequest, "Invalid site type %q: SITE, INET_DOMAIN or ANDROID_APP", site.Type)
	}
	return nil
}

func methodFits(site Site, method string) error {
	ok := map[string][]string{
		"SITE":        {"FILE", "META", "ANALYTICS", "TAG_MANAGER"},
		"INET_DOMAIN": {"DNS", "DNS_TXT", "DNS_CNAME"},
		"ANDROID_APP": {"ANDROID_DEVELOPER"},
	}[site.Type]
	if method == "" {
		return fail(http.StatusBadRequest, "Required parameter: verificationMethod")
	}
	if !slices.Contains(ok, method) {
		return fail(http.StatusBadRequest, "The verification method %s cannot verify a %s", method, site.Type)
	}
	return nil
}

func tokenValue(owner string, site Site) string {
	h := sha256.Sum256([]byte(owner + "\x00" + site.Type + "\x00" + strings.ToLower(site.Identifier)))
	return hex.EncodeToString(h[:8])
}

// GetToken is webResource.getToken.
func (s *Server) GetToken(who string, site Site, method string) (any, error) {
	if err := validSite(site); err != nil {
		return nil, err
	}
	if err := methodFits(site, method); err != nil {
		return nil, err
	}
	t := tokenValue(who, site)
	var token string
	switch method {
	case "FILE":
		token = "google" + t + ".html"
	case "META":
		token = `<meta name="google-site-verification" content="` + t + `" />`
	case "DNS", "DNS_TXT":
		token = "google-site-verification=" + t
	case "DNS_CNAME":
		token = t + " gv-" + t + ".dv.googlehosted.com"
	default:
		token = t
	}
	return map[string]string{"method": method, "token": token}, nil
}

// List is webResource.list: the resources who owns.
func (s *Server) List(who string) (any, error) {
	all, err := s.load()
	if err != nil {
		return nil, err
	}
	items := []*Resource{}
	for _, r := range all {
		if slices.Contains(r.Owners, who) {
			items = append(items, r)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return map[string]any{"items": items}, nil
}

// Resources is every verified resource, for the console.
func (s *Server) Resources() ([]*Resource, error) {
	all, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make([]*Resource, 0, len(all))
	for _, r := range all {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *Server) owned(who, id string) (map[string]*Resource, *Resource, error) {
	all, err := s.load()
	if err != nil {
		return nil, nil, err
	}
	r := all[id]
	if r == nil {
		r = all[url.QueryEscape(id)]
	}
	if r == nil || !slices.Contains(r.Owners, who) {
		return nil, nil, fail(http.StatusNotFound, "You are not an owner of this site.")
	}
	return all, r, nil
}

// Get is webResource.get.
func (s *Server) Get(who, id string) (any, error) {
	_, r, err := s.owned(who, id)
	return r, err
}

// Insert is webResource.insert: it checks the token is in place and makes
// who an owner.
func (s *Server) Insert(ctx context.Context, who string, in Resource, method string) (any, error) {
	if err := validSite(in.Site); err != nil {
		return nil, err
	}
	if err := methodFits(in.Site, method); err != nil {
		return nil, err
	}
	if err := s.verify(ctx, who, in.Site, method); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.load()
	if err != nil {
		return nil, err
	}
	id := ID(in.Site)
	r := all[id]
	if r == nil {
		r = &Resource{ID: id, Site: in.Site}
		if in.Site.Type == "INET_DOMAIN" {
			r.Site.Identifier = strings.ToLower(in.Site.Identifier)
		}
		all[id] = r
	}
	for _, o := range append([]string{who}, in.Owners...) {
		if o = strings.ToLower(strings.TrimSpace(o)); o != "" && !slices.Contains(r.Owners, o) {
			r.Owners = append(r.Owners, o)
		}
	}
	if err := s.save(all); err != nil {
		return nil, err
	}
	return r, nil
}

func (s *Server) verify(ctx context.Context, who string, site Site, method string) error {
	t := tokenValue(who, site)
	notFound := fail(http.StatusBadRequest, "The necessary verification token could not be found on your site.")
	switch method {
	case "FILE":
		name := "google" + t + ".html"
		body, err := s.fetch(ctx, site.Identifier+name)
		if err != nil {
			return err
		}
		if !strings.Contains(body, "google-site-verification: "+name) {
			return notFound
		}
		return nil
	case "META":
		body, err := s.fetch(ctx, site.Identifier)
		if err != nil {
			return err
		}
		if !strings.Contains(body, `name="google-site-verification"`) || !strings.Contains(body, `content="`+t+`"`) {
			return notFound
		}
		return nil
	case "DNS", "DNS_TXT", "DNS_CNAME":
		return fail(http.StatusBadRequest, "The %s method looks the token up in the public DNS, which a local instance does not query (it makes no calls off the machine); verify a site served on this machine with FILE or META", method)
	default:
		return fail(http.StatusBadRequest, "The %s method verifies through a Google product a local instance does not serve; use FILE or META", method)
	}
}

func (s *Server) fetch(ctx context.Context, u string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", fail(http.StatusBadRequest, "Invalid site URL %q", u)
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		if errors.Is(err, errOffMachine) {
			return "", fail(http.StatusBadRequest, "The verification token could not be fetched from %s: %v", u, errOffMachine)
		}
		return "", fail(http.StatusBadRequest, "The necessary verification token could not be found on your site: %s could not be fetched.", u)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fail(http.StatusBadRequest, "The necessary verification token could not be found on your site: %s answered %d.", u, resp.StatusCode)
	}
	return string(b), nil
}

// Update is webResource.update (and patch): it replaces the owners list,
// which must keep the caller.
func (s *Server) Update(who, id string, in Resource, patch bool) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, r, err := s.owned(who, id)
	if err != nil {
		return nil, err
	}
	if patch && in.Owners == nil {
		return r, nil
	}
	var owners []string
	for _, o := range in.Owners {
		if o = strings.ToLower(strings.TrimSpace(o)); o != "" && !slices.Contains(owners, o) {
			owners = append(owners, o)
		}
	}
	if !slices.Contains(owners, who) {
		return nil, fail(http.StatusBadRequest, "You cannot remove yourself as an owner; delete the resource to unverify it.")
	}
	r.Owners = owners
	if err := s.save(all); err != nil {
		return nil, err
	}
	return r, nil
}

// Delete is webResource.delete: who is no longer an owner, and a
// resource with no owner left is forgotten.
func (s *Server) Delete(who, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, r, err := s.owned(who, id)
	if err != nil {
		return err
	}
	r.Owners = slices.DeleteFunc(r.Owners, func(o string) bool { return o == who })
	if len(r.Owners) == 0 {
		delete(all, r.ID)
	}
	return s.save(all)
}

// MethodStatuses is every method of the v1 discovery document, by its ID,
// each served (tools/coverage's registry).
func MethodStatuses() map[string]bool {
	return map[string]bool{
		"siteVerification.webResource.delete": true, "siteVerification.webResource.get": true,
		"siteVerification.webResource.getToken": true, "siteVerification.webResource.insert": true,
		"siteVerification.webResource.list": true, "siteVerification.webResource.patch": true,
		"siteVerification.webResource.update": true,
	}
}
