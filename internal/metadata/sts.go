package metadata

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/wif"
)

// The Security Token Service (#1941): sts.googleapis.com's POST /v1/token,
// which exchanges an external credential that a workload identity pool
// provider trusts for a federated access token this instance mints
// (docs.cloud.google.com/iam/docs/reference/sts/rest/v1/TopLevel/token and
// .../iam/docs/workload-identity-federation, read 2026-11-19).
//
//   - the request is form-encoded (Google's auth libraries) or JSON (the
//     discovery client); grant_type must be token exchange and
//     requested_token_type an access token;
//   - the audience names a provider: one that does not exist, or whose pool
//     or itself is disabled or deleted, is invalid_target;
//   - an OIDC token (subject_token_type jwt or id_token) is verified with
//     the provider's jwksJson, or else the keys its issuer's discovery
//     document names, fetched only from a loopback issuer (everything
//     stays local); alg RS256 or ES256, a kid, iss the provider's issuer,
//     aud one of the allowed audiences (or, with none, the provider's own
//     name with or without https:), exp in the future and iat at most 24
//     hours ago; anything else is invalid_grant;
//   - the attribute mapping and condition are CEL (internal/wif); a
//     condition that refuses is invalid_grant;
//   - the token is opaque and lasts an hour; its account is the subject's
//     principal:// identifier, which IAM checks (and the principalSet://
//     identifiers its mapping gave, kept by the directory);
//   - AWS, SAML and X.509 (mTLS) credentials, and Credential Access
//     Boundary downscoping of a Google token, are not exchanged:
//     501 with an OAuth error.

// FederationDirectory is what the STS needs of the IAM API's pools.
type FederationDirectory interface {
	// FederationProvider is the live provider an audience names.
	FederationProvider(audience string) (wif.Provider, bool)
	// RecordFederatedPrincipal keeps a subject's principalSet:// sets.
	RecordFederatedPrincipal(principal string, sets []string) error
	// FederatedPrincipalActive reports whether a subject's pool is live.
	FederatedPrincipalActive(principal string) bool
}

// SetFederation sets the workload identity directory; nil serves no
// /v1/token.
func (c *Credentials) SetFederation(d FederationDirectory) {
	c.dirMu.Lock()
	c.fed = d
	c.dirMu.Unlock()
}

func (c *Credentials) federation() FederationDirectory {
	c.dirMu.Lock()
	defer c.dirMu.Unlock()
	return c.fed
}

// federatedLive reports whether a token minted for email may be used: an
// account's always is; a federated subject's only while its pool is live.
func (c *Credentials) federatedLive(email string) bool {
	if !strings.HasPrefix(strings.ToLower(email), "principal://") {
		return true
	}
	d := c.federation()
	return d != nil && d.FederatedPrincipalActive(email)
}

// Token types.
const (
	stsGrantType       = "urn:ietf:params:oauth:grant-type:token-exchange"
	stsAccessTokenType = "urn:ietf:params:oauth:token-type:access_token"
	stsJWTType         = "urn:ietf:params:oauth:token-type:jwt"
	stsIDTokenType     = "urn:ietf:params:oauth:token-type:id_token"
	stsAWSType         = "urn:ietf:params:aws:token-type:aws4_request"
	stsSAMLType        = "urn:ietf:params:oauth:token-type:saml2"
	stsMTLSType        = "urn:ietf:params:oauth:token-type:mtls"
	// FederatedTokenTTL is how long a federated access token lasts.
	FederatedTokenTTL = time.Hour
)

// STSPath is the Security Token Service's token method.
const STSPath = "/v1/token"

func stsError(w http.ResponseWriter, code int, kind, desc string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": kind, "error_description": desc})
}

// stsRequest is the exchange's parameters, in either encoding.
type stsRequest struct {
	GrantType          string `json:"grantType"`
	Audience           string `json:"audience"`
	Scope              string `json:"scope"`
	RequestedTokenType string `json:"requestedTokenType"`
	SubjectToken       string `json:"subjectToken"`
	SubjectTokenType   string `json:"subjectTokenType"`
	ActorToken         string `json:"actorToken"`
	Options            string `json:"options"`
}

