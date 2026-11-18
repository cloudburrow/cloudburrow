package privateca

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1" //nolint:gosec // RFC 5280 §4.2.1.2 method 1 key identifier, as Google's CAs compute it.
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"net"
	"net/url"
	"strings"
	"time"

	"cloud.google.com/go/security/privateca/apiv1/privatecapb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// newKey makes a CA's signing key for a KeyVersionSpec algorithm, and the
// signature algorithm it signs with.
func newKey(alg privatecapb.CertificateAuthority_SignHashAlgorithm) (crypto.Signer, x509.SignatureAlgorithm, error) {
	rsaKey := func(bits int) (crypto.Signer, error) { return rsa.GenerateKey(rand.Reader, bits) }
	var (
		k   crypto.Signer
		sa  x509.SignatureAlgorithm
		err error
	)
	switch alg {
	case privatecapb.CertificateAuthority_RSA_PSS_2048_SHA256:
		k, err = rsaKey(2048)
		sa = x509.SHA256WithRSAPSS
	case privatecapb.CertificateAuthority_RSA_PSS_3072_SHA256:
		k, err = rsaKey(3072)
		sa = x509.SHA256WithRSAPSS
	case privatecapb.CertificateAuthority_RSA_PSS_4096_SHA256:
		k, err = rsaKey(4096)
		sa = x509.SHA256WithRSAPSS
	case privatecapb.CertificateAuthority_RSA_PKCS1_2048_SHA256:
		k, err = rsaKey(2048)
		sa = x509.SHA256WithRSA
	case privatecapb.CertificateAuthority_RSA_PKCS1_3072_SHA256:
		k, err = rsaKey(3072)
		sa = x509.SHA256WithRSA
	case privatecapb.CertificateAuthority_RSA_PKCS1_4096_SHA256:
		k, err = rsaKey(4096)
		sa = x509.SHA256WithRSA
	case privatecapb.CertificateAuthority_EC_P256_SHA256:
		k, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		sa = x509.ECDSAWithSHA256
	case privatecapb.CertificateAuthority_EC_P384_SHA384:
		k, err = ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
		sa = x509.ECDSAWithSHA384
	default:
		return nil, 0, apierror.InvalidArgument("key_spec.algorithm %s is not a signing algorithm", alg)
	}
	return k, sa, err
}

// signatureAlgorithm is what cert's key signs with, as the CA was made.
func signatureAlgorithm(ca *x509.Certificate) x509.SignatureAlgorithm { return ca.SignatureAlgorithm }

// subjectName is a Subject as an X.509 name. An rdn_sequence is kept in
// its order.
func subjectName(s *privatecapb.Subject) (pkix.Name, error) {
	var n pkix.Name
	if s == nil {
		return n, nil
	}
	one := func(v string) []string {
		if v == "" {
			return nil
		}
		return []string{v}
	}
	n.CommonName = s.GetCommonName()
	n.Country = one(s.GetCountryCode())
	n.Organization = one(s.GetOrganization())
	n.OrganizationalUnit = one(s.GetOrganizationalUnit())
	n.Locality = one(s.GetLocality())
	n.Province = one(s.GetProvince())
	n.StreetAddress = one(s.GetStreetAddress())
	n.PostalCode = one(s.GetPostalCode())
	if len(s.GetRdnSequence()) > 0 {
		return n, apierror.Unimplemented("subject.rdn_sequence (SubjectRequestMode RDN_SEQUENCE) is not implemented in CloudBurrow yet")
	}
	return n, nil
}

// subjectProto is name as a Subject.
func subjectProto(n pkix.Name) *privatecapb.Subject {
	first := func(v []string) string {
		if len(v) == 0 {
			return ""
		}
		return v[0]
	}
	return &privatecapb.Subject{CommonName: n.CommonName, CountryCode: first(n.Country), Organization: first(n.Organization),
		OrganizationalUnit: first(n.OrganizationalUnit), Locality: first(n.Locality), Province: first(n.Province),
		StreetAddress: first(n.StreetAddress), PostalCode: first(n.PostalCode)}
}

