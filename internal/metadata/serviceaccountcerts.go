package metadata

// A service account's public certificate URLs (#1750). On Google,
// https://www.googleapis.com/service_accounts/v1/metadata/x509/EMAIL (and
// the older .../robot/v1/metadata/x509/EMAIL a key file's
// client_x509_cert_url names) is a JSON object of the account's key IDs to
// their PEM X.509 certificates, and .../service_accounts/v1/metadata/jwk/EMAIL
// the same keys as a JSON Web Key Set: what a verifier of signJwt's and
// signBlob's signatures, or of a key file's self-signed JWTs, fetches
// (docs.cloud.google.com/iam/docs/reference/credentials/rest/v1/projects.serviceAccounts/signJwt
// and .../keys-create-delete, read 2026-11-17). Here they are served on
// this port: the account's enabled, valid keys, its system-managed one
// first. An account that is not live is 404 NOT_FOUND (Google's body for
// it is not documented).

import (
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// PublicCert is one public key of an account.
type PublicCert struct {
	ID      string
	CertPEM string
	Key     *rsa.PublicKey
}

// The certificate URL paths, as on www.googleapis.com.
const (
	saX509Prefix    = "/service_accounts/v1/metadata/x509/"
	saJWKPrefix     = "/service_accounts/v1/metadata/jwk/"
	robotX509Prefix = "/robot/v1/metadata/x509/"
)

// ClientCertURL is the client_x509_cert_url of a key file whose token
// endpoint is base + "/token".
func ClientCertURL(base, email string) string {
	return base + robotX509Prefix + strings.ReplaceAll(url.PathEscape(email), "@", "%40")
}

func (s *Server) serviceAccountCerts(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	var prefix string
	for _, p := range []string{saX509Prefix, saJWKPrefix, robotX509Prefix} {
		if strings.HasPrefix(path, p) {
			prefix = p
		}
	}
	account := strings.TrimPrefix(path, prefix)
	d := s.creds.directory()
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		iamError(w, http.StatusMethodNotAllowed, "INVALID_ARGUMENT", "only GET is served")
		return
	}
	var certs []PublicCert
	ok := false
	if d != nil && account != "" && !strings.Contains(account, "/") {
		certs, ok = d.PublicCerts(account)
	}
	if !ok {
		iamError(w, http.StatusNotFound, "NOT_FOUND", "Service account "+account+" not found.")
		return
	}
	var body any
	if prefix == saJWKPrefix {
		keys := []map[string]string{}
		for _, c := range certs {
			keys = append(keys, map[string]string{"kty": "RSA", "alg": "RS256", "use": "sig", "kid": c.ID,
				"n": base64.RawURLEncoding.EncodeToString(c.Key.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(bigEndian(c.Key.E))})
		}
		body = map[string]any{"keys": keys}
	} else {
		m := map[string]string{}
		for _, c := range certs {
			m[c.ID] = c.CertPEM
		}
		body = m
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(body)
}

func isCertPath(p string) bool {
	return strings.HasPrefix(p, saX509Prefix) || strings.HasPrefix(p, saJWKPrefix) || strings.HasPrefix(p, robotX509Prefix)
}