func readSTSRequest(r *http.Request) (stsRequest, error) {
	var in stsRequest
	body := http.MaxBytesReader(nil, r.Body, 5<<20)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		b, err := io.ReadAll(body)
		if err != nil {
			return in, err
		}
		// The JSON mapping accepts both lowerCamel and the proto names.
		var raw map[string]any
		if err := json.Unmarshal(b, &raw); err != nil {
			return in, err
		}
		get := func(camel, snake string) string {
			for _, k := range []string{camel, snake} {
				if s, ok := raw[k].(string); ok {
					return s
				}
			}
			return ""
		}
		in = stsRequest{GrantType: get("grantType", "grant_type"), Audience: get("audience", "audience"), Scope: get("scope", "scope"),
			RequestedTokenType: get("requestedTokenType", "requested_token_type"), SubjectToken: get("subjectToken", "subject_token"),
			SubjectTokenType: get("subjectTokenType", "subject_token_type"), ActorToken: get("actorToken", "actor_token"), Options: get("options", "options")}
		return in, nil
	}
	r.Body = body
	if err := r.ParseForm(); err != nil {
		return in, err
	}
	f := r.PostForm
	return stsRequest{GrantType: f.Get("grant_type"), Audience: f.Get("audience"), Scope: f.Get("scope"),
		RequestedTokenType: f.Get("requested_token_type"), SubjectToken: f.Get("subject_token"),
		SubjectTokenType: f.Get("subject_token_type"), ActorToken: f.Get("actor_token"), Options: f.Get("options")}, nil
}

// sts serves POST /v1/token.
func (s *Server) sts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		stsError(w, http.StatusMethodNotAllowed, "invalid_request", "The token endpoint requires POST.")
		return
	}
	d := s.creds.federation()
	if d == nil {
		stsError(w, http.StatusNotImplemented, "invalid_request", "This instance serves no workload identity pools (no project registry).")
		return
	}
	in, err := readSTSRequest(r)
	if err != nil {
		stsError(w, http.StatusBadRequest, "invalid_request", "Unable to parse the request: "+err.Error())
		return
	}
	switch {
	case in.GrantType == "":
		stsError(w, http.StatusBadRequest, "invalid_request", "Missing required parameter: grant_type.")
		return
	case in.GrantType != stsGrantType:
		stsError(w, http.StatusBadRequest, "unsupported_grant_type", fmt.Sprintf("Invalid value for \"grant_type\": %s. The grant type must be %s.", in.GrantType, stsGrantType))
		return
	case in.RequestedTokenType == "":
		stsError(w, http.StatusBadRequest, "invalid_request", "Missing required parameter: requested_token_type.")
		return
	case in.RequestedTokenType != stsAccessTokenType:
		stsError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("Invalid value for \"requested_token_type\": %s.", in.RequestedTokenType))
		return
	case in.SubjectToken == "":
		stsError(w, http.StatusBadRequest, "invalid_request", "Missing required parameter: subject_token.")
		return
	case in.SubjectTokenType == "":
		stsError(w, http.StatusBadRequest, "invalid_request", "Missing required parameter: subject_token_type.")
		return
	}
	if in.Options != "" {
		var opts map[string]any
		if json.Unmarshal([]byte(in.Options), &opts) != nil {
			stsError(w, http.StatusBadRequest, "invalid_request", "Invalid value for \"options\": it must be a JSON object.")
			return
		}
		if _, ok := opts["accessBoundary"]; ok {
			stsError(w, http.StatusNotImplemented, "invalid_request", "Credential Access Boundary (options.accessBoundary) is not implemented by CloudBurrow.")
			return
		}
	}
	if in.ActorToken != "" {
		stsError(w, http.StatusNotImplemented, "invalid_request", "actor_token (delegated actor tokens) is not implemented by CloudBurrow.")
		return
	}
	switch in.SubjectTokenType {
	case stsJWTType, stsIDTokenType:
	case stsAccessTokenType:
		stsError(w, http.StatusNotImplemented, "invalid_request", "Exchanging a Google access token (Credential Access Boundary) is not implemented by CloudBurrow.")
		return
	case stsAWSType, stsSAMLType, stsMTLSType:
		stsError(w, http.StatusNotImplemented, "invalid_request", "Exchanging a "+in.SubjectTokenType+" credential is not implemented by CloudBurrow: only OIDC tokens are verified locally.")
		return
	default:
		stsError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("Invalid value for \"subject_token_type\": %s.", in.SubjectTokenType))
		return
	}
	if in.Audience == "" {
		stsError(w, http.StatusBadRequest, "invalid_request", "Missing required parameter: audience.")
		return
	}
	if strings.Contains(in.Audience, "/workforcePools/") {
		stsError(w, http.StatusNotImplemented, "invalid_request", "Workforce Identity Federation (workforcePools) is not implemented by CloudBurrow.")
		return
	}
	p, ok := d.FederationProvider(in.Audience)
	if !ok {
		stsError(w, http.StatusBadRequest, "invalid_target", "The target service indicated by the \"audience\" parameters is invalid. "+
			"This might either be because the pool or provider is disabled or deleted or because it doesn't exist.")
		return
	}
	if p.Kind != wif.KindOIDC {
		stsError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("Invalid value for \"subject_token_type\": %s is not supported by the %s provider %s.", in.SubjectTokenType, p.Kind, p.Name()))
		return
	}
	now := time.Now()
	attrs, err := s.exchangeOIDC(r.Context(), p, in.SubjectToken, now)
	if err != nil {
		stsError(w, http.StatusBadRequest, "invalid_grant", err.Error())
		return
	}
	principal := wif.Principal(p.ProjectNumber, p.Pool, attrs.Subject)
	if err := d.RecordFederatedPrincipal(principal, wif.Sets(p.ProjectNumber, p.Pool, attrs)); err != nil {
		stsError(w, http.StatusInternalServerError, "server_error", "record the federated principal: "+err.Error())
		return
	}
	tok := s.creds.MintAccessTokenFor(principal, FederatedTokenTTL)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]any{
		"access_token":      tok,
		"issued_token_type": stsAccessTokenType,
		"token_type":        "Bearer",
		"expires_in":        int(FederatedTokenTTL.Seconds()),
	})
}

