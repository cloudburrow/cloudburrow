package compute

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// IAP's browser sign-in (#2160). On Google, a browser that reaches an
// IAP-secured app without a session is redirected to Google's sign-in
// (accounts.google.com) with IAP's OAuth client, whose redirect URI is
// https://iap.googleapis.com/v1/oauth/clientIds/CLIENT_ID:handleRedirect
// (iap/docs/custom-oauth-configuration); IAP then sets its session cookie
// on the app's domain and sends the browser back to the page it asked
// for. An AJAX request (X-Requested-With: XMLHttpRequest) gets 401
// instead, and every response IAP makes itself carries
// X-Goog-IAP-Generated-Response (iap/docs/sessions-howto,
// .../query-parameters-and-headers-howto, read 2026-11-22). The query
// parameter gcp-iap-mode=CLEAR_LOGIN_COOKIE clears IAP's cookies,
// IDENTITY answers the signed-in identity as JSON, DO_SESSION_REFRESH
// signs in again, and SECURE_TOKEN_TEST (with iap-secure-token-test-type)
// sends the backend a deliberately invalid signed header.
//
// Here the sign-in is this instance's own: the authorization endpoint of
// the metadata server (Google Auth Platform's, internal/authplatform),
// whose consent page signs in the instance's user, with the backend
// service's OAuth client (one made in Google Auth Platform, with IAP's
// redirect URI) or IAP's Google-managed one. The browser is sent back to
// the redirect URI on the Compute Engine port, where IAP is served, and
// from there to the app's /_gcp_gatekeeper/authenticate, which sets the
// cookies. Google's docs name neither the cookies nor that path; their
// names are the ones Google's IAP is seen to use (UNVERIFIED).

// IAPSignIn is the sign-in an IAPGate may offer browsers.
type IAPSignIn interface {
	// SignInURL is the authorization endpoint's URL for clientID with
	// redirectURI and state, "" when there is none.
	SignInURL(clientID, redirectURI, state string) string
	// Redeem exchanges a code the authorization endpoint issued to
	// clientID (whose secret is secretSHA256, hex; "" for the
	// Google-managed client) for the signed-in account's email.
	Redeem(clientID, secretSHA256, code, redirectURI string) (email string, err error)
	// ManagedClientID is IAP's Google-managed OAuth client.
	ManagedClientID() string
}

// IAPRedirectPrefix is the path of IAP's OAuth redirect URIs on the
// Compute Engine port, as on iap.googleapis.com:
// /v1/oauth/clientIds/CLIENT_ID:handleRedirect.
const IAPRedirectPrefix = "/v1/oauth/clientIds/"

// IAPRedirectURI is IAP's documented redirect URI of an OAuth client.
func IAPRedirectURI(clientID string) string {
	return "https://iap.googleapis.com" + IAPRedirectPrefix + clientID + ":handleRedirect"
}

// iapAuthPath is where the browser comes back to the app to get its
// session cookie.
const iapAuthPath = "/_gcp_gatekeeper/authenticate"

// iapCookiePrefix begins the session cookie's name; GCP_IAP_UID is the
// signed-in user's ID.
const (
	iapCookiePrefix = "GCP_IAP_AUTH_TOKEN_"
	iapUIDCookie    = "GCP_IAP_UID"
)

// iapPending is a sign-in under way: the page asked for, the backend
// service's resource and its OAuth client.
type iapPending struct {
	origin, uri, resource, clientID, secret string
	email                                   string // set once redeemed (a ticket)
	expires                                 time.Time
}

func (s *Server) iapRemember(m map[string]iapPending, p iapPending) string {
	k := &s.lb.iap
	k.mu.Lock()
	defer k.mu.Unlock()
	now := s.opts.Now()
	for id, v := range m {
		if now.After(v.expires) {
			delete(m, id)
		}
	}
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	id := base64.RawURLEncoding.EncodeToString(b)
	m[id] = p
	return id
}

func (s *Server) iapTake(m map[string]iapPending, id string) (iapPending, bool) {
	k := &s.lb.iap
	k.mu.Lock()
	defer k.mu.Unlock()
	p, ok := m[id]
	delete(m, id)
	if !ok || s.opts.Now().After(p.expires) {
		return iapPending{}, false
	}
	return p, true
}

// iapClient is the OAuth client a backend service's IAP signs browsers
// in with: its own, or the Google-managed one.
func iapClient(iap obj, si IAPSignIn) (string, string) {
	if cid := str(iap, "oauth2ClientId"); cid != "" {
		return cid, str(iap, "oauth2ClientSecretSha256")
	}
	return si.ManagedClientID(), ""
}

func (s *Server) iapSignIn() IAPSignIn {
	si, _ := s.opts.IAP.(IAPSignIn)
	return si
}

