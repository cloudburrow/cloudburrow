package compute

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Identity-Aware Proxy on backend services (#2160), as
// https://docs.cloud.google.com/iap/docs/enabling-compute-howto,
// .../iap/docs/authentication-howto, .../iap/docs/signed-headers-howto,
// .../iap/docs/identity-howto and .../iap/docs/managing-access describe it
// (read 2026-11-22): a backend service with iap.enabled lets a request
// through only with an OpenID Connect ID token (Authorization or
// Proxy-Authorization: Bearer) whose audience is the IAP OAuth client
// (iap.oauth2ClientId, or one of the settings' programmatic clients) and
// whose principal holds roles/iap.httpsResourceAccessor on the backend
// service's IAP resource. Here the token is one this instance signed (the
// metadata server's identity endpoint, generateIdToken, gcloud auth
// print-identity-token through the local endpoints): Google's sign-in
// pages, which a browser is redirected to without a token, are not
// reachable locally (docs/gaps.md). A request let through carries
// Google's headers: x-goog-iap-jwt-assertion (an ES256 JWT, verifiable
// with the key set at IAPKeysPath on the Compute port, as Google's is at
// https://www.gstatic.com/iap/verify/public_key-jwk),
// x-goog-authenticated-user-email and x-goog-authenticated-user-id.

// IAPKeysPath is where the signed header's public keys are served, as a
// JSON Web Key Set.
const IAPKeysPath = "/cloudburrow/iap/public_key-jwk"

// iapIssuer is the signed header's issuer.
const iapIssuer = "https://cloud.google.com/iap"

const lbIAPStateKey = "cloudburrow/iapKey"

type lbIAPKey struct {
	mu  sync.Mutex
	key *ecdsa.PrivateKey
	kid string
	// pending are browser sign-ins sent to the authorization endpoint,
	// by state, and tickets those redeemed, by ticket (lbiapsignin.go).
	pending, tickets map[string]iapPending
}

// iapKeyLocked is the signing key, made once and kept in the state.
func (s *Server) iapKeyLocked() (*ecdsa.PrivateKey, string, error) {
	k := &s.lb.iap
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.key != nil {
		return k.key, k.kid, nil
	}
	if st, ok := s.res[lbIAPStateKey]; ok {
		if b, _ := pem.Decode([]byte(str(st, "key"))); b != nil {
			if pk, err := x509.ParseECPrivateKey(b.Bytes); err == nil {
				k.key, k.kid = pk, str(st, "kid")
				return k.key, k.kid, nil
			}
		}
	}
	pk, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, "", err
	}
	der, _ := x509.MarshalECPrivateKey(pk)
	sum := sha256.Sum256(der)
	k.key, k.kid = pk, base64.RawURLEncoding.EncodeToString(sum[:6])
	s.res[lbIAPStateKey] = obj{"key": string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})), "kid": k.kid}
	_ = s.saveLocked()
	return k.key, k.kid, nil
}

