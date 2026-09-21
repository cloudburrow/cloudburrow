// Package metadata serves a local GCE metadata server and the credentials
// that point Google tooling at CloudBurrow.
//
// Nothing here is a security boundary. CloudBurrow performs no authentication:
// every adapter serves any caller, and these credentials exist so that tools
// which *insist* on having credentials — gcloud, Terraform, the Google SDKs —
// can be run offline without a real Google account. A token minted here is
// accepted by CloudBurrow because CloudBurrow accepts everything, not because
// it was verified. See docs/credentials.md.
package metadata

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CredentialsFileName is the ADC fixture CloudBurrow writes.
const CredentialsFileName = "credentials.json"

// KeyFileName is the private key backing the fixture.
const KeyFileName = "credentials-key.pem"

// ServiceAccountEmail returns the account name the fixture presents.
//
// The domain is the real one because clients parse and sometimes validate its
// shape; the local part says plainly where it came from.
func ServiceAccountEmail(project string) string {
	return fmt.Sprintf("cloudburrow-local@%s.iam.gserviceaccount.com", project)
}

// Credentials is a locally generated service-account identity.
type Credentials struct {
	Project    string
	Email      string
	PrivateKey *rsa.PrivateKey
	KeyID      string
	// TokenURI is where a client exchanges a signed assertion for a token.
	// It points at CloudBurrow's own endpoint, so nothing leaves the machine.
	TokenURI string
}

// adcFile is the JSON layout Google's clients expect for a service account.
type adcFile struct {
	Type                string `json:"type"`
	ProjectID           string `json:"project_id"`
	PrivateKeyID        string `json:"private_key_id"`
	PrivateKey          string `json:"private_key"`
	ClientEmail         string `json:"client_email"`
	ClientID            string `json:"client_id"`
	AuthURI             string `json:"auth_uri"`
	TokenURI            string `json:"token_uri"`
	AuthProviderCertURL string `json:"auth_provider_x509_cert_url"`
	ClientCertURL       string `json:"client_x509_cert_url"`
	// Comment is not part of the format. It is included so that anyone who
	// opens the file — or finds it in a repository by accident — sees
	// immediately that it grants nothing.
	Comment string `json:"_cloudburrow"`
}

// LoadOrCreate returns the instance's credentials, generating them on first
// use.
//
// The key is persisted rather than regenerated per run: a fixture whose key
// changed on every `up` would invalidate any client that had already read it,
// and the resulting failure ("invalid signature") points nowhere useful.
func LoadOrCreate(dir, project, tokenURI string) (*Credentials, error) {
	keyPath := filepath.Join(dir, KeyFileName)

	if pemBytes, err := os.ReadFile(keyPath); err == nil {
		key, err := parseKey(pemBytes)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", keyPath, err)
		}
		return newCredentials(project, key, tokenURI), nil
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read %s: %w", keyPath, err)
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate credentials key: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create credentials directory: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("encode credentials key: %w", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	// 0600 even though the key authorises nothing: a key file that is
	// world-readable teaches the wrong habit.
	if err := os.WriteFile(keyPath, pemBytes, 0o600); err != nil {
		return nil, fmt.Errorf("write credentials key: %w", err)
	}
	return newCredentials(project, key, tokenURI), nil
}

func newCredentials(project string, key *rsa.PrivateKey, tokenURI string) *Credentials {
	return &Credentials{
		Project:    project,
		Email:      ServiceAccountEmail(project),
		PrivateKey: key,
		KeyID:      keyID(key),
		TokenURI:   tokenURI,
	}
}

func parseKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("key is %T, want an RSA key", parsed)
	}
	return key, nil
}

// keyID derives a stable identifier from the public key, so the same key
// always reports the same kid.
func keyID(key *rsa.PrivateKey) string {
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		// Marshalling an RSA public key cannot fail; a fixed identifier is
		// still better than a panic in a development tool.
		return "cloudburrow-local"
	}
	sum := sha256.Sum256(der)
	return fmt.Sprintf("%x", sum[:8])
}

