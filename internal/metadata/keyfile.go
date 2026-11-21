package metadata

import (
	"encoding/json"
	"strings"
	"time"
)

// KeyFileAccount is the account a service account key file names, when
// the key is one this instance holds and may be used (#2111): the ADC
// fixture's own key, or a live, enabled key of a live account in the
// directory whose private key the file carries. The Docker registry's
// _json_key and _json_key_base64 logins present a key file this way, as
// Artifact Registry documents.
func (c *Credentials) KeyFileAccount(keyJSON []byte, now time.Time) (string, bool) {
	var f struct {
		Type         string `json:"type"`
		PrivateKey   string `json:"private_key"`
		PrivateKeyID string `json:"private_key_id"`
		ClientEmail  string `json:"client_email"`
	}
	if json.Unmarshal(keyJSON, &f) != nil || f.Type != "service_account" || f.ClientEmail == "" {
		return "", false
	}
	key, err := parseKey([]byte(f.PrivateKey))
	if err != nil {
		return "", false
	}
	if c.PrivateKey != nil && key.PublicKey.Equal(&c.PrivateKey.PublicKey) {
		return c.Email, strings.EqualFold(f.ClientEmail, c.Email)
	}
	d := c.directory()
	if d == nil || f.PrivateKeyID == "" {
		return "", false
	}
	pub, email, known, usable := d.Key(f.PrivateKeyID, now)
	if !known || !usable || !pub.Equal(&key.PublicKey) || !strings.EqualFold(email, f.ClientEmail) {
		return "", false
	}
	return email, true
}