// serveIAPKeys answers IAPKeysPath.
func (s *Server) serveIAPKeys(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	k, kid, err := s.iapKeyLocked()
	s.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	pad := func(b *big.Int) string {
		buf := make([]byte, 32)
		b.FillBytes(buf)
		return base64.RawURLEncoding.EncodeToString(buf)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(obj{"keys": []any{obj{"alg": "ES256", "crv": "P-256", "kid": kid, "kty": "EC", "use": "sig",
		"x": pad(k.PublicKey.X), "y": pad(k.PublicKey.Y)}}})
}

// iapResource is a backend service's IAP resource name.
func iapResource(bs obj) string {
	if r := str(bs, "cloudburrowIAPResource"); r != "" {
		return r // a forwarding rule's, through a CUSTOM authorization policy
	}
	parts := strings.Split(str(bs, "selfLink"), "/")
	if len(parts) < 4 {
		return ""
	}
	p := "projects/" + parts[1] + "/iap_web/compute"
	if parts[2] == "regions" {
		p += "-" + parts[3]
	}
	return p + "/services/" + lastSeg(str(bs, "selfLink"))
}

// iapPage is the body of IAP's refusals.
func iapRefuse(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Via", "1.1 google")
	if code == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="https://accounts.google.com/"`)
	}
	w.WriteHeader(code)
	_, _ = fmt.Fprint(w, msg)
}

// iapCheck applies a backend service's IAP to a request: ok is false
// when the request was answered; otherwise the identity headers are set
// on r. A browser without a session is sent to the sign-in
// (lbiapsignin.go).
func (s *Server) iapCheck(w http.ResponseWriter, r *http.Request, bs obj, scheme string) bool {
	iap := objOf(bs["iap"])
	if iap == nil || iap["enabled"] != true {
		return true
	}
	// Clients cannot forge IAP's headers.
	for k := range r.Header {
		if lk := strings.ToLower(k); strings.HasPrefix(lk, "x-goog-iap-") || strings.HasPrefix(lk, "x-goog-authenticated-user-") {
			r.Header.Del(k)
		}
	}
	res := iapResource(bs)
	gate := s.opts.IAP
	var settings obj
	if gate != nil {
		settings = gate.Settings(res)
	}
	acc := objOf(settings["accessSettings"])
	if r.Method == http.MethodOptions && objOf(acc["corsSettings"])["allowHttpOptions"] == true &&
		r.Header.Get("Access-Control-Request-Method") != "" {
		return true
	}
	mode := r.URL.Query().Get("gcp-iap-mode")
	if mode == "CLEAR_LOGIN_COOKIE" {
		iapGenerated(w)
		iapClearCookies(w, r)
		q := r.URL.Query()
		q.Del("gcp-iap-mode")
		loc := r.URL.Path
		if len(q) > 0 {
			loc += "?" + q.Encode()
		}
		w.Header().Set("Location", loc)
		w.WriteHeader(http.StatusFound)
		return false
	}
	tok := ""
	for _, h := range []string{"Proxy-Authorization", "Authorization"} {
		if v := r.Header.Get(h); len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
			tok = strings.TrimSpace(v[7:])
			if h == "Proxy-Authorization" {
				r.Header.Del(h)
			}
			break
		}
	}
	email := ""
	if tok == "" {
		if si := s.iapSignIn(); si != nil && mode != "DO_SESSION_REFRESH" {
			cid, _ := iapClient(iap, si)
			email = s.iapSessionEmail(r, cid)
		}
		if email == "" {
			s.iapRedirectToSignIn(w, r, scheme, bs, "Invalid IAP credentials: empty token")
			return false
		}
	} else {
		if gate == nil {
			iapGenerated(w)
			iapRefuse(w, http.StatusUnauthorized, "Invalid IAP credentials: Unable to parse JWT")
			return false
		}
		var aud string
		var ok bool
		email, aud, ok = gate.VerifyIDToken(tok)
		if !ok {
			iapGenerated(w)
			iapRefuse(w, http.StatusUnauthorized, "Invalid IAP credentials: Unable to parse JWT")
			return false
		}
		clients := strs(objOf(acc["oauthSettings"]), "programmaticClients")
		if cid := str(iap, "oauth2ClientId"); cid != "" {
			clients = append(clients, cid)
		}
		if !hasStr(clients, aud) {
			iapGenerated(w)
			iapRefuse(w, http.StatusUnauthorized, "Invalid IAP credentials: Expected JWT to have audience \""+strings.Join(clients, ", ")+"\", but was \""+aud+"\"")
			return false
		}
	}
	if mode == "IDENTITY" {
		// Any signed-in account, with access or not.
		iapIdentity(w, email)
		return false
	}
	if gate == nil || !gate.Allowed(r.Context(), res, email, IAPRequest{Host: r.Host, Path: r.URL.Path, Time: s.opts.Now()}) {
		iapGenerated(w)
		if u := str(objOf(acc["accessDeniedPageSettings"]), "accessDeniedPageUri"); u != "" {
			w.Header().Set("Location", u)
			w.WriteHeader(http.StatusFound)
			return false
		}
		iapRefuse(w, http.StatusForbidden, `<!DOCTYPE html><html lang=en><meta charset=utf-8><title>Error 403 (Forbidden)</title><p><b>403.</b> <ins>That's an error.</ins><p>You don't have access.`)
		return false
	}
	test := ""
	if mode == "SECURE_TOKEN_TEST" {
		test = r.URL.Query().Get("iap-secure-token-test-type")
		if test == "" {
			test = "NOT_SET"
		}
	}
	jwt, err := s.iapAssertion(r.Context(), bs, email, test)
	if err != nil {
		iapGenerated(w)
		iapRefuse(w, http.StatusBadRequest, "Invalid IAP request: "+htmlEscape(err.Error()))
		return false
	}
	uid := "accounts.google.com:" + iapUserID(email)
	r.Header.Set("X-Goog-Iap-Jwt-Assertion", jwt)
	r.Header.Set("X-Goog-Authenticated-User-Email", "accounts.google.com:"+email)
	r.Header.Set("X-Goog-Authenticated-User-Id", uid)
	return true
}

// iapUserID is a stable numeric ID for a principal.
func iapUserID(email string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(email)))
	n := new(big.Int).SetBytes(sum[:9])
	return n.String()
}

// iapAssertion is the signed header: ES256, audience
// /projects/NUMBER/global/backendServices/ID (or regions/R), 10 minutes;
// test is a SECURE_TOKEN_TEST type ("" for none), which spoils it.
func (s *Server) iapAssertion(_ context.Context, bs obj, email, test string) (string, error) {
	s.mu.Lock()
	k, kid, err := s.iapKeyLocked()
	s.mu.Unlock()
	if err != nil {
		return "", err
	}
	parts := strings.Split(str(bs, "selfLink"), "/")
	project := parts[1]
	num := ""
	if s.opts.IAP != nil {
		num = s.opts.IAP.ProjectNumber(project)
	}
	if num == "" {
		num = project
	}
	scope := "global"
	if parts[2] == "regions" {
		scope = "regions/" + parts[3]
	}
	now := s.opts.Now()
	aud := "/projects/" + num + "/" + scope + "/backendServices/" + fmt.Sprint(bs["id"])
	if a := str(bs, "cloudburrowIAPAudience"); a != "" {
		aud = "/projects/" + num + "/" + a
	}
	claims := obj{"aud": aud, "email": email,
		"exp": now.Add(10 * time.Minute).Unix(), "iat": now.Unix(), "iss": iapIssuer, "sub": "accounts.google.com:" + iapUserID(email),
		"identity_source": "GOOGLE"}
	if i := strings.LastIndex(email, "@"); i > 0 && !strings.HasSuffix(email, "gserviceaccount.com") && !strings.HasSuffix(email, "@gmail.com") {
		claims["hd"] = email[i+1:]
	}
	if test != "" {
		if !iapTestClaims(claims, test, now) {
			return "", fmt.Errorf("unknown iap-secure-token-test-type %q", test)
		}
		if test == "SIGNATURE" {
			// Signed by a key that is not IAP's.
			other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				return "", err
			}
			k = other
		}
	}
	return signES256(k, kid, claims)
}

// buildIAP checks a backend service's iap: the OAuth client's secret is
// write-only, kept as its SHA-256 (oauth2ClientSecretSha256), as Google
// returns it.
func buildIAP(b, old obj, scheme string) (obj, error) {
	iap := objOf(b["iap"])
	if iap == nil {
		return nil, nil
	}
	o := obj{"enabled": iap["enabled"] == true}
	if iap["enabled"] == true && (scheme == "INTERNAL" || scheme == "INTERNAL_SELF_MANAGED") {
		return nil, invalid("Invalid value for field 'resource.iap.enabled': 'true'. IAP is not supported for backend services with the %s load balancing scheme.", scheme)
	}
	cid, sec := str(iap, "oauth2ClientId"), str(iap, "oauth2ClientSecret")
	if sec != "" && cid == "" {
		return nil, invalid("Invalid value for field 'resource.iap.oauth2ClientId': ''. An OAuth client secret needs its client ID.")
	}
	if cid != "" {
		o["oauth2ClientId"] = cid
	}
	switch {
	case sec != "":
		sum := sha256.Sum256([]byte(sec))
		o["oauth2ClientSecretSha256"] = fmt.Sprintf("%x", sum)
	case str(iap, "oauth2ClientSecretSha256") != "":
		o["oauth2ClientSecretSha256"] = str(iap, "oauth2ClientSecretSha256")
	case old != nil && str(objOf(old["iap"]), "oauth2ClientId") == cid:
		if v := str(objOf(old["iap"]), "oauth2ClientSecretSha256"); v != "" {
			o["oauth2ClientSecretSha256"] = v
		}
	}
	return o, nil
}
