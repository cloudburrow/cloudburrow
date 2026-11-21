package wif_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/wif"
	"github.com/cloudburrow/cloudburrow/internal/wif/samltest"
)

const testAudience = "https://iam.googleapis.com/locations/global/workforcePools/pool-1/providers/saml-1"

func TestVerifySAML(t *testing.T) {
	idp, err := samltest.NewIdP("https://idp.example.test/metadata")
	if err != nil {
		t.Fatal(err)
	}
	md, err := wif.ParseSAMLMetadata(idp.Metadata())
	if err != nil || md.EntityID != idp.EntityID || len(md.Certs) != 1 {
		t.Fatalf("metadata: %+v %v", md, err)
	}
	other, _ := samltest.NewIdP(idp.EntityID)
	want := wif.SAMLExpect{Audience: testAudience, Recipients: []string{"https://auth.cloud.google/signin-callback/locations/global/workforcePools/pool-1/providers/saml-1"}}
	base := samltest.Options{Audience: testAudience, NameID: "alice", Recipient: want.Recipients[0],
		Attributes: map[string][]string{"groups": {"eng", "ops"}}, SignAssertion: true}
	now := time.Now()

	ok := func(name string, o samltest.Options, i *samltest.IdP) wif.SAMLAssertion {
		t.Helper()
		tok, err := i.Response(o)
		if err != nil {
			t.Fatal(err)
		}
		a, err := wif.VerifySAML(tok, md, nil, want, now)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return a
	}
	a := ok("assertion signed", base, idp)
	if a.Subject != "alice" || len(a.Attributes["groups"]) != 2 {
		t.Fatalf("assertion: %+v", a)
	}
	o := base
	o.SignAssertion, o.SignResponse = false, true
	ok("response signed", o, idp)
	o.SignAssertion = true
	sess := now.Add(20 * time.Minute).Truncate(time.Second)
	o.SessionNotOnOrAfter = sess
	if a := ok("both signed", o, idp); !a.SessionNotOnOrAfter.Equal(sess) {
		t.Fatalf("session: %v", a.SessionNotOnOrAfter)
	}
	attrs, err := wif.ApplyWorkforce(map[string]string{"google.subject": "assertion.subject", "google.groups": "assertion.attributes['groups']"}, "", a.Map())
	if err != nil || attrs.Subject != "alice" || len(attrs.Groups) != 2 {
		t.Fatalf("mapping: %+v %v", attrs, err)
	}

	bad := func(name, wantErr string, tok string) {
		t.Helper()
		_, err := wif.VerifySAML(tok, md, nil, want, now)
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Fatalf("%s: got %v, want %q", name, err, wantErr)
		}
	}
	tokOf := func(o samltest.Options, i *samltest.IdP) string {
		s, err := i.Response(o)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	o = base
	o.SignAssertion = false
	bad("unsigned", "must be signed", tokOf(o, idp))
	bad("other key", "signature is invalid", tokOf(base, other))
	x, _ := idp.ResponseXML(base)
	tampered := strings.Replace(x, ">alice<", ">mallory<", 1)
	bad("tampered", "signature is invalid", base64.StdEncoding.EncodeToString([]byte(tampered)))
	o = base
	o.IssueInstant, o.NotOnOrAfter = now.Add(-30*time.Minute), now.Add(-time.Minute)
	bad("expired", "expired", tokOf(o, idp))
	o = base
	o.Audience = "https://iam.googleapis.com/locations/global/workforcePools/pool-1/providers/other"
	bad("audience", "does not match the expected audience", tokOf(o, idp))
	o = base
	o.Recipient = "https://elsewhere.example.test/acs"
	bad("recipient", "Recipient", tokOf(o, idp))
	o = base
	o.Issuer = "https://other.example.test"
	bad("issuer", "Issuer", tokOf(o, idp))
	o = base
	o.SessionNotOnOrAfter = now.Add(-time.Minute)
	bad("session", "session has expired", tokOf(o, idp))
	bad("garbage", "base64", "%%%")

	// An assertion encrypted to a provider key.
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "k"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	cert, _ := x509.ParseCertificate(der)
	o = base
	o.EncryptTo = cert
	tok := tokOf(o, idp)
	if a, err := wif.VerifySAML(tok, md, []*rsa.PrivateKey{key}, want, now); err != nil || a.Subject != "alice" {
		t.Fatalf("encrypted: %+v %v", a, err)
	}
	other2, _ := rsa.GenerateKey(rand.Reader, 2048)
	if _, err := wif.VerifySAML(tok, md, []*rsa.PrivateKey{other2}, want, now); err == nil || !strings.Contains(err.Error(), "decrypt") {
		t.Fatalf("encrypted, wrong key: %v", err)
	}
}
