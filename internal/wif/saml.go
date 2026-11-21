package wif

// SAML 2.0 assertions for the Security Token Service (#2065): a workload or
// workforce pool's SAML provider trusts the signing certificates of its
// IdP's metadata (idpMetadataXml). Behaviour follows Google's pages
// (docs.cloud.google.com/iam/docs/workload-identity-federation-with-other-providers,
// "Prepare your SAML IdP"; .../iam/docs/workforce-obtaining-short-lived-credentials;
// .../iam/docs/workforce-sign-in-microsoft-entra-id, read 2026-11-20):
//
//   - the subject token is a base64-encoded SAML response or assertion;
//   - either the assertion, the response, or both are signed, by one of
//     the metadata's signing certificates (XML-DSig with exclusive
//     canonicalization, verified by github.com/russellhaering/goxmldsig);
//     only the signed element's verified copy is read afterwards, so a
//     wrapped, unsigned assertion is never mapped;
//   - a response holds exactly one assertion, its IssueInstant less than an
//     hour in the past and its StatusCode Success;
//   - the assertion's Issuer is the metadata's entity ID (Format omitted or
//     ...:nameid-format:entity); its Subject has a NameID and exactly one
//     bearer SubjectConfirmation whose SubjectConfirmationData has a future
//     NotOnOrAfter and no NotBefore (and, for a workforce provider, a
//     Recipient, when present, of the provider's sign-in callback);
//     Conditions' NotBefore is omitted or past, NotOnOrAfter omitted or
//     future, and an Audience is the provider's https://iam.googleapis.com/
//     name; there is an AuthnStatement, whose SessionNotOnOrAfter, when set,
//     is in the future and bounds the token's lifetime;
//   - an EncryptedAssertion is decrypted with one of the provider's
//     encryption keys (RSA-OAEP key transport, AES-CBC or AES-GCM data);
//   - mappings read assertion.subject (the NameID) and
//     assertion.attributes['NAME'] (a list of the Attribute's values).

import (
	"bytes"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rsa"
	_ "crypto/sha1" // RSA-OAEP's SHA-1 is xmlenc's default digest
	_ "crypto/sha256"
	_ "crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/beevik/etree"
	dsig "github.com/russellhaering/goxmldsig"
	"github.com/russellhaering/goxmldsig/etreeutils"
)

// SAML namespaces and values.
const (
	samlAssertionNS = "urn:oasis:names:tc:SAML:2.0:assertion"
	samlProtocolNS  = "urn:oasis:names:tc:SAML:2.0:protocol"
	samlSuccess     = "urn:oasis:names:tc:SAML:2.0:status:Success"
	samlBearer      = "urn:oasis:names:tc:SAML:2.0:cm:bearer"
	samlEntityFmt   = "urn:oasis:names:tc:SAML:2.0:nameid-format:entity"
	dsigNS          = "http://www.w3.org/2000/09/xmldsig#"
	xencNS          = "http://www.w3.org/2001/04/xmlenc#"
	xenc11NS        = "http://www.w3.org/2009/xmlenc11#"
	// MaxSAMLToken is the largest subject token read.
	MaxSAMLToken = 1 << 20
)

// SAMLExpect is what an assertion must name for a provider.
type SAMLExpect struct {
	// Audience is the provider's https://iam.googleapis.com/... name.
	Audience string
	// Recipients, when set, are the values a SubjectConfirmationData's
	// Recipient may take.
	Recipients []string
}

// SAMLAssertion is a verified assertion, as the mapping reads it.
type SAMLAssertion struct {
	Subject    string
	Attributes map[string][]string
	// SessionNotOnOrAfter is the AuthnStatement's, zero when unset.
	SessionNotOnOrAfter time.Time
}

