package storage

import (
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// A service account's public certificates (#1786). Google's Cloud Storage
// verifies a GOOG4-RSA-SHA256 or V2 signature against the keys of the
// account that signed it: its Google-managed keys and any user-managed
// ones, which www.googleapis.com/service_accounts/v1/metadata/x509/EMAIL
// publishes as a JSON object of key IDs to PEM certificates
// (docs.cloud.google.com/storage/docs/access-control/signed-urls, and
// iam/docs/service-account-creds). Here an account whose key was not
// registered (SigningKeys) is looked up at the certificate source
// Options.CertSources names for its email's domain, such as the BigQuery
// front for the connections' service agents
// (gcp-sa-bigquery-condel.iam.gserviceaccount.com), on every request, so
// a key that is gone verifies nothing. A source that does not answer, or
// an account it does not know, verifies nothing: the URL is refused.

// certSourceTimeout bounds one read of a certificate source.
const certSourceTimeout = 10 * time.Second

// rsaKeys are the public keys a signature of accessID may verify against,
// or the refusal.
func (s *Server) rsaKeys(accessID string) ([]*rsa.PublicKey, error) {
	if k, ok := s.signingKeys[accessID]; ok {
		return []*rsa.PublicKey{k}, nil
	}
	_, domain, _ := strings.Cut(accessID, "@")
	base, ok := s.certSources[strings.ToLower(domain)]
	if !ok || domain == "" {
		return nil, denied("No signing certificate is registered for %s; register one (storage-server --signing-cert) to verify its signatures.", accessID)
	}
	keys, err := fetchCerts(s.certClient, base, accessID)
	if err != nil {
		return nil, denied("The signing certificates of %s could not be read: %v", accessID, err)
	}
	if len(keys) == 0 {
		return nil, denied("%s has no signing certificate.", accessID)
	}
	return keys, nil
}

func fetchCerts(c *http.Client, base, email string) ([]*rsa.PublicKey, error) {
	if c == nil {
		c = &http.Client{Timeout: certSourceTimeout}
	}
	u := strings.TrimRight(base, "/") + "/service_accounts/v1/metadata/x509/" + url.PathEscape(email)
	resp, err := c.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var m map[string]string
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&m); err != nil {
		return nil, err
	}
	var out []*rsa.PublicKey
	for _, pemText := range m {
		if k, err := ParseSigningKey([]byte(pemText)); err == nil {
			out = append(out, k)
		}
	}
	return out, nil
}
