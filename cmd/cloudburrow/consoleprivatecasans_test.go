package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"testing"

	"cloud.google.com/go/security/privateca/apiv1/privatecapb"
)

// Request certificate from a config takes custom SANs (OID=text lines) and
// a subject key ID, which the issued certificate carries (#1961); a line
// that is not OID=text is refused before the call.
func TestConsolePrivateCACustomSANsAndSubjectKeyID(t *testing.T) {
	ctx := context.Background()
	p, c := privateCAFixture(t)
	const project = "pca-sans"
	pool, err := p.Create(ctx, project, map[string]string{"caPoolId": "pool", "location": "us-central1", "tier": "ENTERPRISE",
		"maximumLifetimeDays": "90", "allowCsr": "true", "allowConfig": "true"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, project, []string{pool}, "createca", map[string]string{"certificateAuthorityId": "root", "type": "SELF_SIGNED",
		"commonName": "Root", "organization": "CloudBurrow", "algorithm": "EC_P256_SHA256", "lifetimeDays": "365"}); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, project, []string{pool, "certificateAuthorities", "root"}, "enable", nil); err != nil {
		t.Fatal(err)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	pubDER, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	pub := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
	values := map[string]string{"certificateId": "sans", "commonName": "a.example.com", "dnsNames": "a.example.com", "publicKey": pub,
		"usage": "server_auth", "lifetimeDays": "30", "customSans": "1.3.6.1.4.1.311.20.2.3=alice@example.com", "subjectKeyId": "abcdef01"}
	bad := map[string]string{}
	for k, v := range values {
		bad[k] = v
	}
	bad["customSans"] = "no equals sign"
	if err := p.ActAt(ctx, project, []string{pool}, "requestconfig", bad); err == nil {
		t.Error("a custom SAN line without OID=text was accepted")
	}
	if err := p.ActAt(ctx, project, []string{pool}, "requestconfig", values); err != nil {
		t.Fatal(err)
	}
	crt, err := c.GetCertificate(ctx, &privatecapb.GetCertificateRequest{Name: pool + "/certificates/sans"})
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode([]byte(crt.GetPemCertificate()))
	leaf, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(leaf.SubjectKeyId, []byte{0xab, 0xcd, 0xef, 0x01}) {
		t.Errorf("subject key ID = %x; want abcdef01", leaf.SubjectKeyId)
	}
	want, _ := asn1.MarshalWithParams("alice@example.com", "utf8")
	cs := crt.GetCertificateDescription().GetSubjectDescription().GetSubjectAltName().GetCustomSans()
	if len(cs) != 1 || !bytes.Equal(cs[0].GetValue(), want) || len(leaf.DNSNames) != 1 {
		t.Errorf("custom SANs = %v, DNS names %v; want the UPN beside a.example.com", cs, leaf.DNSNames)
	}
}
