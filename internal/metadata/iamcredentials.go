package metadata

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// IAM Credentials (#303): iamcredentials.googleapis.com's generateAccessToken,
// generateIdToken and signJwt, for code that impersonates a service account.
//
// Everything here is local. The tokens are minted by this instance and signed
// with its own key; an ID token verifies against this instance's /certs and
// never against Google's. **No permission is checked**: any caller may
// impersonate any account name, because nothing in CloudBurrow authenticates
// or authorises a request (ADR 0004). What impersonation exists for — the code
// path that asks for a token as another identity — runs; the security it
// provides in production does not exist here, and the docs say so.
//
// signBlob is UNIMPLEMENTED: it signs arbitrary bytes, which is what a
// signed-URL library uses, and a local signature would produce URLs that no
// Google service accepts.

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
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		iamError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "decode request: "+err.Error())
		return
	}
	switch verb {
	case "generateAccessToken":
		ttl := TokenTTL
		if body.Lifetime != "" {
			d, err := time.ParseDuration(body.Lifetime)
			if err != nil || d <= 0 || d > 12*time.Hour {
				iamError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "lifetime must be a duration up to 43200s, such as 3600s")
				return
			}
			ttl = d
		}
		writeJSON(w, map[string]string{
			"accessToken": s.creds.MintAccessToken(),
			"expireTime":  time.Now().Add(ttl).UTC().Format(time.RFC3339),
		})
	case "generateIdToken":
		if body.Audience == "" {
			iamError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "audience is required")
			return
		}
		tok, err := s.creds.mintIDTokenAs(account, body.Audience, body.IncludeEmail, time.Now(), TokenTTL)
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
		signed, err := s.creds.sign(map[string]any{"alg": "RS256", "typ": "JWT", "kid": s.creds.KeyID}, claims)
		if err != nil {
			iamError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
			return
		}
		writeJSON(w, map[string]string{"keyId": s.creds.KeyID, "signedJwt": signed})
	case "signBlob":
		iamError(w, http.StatusNotImplemented, "UNIMPLEMENTED",
			"signBlob is not implemented: a local signature would make signed URLs no Google service accepts")
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
func (c *Credentials) mintIDTokenAs(account, audience string, includeEmail bool, now time.Time, ttl time.Duration) (string, error) {
	email := account
	if email == "-" {
		email = c.Email
	}
	claims := map[string]any{
		"iss": "https://accounts.google.com",
		"aud": audience,
		"sub": subjectFor(email),
		"azp": subjectFor(email),
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
	signingInput, err := joinSegments(header, claims)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.PrivateKey, cryptoSHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("sign token: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}