// Map is the assertion as CEL's `assertion`.
func (a SAMLAssertion) Map() map[string]any {
	attrs := make(map[string]any, len(a.Attributes))
	for k, vs := range a.Attributes {
		l := make([]any, len(vs))
		for i, v := range vs {
			l[i] = v
		}
		attrs[k] = l
	}
	return map[string]any{"subject": a.Subject, "attributes": attrs}
}

// SAMLMetadata is what a provider's idpMetadataXml gives the exchange.
type SAMLMetadata struct {
	EntityID string
	Certs    []*x509.Certificate
}

// ParseSAMLMetadata reads an IdP metadata document's entity ID and
// signing certificates.
func ParseSAMLMetadata(doc string) (SAMLMetadata, error) {
	var md struct {
		EntityID string `xml:"entityID,attr"`
		IDP      struct {
			Keys []struct {
				Use   string   `xml:"use,attr"`
				Certs []string `xml:"KeyInfo>X509Data>X509Certificate"`
			} `xml:"KeyDescriptor"`
		} `xml:"IDPSSODescriptor"`
	}
	if err := xml.Unmarshal([]byte(doc), &md); err != nil {
		return SAMLMetadata{}, fmt.Errorf("The provider's IdP metadata is not valid: %v", err)
	}
	out := SAMLMetadata{EntityID: md.EntityID}
	for _, k := range md.IDP.Keys {
		if k.Use != "" && k.Use != "signing" {
			continue
		}
		for _, c := range k.Certs {
			der, err := base64.StdEncoding.DecodeString(stripSpace(c))
			if err != nil {
				continue
			}
			if cert, err := x509.ParseCertificate(der); err == nil {
				out.Certs = append(out.Certs, cert)
			}
		}
	}
	return out, nil
}

func stripSpace(s string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
			return -1
		}
		return r
	}, s)
}

// decodeSAMLToken is the subject token's XML: base64 (standard or URL,
// padded or not), or the XML itself.
func decodeSAMLToken(token string) ([]byte, error) {
	t := strings.TrimSpace(token)
	if len(t) > MaxSAMLToken {
		return nil, fmt.Errorf("The SAML assertion exceeds %d bytes.", MaxSAMLToken)
	}
	if strings.HasPrefix(t, "<") {
		return []byte(t), nil
	}
	t = stripSpace(t)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(t); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("Unable to parse the SAML assertion: the subject token must be a base64-encoded SAML response or assertion.")
}

// child is el's first child element named tag in namespace ns.
func child(el *etree.Element, ns, tag string) *etree.Element {
	if el == nil {
		return nil
	}
	for _, c := range el.ChildElements() {
		if c.Tag == tag && c.NamespaceURI() == ns {
			return c
		}
	}
	return nil
}

func children(el *etree.Element, ns, tag string) []*etree.Element {
	var out []*etree.Element
	if el == nil {
		return out
	}
	for _, c := range el.ChildElements() {
		if c.Tag == tag && c.NamespaceURI() == ns {
			out = append(out, c)
		}
	}
	return out
}

func parseSAMLTime(s string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, strings.TrimSpace(s))
}

// verifySigned validates el's enveloped signature with one of certs and
// returns the verified copy, self-contained in its namespaces.
func verifySigned(el *etree.Element, certs []*x509.Certificate, now time.Time) (*etree.Element, error) {
	ctx, err := etreeutils.NSBuildParentContext(el)
	if err != nil {
		return nil, err
	}
	ctx, err = ctx.SubContext(el)
	if err != nil {
		return nil, err
	}
	detached, err := etreeutils.NSDetatch(ctx, el)
	if err != nil {
		return nil, err
	}
	var last error = errors.New("the provider's IdP metadata has no signing certificate")
	for _, c := range certs {
		// One certificate at a time, so a signature without KeyInfo is
		// tried against each of the metadata's certificates.
		vc := dsig.NewDefaultValidationContext(&dsig.MemoryX509CertificateStore{Roots: []*x509.Certificate{c}})
		vc.Clock = dsig.NewFakeClockAt(now)
		out, err := vc.Validate(detached)
		if err == nil {
			return out, nil
		}
		last = err
	}
	return nil, last
}