// applySANs sets t's subject alternative names.
func applySANs(t *x509.Certificate, san *privatecapb.SubjectAltNames) error {
	if san == nil {
		return nil
	}
	if len(san.GetCustomSans()) > 0 {
		return apierror.Unimplemented("subject_alt_name.custom_sans is not implemented in CloudBurrow yet")
	}
	t.DNSNames = append(t.DNSNames, san.GetDnsNames()...)
	t.EmailAddresses = append(t.EmailAddresses, san.GetEmailAddresses()...)
	for _, ip := range san.GetIpAddresses() {
		p := net.ParseIP(ip)
		if p == nil {
			return apierror.InvalidArgument("subject_alt_name.ip_addresses: %q is not an IP address", ip)
		}
		t.IPAddresses = append(t.IPAddresses, p)
	}
	for _, u := range san.GetUris() {
		p, err := url.Parse(u)
		if err != nil || p.Scheme == "" {
			return apierror.InvalidArgument("subject_alt_name.uris: %q is not a URI", u)
		}
		t.URIs = append(t.URIs, p)
	}
	return nil
}

func oid(o *privatecapb.ObjectId) asn1.ObjectIdentifier {
	out := make(asn1.ObjectIdentifier, len(o.GetObjectIdPath()))
	for i, v := range o.GetObjectIdPath() {
		out[i] = int(v)
	}
	return out
}

func oidProto(o asn1.ObjectIdentifier) *privatecapb.ObjectId {
	out := &privatecapb.ObjectId{}
	for _, v := range o {
		out.ObjectIdPath = append(out.ObjectIdPath, int32(v))
	}
	return out
}

// mergeX509 is req with base's set fields over it: a CA pool's
// baseline_values "will be applied to all certificates issued through this
// CaPool. If a certificate request includes conflicting values for the same
// properties, they will be overwritten by the values defined here."
func mergeX509(req, base *privatecapb.X509Parameters) *privatecapb.X509Parameters {
	out := &privatecapb.X509Parameters{}
	if req != nil {
		out = clone(req)
	}
	if base == nil {
		return out
	}
	if base.KeyUsage != nil {
		out.KeyUsage = clone(base.KeyUsage)
	}
	if base.CaOptions != nil {
		out.CaOptions = clone(base.CaOptions)
	}
	if len(base.PolicyIds) > 0 {
		out.PolicyIds = base.PolicyIds
	}
	if len(base.AiaOcspServers) > 0 {
		out.AiaOcspServers = base.AiaOcspServers
	}
	if base.NameConstraints != nil {
		out.NameConstraints = clone(base.NameConstraints)
	}
	if len(base.AdditionalExtensions) > 0 {
		out.AdditionalExtensions = base.AdditionalExtensions
	}
	return out
}

