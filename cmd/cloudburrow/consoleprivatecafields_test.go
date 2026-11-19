package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"strings"
	"testing"
	"time"

	kmsapi "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/security/privateca/apiv1/privatecapb"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

func privateCAPropsText(d console.Detail) string {
	var b strings.Builder
	for _, s := range d.Sections {
		for _, g := range s.Groups {
			for _, p := range g.Properties {
				fmt.Fprintf(&b, "%s: %s\n", p.Label, p.Value)
			}
		}
	}
	return b.String()
}

// The console's controls for #1834's fields: Create CA pool with a CMEK
// key, a requester-specified not-before time, a CEL expression and
// passthrough extensions; Create CA with a Cloud KMS key version and a
// subject RDN sequence; Request certificate from a config with an RDN
// sequence, the RDN_SEQUENCE subject mode and a requested not-before time;
// Create and Edit template with a CEL expression and passthrough
// extensions. The official client reads each back, and each page shows it.
func TestConsolePrivateCAKMSCMEKRDNAndCEL(t *testing.T) {
	ctx := context.Background()
	ks := &kmsService{cfg: config.Config{BindAddress: "127.0.0.1"}, db: store.NewMemory()}
	if err := ks.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ks.Stop(context.Background()) })
	k, err := kmsapi.NewKeyManagementClient(ctx, clientOpts(ks.Addr())...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k.Close() })
	const project = "pca-fields"
	ring, err := k.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + project + "/locations/us-central1", KeyRingId: "ca"})
	if err != nil {
		t.Fatal(err)
	}
	sk, err := k.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "sign", CryptoKey: &kmspb.CryptoKey{
		Purpose: kmspb.CryptoKey_ASYMMETRIC_SIGN, VersionTemplate: &kmspb.CryptoKeyVersionTemplate{Algorithm: kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256}}})
	if err != nil {
		t.Fatal(err)
	}
	ek, err := k.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "cmek", CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	version := sk.GetName() + "/cryptoKeyVersions/1"

	p, c := privateCAFixtureWith(t, ks)
	tp := privateCATemplatesProvider{svc: p.svc}
	const cel = `subject_alt_names.all(san, san.type == DNS && san.value.endsWith(".example.com"))`
	pool, err := p.Create(ctx, project, map[string]string{"caPoolId": "fields", "location": "us-central1", "tier": "ENTERPRISE",
		"encryptionKey": ek.GetName(), "allowRequesterNotBefore": "true", "publishCaCert": "true", "celExpression": cel, "passthroughExtensions": "EXTENDED_KEY_USAGE, 1.2.3.4"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.GetCaPool(ctx, &privatecapb.GetCaPoolRequest{Name: pool})
	ip := got.GetIssuancePolicy()
	if err != nil || got.GetEncryptionSpec().GetCloudKmsKey() != ek.GetName() || !ip.GetAllowRequesterSpecifiedNotBeforeTime() ||
		ip.GetIdentityConstraints().GetCelExpression().GetExpression() != cel ||
		privateCAExtensionsText(ip.GetPassthroughExtensions()) != "EXTENDED_KEY_USAGE,1.2.3.4" ||
		!got.GetPublishingOptions().GetPublishCaCert() || got.GetPublishingOptions().GetPublishCrl() {
		t.Fatalf("the pool made on the page = %v, %v", got, err)
	}
	if _, err := p.Create(ctx, project, map[string]string{"caPoolId": "devops", "location": "us-central1", "tier": "DEVOPS", "encryptionKey": ek.GetName()}); err == nil {
		t.Errorf("a DevOps pool took a CMEK key")
	}
	d, _ := p.Detail(ctx, project, []string{pool})
	text := privateCAPropsText(d)
	for _, want := range []string{"Customer-managed encryption key: " + ek.GetName(), "Requester-specified not-before time: Allowed",
		"Identity CEL expression: " + cel, "Passthrough extensions: EXTENDED_KEY_USAGE,1.2.3.4"} {
		if !strings.Contains(text, want) {
			t.Errorf("the pool's page does not show %q:\n%s", want, text)
		}
	}

	if err := p.ActAt(ctx, project, []string{pool}, "createca", map[string]string{"certificateAuthorityId": "root", "type": "SELF_SIGNED",
		"rdnSequence": "C=US\nO=CloudBurrow\nOU=Ops+OU=Dev\nCN=Fields Root", "algorithm": "RSA_PKCS1_4096_SHA256", "kmsKeyVersion": version, "lifetimeDays": "365"}); err != nil {
		t.Fatal(err)
	}
	caPath := []string{pool, "certificateAuthorities", "root"}
	if err := p.ActAt(ctx, project, caPath, "enable", nil); err != nil {
		t.Fatal(err)
	}
	ca, err := c.GetCertificateAuthority(ctx, &privatecapb.GetCertificateAuthorityRequest{Name: pool + "/certificateAuthorities/root"})
	if err != nil || ca.GetKeySpec().GetCloudKmsKeyVersion() != version {
		t.Fatalf("the CA made on the page = %v, %v; want its key in %s", ca, err, version)
	}
	rootCert, _ := pem.Decode([]byte(ca.GetPemCaCertificates()[0]))
	rc, err := x509.ParseCertificate(rootCert.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pk, _ := k.GetPublicKey(ctx, &kmspb.GetPublicKeyRequest{Name: version})
	if blk, _ := pem.Decode([]byte(pk.GetPem())); blk == nil || string(blk.Bytes) != string(rc.RawSubjectPublicKeyInfo) {
		t.Errorf("the CA's public key is not the Cloud KMS version's")
	}
	if s := rc.Subject.String(); s != "CN=Fields Root,OU=Dev+OU=Ops,O=CloudBurrow,C=US" {
		t.Errorf("the CA's subject is %s; want the RDN sequence in its order", s)
	}
	d, _ = p.Detail(ctx, project, caPath)
	if text := privateCAPropsText(d); !strings.Contains(text, "Cloud KMS key version: "+version) {
		t.Errorf("the CA's page does not show its Cloud KMS key version:\n%s", text)
	}
	err = p.ActAt(ctx, project, []string{pool}, "createca", map[string]string{"certificateAuthorityId": "bucket", "type": "SELF_SIGNED",
		"commonName": "Bucket", "algorithm": "EC_P256_SHA256", "lifetimeDays": "30", "gcsBucket": "no-such-bucket"})
	if err == nil || !strings.Contains(err.Error(), "gcs_bucket") {
		t.Errorf("Create CA with a publishing bucket this instance cannot reach = %v; want gcs_bucket passed to the service and refused there", err)
	}

	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	pub := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	nb := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	request := func(id, dns string) error {
		return p.ActAt(ctx, project, []string{pool}, "requestconfig", map[string]string{"certificateId": id, "rdnSequence": "O=CloudBurrow\nCN=" + dns,
			"dnsNames": dns, "publicKey": pub, "usage": "server_auth", "lifetimeDays": "7", "subjectMode": "RDN_SEQUENCE",
			"requestedNotBefore": nb.Format(time.RFC3339)})
	}
	if err := request("bad", "web.other.org"); err == nil {
		t.Errorf("a certificate the pool's CEL expression refuses was issued")
	}
	if err := request("web", "web.example.com"); err != nil {
		t.Fatal(err)
	}
	crt, err := c.GetCertificate(ctx, &privatecapb.GetCertificateRequest{Name: pool + "/certificates/web"})
	if err != nil || crt.GetSubjectMode() != privatecapb.SubjectRequestMode_RDN_SEQUENCE || !crt.GetRequestedNotBeforeTime().AsTime().Equal(nb) {
		t.Fatalf("the certificate requested on the page = %v, %v", crt, err)
	}
	blk, _ := pem.Decode([]byte(crt.GetPemCertificate()))
	leaf, _ := x509.ParseCertificate(blk.Bytes)
	if !leaf.NotBefore.Equal(nb) || leaf.Subject.String() != "CN=web.example.com,O=CloudBurrow" {
		t.Errorf("the leaf's not-before %v and subject %s; want %v and the RDN sequence", leaf.NotBefore, leaf.Subject, nb)
	}
	d, _ = p.Detail(ctx, project, []string{pool, "certificates", "web"})
	if text := privateCAPropsText(d); !strings.Contains(text, "Subject mode: RDN_SEQUENCE") {
		t.Errorf("the certificate's page does not show its subject mode:\n%s", text)
	}

	tmpl, err := tp.Create(ctx, project, map[string]string{"certificateTemplateId": "cel", "location": "us-central1", "usages": "digital_signature",
		"celExpression": cel, "passthroughExtensions": "BASE_KEY_USAGE,1.2.3.5"})
	if err != nil {
		t.Fatal(err)
	}
	gt, err := c.GetCertificateTemplate(ctx, &privatecapb.GetCertificateTemplateRequest{Name: tmpl})
	if err != nil || gt.GetIdentityConstraints().GetCelExpression().GetExpression() != cel ||
		privateCAExtensionsText(gt.GetPassthroughExtensions()) != "BASE_KEY_USAGE,1.2.3.5" {
		t.Fatalf("the template made on the page = %v, %v", gt, err)
	}
	const cel2 = `subject.common_name == "x"`
	if err := tp.ActAt(ctx, project, []string{tmpl}, "edit", map[string]string{"usages": "digital_signature", "celExpression": cel2,
		"passthroughExtensions": "CA_OPTIONS"}); err != nil {
		t.Fatal(err)
	}
	gt, _ = c.GetCertificateTemplate(ctx, &privatecapb.GetCertificateTemplateRequest{Name: tmpl})
	if gt.GetIdentityConstraints().GetCelExpression().GetExpression() != cel2 || privateCAExtensionsText(gt.GetPassthroughExtensions()) != "CA_OPTIONS" ||
		!gt.GetIdentityConstraints().GetAllowSubjectPassthrough() {
		t.Errorf("the template after Edit = %v", gt)
	}
	d, _ = tp.Detail(ctx, project, []string{tmpl})
	if text := privateCAPropsText(d); !strings.Contains(text, "Identity CEL expression: "+cel2) || !strings.Contains(text, "Passthrough extensions: CA_OPTIONS") {
		t.Errorf("the template's page:\n%s", text)
	}
}

func TestPrivateCAFormParsers(t *testing.T) {
	seq, err := privateCAParseRDN("C=US\n o = Example \nOU=a+1.2.3=b\n")
	if err != nil || len(seq) != 3 || privateCARDNText(seq) != "C=US\nO=Example\nOU=a+1.2.3=b" {
		t.Errorf("RDN = %v, %v", seq, err)
	}
	for _, bad := range []string{"CN", "XX=1", "CN="} {
		if _, err := privateCAParseRDN(bad); err == nil {
			t.Errorf("RDN %q was taken", bad)
		}
	}
	if e, err := privateCAParseExtensions(""); e != nil || err != nil {
		t.Errorf("empty extensions = %v, %v", e, err)
	}
	if _, err := privateCAParseExtensions("BOGUS"); err == nil {
		t.Errorf("an unknown extension was taken")
	}
	if _, err := privateCAParseTime("t", "yesterday"); err == nil {
		t.Errorf("a non-RFC 3339 time was taken")
	}
	if privateCAIdentity(map[string]string{}) != nil {
		t.Errorf("default identity constraints are not omitted")
	}
}