// VerifySAML verifies a SAML subject token against a provider's metadata
// and decryption keys, at now, and returns its assertion. Every error's
// text is the STS's invalid_grant description.
func VerifySAML(token string, md SAMLMetadata, keys []*rsa.PrivateKey, want SAMLExpect, now time.Time) (SAMLAssertion, error) {
	raw, err := decodeSAMLToken(token)
	if err != nil {
		return SAMLAssertion{}, err
	}
	if bytes.Contains(raw, []byte("<!DOCTYPE")) || bytes.Contains(raw, []byte("<!ENTITY")) {
		return SAMLAssertion{}, errors.New("Unable to parse the SAML assertion: a document type declaration is not allowed.")
	}
	doc := etree.NewDocument()
	if err := doc.ReadFromBytes(raw); err != nil || doc.Root() == nil {
		return SAMLAssertion{}, fmt.Errorf("Unable to parse the SAML assertion: %v", err)
	}
	root := doc.Root()
	var assertion *etree.Element
	signed := false
	switch {
	case root.Tag == "Response" && root.NamespaceURI() == samlProtocolNS:
		if child(root, dsigNS, "Signature") != nil {
			v, err := verifySigned(root, md.Certs, now)
			if err != nil {
				return SAMLAssertion{}, fmt.Errorf("The SAML response's signature is invalid: %v", err)
			}
			root, signed = v, true
		}
		code := child(child(root, samlProtocolNS, "Status"), samlProtocolNS, "StatusCode")
		if code == nil || code.SelectAttrValue("Value", "") != samlSuccess {
			return SAMLAssertion{}, errors.New("The SAML response's status is not " + samlSuccess + ".")
		}
		ii, err := parseSAMLTime(root.SelectAttrValue("IssueInstant", ""))
		if err != nil {
			return SAMLAssertion{}, errors.New("The SAML response has no valid IssueInstant.")
		}
		if now.Sub(ii) >= time.Hour {
			return SAMLAssertion{}, errors.New("The SAML response's IssueInstant is more than 1 hour in the past.")
		}
		plain, enc := children(root, samlAssertionNS, "Assertion"), children(root, samlAssertionNS, "EncryptedAssertion")
		if len(plain)+len(enc) != 1 {
			return SAMLAssertion{}, errors.New("The SAML response must contain exactly one assertion.")
		}
		if len(enc) == 1 {
			if assertion, err = decryptAssertion(enc[0], keys); err != nil {
				return SAMLAssertion{}, err
			}
		} else {
			assertion = plain[0]
		}
	case root.Tag == "Assertion" && root.NamespaceURI() == samlAssertionNS:
		assertion = root
	case root.Tag == "EncryptedAssertion" && root.NamespaceURI() == samlAssertionNS:
		if assertion, err = decryptAssertion(root, keys); err != nil {
			return SAMLAssertion{}, err
		}
	default:
		return SAMLAssertion{}, errors.New("Unable to parse the SAML assertion: the document is neither a SAML 2.0 Response nor an Assertion.")
	}
	if child(assertion, dsigNS, "Signature") != nil {
		v, err := verifySigned(assertion, md.Certs, now)
		if err != nil {
			return SAMLAssertion{}, fmt.Errorf("The SAML assertion's signature is invalid: %v", err)
		}
		assertion, signed = v, true
	}
	if !signed {
		return SAMLAssertion{}, errors.New("Either the SAML assertion or the response must be signed.")
	}
	return checkAssertion(assertion, md, want, now)
}

