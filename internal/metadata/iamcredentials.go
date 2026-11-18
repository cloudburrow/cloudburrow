package metadata

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// IAM Credentials (#303): iamcredentials.googleapis.com's generateAccessToken,
// generateIdToken, signBlob and signJwt, for code that impersonates a
// service account.
//
// Everything here is local. The tokens are minted by this instance and signed
// with its own key; an ID token verifies against this instance's /certs and
// never against Google's.
//
// With the IAM service accounts (#1747, serviceaccounts.go) a call naming
// one of them is authenticated and authorised as Google's is, and signBlob
// and signJwt sign with its system-managed key. A call naming the
// instance's own account ("-" or the ADC fixture's email) keeps the
// earlier behaviour: no permission is checked, signJwt signs with the
// instance key, and signBlob is UNIMPLEMENTED.

// iamPrefix is the collection every IAM Credentials method is under.
const iamPrefix = "/v1/projects/"

func iamError(w http.ResponseWriter, code int, status, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"code": code, "message": msg, "status": status},
	})
}

// iamCredentials dispatches POST /v1/projects/-/serviceAccounts/{account}:{verb}.
func (s *Server) iamCredentials(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, iamPrefix)
	_, after, ok := strings.Cut(rest, "/serviceAccounts/")
	account, verb, hasVerb := strings.Cut(after, ":")
	if !ok || !hasVerb || account == "" {
		iamError(w, http.StatusNotFound, "NOT_FOUND", "unknown IAM Credentials path "+r.URL.Path)
		return
	}
	if r.Method != http.MethodPost {
		iamError(w, http.StatusMethodNotAllowed, "INVALID_ARGUMENT", verb+" requires POST")
		return
	}
	var body struct {
		Scope        []string `json:"scope"`
		Lifetime     string   `json:"lifetime"`
		Audience     string   `json:"audience"`
		IncludeEmail bool     `json:"includeEmail"`
		Payload      string   `json:"payload"`
		Delegates    []string `json:"delegates"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		iamError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "decode request: "+err.Error())
		return
	}
	// With the IAM service accounts (#1747), the call is Google's:
	// authenticated, authorised and on a live account (serviceaccounts.go).
	// The instance's own account keeps the earlier behaviour.
	var target *DirectoryAccount
	if d := s.creds.directory(); d != nil && account != "-" && !strings.EqualFold(account, s.creds.Email) {
		t, ok := s.authorize(w, r, d, verb, account, body.Delegates)
		if !ok {
			return
		}
		target = &t
		account = t.Email
	}
	maxLifetime := 12 * time.Hour
	if target != nil {
		maxLifetime = time.Hour
	}
	switch verb {
	case "generateAccessToken":
		if target != nil && len(body.Scope) == 0 {
			iamError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "Scope is required.")
			return
		}
		ttl := TokenTTL
		if body.Lifetime != "" {
			d, err := time.ParseDuration(body.Lifetime)
			if err != nil || d <= 0 || d > maxLifetime {
				iamError(w, http.StatusBadRequest, "INVALID_ARGUMENT", fmt.Sprintf("lifetime must be a duration up to %ds, such as 3600s", int(maxLifetime.Seconds())))
				return
			}
			ttl = d
		}
		writeJSON(w, map[string]string{
			"accessToken": s.creds.MintAccessTokenFor(s.creds.accountName(account), ttl),
			"expireTime":  time.Now().Add(ttl).UTC().Format(time.RFC3339),
		})
	case "generateIdToken":
		if body.Audience == "" {
			iamError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "audience is required")
			return
		}
		tok, err := s.creds.mintIDTokenAs(account, body.Audience, body.IncludeEmail, target, time.Now(), TokenTTL)
		if err != nil {
			iamError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
			return
		}
		writeJSON(w, map[string]string{"token": tok})
	case "signJwt":
		if body.Payload == "" {
			iamError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "payload is required")
			return
		}
		var claims map[string]any
		if err := json.Unmarshal([]byte(body.Payload), &claims); err != nil {
			iamError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "payload must be a JSON object of claims")
			return
		}
		key, kid := s.creds.PrivateKey, s.creds.KeyID
		if target != nil {
			if exp, ok := claims["exp"].(float64); ok && time.Unix(int64(exp), 0).After(time.Now().Add(12*time.Hour)) {
				iamError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "The exp claim must be at most 12 hours in the future.")
				return
			}
			var err error
			if key, kid, err = s.creds.directory().Signer(target.Email); err != nil {
				iamError(w, http.StatusInternalServerError, "INTERNAL", "sign: "+err.Error())
				return
			}
		}
		signed, err := signWith(key, map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid}, claims)
		if err != nil {
			iamError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
			return
		}
		writeJSON(w, map[string]string{"keyId": kid, "signedJwt": signed})
	case "signBlob":
		if target == nil {
			iamError(w, http.StatusNotImplemented, "UNIMPLEMENTED",
				"signBlob as the instance's own account is not implemented: create a service account (IAM API) and sign as it")
			return
		}
		var blob struct {
			Payload []byte `json:"payload"`
		}
		_ = json.Unmarshal([]byte(`{"payload":`+strconv.Quote(body.Payload)+`}`), &blob)
		if len(blob.Payload) == 0 {
			iamError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "payload is required, base64-encoded")
			return
		}
		key, kid, err := s.creds.directory().Signer(target.Email)
		if err != nil {
			iamError(w, http.StatusInternalServerError, "INTERNAL", "sign: "+err.Error())
			return
		}
		sum := sha256.Sum256(blob.Payload)
		sig, err := rsa.SignPKCS1v15(rand.Reader, key, cryptoSHA256, sum[:])
		if err != nil {
			iamError(w, http.StatusInternalServerError, "INTERNAL", "sign: "+err.Error())
			return
		}
		writeJSON(w, map[string]string{"keyId": kid, "signedBlob": base64.StdEncoding.EncodeToString(sig)})
	default:
		iamError(w, http.StatusNotImplemented, "UNIMPLEMENTED", verb+" is not implemented")
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// mintIDTokenAs is an ID token for the impersonated account, which is what
// generateIdToken returns: its subject and, when asked, its email are the
// account's, not the instance's own.
func (c *Credentials) mintIDTokenAs(account, audience string, includeEmail bool, target *DirectoryAccount, now time.Time, ttl time.Duration) (string, error) {
	email := account
	if email == "-" {
		email = c.Email
	}
	sub := subjectFor(email)
	if target != nil {
		// A service account's subject is its unique ID, as Google's is.
		sub = target.UniqueID
	}
	claims := map[string]any{
		"iss": "https://accounts.google.com",
		"aud": audience,
		"sub": sub,
		"azp": sub,
		"iat": now.Unix(),
		"exp": now.Add(ttl).Unix(),
		// Named so that anything logging the token makes its origin obvious.
		"cloudburrow": "local-development-token",
	}
	if includeEmail {
		claims["email"] = email
		claims["email_verified"] = true
	}
	return c.sign(map[string]any{"alg": "RS256", "typ": "JWT", "kid": c.KeyID}, claims)
}

// subjectFor is a stable numeric subject for an account, as Google's are
// numeric; derived, so it is the same every time and claims nothing.
func subjectFor(email string) string {
	sum := sha256.Sum256([]byte(email))
	var n uint64
	for _, b := range sum[:8] {
		n = n<<8 | uint64(b)
	}
	return fmt.Sprintf("1%020d", n%10000000000000000000)
}

// sign renders and signs a JWT with the instance key.
func (c *Credentials) sign(header, claims map[string]any) (string, error) {
	return signWith(c.PrivateKey, header, claims)
}

// signWith renders and signs a JWT with key.
func signWith(key *rsa.PrivateKey, header, claims map[string]any) (string, error) {
	signingInput, err := joinSegments(header, claims)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, cryptoSHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("sign token: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// ServiceAccountIDToken is the OIDC token Cloud Tasks and Cloud Scheduler
// attach to an HTTP target with oidcToken (#1243): an ID token for the
// task's or job's service account, with its email, for the audience. It is
// signed by this instance, so it verifies against this instance's /certs and
// never against Google's.
func (c *Credentials) ServiceAccountIDToken(account, audience string) (string, error) {
	return c.mintIDTokenAs(account, audience, true, nil, time.Now(), TokenTTL)
}

// ServiceAccountAccessToken is the OAuth access token Cloud Tasks and Cloud
// Scheduler attach to an HTTP target with oauthToken (#1243). It is opaque,
// as Google's are, so the account and scope are not in it: nothing local
// could check them anyway.
func (c *Credentials) ServiceAccountAccessToken(account, scope string) (string, error) {
	return c.MintAccessTokenFor(c.accountName(account), TokenTTL), nil
}

// accountName is the email an impersonation names: "-" is the instance's
// own account.
func (c *Credentials) accountName(account string) string {
	if account == "" || account == "-" {
		return c.Email
	}
	return account
}
