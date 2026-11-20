package privateca

import (
	"bytes"
	"context"
	"encoding/asn1"
	"testing"
	"time"

	"cloud.google.com/go/security/privateca/apiv1/privatecapb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/durationpb"
)

// Custom SANs are written as otherNames beside the DNS names, and read
// back as custom_sans; a value that is not a UTF8String is refused; a
// requested subject key ID is the certificate's, and one not in lowercase
// hexadecimal is refused (#1961).
func TestCustomSANsAndSubjectKeyID(t *testing.T) {
	s, _ := NewServer("")
	a := s.API()
	ctx := context.Background()
	if _, err := a.CreateCaPool(ctx, &privatecapb.CreateCaPoolRequest{Parent: loc, CaPoolId: "p", CaPool: &privatecapb.CaPool{Tier: privatecapb.CaPool_ENTERPRISE}}); err != nil {
		t.Fatal(err)
	}
	pool := loc + "/caPools/p"
	if _, err := a.CreateCertificateAuthority(ctx, &privatecapb.CreateCertificateAuthorityRequest{Parent: pool, CertificateAuthorityId: "r",
		CertificateAuthority: &privatecapb.CertificateAuthority{Type: privatecapb.CertificateAuthority_SELF_SIGNED, Lifetime: durationpb.New(48 * time.Hour),
			Config: caConfig(&privatecapb.Subject{CommonName: "r"}), KeySpec: &privatecapb.CertificateAuthority_KeyVersionSpec{
				KeyVersion: &privatecapb.CertificateAuthority_KeyVersionSpec_Algorithm{Algorithm: privatecapb.CertificateAuthority_EC_P256_SHA256}}}}); err != nil {
		t.Fatal(err)
	}
	_, _ = a.EnableCertificateAuthority(ctx, &privatecapb.EnableCertificateAuthorityRequest{Name: pool + "/certificateAuthorities/r"})
	upn := asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 311, 20, 2, 3}
	val, _ := asn1.MarshalWithParams("user@example.com", "utf8")
	issue := func(id string, custom []*privatecapb.X509Extension, kid string) (*privatecapb.Certificate, error) {
		cfg := leafConfig(t, "a", "a.example.com")
		cfg.Config.SubjectConfig.SubjectAltName.CustomSans = custom
		if kid != "" {
			cfg.Config.SubjectKeyId = &privatecapb.CertificateConfig_KeyId{KeyId: kid}
		}
		return a.CreateCertificate(ctx, &privatecapb.CreateCertificateRequest{Parent: pool, CertificateId: id,
			Certificate: &privatecapb.Certificate{Lifetime: durationpb.New(time.Hour), CertificateConfig: cfg}})
	}
	_, err := issue("bad", []*privatecapb.X509Extension{{ObjectId: oidProto(upn), Value: []byte("raw")}}, "")
	wantCode(t, "a custom SAN that is not a UTF8String", err, codes.InvalidArgument)
	_, err = issue("badkid", nil, "ABCD")
	wantCode(t, "an uppercase key ID", err, codes.InvalidArgument)
	c, err := issue("leaf", []*privatecapb.X509Extension{{ObjectId: oidProto(upn), Value: val}}, "0102030405")
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := parsePEMCert(c.GetPemCertificate())
	if err != nil {
		t.Fatal(err)
	}
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "a.example.com" {
		t.Errorf("DNS names = %v; want a.example.com beside the custom SAN", leaf.DNSNames)
	}
	if !bytes.Equal(leaf.SubjectKeyId, []byte{1, 2, 3, 4, 5}) || c.GetCertificateDescription().GetSubjectKeyId().GetKeyId() != "0102030405" {
		t.Errorf("subject key ID = %x (%v); want 0102030405", leaf.SubjectKeyId, c.GetCertificateDescription().GetSubjectKeyId())
	}
	got := c.GetCertificateDescription().GetSubjectDescription().GetSubjectAltName().GetCustomSans()
	if len(got) != 1 || !oid(got[0].GetObjectId()).Equal(upn) || !bytes.Equal(got[0].GetValue(), val) {
		t.Errorf("custom_sans described = %v; want the UPN otherName", got)
	}
}