func checkAssertion(a *etree.Element, md SAMLMetadata, want SAMLExpect, now time.Time) (SAMLAssertion, error) {
	var out SAMLAssertion
	iss := child(a, samlAssertionNS, "Issuer")
	if iss == nil || strings.TrimSpace(iss.Text()) != md.EntityID {
		got := ""
		if iss != nil {
			got = strings.TrimSpace(iss.Text())
		}
		return out, fmt.Errorf("The SAML assertion's Issuer %q does not match the IdP entity ID %q.", got, md.EntityID)
	}
	if f := iss.SelectAttrValue("Format", ""); f != "" && f != samlEntityFmt {
		return out, fmt.Errorf("The SAML assertion's Issuer Format must be omitted or %s.", samlEntityFmt)
	}
	subj := child(a, samlAssertionNS, "Subject")
	nameID := child(subj, samlAssertionNS, "NameID")
	if nameID == nil || strings.TrimSpace(nameID.Text()) == "" {
		return out, errors.New("The SAML assertion's Subject has no NameID.")
	}
	out.Subject = strings.TrimSpace(nameID.Text())
	var bearer []*etree.Element
	for _, sc := range children(subj, samlAssertionNS, "SubjectConfirmation") {
		if sc.SelectAttrValue("Method", "") == samlBearer {
			bearer = append(bearer, sc)
		}
	}
	if len(bearer) != 1 {
		return out, errors.New("The SAML assertion's Subject must have exactly one SubjectConfirmation with Method " + samlBearer + ".")
	}
	scd := child(bearer[0], samlAssertionNS, "SubjectConfirmationData")
	if scd == nil {
		return out, errors.New("The SAML assertion's SubjectConfirmation has no SubjectConfirmationData.")
	}
	if scd.SelectAttr("NotBefore") != nil {
		return out, errors.New("The SAML assertion's SubjectConfirmationData must not have a NotBefore.")
	}
	t, err := parseSAMLTime(scd.SelectAttrValue("NotOnOrAfter", ""))
	if err != nil {
		return out, errors.New("The SAML assertion's SubjectConfirmationData has no valid NotOnOrAfter.")
	}
	if !now.Before(t) {
		return out, errors.New("The SAML assertion has expired: its SubjectConfirmationData NotOnOrAfter is not in the future.")
	}
	if r := scd.SelectAttrValue("Recipient", ""); r != "" && len(want.Recipients) > 0 && !contains(want.Recipients, r) {
		return out, fmt.Errorf("The SAML assertion's Recipient %q is not %s.", r, strings.Join(want.Recipients, " or "))
	}
	cond := child(a, samlAssertionNS, "Conditions")
	if cond == nil {
		return out, errors.New("The SAML assertion has no Conditions with an Audience.")
	}
	if s := cond.SelectAttrValue("NotBefore", ""); s != "" {
		nb, err := parseSAMLTime(s)
		if err != nil || now.Before(nb) {
			return out, errors.New("The SAML assertion is not yet valid: its Conditions NotBefore is in the future.")
		}
	}
	if s := cond.SelectAttrValue("NotOnOrAfter", ""); s != "" {
		na, err := parseSAMLTime(s)
		if err != nil || !now.Before(na) {
			return out, errors.New("The SAML assertion has expired: its Conditions NotOnOrAfter is not in the future.")
		}
	}
	var auds []string
	for _, ar := range children(cond, samlAssertionNS, "AudienceRestriction") {
		for _, au := range children(ar, samlAssertionNS, "Audience") {
			auds = append(auds, strings.TrimSpace(au.Text()))
		}
	}
	if !contains(auds, want.Audience) {
		return out, fmt.Errorf("The audience in the SAML assertion [%s] does not match the expected audience %s.", strings.Join(auds, ", "), want.Audience)
	}
	authn := children(a, samlAssertionNS, "AuthnStatement")
	if len(authn) == 0 {
		return out, errors.New("The SAML assertion has no AuthnStatement.")
	}
	for _, st := range authn {
		if s := st.SelectAttrValue("SessionNotOnOrAfter", ""); s != "" {
			sn, err := parseSAMLTime(s)
			if err != nil || !now.Before(sn) {
				return out, errors.New("The SAML assertion's session has expired: its SessionNotOnOrAfter is not in the future.")
			}
			if out.SessionNotOnOrAfter.IsZero() || sn.Before(out.SessionNotOnOrAfter) {
				out.SessionNotOnOrAfter = sn
			}
		}
	}
	out.Attributes = map[string][]string{}
	for _, as := range children(a, samlAssertionNS, "AttributeStatement") {
		for _, at := range children(as, samlAssertionNS, "Attribute") {
			name := at.SelectAttrValue("Name", "")
			if name == "" {
				continue
			}
			vals := out.Attributes[name]
			for _, v := range children(at, samlAssertionNS, "AttributeValue") {
				vals = append(vals, v.Text())
			}
			if vals == nil {
				vals = []string{}
			}
			out.Attributes[name] = vals
		}
	}
	return out, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ---- XML Encryption (xmlenc) of an EncryptedAssertion

// findDeep is the first element named tag in ns at or below el.
func findDeep(el *etree.Element, ns, tag string) *etree.Element {
	if el == nil {
		return nil
	}
	if el.Tag == tag && el.NamespaceURI() == ns {
		return el
	}
	for _, c := range el.ChildElements() {
		if f := findDeep(c, ns, tag); f != nil {
			return f
		}
	}
	return nil
}

func cipherValue(el *etree.Element) ([]byte, error) {
	cv := child(child(el, xencNS, "CipherData"), xencNS, "CipherValue")
	if cv == nil {
		return nil, errors.New("no CipherValue")
	}
	return base64.StdEncoding.DecodeString(stripSpace(cv.Text()))
}

func digestHash(alg string) (crypto.Hash, bool) {
	switch alg {
	case "", "http://www.w3.org/2000/09/xmldsig#sha1":
		return crypto.SHA1, true
	case "http://www.w3.org/2001/04/xmlenc#sha256":
		return crypto.SHA256, true
	case "http://www.w3.org/2001/04/xmldsig-more#sha384":
		return crypto.SHA384, true
	case "http://www.w3.org/2001/04/xmlenc#sha512":
		return crypto.SHA512, true
	}
	return 0, false
}

func mgfHash(alg string) (crypto.Hash, bool) {
	switch alg {
	case "", xenc11NS + "mgf1sha1":
		return crypto.SHA1, true
	case xenc11NS + "mgf1sha256":
		return crypto.SHA256, true
	case xenc11NS + "mgf1sha384":
		return crypto.SHA384, true
	case xenc11NS + "mgf1sha512":
		return crypto.SHA512, true
	}
	return 0, false
}

// unwrapKey is the EncryptedKey's content key, decrypted with one of keys.
func unwrapKey(ek *etree.Element, keys []*rsa.PrivateKey) ([]byte, error) {
	em := child(ek, xencNS, "EncryptionMethod")
	if em == nil {
		return nil, errors.New("the EncryptedKey has no EncryptionMethod")
	}
	alg := em.SelectAttrValue("Algorithm", "")
	dm := child(em, dsigNS, "DigestMethod")
	dAlg := ""
	if dm != nil {
		dAlg = dm.SelectAttrValue("Algorithm", "")
	}
	h, ok := digestHash(dAlg)
	if !ok {
		return nil, fmt.Errorf("unsupported key transport digest %s", dAlg)
	}
	mgf := h
	switch alg {
	case xencNS + "rsa-oaep-mgf1p":
		mgf = crypto.SHA1
	case xenc11NS + "rsa-oaep":
		m := child(em, xenc11NS, "MGF")
		mAlg := ""
		if m != nil {
			mAlg = m.SelectAttrValue("Algorithm", "")
		}
		if mgf, ok = mgfHash(mAlg); !ok {
			return nil, fmt.Errorf("unsupported MGF %s", mAlg)
		}
	default:
		return nil, fmt.Errorf("unsupported key transport algorithm %q (RSA-OAEP only)", alg)
	}
	ct, err := cipherValue(ek)
	if err != nil {
		return nil, err
	}
	var label []byte
	if p := child(em, xencNS, "OAEPparams"); p != nil {
		if label, err = base64.StdEncoding.DecodeString(stripSpace(p.Text())); err != nil {
			return nil, err
		}
	}
	for _, k := range keys {
		if out, err := k.Decrypt(nil, ct, &rsa.OAEPOptions{Hash: h, MGFHash: mgf, Label: label}); err == nil {
			return out, nil
		}
	}
	return nil, errors.New("none of the provider's encryption keys decrypts it")
}

func decryptAssertion(ea *etree.Element, keys []*rsa.PrivateKey) (*etree.Element, error) {
	fail := func(err error) (*etree.Element, error) {
		return nil, fmt.Errorf("Unable to decrypt the SAML assertion: %v", err)
	}
	if len(keys) == 0 {
		return fail(errors.New("the provider has no encryption key"))
	}
	ed := child(ea, xencNS, "EncryptedData")
	if ed == nil {
		return fail(errors.New("no EncryptedData"))
	}
	ek := findDeep(ed, xencNS, "EncryptedKey")
	if ek == nil {
		ek = findDeep(ea, xencNS, "EncryptedKey")
	}
	if ek == nil {
		return fail(errors.New("no EncryptedKey"))
	}
	key, err := unwrapKey(ek, keys)
	if err != nil {
		return fail(err)
	}
	em := child(ed, xencNS, "EncryptionMethod")
	if em == nil {
		return fail(errors.New("the EncryptedData has no EncryptionMethod"))
	}
	ct, err := cipherValue(ed)
	if err != nil {
		return fail(err)
	}
	var plain []byte
	switch alg := em.SelectAttrValue("Algorithm", ""); alg {
	case xencNS + "aes128-cbc", xencNS + "aes192-cbc", xencNS + "aes256-cbc":
		plain, err = aesCBC(key, ct)
	case xenc11NS + "aes128-gcm", xenc11NS + "aes192-gcm", xenc11NS + "aes256-gcm":
		plain, err = aesGCM(key, ct)
	default:
		err = fmt.Errorf("unsupported data encryption algorithm %q", alg)
	}
	if err != nil {
		return fail(err)
	}
	if bytes.Contains(plain, []byte("<!DOCTYPE")) {
		return fail(errors.New("a document type declaration is not allowed"))
	}
	doc := etree.NewDocument()
	if err := doc.ReadFromBytes(plain); err != nil || doc.Root() == nil {
		return fail(fmt.Errorf("the plaintext is not XML: %v", err))
	}
	a := doc.Root()
	if a.Tag != "Assertion" || a.NamespaceURI() != samlAssertionNS {
		return fail(errors.New("the plaintext is not a SAML 2.0 Assertion"))
	}
	return a, nil
}

func aesCBC(key, ct []byte) ([]byte, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(ct) < 2*aes.BlockSize || len(ct)%aes.BlockSize != 0 {
		return nil, errors.New("bad ciphertext length")
	}
	iv, data := ct[:aes.BlockSize], append([]byte(nil), ct[aes.BlockSize:]...)
	cipher.NewCBCDecrypter(b, iv).CryptBlocks(data, data)
	pad := int(data[len(data)-1])
	if pad == 0 || pad > aes.BlockSize {
		return nil, errors.New("bad padding")
	}
	return data[:len(data)-pad], nil
}

func aesGCM(key, ct []byte) ([]byte, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(b)
	if err != nil {
		return nil, err
	}
	if len(ct) < g.NonceSize()+g.Overhead() {
		return nil, errors.New("bad ciphertext length")
	}
	return g.Open(nil, ct[:g.NonceSize()], ct[g.NonceSize():], nil)
}