// iapGenerated marks a response IAP made itself.
func iapGenerated(w http.ResponseWriter) { w.Header().Set("X-Goog-IAP-Generated-Response", "true") }

// iapCookieName is the session cookie of an OAuth client.
func iapCookieName(clientID string) string {
	sum := sha256.Sum256([]byte(clientID))
	return iapCookiePrefix + strings.ToUpper(hex.EncodeToString(sum[:8]))
}

// iapRedirectToSignIn sends a browser to the sign-in, or answers 401 to
// an AJAX request (or when no sign-in is served).
func (s *Server) iapRedirectToSignIn(w http.ResponseWriter, r *http.Request, scheme string, bs obj, msg string) {
	iapGenerated(w)
	si := s.iapSignIn()
	if si == nil || strings.EqualFold(r.Header.Get("X-Requested-With"), "XMLHttpRequest") {
		iapRefuse(w, http.StatusUnauthorized, msg)
		return
	}
	cid, sec := iapClient(objOf(bs["iap"]), si)
	q := r.URL.Query()
	q.Del("gcp-iap-mode")
	uri := r.URL.Path
	if len(q) > 0 {
		uri += "?" + q.Encode()
	}
	state := s.iapRemember(s.lb.iap.pendingMap(), iapPending{origin: scheme + "://" + r.Host, uri: uri, resource: iapResource(bs),
		clientID: cid, secret: sec, expires: s.opts.Now().Add(10 * time.Minute)})
	u := si.SignInURL(cid, IAPRedirectURI(cid), state)
	if u == "" {
		iapRefuse(w, http.StatusUnauthorized, msg)
		return
	}
	w.Header().Set("Location", u)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusFound)
}

func (k *lbIAPKey) pendingMap() map[string]iapPending {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.pending == nil {
		k.pending = map[string]iapPending{}
	}
	return k.pending
}

func (k *lbIAPKey) ticketMap() map[string]iapPending {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.tickets == nil {
		k.tickets = map[string]iapPending{}
	}
	return k.tickets
}

// ServeIAPRedirect is IAP's OAuth redirect URI
// (/v1/oauth/clientIds/CLIENT_ID:handleRedirect on the Compute Engine
// port): it redeems the code and sends the browser back to the app.
func (s *Server) ServeIAPRedirect(w http.ResponseWriter, r *http.Request) {
	iapGenerated(w)
	rest := strings.TrimPrefix(r.URL.Path, IAPRedirectPrefix)
	cid, ok := strings.CutSuffix(rest, ":handleRedirect")
	si := s.iapSignIn()
	if !ok || si == nil || r.Method != http.MethodGet {
		iapRefuse(w, http.StatusNotFound, "Not found")
		return
	}
	q := r.URL.Query()
	p, ok := s.iapTake(s.lb.iap.pendingMap(), q.Get("state"))
	if !ok || p.clientID != cid {
		iapRefuse(w, http.StatusBadRequest, `<!DOCTYPE html><html lang=en><meta charset=utf-8><title>Error 400 (Bad Request)</title><p><b>400.</b> <ins>That's an error.</ins><p>The sign-in request is unknown or expired. Open the app again.`)
		return
	}
	if e := q.Get("error"); e != "" {
		iapRefuse(w, http.StatusForbidden, `<!DOCTYPE html><html lang=en><meta charset=utf-8><title>Error 403 (Forbidden)</title><p><b>403.</b> <ins>That's an error.</ins><p>Sign-in was not completed (`+htmlEscape(e)+`).`)
		return
	}
	email, err := si.Redeem(cid, p.secret, q.Get("code"), IAPRedirectURI(cid))
	if err != nil {
		iapRefuse(w, http.StatusUnauthorized, "Invalid IAP credentials: "+htmlEscape(err.Error()))
		return
	}
	p.email = email
	p.expires = s.opts.Now().Add(2 * time.Minute)
	t := s.iapRemember(s.lb.iap.ticketMap(), p)
	w.Header().Set("Location", p.origin+iapAuthPath+"?"+url.Values{"gcp-iap-ticket": {t}}.Encode())
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusFound)
}

func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