// applyX509 sets t's extensions from p.
func applyX509(t *x509.Certificate, p *privatecapb.X509Parameters) error {
	if p == nil {
		return nil
	}
	if ku := p.GetKeyUsage(); ku != nil {
		b := ku.GetBaseKeyUsage()
		for _, f := range []struct {
			on bool
			u  x509.KeyUsage
		}{
			{b.GetDigitalSignature(), x509.KeyUsageDigitalSignature}, {b.GetContentCommitment(), x509.KeyUsageContentCommitment},
			{b.GetKeyEncipherment(), x509.KeyUsageKeyEncipherment}, {b.GetDataEncipherment(), x509.KeyUsageDataEncipherment},
			{b.GetKeyAgreement(), x509.KeyUsageKeyAgreement}, {b.GetCertSign(), x509.KeyUsageCertSign},
			{b.GetCrlSign(), x509.KeyUsageCRLSign}, {b.GetEncipherOnly(), x509.KeyUsageEncipherOnly},
			{b.GetDecipherOnly(), x509.KeyUsageDecipherOnly},
		} {
			if f.on {
				t.KeyUsage |= f.u
			}
		}
		e := ku.GetExtendedKeyUsage()
		for _, f := range []struct {
			on bool
			u  x509.ExtKeyUsage
		}{
			{e.GetServerAuth(), x509.ExtKeyUsageServerAuth}, {e.GetClientAuth(), x509.ExtKeyUsageClientAuth},
			{e.GetCodeSigning(), x509.ExtKeyUsageCodeSigning}, {e.GetEmailProtection(), x509.ExtKeyUsageEmailProtection},
			{e.GetTimeStamping(), x509.ExtKeyUsageTimeStamping}, {e.GetOcspSigning(), x509.ExtKeyUsageOCSPSigning},
		} {
			if f.on {
				t.ExtKeyUsage = append(t.ExtKeyUsage, f.u)
			}
		}
		for _, o := range ku.GetUnknownExtendedKeyUsages() {
			t.UnknownExtKeyUsage = append(t.UnknownExtKeyUsage, oid(o))
		}
	}
	if co := p.GetCaOptions(); co != nil && co.IsCa != nil {
		t.BasicConstraintsValid = true
		t.IsCA = co.GetIsCa()
		if co.MaxIssuerPathLength != nil {
			if co.GetMaxIssuerPathLength() < 0 {
				return apierror.InvalidArgument("ca_options.max_issuer_path_length must not be negative")
			}
			t.MaxPathLen = int(co.GetMaxIssuerPathLength())
			t.MaxPathLenZero = t.MaxPathLen == 0
		} else {
			t.MaxPathLen = -1
		}
	}
	for _, o := range p.GetPolicyIds() {
		t.PolicyIdentifiers = append(t.PolicyIdentifiers, oid(o))
	}
	t.OCSPServer = append(t.OCSPServer, p.GetAiaOcspServers()...)
	if nc := p.GetNameConstraints(); nc != nil {
		t.PermittedDNSDomainsCritical = nc.GetCritical()
		t.PermittedDNSDomains = nc.GetPermittedDnsNames()
		t.ExcludedDNSDomains = nc.GetExcludedDnsNames()
		t.PermittedEmailAddresses = nc.GetPermittedEmailAddresses()
		t.ExcludedEmailAddresses = nc.GetExcludedEmailAddresses()
		t.PermittedURIDomains = nc.GetPermittedUris()
		t.ExcludedURIDomains = nc.GetExcludedUris()
		for _, r := range nc.GetPermittedIpRanges() {
			_, n, err := net.ParseCIDR(r)
			if err != nil {
				return apierror.InvalidArgument("name_constraints.permitted_ip_ranges: %q is not a CIDR range", r)
			}
			t.PermittedIPRanges = append(t.PermittedIPRanges, n)
		}
		for _, r := range nc.GetExcludedIpRanges() {
			_, n, err := net.ParseCIDR(r)
			if err != nil {
				return apierror.InvalidArgument("name_constraints.excluded_ip_ranges: %q is not a CIDR range", r)
			}
			t.ExcludedIPRanges = append(t.ExcludedIPRanges, n)
		}
	}
	for _, x := range p.GetAdditionalExtensions() {
		t.ExtraExtensions = append(t.ExtraExtensions, pkix.Extension{Id: oid(x.GetObjectId()), Critical: x.GetCritical(), Value: x.GetValue()})
	}
	return nil
}

// keyID is RFC 5280 §4.2.1.2 method (1): SHA-1 of the subjectPublicKey.
func keyID(pub crypto.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	var spki struct {
		Algorithm pkix.AlgorithmIdentifier
		PublicKey asn1.BitString
	}
	if _, err := asn1.Unmarshal(der, &spki); err != nil {
		return nil, err
	}
	sum := sha1.Sum(spki.PublicKey.Bytes) //nolint:gosec // key identifier, not a security hash.
	return sum[:], nil
}

// serial is a random positive 20-octet serial number, the most RFC 5280
// allows.
func serial() *big.Int {
	b := make([]byte, 20)
	_, _ = rand.Read(b)
	b[0] &= 0x7f
	if b[0] == 0 {
		b[0] = 1
	}
	return new(big.Int).SetBytes(b)
}

// parsePublicKey is a CertificateConfig's public_key: PEM, the only format
// (PublicKey.KeyFormat), of a SubjectPublicKeyInfo.
func parsePublicKey(pk *privatecapb.PublicKey) (crypto.PublicKey, error) {
	if pk == nil || len(pk.GetKey()) == 0 {
		return nil, apierror.InvalidArgument("config.public_key is required")
	}
	if pk.GetFormat() != privatecapb.PublicKey_PEM {
		return nil, apierror.InvalidArgument("config.public_key.format must be PEM")
	}
	blk, _ := pem.Decode(pk.GetKey())
	if blk == nil {
		return nil, apierror.InvalidArgument("config.public_key.key is not PEM")
	}
	pub, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if err != nil {
		return nil, apierror.InvalidArgument("config.public_key.key: %v", err)
	}
	return pub, nil
}

func publicKeyProto(pub crypto.PublicKey) *privatecapb.PublicKey {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil
	}
	return &privatecapb.PublicKey{Format: privatecapb.PublicKey_PEM, Key: pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})}
}

