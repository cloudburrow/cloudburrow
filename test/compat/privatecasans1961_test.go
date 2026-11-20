//go:build compat

package compat

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"fmt"
	"testing"
	"time"

	"cloud.google.com/go/security/privateca/apiv1/privatecapb"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/durationpb"
)

// otherName is RFC 5280's otherName GeneralName, as crypto/x509 leaves it.
type otherName struct {
	TypeID asn1.ObjectIdentifier
	Value  asn1.RawValue `asn1:"explicit,tag:0"`
}

// TestPrivateCACustomSANsAndSubjectKeyID (#1961), over gRPC and REST with
// the official privateca/apiv1 client: a certificate issued from a config
// with subject_alt_name.custom_sans (an object ID and an ASN.1 UTF8String,
// here a Microsoft UPN) and a subject_key_id carries, parsed with
// crypto/x509, a subjectAltName extension holding the DNS name and the
// custom SAN as an otherName with that type ID and value, and exactly the
// requested subjectKeyIdentifier; the description reports both. A custom
// SAN whose value is not a UTF8String, and a key ID not in lowercase
// hexadecimal, are INVALID_ARGUMENT.
//
// covers: google.cloud.security.privateca.v1.CertificateAuthorityService/CreateCertificate
func TestPrivateCACustomSANsAndSubjectKeyID(t *testing.T) {
	h := New(t)
	for transport, c := range privateCAClients(t, h) {
		t.Run(transport, func(t *testing.T) {
			ctx := h.Context()
			loc := "projects/" + h.Project() + "/locations/us-central1"
			pool := loc + "/caPools/" + fmt.Sprintf("sans-%s-%d", transport, time.Now().UnixNano()%1e9)
			pop, err := c.CreateCaPool(ctx, &privatecapb.CreateCaPoolRequest{Parent: loc, CaPoolId: pool[len(loc+"/caPools/"):],
				CaPool: &privatecapb.CaPool{Tier: privatecapb.CaPool_ENTERPRISE}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pop.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			cop, err := c.CreateCertificateAuthority(ctx, &privatecapb.CreateCertificateAuthorityRequest{Parent: pool, CertificateAuthorityId: "root",
				CertificateAuthority: &privatecapb.CertificateAuthority{Type: privatecapb.CertificateAuthority_SELF_SIGNED, Config: caConfig("SANs Root"),
					Lifetime: durationpb.New(10 * 24 * time.Hour),
					KeySpec:  &privatecapb.CertificateAuthority_KeyVersionSpec{KeyVersion: &privatecapb.CertificateAuthority_KeyVersionSpec_Algorithm{Algorithm: privatecapb.CertificateAuthority_EC_P256_SHA256}}}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := cop.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			eop, err := c.EnableCertificateAuthority(ctx, &privatecapb.EnableCertificateAuthorityRequest{Name: pool + "/certificateAuthorities/root"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := eop.Wait(ctx); err != nil {
				t.Fatal(err)
			}

			k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
			upn := asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 311, 20, 2, 3}
			upnID := &privatecapb.ObjectId{ObjectIdPath: []int32{1, 3, 6, 1, 4, 1, 311, 20, 2, 3}}
			value, _ := asn1.MarshalWithParams("alice@example.com", "utf8")
			issue := func(id string, custom []*privatecapb.X509Extension, kid string) (*privatecapb.Certificate, error) {
				cfg := &privatecapb.CertificateConfig{
					PublicKey: &privatecapb.PublicKey{Format: privatecapb.PublicKey_PEM, Key: pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})},
					SubjectConfig: &privatecapb.CertificateConfig_SubjectConfig{Subject: &privatecapb.Subject{CommonName: "alice"},
						SubjectAltName: &privatecapb.SubjectAltNames{DnsNames: []string{"alice.example.com"}, CustomSans: custom}},
					X509Config: &privatecapb.X509Parameters{KeyUsage: &privatecapb.KeyUsage{BaseKeyUsage: &privatecapb.KeyUsage_KeyUsageOptions{DigitalSignature: true}}},
				}
				if kid != "" {
					cfg.SubjectKeyId = &privatecapb.CertificateConfig_KeyId{KeyId: kid}
				}
				return c.CreateCertificate(ctx, &privatecapb.CreateCertificateRequest{Parent: pool, CertificateId: id,
					Certificate: &privatecapb.Certificate{Lifetime: durationpb.New(time.Hour), CertificateConfig: &privatecapb.Certificate_Config{Config: cfg}}})
			}
			_, err = issue("bad-san", []*privatecapb.X509Extension{{ObjectId: upnID, Value: []byte("not asn.1")}}, "")
			wantPrivateCACode(t, "a custom SAN that is not a UTF8String", err, codes.InvalidArgument)
			_, err = issue("bad-kid", nil, "NOT-HEX")
			wantPrivateCACode(t, "a subject key ID not in lowercase hexadecimal", err, codes.InvalidArgument)

			const kid = "00112233445566778899aabbccddeeff00112233"
			got, err := issue("leaf", []*privatecapb.X509Extension{{ObjectId: upnID, Value: value}}, kid)
			if err != nil {
				t.Fatalf("CreateCertificate with custom SANs and a subject key ID: %v", err)
			}
			cert := parsePEMCertificate(t, got.GetPemCertificate())
			if len(cert.DNSNames) != 1 || cert.DNSNames[0] != "alice.example.com" {
				t.Errorf("DNS names = %v; want alice.example.com", cert.DNSNames)
			}
			if fmt.Sprintf("%x", cert.SubjectKeyId) != kid {
				t.Errorf("subjectKeyIdentifier = %x; want %s", cert.SubjectKeyId, kid)
			}
			var others []otherName
			for _, e := range cert.Extensions {
				if !e.Id.Equal(asn1.ObjectIdentifier{2, 5, 29, 17}) {
					continue
				}
				var names []asn1.RawValue
				if _, err := asn1.Unmarshal(e.Value, &names); err != nil {
					t.Fatalf("subjectAltName: %v", err)
				}
				for _, n := range names {
					if n.Class == asn1.ClassContextSpecific && n.Tag == 0 {
						var o otherName
						if _, err := asn1.UnmarshalWithParams(n.FullBytes, &o, "tag:0"); err != nil {
							t.Fatalf("otherName: %v", err)
						}
						others = append(others, o)
					}
				}
			}
			if len(others) != 1 || !others[0].TypeID.Equal(upn) || !bytes.Equal(others[0].Value.Bytes, value) {
				t.Errorf("otherNames = %+v; want the UPN %s = alice@example.com", others, upn)
			}
			d := got.GetCertificateDescription()
			if d.GetSubjectKeyId().GetKeyId() != kid {
				t.Errorf("description subject_key_id = %v; want %s", d.GetSubjectKeyId(), kid)
			}
			if cs := d.GetSubjectDescription().GetSubjectAltName().GetCustomSans(); len(cs) != 1 || !bytes.Equal(cs[0].GetValue(), value) {
				t.Errorf("description custom_sans = %v; want the UPN", cs)
			}
		})
	}
}

// TestTerraformPrivateCACertificateSubjectKeyID (#1961): a
// google_privateca_certificate with config.subject_key_id applies with
// egress to Google blocked; the certificate the official client reads,
// parsed with crypto/x509, carries that subjectKeyIdentifier; the plan is
// clean; destroy removes it.
// covers: google.cloud.security.privateca.v1.CertificateAuthorityService/CreateCertificate, google.cloud.security.privateca.v1.CertificateAuthorityService/GetCertificate
func TestTerraformPrivateCACertificateSubjectKeyID(t *testing.T) {
	h := New(t)
	c := privateCAClients(t, h)["grpc"] // skips without CLOUDBURROW_TEST_PRIVATECA
	ctx := h.Context()
	m := newTFModule(t)
	n := time.Now().UnixNano() % 1e6
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	pubDER, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	pub := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
	const kid = "0a0b0c0d0e0f101112131415161718191a1b1c1d"
	m.write(fmt.Sprintf(`
resource "google_privateca_ca_pool" "p" {
  project  = %[1]q
  location = "us-central1"
  name     = "tfski-%[2]d"
  tier     = "ENTERPRISE"
}
resource "google_privateca_certificate_authority" "root" {
  project                                = %[1]q
  location                               = "us-central1"
  pool                                   = google_privateca_ca_pool.p.name
  certificate_authority_id               = "root"
  deletion_protection                    = false
  skip_grace_period                      = true
  ignore_active_certificates_on_deletion = true
  lifetime                               = "864000s"
  config {
    subject_config {
      subject {
        organization = "CloudBurrow"
        common_name  = "SKI Root"
      }
    }
    x509_config {
      ca_options {
        is_ca = true
      }
      key_usage {
        base_key_usage {
          cert_sign = true
          crl_sign  = true
        }
        extended_key_usage {}
      }
    }
  }
  key_spec {
    algorithm = "EC_P256_SHA256"
  }
}
resource "google_privateca_certificate" "c" {
  project               = %[1]q
  location              = "us-central1"
  pool                  = google_privateca_ca_pool.p.name
  name                  = "tfski"
  certificate_authority = google_privateca_certificate_authority.root.certificate_authority_id
  lifetime              = "3600s"
  config {
    subject_config {
      subject {
        common_name = "ski.example.com"
      }
      subject_alt_name {
        dns_names = ["ski.example.com"]
      }
    }
    x509_config {
      ca_options {
        is_ca = false
      }
      key_usage {
        base_key_usage {
          digital_signature = true
        }
        extended_key_usage {}
      }
    }
    subject_key_id {
      key_id = %[3]q
    }
    public_key {
      format = "PEM"
      key    = base64encode(%[4]q)
    }
  }
}
`, h.Project(), n, kid, pub))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	name := fmt.Sprintf("projects/%s/locations/us-central1/caPools/tfski-%d/certificates/tfski", h.Project(), n)
	got, err := c.GetCertificate(ctx, &privatecapb.GetCertificateRequest{Name: name})
	if err != nil {
		t.Fatalf("GetCertificate %s: %v", name, err)
	}
	if cert := parsePEMCertificate(t, got.GetPemCertificate()); fmt.Sprintf("%x", cert.SubjectKeyId) != kid {
		t.Errorf("the Terraform certificate's subjectKeyIdentifier = %x; want %s", cert.SubjectKeyId, kid)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
}
