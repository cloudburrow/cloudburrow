package privateca

import (
	"crypto/x509/pkix"
	"encoding/asn1"

	"cloud.google.com/go/security/privateca/apiv1/privatecapb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

func asn1Unmarshal(b []byte, v any) ([]byte, error) { return asn1.Unmarshal(b, v) }

// attributeOIDs are the OIDs of the named AttributeTypes (RFC 4519).
var attributeOIDs = map[privatecapb.AttributeType]asn1.ObjectIdentifier{
	privatecapb.AttributeType_COMMON_NAME:         {2, 5, 4, 3},
	privatecapb.AttributeType_COUNTRY_CODE:        {2, 5, 4, 6},
	privatecapb.AttributeType_ORGANIZATION:        {2, 5, 4, 10},
	privatecapb.AttributeType_ORGANIZATIONAL_UNIT: {2, 5, 4, 11},
	privatecapb.AttributeType_LOCALITY:            {2, 5, 4, 7},
	privatecapb.AttributeType_PROVINCE:            {2, 5, 4, 8},
	privatecapb.AttributeType_STREET_ADDRESS:      {2, 5, 4, 9},
	privatecapb.AttributeType_POSTAL_CODE:         {2, 5, 4, 17},
}

// rdnSubject is a Subject's rdn_sequence ("can be used in place of the
// named subject fields") as a DER Name, in its order and with its
// multi-valued RDNs; nil when it has none.
func rdnSubject(s *privatecapb.Subject) ([]byte, error) {
	if len(s.GetRdnSequence()) == 0 {
		return nil, nil
	}
	var seq pkix.RDNSequence
	for i, rdn := range s.GetRdnSequence() {
		if len(rdn.GetAttributes()) == 0 {
			return nil, apierror.InvalidArgument("subject.rdn_sequence[%d] has no attributes", i)
		}
		var set pkix.RelativeDistinguishedNameSET
		for j, a := range rdn.GetAttributes() {
			var id asn1.ObjectIdentifier
			switch {
			case a.GetObjectId() != nil:
				id = oid(a.GetObjectId())
			case a.GetType() != privatecapb.AttributeType_ATTRIBUTE_TYPE_UNSPECIFIED:
				id = attributeOIDs[a.GetType()]
			}
			if len(id) < 2 {
				return nil, apierror.InvalidArgument("subject.rdn_sequence[%d].attributes[%d] needs a type or an object_id", i, j)
			}
			set = append(set, pkix.AttributeTypeAndValue{Type: id, Value: a.GetValue()})
		}
		seq = append(seq, set)
	}
	der, err := asn1.Marshal(seq)
	if err != nil {
		return nil, apierror.InvalidArgument("subject.rdn_sequence: %v", err)
	}
	return der, nil
}

// rdnProto is a DER Name as an rdn_sequence.
func rdnProto(raw []byte) []*privatecapb.RelativeDistinguishedName {
	var seq pkix.RDNSequence
	if _, err := asn1.Unmarshal(raw, &seq); err != nil {
		return nil
	}
	byOID := map[string]privatecapb.AttributeType{}
	for t, o := range attributeOIDs {
		byOID[o.String()] = t
	}
	var out []*privatecapb.RelativeDistinguishedName
	for _, set := range seq {
		rdn := &privatecapb.RelativeDistinguishedName{}
		for _, a := range set {
			v, _ := a.Value.(string)
			atv := &privatecapb.AttributeTypeAndValue{Value: v}
			if t, ok := byOID[a.Type.String()]; ok {
				atv.AttributeType = &privatecapb.AttributeTypeAndValue_Type{Type: t}
			} else {
				atv.AttributeType = &privatecapb.AttributeTypeAndValue_ObjectId{ObjectId: oidProto(a.Type)}
			}
			rdn.Attributes = append(rdn.Attributes, atv)
		}
		out = append(out, rdn)
	}
	return out
}

// ---- passthrough_extensions ----

// filterExtensions drops from a request's X.509 parameters the extensions
// c does not let through: "If a certificate request sets extensions that
// don't appear in the passthrough_extensions, those extensions will be
// dropped." No constraints (nil) keep everything.
func filterExtensions(p *privatecapb.X509Parameters, c *privatecapb.CertificateExtensionConstraints) *privatecapb.X509Parameters {
	if p == nil || c == nil {
		return p
	}
	known := map[privatecapb.CertificateExtensionConstraints_KnownCertificateExtension]bool{}
	for _, k := range c.GetKnownExtensions() {
		known[k] = true
	}
	out := clone(p)
	if out.KeyUsage != nil {
		if !known[privatecapb.CertificateExtensionConstraints_BASE_KEY_USAGE] {
			out.KeyUsage.BaseKeyUsage = nil
		}
		if !known[privatecapb.CertificateExtensionConstraints_EXTENDED_KEY_USAGE] {
			out.KeyUsage.ExtendedKeyUsage, out.KeyUsage.UnknownExtendedKeyUsages = nil, nil
		}
	}
	if !known[privatecapb.CertificateExtensionConstraints_CA_OPTIONS] {
		out.CaOptions = nil
	}
	if !known[privatecapb.CertificateExtensionConstraints_POLICY_IDS] {
		out.PolicyIds = nil
	}
	if !known[privatecapb.CertificateExtensionConstraints_AIA_OCSP_SERVERS] {
		out.AiaOcspServers = nil
	}
	if !known[privatecapb.CertificateExtensionConstraints_NAME_CONSTRAINTS] {
		out.NameConstraints = nil
	}
	allowed := map[string]bool{}
	for _, o := range c.GetAdditionalExtensions() {
		allowed[oid(o).String()] = true
	}
	var extra []*privatecapb.X509Extension
	for _, x := range out.GetAdditionalExtensions() {
		if allowed[oid(x.GetObjectId()).String()] {
			extra = append(extra, x)
		}
	}
	out.AdditionalExtensions = extra
	return out
}

// outsideExtensions names an extension p sets that c does not let
// through, or "": a pool's baseline_values outside a template's
// passthrough_extensions, or a template's predefined_values outside a
// pool's, fail the issuance.
func outsideExtensions(p *privatecapb.X509Parameters, c *privatecapb.CertificateExtensionConstraints) string {
	if p == nil || c == nil {
		return ""
	}
	f := filterExtensions(p, c)
	switch {
	case p.GetKeyUsage().GetBaseKeyUsage() != nil && f.GetKeyUsage().GetBaseKeyUsage() == nil:
		return "BASE_KEY_USAGE"
	case (p.GetKeyUsage().GetExtendedKeyUsage() != nil || len(p.GetKeyUsage().GetUnknownExtendedKeyUsages()) > 0) &&
		f.GetKeyUsage().GetExtendedKeyUsage() == nil && len(f.GetKeyUsage().GetUnknownExtendedKeyUsages()) == 0:
		return "EXTENDED_KEY_USAGE"
	case p.GetCaOptions() != nil && f.GetCaOptions() == nil:
		return "CA_OPTIONS"
	case len(p.GetPolicyIds()) > 0 && len(f.GetPolicyIds()) == 0:
		return "POLICY_IDS"
	case len(p.GetAiaOcspServers()) > 0 && len(f.GetAiaOcspServers()) == 0:
		return "AIA_OCSP_SERVERS"
	case p.GetNameConstraints() != nil && f.GetNameConstraints() == nil:
		return "NAME_CONSTRAINTS"
	case len(p.GetAdditionalExtensions()) != len(f.GetAdditionalExtensions()):
		return "additional extension"
	}
	return ""
}