// describe is a parsed certificate as CertificateDescription.
func describe(c *x509.Certificate) *privatecapb.CertificateDescription {
	san := &privatecapb.SubjectAltNames{DnsNames: c.DNSNames, EmailAddresses: c.EmailAddresses}
	for _, ip := range c.IPAddresses {
		san.IpAddresses = append(san.IpAddresses, ip.String())
	}
	for _, u := range c.URIs {
		san.Uris = append(san.Uris, u.String())
	}
	d := &privatecapb.CertificateDescription{
		SubjectDescription: &privatecapb.CertificateDescription_SubjectDescription{
			Subject:         subjectProto(c.Subject),
			SubjectAltName:  san,
			HexSerialNumber: hex.EncodeToString(c.SerialNumber.Bytes()),
			Lifetime:        durationpb.New(c.NotAfter.Sub(c.NotBefore)),
			NotBeforeTime:   timestamppb.New(c.NotBefore),
			NotAfterTime:    timestamppb.New(c.NotAfter),
		},
		X509Description:           x509Description(c),
		PublicKey:                 publicKeyProto(c.PublicKey),
		CrlDistributionPoints:     c.CRLDistributionPoints,
		AiaIssuingCertificateUrls: c.IssuingCertificateURL,
	}
	if len(c.SubjectKeyId) > 0 {
		d.SubjectKeyId = &privatecapb.CertificateDescription_KeyId{KeyId: hex.EncodeToString(c.SubjectKeyId)}
	}
	if len(c.AuthorityKeyId) > 0 {
		d.AuthorityKeyId = &privatecapb.CertificateDescription_KeyId{KeyId: hex.EncodeToString(c.AuthorityKeyId)}
	}
	sum := sha256.Sum256(c.Raw)
	d.CertFingerprint = &privatecapb.CertificateDescription_CertificateFingerprint{Sha256Hash: hex.EncodeToString(sum[:])}
	tbs := sha256.Sum256(c.RawTBSCertificate)
	d.TbsCertificateDigest = hex.EncodeToString(tbs[:])
	return d
}

// x509Description is c's extensions as X509Parameters.
func x509Description(c *x509.Certificate) *privatecapb.X509Parameters {
	p := &privatecapb.X509Parameters{AiaOcspServers: c.OCSPServer}
	ku := &privatecapb.KeyUsage{BaseKeyUsage: &privatecapb.KeyUsage_KeyUsageOptions{
		DigitalSignature:  c.KeyUsage&x509.KeyUsageDigitalSignature != 0,
		ContentCommitment: c.KeyUsage&x509.KeyUsageContentCommitment != 0,
		KeyEncipherment:   c.KeyUsage&x509.KeyUsageKeyEncipherment != 0,
		DataEncipherment:  c.KeyUsage&x509.KeyUsageDataEncipherment != 0,
		KeyAgreement:      c.KeyUsage&x509.KeyUsageKeyAgreement != 0,
		CertSign:          c.KeyUsage&x509.KeyUsageCertSign != 0,
		CrlSign:           c.KeyUsage&x509.KeyUsageCRLSign != 0,
		EncipherOnly:      c.KeyUsage&x509.KeyUsageEncipherOnly != 0,
		DecipherOnly:      c.KeyUsage&x509.KeyUsageDecipherOnly != 0,
	}, ExtendedKeyUsage: &privatecapb.KeyUsage_ExtendedKeyUsageOptions{}}
	for _, e := range c.ExtKeyUsage {
		switch e {
		case x509.ExtKeyUsageServerAuth:
			ku.ExtendedKeyUsage.ServerAuth = true
		case x509.ExtKeyUsageClientAuth:
			ku.ExtendedKeyUsage.ClientAuth = true
		case x509.ExtKeyUsageCodeSigning:
			ku.ExtendedKeyUsage.CodeSigning = true
		case x509.ExtKeyUsageEmailProtection:
			ku.ExtendedKeyUsage.EmailProtection = true
		case x509.ExtKeyUsageTimeStamping:
			ku.ExtendedKeyUsage.TimeStamping = true
		case x509.ExtKeyUsageOCSPSigning:
			ku.ExtendedKeyUsage.OcspSigning = true
		}
	}
	for _, o := range c.UnknownExtKeyUsage {
		ku.UnknownExtendedKeyUsages = append(ku.UnknownExtendedKeyUsages, oidProto(o))
	}
	p.KeyUsage = ku
	if c.BasicConstraintsValid {
		isCA := c.IsCA
		p.CaOptions = &privatecapb.X509Parameters_CaOptions{IsCa: &isCA}
		if c.MaxPathLen > 0 || c.MaxPathLenZero {
			l := int32(c.MaxPathLen)
			p.CaOptions.MaxIssuerPathLength = &l
		}
	}
	for _, o := range c.PolicyIdentifiers {
		p.PolicyIds = append(p.PolicyIds, oidProto(o))
	}
	return p
}

