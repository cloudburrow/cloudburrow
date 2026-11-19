package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/security/privateca/apiv1/privatecapb"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// The Certificate Authority Service console's controls for the fields
// #1834 serves beyond the basics: a subject as an RDN sequence, passthrough
// extensions, a CEL expression, and a requested not-before time. Each is
// text in a form, parsed here into the API's message, and shown back the
// same way on the resource's page.

// privateCAAttrKeys are the RDN attribute names a form takes, as RFC 4514
// writes them, by the AttributeType each is.
var privateCAAttrKeys = map[string]privatecapb.AttributeType{
	"CN": privatecapb.AttributeType_COMMON_NAME, "C": privatecapb.AttributeType_COUNTRY_CODE,
	"O": privatecapb.AttributeType_ORGANIZATION, "OU": privatecapb.AttributeType_ORGANIZATIONAL_UNIT,
	"L": privatecapb.AttributeType_LOCALITY, "ST": privatecapb.AttributeType_PROVINCE,
	"STREET": privatecapb.AttributeType_STREET_ADDRESS, "POSTALCODE": privatecapb.AttributeType_POSTAL_CODE,
}

const privateCARDNHelp = "Optional: the subject as an ordered RDN sequence, used in place of the named fields. " +
	"One RDN per line, its attributes joined by +, each NAME=value with NAME one of CN, C, O, OU, L, ST, STREET, POSTALCODE or a dotted OID " +
	"(for example O=Example, then OU=Ops+OU=Dev, then CN=host)."

// privateCAOID parses a dotted object identifier.
func privateCAOID(s string) (*privatecapb.ObjectId, error) {
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("%q is not a dotted object identifier", s)
	}
	out := &privatecapb.ObjectId{}
	for _, p := range parts {
		n, err := strconv.ParseInt(p, 10, 32)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("%q is not a dotted object identifier", s)
		}
		out.ObjectIdPath = append(out.ObjectIdPath, int32(n))
	}
	return out, nil
}

func privateCAOIDText(o *privatecapb.ObjectId) string {
	parts := make([]string, len(o.GetObjectIdPath()))
	for i, n := range o.GetObjectIdPath() {
		parts[i] = strconv.Itoa(int(n))
	}
	return strings.Join(parts, ".")
}

// privateCAParseRDN parses an RDN sequence, one RDN per line; nil for
// empty text.
func privateCAParseRDN(text string) ([]*privatecapb.RelativeDistinguishedName, error) {
	var out []*privatecapb.RelativeDistinguishedName
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		rdn := &privatecapb.RelativeDistinguishedName{}
		for _, av := range strings.Split(line, "+") {
			k, v, ok := strings.Cut(av, "=")
			k, v = strings.TrimSpace(k), strings.TrimSpace(v)
			if !ok || k == "" || v == "" {
				return nil, fmt.Errorf("RDN sequence: %q is not NAME=value", av)
			}
			a := &privatecapb.AttributeTypeAndValue{Value: v}
			if t, ok := privateCAAttrKeys[strings.ToUpper(k)]; ok {
				a.AttributeType = &privatecapb.AttributeTypeAndValue_Type{Type: t}
			} else if o, err := privateCAOID(k); err == nil {
				a.AttributeType = &privatecapb.AttributeTypeAndValue_ObjectId{ObjectId: o}
			} else {
				return nil, fmt.Errorf("RDN sequence: %q is not CN, C, O, OU, L, ST, STREET, POSTALCODE or a dotted OID", k)
			}
			rdn.Attributes = append(rdn.Attributes, a)
		}
		out = append(out, rdn)
	}
	return out, nil
}

// privateCARDNText is an RDN sequence as the form takes it, one RDN per
// line in its order.
func privateCARDNText(seq []*privatecapb.RelativeDistinguishedName) string {
	names := map[privatecapb.AttributeType]string{}
	for k, t := range privateCAAttrKeys {
		names[t] = k
	}
	var lines []string
	for _, rdn := range seq {
		var avs []string
		for _, a := range rdn.GetAttributes() {
			k := names[a.GetType()]
			if a.GetObjectId() != nil {
				k = privateCAOIDText(a.GetObjectId())
			}
			avs = append(avs, k+"="+a.GetValue())
		}
		lines = append(lines, strings.Join(avs, "+"))
	}
	return strings.Join(lines, "\n")
}

const privateCAExtensionsHelp = "Optional: comma-separated extensions a request may carry through, each a known extension " +
	"(BASE_KEY_USAGE, EXTENDED_KEY_USAGE, CA_OPTIONS, POLICY_IDS, AIA_OCSP_SERVERS, NAME_CONSTRAINTS) or a dotted OID."

