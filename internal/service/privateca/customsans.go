package privateca

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"strings"

	"cloud.google.com/go/security/privateca/apiv1/privatecapb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Custom subject alternative names and a requested subject key ID (#1961),
// as resources.proto documents them:
//
//   - SubjectAltNames.custom_sans "Contains additional subject alternative
//     name values. For each custom_san, the `value` field must contain an
//     ASN.1 encoded UTF8String." Each is an X509Extension whose object_id
//     names the value's type, so it is written into the subjectAltName
//     extension (2.5.29.17) as an RFC 5280 otherName { type-id, value },
//     beside the DNS names, email addresses, IP addresses and URIs; a
//     custom SAN marked critical makes the extension critical.
//   - CertificateConfig.subject_key_id "Provides a means of identifiying
//     certificates that contain a particular public key, per RFC 5280
//     4.2.1.2"; its key_id is "encoded in lowercase hexadecimal", and it is
//     the certificate's subjectKeyIdentifier instead of the SHA-1 of the
//     public key.

var oidSubjectAltName = asn1.ObjectIdentifier{2, 5, 29, 17}

// GeneralName tags (RFC 5280 4.2.1.6).
const (
	gnOther = 0
	gnEmail = 1
	gnDNS   = 2
	gnURI   = 6
	gnIP    = 7
)

// checkCustomSANs validates custom_sans as documented.
func checkCustomSANs(custom []*privatecapb.X509Extension) error {
	for i, c := range custom {
		if len(c.GetObjectId().GetObjectIdPath()) < 2 {
			return apierror.InvalidArgument("subject_alt_name.custom_sans[%d].object_id must name an object identifier", i)
		}
		var v asn1.RawValue
		rest, err := asn1.Unmarshal(c.GetValue(), &v)
		if err != nil || len(rest) > 0 || v.Class != asn1.ClassUniversal || v.Tag != asn1.TagUTF8String || v.IsCompound {
			return apierror.InvalidArgument("subject_alt_name.custom_sans[%d].value must contain an ASN.1 encoded UTF8String", i)
		}
	}
	return nil
}

// applyCustomSANs writes t's subjectAltName extension itself, with t's
// names and the custom SANs as otherNames: crypto/x509 writes no
// otherName, and leaves the extension to ExtraExtensions when one is there.
func applyCustomSANs(t *x509.Certificate, custom []*privatecapb.X509Extension) error {
	if len(custom) == 0 {
		return nil
	}
	if err := checkCustomSANs(custom); err != nil {
		return err
	}
	var names []asn1.RawValue
	for _, e := range t.EmailAddresses {
		names = append(names, asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: gnEmail, Bytes: []byte(e)})
	}
	for _, d := range t.DNSNames {
		names = append(names, asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: gnDNS, Bytes: []byte(d)})
	}
	for _, u := range t.URIs {
		names = append(names, asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: gnURI, Bytes: []byte(u.String())})
	}
	for _, ip := range t.IPAddresses {
		b := ip.To4()
		if b == nil {
			b = ip
		}
		names = append(names, asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: gnIP, Bytes: b})
	}
	critical := false
	for _, c := range custom {
		typeID, err := asn1.Marshal(oid(c.GetObjectId()))
		if err != nil {
			return apierror.InvalidArgument("subject_alt_name.custom_sans: %v", err)
		}
		value, err := asn1.Marshal(asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: c.GetValue()})
		if err != nil {
			return apierror.InvalidArgument("subject_alt_name.custom_sans: %v", err)
		}
		names = append(names, asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: gnOther, IsCompound: true,
			Bytes: append(typeID, value...)})
		critical = critical || c.GetCritical()
	}
	der, err := asn1.Marshal(names)
	if err != nil {
		return apierror.InvalidArgument("subject_alt_name: %v", err)
	}
	// RFC 5280 4.2.1.6: critical when the subject is empty.
	critical = critical || (t.Subject.String() == "" && len(t.RawSubject) == 0)
	exts := t.ExtraExtensions[:0:0]
	for _, e := range t.ExtraExtensions {
		if !e.Id.Equal(oidSubjectAltName) {
			exts = append(exts, e)
		}
	}
	t.ExtraExtensions = append(exts, pkix.Extension{Id: oidSubjectAltName, Critical: critical, Value: der})
	return nil
}

// hasCustomSANs reports whether t carries a subjectAltName CloudBurrow
// wrote itself.
func hasCustomSANs(t *x509.Certificate) bool {
	for _, e := range t.ExtraExtensions {
		if e.Id.Equal(oidSubjectAltName) {
			return true
		}
	}
	return false
}

// customSANsOf is a certificate's otherNames, as custom_sans.
func customSANsOf(c *x509.Certificate) []*privatecapb.X509Extension {
	var out []*privatecapb.X509Extension
	for _, e := range c.Extensions {
		if !e.Id.Equal(oidSubjectAltName) {
			continue
		}
		var names []asn1.RawValue
		if _, err := asn1.Unmarshal(e.Value, &names); err != nil {
			return nil
		}
		for _, n := range names {
			if n.Class != asn1.ClassContextSpecific || n.Tag != gnOther {
				continue
			}
			var typeID asn1.ObjectIdentifier
			rest, err := asn1.Unmarshal(n.Bytes, &typeID)
			if err != nil {
				continue
			}
			var v asn1.RawValue
			if _, err := asn1.Unmarshal(rest, &v); err != nil || v.Class != asn1.ClassContextSpecific || v.Tag != 0 {
				continue
			}
			out = append(out, &privatecapb.X509Extension{ObjectId: oidProto(typeID), Critical: e.Critical, Value: v.Bytes})
		}
	}
	return out
}

// requestedKeyID is config.subject_key_id.key_id decoded, nil when unset.
func requestedKeyID(kid *privatecapb.CertificateConfig_KeyId) ([]byte, error) {
	s := kid.GetKeyId()
	if s == "" {
		return nil, nil
	}
	b, err := hex.DecodeString(s)
	if err != nil || s != strings.ToLower(s) {
		return nil, apierror.InvalidArgument("config.subject_key_id.key_id %q must be encoded in lowercase hexadecimal", s)
	}
	return b, nil
}
