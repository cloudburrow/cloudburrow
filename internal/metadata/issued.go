package metadata

import (
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Issued access tokens (#1614). An access token stays opaque, as Google's
// is, but the instance remembers each one it mints, by its SHA-256 only,
// with the account it was minted for and when it expires, so a service
// that must check a token CloudBurrow issued can: Memorystore's IAM auth
// (redisadmin's iamauth.go) gives its nodes the hashes of the live tokens
// of the principals granted the connect role, and a client's AUTH with any
// other string is refused, as Google refuses a token it did not issue.
//
// The log is kept in IssuedFileName beside the key, so a token a client
// cached keeps working across `stop` and `up` until it expires, as a Google
// token outlives a restart of the service it is sent to. Nothing else
// authenticates with it: every other adapter still serves any caller
// (ADR-0004).

// IssuedFileName is the log of minted access tokens' hashes.
const IssuedFileName = "issued-tokens.json"

// IssuedToken is one minted access token, as the log keeps it.
type IssuedToken struct {
	// Hash is the token's SHA-256, in hex: what a Valkey ACL password
	// given as #<hash> is.
	Hash string `json:"hash"`
	// Email is the account it was minted for; "" when the request named
	// none CloudBurrow could check (a refresh token, an assertion not
	// signed by this instance's key).
	Email   string    `json:"email,omitempty"`
	Expires time.Time `json:"expires"`
}

// issuedLog is the instance's minted tokens.
type issuedLog struct {
	mu     sync.Mutex
	path   string
	tokens map[string]IssuedToken
	hooks  []func(IssuedToken)
}

func newIssuedLog(dir string) *issuedLog {
	l := &issuedLog{tokens: map[string]IssuedToken{}}
	if dir == "" {
		return l
	}
	l.path = filepath.Join(dir, IssuedFileName)
	if b, err := os.ReadFile(l.path); err == nil {
		var list []IssuedToken
		if json.Unmarshal(b, &list) == nil {
			now := time.Now()
			for _, t := range list {
				if t.Expires.After(now) {
					l.tokens[t.Hash] = t
				}
			}
		}
	}
	return l
}

// TokenHash is a token's SHA-256 in hex.
func TokenHash(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// issued returns c's log, made on first use for credentials built
// without LoadOrCreate.
func (c *Credentials) issued() *issuedLog {
	c.issuedOnce.Do(func() {
		if c.log == nil {
			c.log = newIssuedLog("")
		}
	})
	return c.log
}

// record keeps a minted token and tells every hook, before the token is
// handed out, so a service that checks it already knows it when the
// client first sends it.
func (l *issuedLog) record(tok, email string, ttl time.Duration) {
	it := IssuedToken{Hash: TokenHash(tok), Email: email, Expires: time.Now().Add(ttl)}
	l.mu.Lock()
	now := time.Now()
	for h, t := range l.tokens {
		if !t.Expires.After(now) {
			delete(l.tokens, h)
		}
	}
	l.tokens[it.Hash] = it
	if l.path != "" {
		list := make([]IssuedToken, 0, len(l.tokens))
		for _, t := range l.tokens {
			list = append(list, t)
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Hash < list[j].Hash })
		if b, err := json.Marshal(list); err == nil {
			// Hashes only: the file lets nobody recover a token.
			_ = os.WriteFile(l.path, b, 0o600)
		}
	}
	hooks := append([]func(IssuedToken){}, l.hooks...)
	l.mu.Unlock()
	for _, h := range hooks {
		h(it)
	}
}

// LiveTokens are the minted access tokens that have not expired at now.
func (c *Credentials) LiveTokens(now time.Time) []IssuedToken {
	l := c.issued()
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]IssuedToken, 0, len(l.tokens))
	for _, t := range l.tokens {
		if t.Expires.After(now) {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hash < out[j].Hash })
	return out
}

// OnIssue calls f with every access token minted from now on, before it is
// returned to the client that asked for it.
func (c *Credentials) OnIssue(f func(IssuedToken)) {
	l := c.issued()
	l.mu.Lock()
	l.hooks = append(l.hooks, f)
	l.mu.Unlock()
}

// MintAccessTokenFor returns an opaque bearer token for email, valid for
// ttl, and records it.
func (c *Credentials) MintAccessTokenFor(email string, ttl time.Duration) string {
	tok := c.MintAccessToken()
	c.issued().record(tok, email, ttl)
	return tok
}

// assertionAccount is the account a JWT-bearer assertion names, when it is
// signed with this instance's key: its iss, the client_email of the ADC
// fixture that signed it. "" for anything else.
func (c *Credentials) assertionAccount(assertion string) string {
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		return ""
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return ""
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(&c.PrivateKey.PublicKey, cryptoSHA256, sum[:], sig) != nil {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Iss string `json:"iss"`
		Sub string `json:"sub"`
		Exp int64  `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || (claims.Exp != 0 && time.Unix(claims.Exp, 0).Before(time.Now())) {
		return ""
	}
	if claims.Sub != "" && strings.Contains(claims.Sub, "@") {
		// A domain-wide delegation's subject is the account acted as.
		return claims.Sub
	}
	return claims.Iss
}