// privateCAParseExtensions parses passthrough extensions; nil for empty
// text.
func privateCAParseExtensions(text string) (*privatecapb.CertificateExtensionConstraints, error) {
	var out *privatecapb.CertificateExtensionConstraints
	for _, e := range strings.Split(text, ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if out == nil {
			out = &privatecapb.CertificateExtensionConstraints{}
		}
		if v, ok := privatecapb.CertificateExtensionConstraints_KnownCertificateExtension_value[strings.ToUpper(e)]; ok && v != 0 {
			out.KnownExtensions = append(out.KnownExtensions, privatecapb.CertificateExtensionConstraints_KnownCertificateExtension(v))
			continue
		}
		o, err := privateCAOID(e)
		if err != nil {
			return nil, fmt.Errorf("passthrough extensions: %q is not a known extension or a dotted OID", e)
		}
		out.AdditionalExtensions = append(out.AdditionalExtensions, o)
	}
	return out, nil
}

func privateCAExtensionsText(c *privatecapb.CertificateExtensionConstraints) string {
	var out []string
	for _, k := range c.GetKnownExtensions() {
		out = append(out, k.String())
	}
	for _, o := range c.GetAdditionalExtensions() {
		out = append(out, privateCAOIDText(o))
	}
	return strings.Join(out, ",")
}

const privateCACELHelp = "Optional: a CEL expression over subject and subject_alt_names that a request must satisfy, " +
	"for example subject_alt_names.all(san, san.type == DNS && san.value.endsWith(\".example.com\"))."

// privateCAIdentity is identity constraints from a form: the two
// passthrough booleans and an optional CEL expression; nil when every
// value is the default (both allowed, no expression).
func privateCAIdentity(values map[string]string) *privatecapb.CertificateIdentityConstraints {
	subj, sans := values["allowSubjectPassthrough"] != "false", values["allowSansPassthrough"] != "false"
	cel := strings.TrimSpace(values["celExpression"])
	if subj && sans && cel == "" {
		return nil
	}
	ic := &privatecapb.CertificateIdentityConstraints{AllowSubjectPassthrough: &subj, AllowSubjectAltNamesPassthrough: &sans}
	if cel != "" {
		ic.CelExpression = &expr.Expr{Expression: cel}
	}
	return ic
}

// privateCAParseTime parses an RFC 3339 time; nil for empty text.
func privateCAParseTime(field, v string) (*timestamppb.Timestamp, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return nil, fmt.Errorf("%s: %q is not an RFC 3339 time such as 2026-01-02T15:04:05Z", field, v)
	}
	return timestamppb.New(t), nil
}

// privateCASubjectModes are a certificate request's subject modes.
func privateCASubjectModes() []string {
	return []string{privatecapb.SubjectRequestMode_DEFAULT.String(), privatecapb.SubjectRequestMode_RDN_SEQUENCE.String(),
		privatecapb.SubjectRequestMode_REFLECTED_SPIFFE.String()}
}

func privateCASubjectModeField() console.Field {
	return console.Field{Name: "subjectMode", Label: "Subject mode", Type: "select", Default: privatecapb.SubjectRequestMode_DEFAULT.String(),
		Options: privateCASubjectModes(),
		Help:    "DEFAULT: the subject's named fields. RDN_SEQUENCE: the subject exactly as its RDN sequence (or the CSR's) orders it. REFLECTED_SPIFFE: the subject and SAN from the caller's SPIFFE identity."}
}

func privateCANotBeforeField() console.Field {
	return console.Field{Name: "requestedNotBefore", Label: "Requested not-before time", Type: "text",
		Help: "Optional, RFC 3339 (2026-01-02T15:04:05Z): the certificate's not-before time, which the pool's issuance policy must allow."}
}

// privateCASubjectMode is a certificate's subject mode, "" for none.
func privateCASubjectMode(m privatecapb.SubjectRequestMode) string {
	if m == privatecapb.SubjectRequestMode_SUBJECT_REQUEST_MODE_UNSPECIFIED {
		return ""
	}
	return m.String()
}

// privateCAKeyProperty is a CA's key, as its page shows it: the Cloud KMS
// version that holds it, or the algorithm of the key Google manages.
func privateCAKeyProperty(k *privatecapb.CertificateAuthority_KeyVersionSpec) console.Property {
	if v := k.GetCloudKmsKeyVersion(); v != "" {
		return console.Property{Label: "Cloud KMS key version", Value: v}
	}
	return console.Property{Label: "Key algorithm", Value: k.GetAlgorithm().String()}
}