// iapAuthenticate answers the app's /_gcp_gatekeeper/authenticate: the
// session cookie for the ticket, and back to the page first asked for. ok
// is false when the request is no sign-in's.
func (s *Server) iapAuthenticate(w http.ResponseWriter, r *http.Request, scheme string) bool {
	t := r.URL.Query().Get("gcp-iap-ticket")
	if r.URL.Path != iapAuthPath || t == "" {
		return false
	}
	iapGenerated(w)
	p, ok := s.iapTake(s.lb.iap.ticketMap(), t)
	if !ok || p.origin != scheme+"://"+r.Host {
		iapRefuse(w, http.StatusUnauthorized, "Invalid IAP credentials: the sign-in expired")
		return true
	}
	ttl := 24 * time.Hour
	if s.opts.IAP != nil {
		if d := durationString(str(objOf(objOf(s.opts.IAP.Settings(p.resource)["accessSettings"])["reauthSettings"]), "maxAge")); d > 0 {
			ttl = d
		}
	}
	tok, err := s.iapSession(p.clientID, p.email, ttl)
	if err != nil {
		iapRefuse(w, http.StatusInternalServerError, "IAP could not sign the session")
		return true
	}
	secure := scheme == "https"
	http.SetCookie(w, &http.Cookie{Name: iapCookieName(p.clientID), Value: tok, Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: int(ttl.Seconds())})
	http.SetCookie(w, &http.Cookie{Name: iapUIDCookie, Value: iapUserID(p.email), Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: int(ttl.Seconds())})
	w.Header().Set("Location", p.uri)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusFound)
	return true
}

// durationString is a protobuf Duration's JSON ("3600s"), 0 when none.
func durationString(v string) time.Duration {
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0
	}
	return d
}

// iapClearCookies expires every IAP cookie the request carries.
func iapClearCookies(w http.ResponseWriter, r *http.Request) {
	for _, c := range r.Cookies() {
		if strings.HasPrefix(c.Name, iapCookiePrefix) || c.Name == iapUIDCookie {
			http.SetCookie(w, &http.Cookie{Name: c.Name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
		}
	}
}

// iapSession is a session cookie's value: a JWT signed with IAP's key.
func (s *Server) iapSession(clientID, email string, ttl time.Duration) (string, error) {
	now := s.opts.Now()
	return s.iapSign(obj{"iss": iapIssuer, "aud": clientID, "email": email, "sub": "accounts.google.com:" + iapUserID(email),
		"iat": now.Unix(), "exp": now.Add(ttl).Unix(), "typ": "iap_session"})
}

func (s *Server) iapSign(claims obj) (string, error) {
	s.mu.Lock()
	k, kid, err := s.iapKeyLocked()
	s.mu.Unlock()
	if err != nil {
		return "", err
	}
	return signES256(k, kid, claims)
}

func signES256(k *ecdsa.PrivateKey, kid string, claims obj) (string, error) {
	hb, _ := json.Marshal(obj{"alg": "ES256", "typ": "JWT", "kid": kid})
	cb, _ := json.Marshal(claims)
	in := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(cb)
	sum := sha256.Sum256([]byte(in))
	rr, ss, err := ecdsa.Sign(rand.Reader, k, sum[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	rr.FillBytes(sig[:32])
	ss.FillBytes(sig[32:])
	return in + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// iapSessionEmail is the account of a request's session cookie for
// clientID, "" when it has no live one.
func (s *Server) iapSessionEmail(r *http.Request, clientID string) string {
	c, err := r.Cookie(iapCookieName(clientID))
	if err != nil || c.Value == "" {
		return ""
	}
	parts := strings.Split(c.Value, ".")
	if len(parts) != 3 {
		return ""
	}
	s.mu.Lock()
	k, _, err := s.iapKeyLocked()
	s.mu.Unlock()
	if err != nil {
		return ""
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return ""
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(&k.PublicKey, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return ""
	}
	cb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var cl obj
	if json.Unmarshal(cb, &cl) != nil || str(cl, "typ") != "iap_session" || str(cl, "aud") != clientID {
		return ""
	}
	if exp, _ := cl["exp"].(float64); int64(exp) < s.opts.Now().Unix() {
		return ""
	}
	return str(cl, "email")
}

// iapIdentity answers gcp-iap-mode=IDENTITY.
func iapIdentity(w http.ResponseWriter, email string) {
	iapGenerated(w)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]string{"email": "accounts.google.com:" + email, "sub": "accounts.google.com:" + iapUserID(email)})
}

// iapTestClaims spoils the signed header's claims as
// gcp-iap-mode=SECURE_TOKEN_TEST&iap-secure-token-test-type=T asks; ok is
// false for an unknown type. SIGNATURE is applied by the caller.
func iapTestClaims(claims obj, typ string, now time.Time) bool {
	switch typ {
	case "", "NOT_SET", "SIGNATURE":
	case "FUTURE_ISSUE":
		claims["iat"] = now.Add(time.Hour).Unix()
	case "PAST_EXPIRATION":
		claims["exp"] = now.Add(-time.Hour).Unix()
	case "ISSUER":
		claims["iss"] = "https://cloud.google.com/iap-invalid"
	case "AUDIENCE":
		claims["aud"] = "/projects/0/apps/invalid-audience"
	default:
		return false
	}
	return true
}
