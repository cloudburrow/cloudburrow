package wif

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"
)

func TestExtract(t *testing.T) {
	arn := "arn:aws:sts::123456789012:assumed-role/my-role/session"
	for tmpl, want := range map[string]string{
		"{account_arn}assumed-role/": "arn:aws:sts::123456789012:",
		"assumed-role/{role_name}/":  "my-role",
		"session{x}":                 "",
		"nothere/{x}/":               "",
	} {
		if got := Extract(arn, tmpl); got != want {
			t.Errorf("Extract(%q) = %q; want %q", tmpl, got, want)
		}
	}
}

func TestMappingAndCondition(t *testing.T) {
	m := map[string]string{
		"google.subject":    "assertion.sub",
		"google.groups":     "assertion.groups",
		"attribute.repo":    "assertion.repo",
		"attribute.missing": "assertion.nope",
	}
	if err := ValidateMapping(m, true); err != nil {
		t.Fatal(err)
	}
	a := map[string]any{"sub": "ci-1", "groups": []any{"admins", "dev"}, "repo": "org/app"}
	got, err := Apply(m, "'admins' in google.groups && attribute.repo == 'org/app'", a)
	if err != nil {
		t.Fatal(err)
	}
	if got.Subject != "ci-1" || len(got.Groups) != 2 || got.Custom["repo"][0] != "org/app" {
		t.Fatalf("Apply = %+v", got)
	}
	if _, err := Apply(m, "attribute.repo == 'other'", a); err != ErrCondition {
		t.Fatalf("refusing condition = %v; want ErrCondition", err)
	}
	sets := Sets("42", "pool", got)
	want := []string{
		"principalSet://iam.googleapis.com/projects/42/locations/global/workloadIdentityPools/pool/*",
		"principalSet://iam.googleapis.com/projects/42/locations/global/workloadIdentityPools/pool/group/admins",
		"principalSet://iam.googleapis.com/projects/42/locations/global/workloadIdentityPools/pool/group/dev",
		"principalSet://iam.googleapis.com/projects/42/locations/global/workloadIdentityPools/pool/attribute.repo/org/app",
	}
	if strings.Join(sets, "\n") != strings.Join(want, "\n") {
		t.Fatalf("Sets = %v", sets)
	}
	for _, bad := range []map[string]string{
		{"attribute.Bad": "assertion.x", "google.subject": "assertion.sub"},
		{"other.key": "assertion.sub"},
		{"attribute.x": "assertion.x"},
		{"google.subject": "assertion.sub +"},
	} {
		if ValidateMapping(bad, true) == nil {
			t.Errorf("ValidateMapping(%v) accepted", bad)
		}
	}
	if ValidateCondition("assertion.sub") == nil {
		// Dyn is allowed; a literal non-bool is not.
	}
	if ValidateCondition("'a'") == nil {
		t.Error("a string condition was accepted")
	}
	n, p, ok := PoolOfPrincipal(Principal("42", "pool", "ci-1"))
	if !ok || n != "42" || p != "pool" {
		t.Fatalf("PoolOfPrincipal = %q %q %v", n, p, ok)
	}
	if _, _, ok := PoolOfPrincipal(strings.ToLower(sets[0])); !ok {
		t.Fatal("PoolOfPrincipal of a lowercased principalSet failed")
	}
}

func seg(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

func TestVerifyRS256AndES256(t *testing.T) {
	now := time.Now()
	claims := map[string]any{"iss": "https://idp", "sub": "s", "aud": "a", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}
	rk, _ := rsa.GenerateKey(rand.Reader, 2048)
	in := seg(map[string]any{"alg": "RS256", "kid": "r1"}) + "." + seg(claims)
	sum := sha256.Sum256([]byte(in))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, rk, crypto.SHA256, sum[:])
	rsTok := in + "." + base64.RawURLEncoding.EncodeToString(sig)

	ek, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	in2 := seg(map[string]any{"alg": "ES256", "kid": "e1"}) + "." + seg(claims)
	sum2 := sha256.Sum256([]byte(in2))
	r, s, _ := ecdsa.Sign(rand.Reader, ek, sum2[:])
	es := make([]byte, 64)
	r.FillBytes(es[:32])
	s.FillBytes(es[32:])
	esTok := in2 + "." + base64.RawURLEncoding.EncodeToString(es)

	b64 := func(i *big.Int) string { return base64.RawURLEncoding.EncodeToString(i.Bytes()) }
	set := JWKS{Keys: []JWK{
		{Kty: "RSA", Kid: "r1", Alg: "RS256", Use: "sig", N: b64(rk.N), E: b64(big.NewInt(int64(rk.E)))},
		{Kty: "EC", Kid: "e1", Crv: "P-256", X: b64(ek.X), Y: b64(ek.Y)},
	}}
	raw, _ := json.Marshal(set)
	parsed, err := ParseJWKS(raw)
	if err != nil {
		t.Fatal(err)
	}
	for name, tok := range map[string]string{"RS256": rsTok, "ES256": esTok} {
		v, err := Verify(tok, parsed)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := v.CheckTimes(now); err != nil {
			t.Fatalf("%s times: %v", name, err)
		}
	}
	if _, err := Verify(rsTok[:len(rsTok)-4]+"AAAA", parsed); err == nil {
		t.Fatal("a bad signature verified")
	}
	stale := Token{Claims: map[string]any{"iat": float64(now.Add(-25 * time.Hour).Unix()), "exp": float64(now.Add(time.Hour).Unix())}}
	if stale.CheckTimes(now) == nil {
		t.Fatal("a token issued 25 hours ago was accepted")
	}
}
