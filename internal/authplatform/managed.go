package authplatform

import "sync"

// Google-managed OAuth clients (#2160): a client a Google service owns
// rather than a project, such as the one Identity-Aware Proxy signs users
// in with when a backend service names no OAuth client of its own
// (https://docs.cloud.google.com/iap/docs/custom-oauth-configuration, read
// 2026-11-22: "Google-managed OAuth clients can only be used to manage
// access for internal users", under a Google Cloud brand). It is in no
// project's list of clients, and its owner, in this process, redeems its
// codes with Redeem.

type managedClient struct {
	c *Client
	b *Brand
}

var managedMu sync.Mutex

// RegisterManaged adds (or replaces) a Google-managed client, which the
// authorization endpoint then accepts as it accepts a project's.
func (s *Store) RegisterManaged(c Client, b Brand) {
	managedMu.Lock()
	defer managedMu.Unlock()
	if s.managed == nil {
		s.managed = map[string]managedClient{}
	}
	s.managed[c.ClientID] = managedClient{c: &c, b: &b}
}

// managedClient is a Google-managed client, nil when clientID is none.
func (s *Store) managedOf(clientID string) *managedClient {
	managedMu.Lock()
	defer managedMu.Unlock()
	if m, ok := s.managed[clientID]; ok {
		return &m
	}
	return nil
}

// Redeem exchanges an authorization code for the confidential client
// clientID, on behalf of the service in this process that owns the client
// and holds its secret as its SHA-256 (hex, as Compute Engine keeps a
// backend service's iap.oauth2ClientSecretSha256): a Google-managed
// client's secret is its owner's, and is not checked.
func (s *Store) Redeem(clientID, secretSHA256, code, redirectURI string) (*Grant, *Error) {
	_, _, c, err := s.find(clientID)
	if err != nil {
		return nil, errorf(500, "server_error", "%v", err)
	}
	if c == nil {
		return nil, errorf(401, "invalid_client", "The OAuth client was not found.")
	}
	if s.managedOf(clientID) == nil && HasSecret(c.Type) {
		ok := false
		for _, sec := range c.Secrets {
			if sec.Enabled && sec.Hash == secretSHA256 {
				ok = true
			}
		}
		if !ok {
			return nil, errorf(401, "invalid_client", "Unauthorized")
		}
	}
	s.mu.Lock()
	g := s.codes[code]
	delete(s.codes, code)
	s.mu.Unlock()
	if g == nil || g.clientID != c.ClientID || s.now().After(g.expires) {
		return nil, errorf(400, "invalid_grant", "Malformed auth code.")
	}
	if g.redirectURI != redirectURI {
		return nil, errorf(400, "redirect_uri_mismatch", "Bad Request")
	}
	return &Grant{Email: g.email, Scope: g.scope, ClientID: c.ClientID}, nil
}
