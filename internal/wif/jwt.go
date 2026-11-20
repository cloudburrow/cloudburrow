package wif

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"crypto"
)

// MaxTokenAge is how long after its iat Google accepts an OIDC token: "no
// more than 24 hours in the past".
const MaxTokenAge = 24 * time.Hour

// JWK is one key of a JSON Web Key Set, as jwksJson and a jwks_uri carry
// it: RSA or EC (P-256), for signatures.
type JWK struct {
	Kty string `json:"kty"`
	Alg string `json:"alg,omitempty"`
	Use string `json:"use,omitempty"`
	Kid string `json:"kid,omitempty"`
	N   string `json:"n,omitempty"`
	E   string `json:"e,omitempty"`
	Crv string `json:"crv,omitempty"`
	X   string `json:"x,omitempty"`
	Y   string `json:"y,omitempty"`
}

// JWKS is a key set.
type JWKS struct {
	Keys []JWK `json:"keys"`
}

// ParseJWKS reads and checks a key set: every key RSA or EC, with the
// fields its type needs.
func ParseJWKS(b []byte) (JWKS, error) {
	var set JWKS
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&set); err != nil {
		return set, fmt.Errorf("The JWKS is not a valid JSON Web Key Set: %v", err)
	}
	if len(set.Keys) == 0 {
		return set, errors.New("The JWKS must contain at least one key")
	}
	for i, k := range set.Keys {
		if _, err := k.public(); err != nil {
			return set, fmt.Errorf("JWKS key %d: %v", i, err)
		}
	}
	return set, nil
}

func b64int(s string) (*big.Int, error) {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil || len(b) == 0 {
		return nil, errors.New("invalid base64url integer")
	}
	return new(big.Int).SetBytes(b), nil
}

func (k JWK) public() (crypto.PublicKey, error) {
	switch k.Kty {
	case "RSA":
		n, err := b64int(k.N)
		if err != nil {
			return nil, fmt.Errorf("RSA key n: %v", err)
		}
		e, err := b64int(k.E)
		if err != nil || !e.IsInt64() {
			return nil, errors.New("RSA key e is invalid")
		}
		return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
	case "EC":
		if k.Crv != "P-256" {
			return nil, fmt.Errorf("EC curve %q is not supported; use P-256", k.Crv)
		}
		x, err := b64int(k.X)
		if err != nil {
			return nil, fmt.Errorf("EC key x: %v", err)
		}
		y, err := b64int(k.Y)
		if err != nil {
			return nil, fmt.Errorf("EC key y: %v", err)
		}
		pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}
		if !pub.Curve.IsOnCurve(x, y) { //nolint:staticcheck // validating a JWK point
			return nil, errors.New("EC key is not on P-256")
		}
		return pub, nil
	}
	return nil, fmt.Errorf("key type %q is not supported; use RSA or EC", k.Kty)
}

// Token is a verified external OIDC token.
type Token struct {
	Header map[string]any
	Claims map[string]any
}

// ParseUnverified splits a compact JWT into its header and claims.
func ParseUnverified(tok string) (Token, []string, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return Token{}, nil, errors.New("Unable to parse the ID Token.")
	}
	var t Token
	for i, dst := range []*map[string]any{&t.Header, &t.Claims} {
		b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[i], "="))
		if err != nil {
			return Token{}, nil, errors.New("Unable to parse the ID Token.")
		}
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.UseNumber()
		if err := dec.Decode(dst); err != nil {
			return Token{}, nil, errors.New("Unable to parse the ID Token.")
		}
	}
	return t, parts, nil
}

// Verify checks the signature of tok against set: alg RS256 or ES256 and
// a kid the set has.
func Verify(tok string, set JWKS) (Token, error) {
	t, parts, err := ParseUnverified(tok)
	if err != nil {
		return t, err
	}
	alg, _ := t.Header["alg"].(string)
	kid, _ := t.Header["kid"].(string)
	if alg != "RS256" && alg != "ES256" {
		return t, fmt.Errorf("The ID Token's signing algorithm %q is not supported; it must be RS256 or ES256.", alg)
	}
	if kid == "" {
		return t, errors.New("The ID Token's header must include a kid.")
	}
	sig, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[2], "="))
	if err != nil {
		return t, errors.New("Invalid ID Token signature.")
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	for _, k := range set.Keys {
		if k.Kid != kid {
			continue
		}
		pub, err := k.public()
		if err != nil {
			continue
		}
		switch p := pub.(type) {
		case *rsa.PublicKey:
			if alg == "RS256" && rsa.VerifyPKCS1v15(p, crypto.SHA256, sum[:], sig) == nil {
				return t, nil
			}
		case *ecdsa.PublicKey:
			if alg == "ES256" && len(sig) == 64 &&
				ecdsa.Verify(p, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
				return t, nil
			}
		}
	}
	return t, fmt.Errorf("Invalid ID Token signature: no key in the provider's JWKS with kid %q verifies it.", kid)
}

// claimTime is a numeric date claim.
func claimTime(v any) (time.Time, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return time.Time{}, false
		}
		return time.Unix(int64(f), 0), true
	case float64:
		return time.Unix(int64(x), 0), true
	}
	return time.Time{}, false
}

// CheckTimes applies Google's iat and exp rules at now.
func (t Token) CheckTimes(now time.Time) error {
	exp, ok := claimTime(t.Claims["exp"])
	if !ok {
		return errors.New("The ID Token is missing the exp claim.")
	}
	iat, ok := claimTime(t.Claims["iat"])
	if !ok {
		return errors.New("The ID Token is missing the iat claim.")
	}
	if !exp.After(now) {
		return fmt.Errorf("ID Token expired at %d.", exp.Unix())
	}
	if iat.After(now.Add(time.Minute)) {
		return fmt.Errorf("ID Token issued at %d is in the future.", iat.Unix())
	}
	if now.Sub(iat) > MaxTokenAge {
		return fmt.Errorf("ID Token issued at %d is stale to sign-in.", iat.Unix())
	}
	return nil
}

// Audiences are the token's aud, a string or a list.
func (t Token) Audiences() []string {
	switch x := t.Claims["aud"].(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// Assertion is the claims as CEL's `assertion`: numbers as float64, as
// JSON's are.
func (t Token) Assertion() map[string]any {
	b, _ := json.Marshal(t.Claims)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}