// WriteADC writes the application default credentials fixture and returns its
// path.
func (c *Credentials) WriteADC(dir string) (string, error) {
	der, err := x509.MarshalPKCS8PrivateKey(c.PrivateKey)
	if err != nil {
		return "", fmt.Errorf("encode credentials key: %w", err)
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))

	body, err := json.MarshalIndent(adcFile{
		Type:         "service_account",
		ProjectID:    c.Project,
		PrivateKeyID: c.KeyID,
		PrivateKey:   keyPEM,
		ClientEmail:  c.Email,
		ClientID:     "000000000000000000000",
		AuthURI:      c.TokenURI,
		TokenURI:     c.TokenURI,
		// The cert URLs point at CloudBurrow too, so a client that fetches
		// them to verify a token does not reach out to Google.
		AuthProviderCertURL: strings.TrimSuffix(c.TokenURI, "/token") + "/certs",
		ClientCertURL:       strings.TrimSuffix(c.TokenURI, "/token") + "/certs",
		Comment: "Generated by CloudBurrow for local development. This key authorises " +
			"nothing: it is accepted only by a local CloudBurrow instance, which " +
			"performs no authentication at all.",
	}, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode credentials: %w", err)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create credentials directory: %w", err)
	}
	path := filepath.Join(dir, CredentialsFileName)
	if err := os.WriteFile(path, append(body, '\n'), 0o600); err != nil {
		return "", fmt.Errorf("write credentials: %w", err)
	}
	return path, nil
}

// MintAccessToken returns an opaque bearer token.
//
// It is deliberately not a JWT: an access token is opaque on Google too, and
// making it look verifiable would invite someone to try to verify it.
func (c *Credentials) MintAccessToken() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "cloudburrow-local-token"
	}
	return "cbl_" + base64.RawURLEncoding.EncodeToString(b[:])
}

// MintIDToken returns an RS256 JWT for the given audience.
//
// It is signed with the instance's own key, so it is internally consistent and
// structurally valid. It will **not** verify against Google's public
// certificates, because it was not issued by Google — see
// docs/compatibility.md.
func (c *Credentials) MintIDToken(audience string, now time.Time, ttl time.Duration) (string, error) {
	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": c.KeyID}
	claims := map[string]any{
		"iss":            "https://accounts.google.com",
		"aud":            audience,
		"sub":            "000000000000000000000",
		"email":          c.Email,
		"email_verified": true,
		"iat":            now.Unix(),
		"exp":            now.Add(ttl).Unix(),
		// Named so that anything logging the token makes its origin obvious.
		"cloudburrow": "local-development-token",
	}

	signingInput, err := joinSegments(header, claims)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.PrivateKey, cryptoSHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("sign id token: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func joinSegments(header, claims map[string]any) (string, error) {
	h, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("encode token header: %w", err)
	}
	c, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("encode token claims: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(h) + "." +
		base64.RawURLEncoding.EncodeToString(c), nil
}

// JWKS renders the public key as a JSON Web Key Set, so a client configured
// to fetch certificates from this instance gets a usable answer rather than
// reaching out to Google.
func (c *Credentials) JWKS() ([]byte, error) {
	pub := c.PrivateKey.PublicKey
	n := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(bigEndian(pub.E))
	return json.MarshalIndent(map[string]any{
		"keys": []map[string]string{{
			"kty": "RSA", "alg": "RS256", "use": "sig",
			"kid": c.KeyID, "n": n, "e": e,
		}},
	}, "", "  ")
}

func bigEndian(v int) []byte {
	var out []byte
	for v > 0 {
		out = append([]byte{byte(v & 0xff)}, out...)
		v >>= 8
	}
	if len(out) == 0 {
		return []byte{0}
	}
	return out
}