// exchangeOIDC verifies an OIDC token for p and maps its attributes.
func (s *Server) exchangeOIDC(ctx context.Context, p wif.Provider, token string, now time.Time) (wif.Attributes, error) {
	unverified, _, err := wif.ParseUnverified(token)
	if err != nil {
		return wif.Attributes{}, err
	}
	iss, _ := unverified.Claims["iss"].(string)
	if iss != p.IssuerURI {
		return wif.Attributes{}, fmt.Errorf("The issuer in ID Token %s does not match the expected issuer %s.", iss, p.IssuerURI)
	}
	keys := []byte(p.JWKSJSON)
	if len(keys) == 0 {
		if keys, err = fetchLocalJWKS(ctx, p); err != nil {
			return wif.Attributes{}, err
		}
	}
	set, err := wif.ParseJWKS(keys)
	if err != nil {
		return wif.Attributes{}, err
	}
	t, err := wif.Verify(token, set)
	if err != nil {
		return wif.Attributes{}, err
	}
	if err := t.CheckTimes(now); err != nil {
		return wif.Attributes{}, err
	}
	allowed := p.AllowedAudiences
	if len(allowed) == 0 {
		name := p.Name()
		allowed = []string{name, "https:" + name}
	}
	auds := t.Audiences()
	match := false
	for _, a := range auds {
		for _, want := range allowed {
			if a == want {
				match = true
			}
		}
	}
	if !match {
		return wif.Attributes{}, fmt.Errorf("The audience in ID Token [%s] does not match the expected audience %s.", strings.Join(auds, ", "), strings.Join(allowed, ", "))
	}
	mapping := p.Mapping
	if len(mapping) == 0 {
		mapping = map[string]string{"google.subject": "assertion.sub"}
	}
	return wif.Apply(mapping, p.Condition, t.Assertion())
}

// fetchLocalJWKS reads the issuer's discovery document and its jwks_uri,
// both on a loopback address only: CloudBurrow never reaches a remote
// identity provider.
func fetchLocalJWKS(ctx context.Context, p wif.Provider) ([]byte, error) {
	disc := p.DiscoveryURI
	if disc == "" {
		disc = strings.TrimSuffix(p.IssuerURI, "/") + "/.well-known/openid-configuration"
	}
	b, err := getLoopback(ctx, disc)
	if err != nil {
		return nil, err
	}
	var doc struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if json.Unmarshal(b, &doc) != nil || doc.JWKSURI == "" {
		return nil, fmt.Errorf("The issuer's discovery document at %s has no jwks_uri.", disc)
	}
	return getLoopback(ctx, doc.JWKSURI)
}

func getLoopback(ctx context.Context, raw string) ([]byte, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("Invalid URL %q.", raw)
	}
	ip := net.ParseIP(u.Hostname())
	if u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, fmt.Errorf("CloudBurrow fetches an issuer's keys only from a loopback address, not %s: set the provider's oidc.jwksJson instead.", u.Host)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Unable to fetch %s: %v", raw, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Unable to fetch %s: HTTP %d", raw, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}