// checkKeyType enforces a pool's allowed_key_types on pub. An empty list
// allows any key.
func checkKeyType(pub crypto.PublicKey, allowed []*privatecapb.CaPool_IssuancePolicy_AllowedKeyType) error {
	if len(allowed) == 0 {
		return nil
	}
	for _, a := range allowed {
		switch k := pub.(type) {
		case *rsa.PublicKey:
			r := a.GetRsa()
			if r == nil {
				continue
			}
			bits := int64(k.N.BitLen())
			if (r.GetMinModulusSize() == 0 || bits >= r.GetMinModulusSize()) && (r.GetMaxModulusSize() == 0 || bits <= r.GetMaxModulusSize()) {
				return nil
			}
		case *ecdsa.PublicKey:
			e := a.GetEllipticCurve()
			if e == nil {
				continue
			}
			switch e.GetSignatureAlgorithm() {
			case privatecapb.CaPool_IssuancePolicy_AllowedKeyType_EcKeyType_EC_SIGNATURE_ALGORITHM_UNSPECIFIED:
				return nil
			case privatecapb.CaPool_IssuancePolicy_AllowedKeyType_EcKeyType_ECDSA_P256:
				if k.Curve == elliptic.P256() {
					return nil
				}
			case privatecapb.CaPool_IssuancePolicy_AllowedKeyType_EcKeyType_ECDSA_P384:
				if k.Curve == elliptic.P384() {
					return nil
				}
			}
		}
	}
	return apierror.InvalidArgument("the certificate's public key is not one of the CA pool's issuance_policy.allowed_key_types")
}

// validity is the not-before and not-after times of a certificate of
// lifetime issued now by a CA valid until caEnd. The lifetime is cut to
// the pool's maximum_lifetime ("If a cert's requested lifetime exceeds this,
// it will be truncated") and to the CA's own expiry (UNVERIFIED for the
// CA's).
// Docs re-read 2026-11-18: https://docs.cloud.google.com/certificate-authority-service/docs/reference/rest/v1/projects.locations.caPools
func validity(now time.Time, lifetime, maxLifetime *durationpb.Duration, backdate *durationpb.Duration, caEnd time.Time) (time.Time, time.Time, error) {
	if lifetime == nil {
		return time.Time{}, time.Time{}, apierror.InvalidArgument("lifetime is required")
	}
	if err := lifetime.CheckValid(); err != nil || lifetime.AsDuration() <= 0 {
		return time.Time{}, time.Time{}, apierror.InvalidArgument("lifetime must be a positive duration")
	}
	life := lifetime.AsDuration()
	if maxLifetime != nil && maxLifetime.AsDuration() > 0 && life > maxLifetime.AsDuration() {
		life = maxLifetime.AsDuration()
	}
	start := now.Truncate(time.Second)
	if backdate != nil && backdate.AsDuration() > 0 {
		start = start.Add(-backdate.AsDuration())
	}
	end := start.Add(life)
	if !caEnd.IsZero() && end.After(caEnd) {
		end = caEnd
	}
	return start, end, nil
}

func isCAConfig(p *privatecapb.X509Parameters) bool { return p.GetCaOptions().GetIsCa() }

// trimPEM drops surrounding white space, for comparisons.
func trimPEM(s string) string { return strings.TrimSpace(s) }

// wrongState is the FAILED_PRECONDITION of a CA in the wrong state.
func wrongState(ca *privatecapb.CertificateAuthority, verb string, want ...privatecapb.CertificateAuthority_State) error {
	var names []string
	for _, w := range want {
		names = append(names, w.String())
	}
	return apierror.FailedPrecondition("CertificateAuthority %s is in state %s; it can be %s only in state %s",
		ca.GetName(), ca.GetState(), verb, strings.Join(names, " or "))
}
