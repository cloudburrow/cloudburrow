// Package samltest is a throwaway SAML 2.0 identity provider for tests
// (#2065): a self-signed RSA signing key generated in memory, its IdP
// metadata document, and signed (and optionally encrypted) SAML responses
// the Security Token Service exchanges. Nothing here reaches a network.
package samltest

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1" //nolint:gosec // xmlenc's rsa-oaep-mgf1p is SHA-1
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/beevik/etree"
	dsig "github.com/russellhaering/goxmldsig"
)

const (
	assertionNS = "urn:oasis:names:tc:SAML:2.0:assertion"
	protocolNS  = "urn:oasis:names:tc:SAML:2.0:protocol"
	xencNS      = "http://www.w3.org/2001/04/xmlenc#"
	xenc11NS    = "http://www.w3.org/2009/xmlenc11#"
)

// IdP is a test identity provider.
type IdP struct {
	EntityID string
	Key      *rsa.PrivateKey
	CertDER  []byte
}

// NewIdP makes an IdP with a fresh 2048-bit key, its certificate valid
// for a day.
func NewIdP(entityID string) (*IdP, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	now := time.Now().Add(-time.Hour)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(now.UnixNano()), Subject: pkix.Name{CommonName: "cloudburrow-test-idp"},
		NotBefore: now, NotAfter: now.Add(25 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	return &IdP{EntityID: entityID, Key: key, CertDER: der}, nil
}

// Metadata is the IdP's SAML 2.0 metadata document.
func (i *IdP) Metadata() string {
	return fmt.Sprintf(`<?xml version="1.0"?>
<md:EntityDescriptor xmlns:md="urn:oasis:names:tc:SAML:2.0:metadata" entityID="%s">
  <md:IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol">
    <md:KeyDescriptor use="signing">
      <ds:KeyInfo xmlns:ds="http://www.w3.org/2000/09/xmldsig#"><ds:X509Data><ds:X509Certificate>%s</ds:X509Certificate></ds:X509Data></ds:KeyInfo>
    </md:KeyDescriptor>
    <md:SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" Location="https://idp.example.test/sso"/>
  </md:IDPSSODescriptor>
</md:EntityDescriptor>`, i.EntityID, base64.StdEncoding.EncodeToString(i.CertDER))
}

// Options shape one response; zero times take sensible defaults
// (now, now+10m, now+1h).
type Options struct {
	Audience            string
	Recipient           string
	NameID              string
	Attributes          map[string][]string
	IssueInstant        time.Time
	NotOnOrAfter        time.Time
	SessionNotOnOrAfter time.Time
	// Issuer overrides the IdP's entity ID.
	Issuer string
	// SignResponse signs the Response; SignAssertion the Assertion.
	SignResponse, SignAssertion bool
	// EncryptTo, a PEM provider key certificate's parsed form, encrypts
	// the assertion (RSA-OAEP, AES-256-GCM).
	EncryptTo *x509.Certificate
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "_" + hex.EncodeToString(b)
}

func ts(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

func (i *IdP) signer() *dsig.SigningContext {
	ctx := dsig.NewDefaultSigningContext(dsig.TLSCertKeyStore{PrivateKey: i.Key, Certificate: [][]byte{i.CertDER}})
	_ = ctx.SetSignatureMethod(dsig.RSASHA256SignatureMethod)
	ctx.Canonicalizer = dsig.MakeC14N10ExclusiveCanonicalizerWithPrefixList("")
	return ctx
}

// ResponseXML is a SAML Response as XML.
func (i *IdP) ResponseXML(o Options) (string, error) {
	now := time.Now()
	if o.IssueInstant.IsZero() {
		o.IssueInstant = now
	}
	if o.NotOnOrAfter.IsZero() {
		o.NotOnOrAfter = now.Add(10 * time.Minute)
	}
	issuer := i.EntityID
	if o.Issuer != "" {
		issuer = o.Issuer
	}
	a := etree.NewElement("saml:Assertion")
	a.CreateAttr("xmlns:saml", assertionNS)
	a.CreateAttr("ID", newID())
	a.CreateAttr("Version", "2.0")
	a.CreateAttr("IssueInstant", ts(o.IssueInstant))
	a.CreateElement("saml:Issuer").SetText(issuer)
	subj := a.CreateElement("saml:Subject")
	nid := subj.CreateElement("saml:NameID")
	nid.CreateAttr("Format", "urn:oasis:names:tc:SAML:1.1:nameid-format:unspecified")
	nid.SetText(o.NameID)
	sc := subj.CreateElement("saml:SubjectConfirmation")
	sc.CreateAttr("Method", "urn:oasis:names:tc:SAML:2.0:cm:bearer")
	scd := sc.CreateElement("saml:SubjectConfirmationData")
	scd.CreateAttr("NotOnOrAfter", ts(o.NotOnOrAfter))
	if o.Recipient != "" {
		scd.CreateAttr("Recipient", o.Recipient)
	}
	cond := a.CreateElement("saml:Conditions")
	cond.CreateAttr("NotBefore", ts(o.IssueInstant.Add(-time.Minute)))
	cond.CreateAttr("NotOnOrAfter", ts(o.NotOnOrAfter))
	cond.CreateElement("saml:AudienceRestriction").CreateElement("saml:Audience").SetText(o.Audience)
	authn := a.CreateElement("saml:AuthnStatement")
	authn.CreateAttr("AuthnInstant", ts(o.IssueInstant))
	if !o.SessionNotOnOrAfter.IsZero() {
		authn.CreateAttr("SessionNotOnOrAfter", ts(o.SessionNotOnOrAfter))
	}
	authn.CreateElement("saml:AuthnContext").CreateElement("saml:AuthnContextClassRef").SetText("urn:oasis:names:tc:SAML:2.0:ac:classes:PasswordProtectedTransport")
	if len(o.Attributes) > 0 {
		st := a.CreateElement("saml:AttributeStatement")
		names := make([]string, 0, len(o.Attributes))
		for n := range o.Attributes {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			at := st.CreateElement("saml:Attribute")
			at.CreateAttr("Name", n)
			for _, v := range o.Attributes[n] {
				at.CreateElement("saml:AttributeValue").SetText(v)
			}
		}
	}
	if o.SignAssertion {
		signed, err := i.signer().SignEnveloped(a)
		if err != nil {
			return "", err
		}
		a = signed
	}
	resp := etree.NewElement("samlp:Response")
	resp.CreateAttr("xmlns:samlp", protocolNS)
	resp.CreateAttr("xmlns:saml", assertionNS)
	resp.CreateAttr("ID", newID())
	resp.CreateAttr("Version", "2.0")
	resp.CreateAttr("IssueInstant", ts(o.IssueInstant))
	if o.Recipient != "" {
		resp.CreateAttr("Destination", o.Recipient)
	}
	resp.CreateElement("saml:Issuer").SetText(issuer)
	resp.CreateElement("samlp:Status").CreateElement("samlp:StatusCode").CreateAttr("Value", "urn:oasis:names:tc:SAML:2.0:status:Success")
	if o.EncryptTo != nil {
		ea, err := encrypt(a, o.EncryptTo)
		if err != nil {
			return "", err
		}
		resp.AddChild(ea)
	} else {
		resp.AddChild(a)
	}
	if o.SignResponse {
		signed, err := i.signer().SignEnveloped(resp)
		if err != nil {
			return "", err
		}
		resp = signed
	}
	doc := etree.NewDocument()
	doc.SetRoot(resp)
	return doc.WriteToString()
}

// Response is a SAML Response, base64-encoded as the STS takes it.
func (i *IdP) Response(o Options) (string, error) {
	x, err := i.ResponseXML(o)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString([]byte(x)), nil
}

// encrypt wraps an assertion in an EncryptedAssertion for cert's key.
func encrypt(a *etree.Element, cert *x509.Certificate) (*etree.Element, error) {
	pub, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("the certificate's key is not RSA")
	}
	doc := etree.NewDocument()
	doc.SetRoot(a.Copy())
	plain, err := doc.WriteToBytes()
	if err != nil {
		return nil, err
	}
	key := make([]byte, 32)
	nonce := make([]byte, 12)
	_, _ = rand.Read(key)
	_, _ = rand.Read(nonce)
	b, _ := aes.NewCipher(key)
	g, _ := cipher.NewGCM(b)
	ct := append(nonce, g.Seal(nil, nonce, plain, nil)...)
	wrapped, err := rsa.EncryptOAEP(sha1.New(), rand.Reader, pub, key, nil)
	if err != nil {
		return nil, err
	}
	ea := etree.NewElement("saml:EncryptedAssertion")
	ea.CreateAttr("xmlns:saml", assertionNS)
	ed := ea.CreateElement("xenc:EncryptedData")
	ed.CreateAttr("xmlns:xenc", xencNS)
	ed.CreateAttr("Type", xencNS+"Element")
	ed.CreateElement("xenc:EncryptionMethod").CreateAttr("Algorithm", xenc11NS+"aes256-gcm")
	ki := ed.CreateElement("ds:KeyInfo")
	ki.CreateAttr("xmlns:ds", "http://www.w3.org/2000/09/xmldsig#")
	ek := ki.CreateElement("xenc:EncryptedKey")
	ek.CreateElement("xenc:EncryptionMethod").CreateAttr("Algorithm", xencNS+"rsa-oaep-mgf1p")
	ek.CreateElement("xenc:CipherData").CreateElement("xenc:CipherValue").SetText(base64.StdEncoding.EncodeToString(wrapped))
	ed.CreateElement("xenc:CipherData").CreateElement("xenc:CipherValue").SetText(base64.StdEncoding.EncodeToString(ct))
	return ea, nil
}
