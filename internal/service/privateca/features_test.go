package privateca

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/security/privateca/apiv1/privatecapb"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/service/kms"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

const loc = "projects/demo-project/locations/us-central1"

func wantCode(t *testing.T, what string, err error, c codes.Code) {
	t.Helper()
	if status.Code(err) != c {
		t.Fatalf("%s = %v; want %s", what, err, c)
	}
}

// kmsKeys makes a key ring with a signing key and an encryption key.
func kmsKeys(t *testing.T, k *kms.Server, alg kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm) (signVersion, encKey string) {
	t.Helper()
	ctx := context.Background()
	if _, err := k.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "ring"}); err != nil && status.Code(err) != codes.AlreadyExists {
		t.Fatal(err)
	}
	ring := loc + "/keyRings/ring"
	sk, err := k.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: "ca-" + strings.ToLower(alg.String()[:5]) + randomHex(2),
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ASYMMETRIC_SIGN, VersionTemplate: &kmspb.CryptoKeyVersionTemplate{Algorithm: alg}}})
	if err != nil {
		t.Fatal(err)
	}
	ek, err := k.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring, CryptoKeyId: "cmek-" + randomHex(2),
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	return sk.GetName() + "/cryptoKeyVersions/1", ek.GetName()
}

type memObjects struct {
	mu   sync.Mutex
	objs map[string][]byte
}

func (m *memObjects) WriteObject(_ context.Context, bucket, name, _ string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if bucket != "pki-bucket" {
		return status.Error(codes.NotFound, "no such bucket")
	}
	m.objs[bucket+"/"+name] = data
	return nil
}

func ecPublicPEM(t *testing.T) []byte {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

func leafConfig(t *testing.T, cn string, dns ...string) *privatecapb.Certificate_Config {
	return &privatecapb.Certificate_Config{Config: &privatecapb.CertificateConfig{
		PublicKey:     &privatecapb.PublicKey{Format: privatecapb.PublicKey_PEM, Key: ecPublicPEM(t)},
		SubjectConfig: &privatecapb.CertificateConfig_SubjectConfig{Subject: &privatecapb.Subject{CommonName: cn}, SubjectAltName: &privatecapb.SubjectAltNames{DnsNames: dns}},
		X509Config: &privatecapb.X509Parameters{CaOptions: &privatecapb.X509Parameters_CaOptions{IsCa: proto.Bool(false)},
			KeyUsage: &privatecapb.KeyUsage{BaseKeyUsage: &privatecapb.KeyUsage_KeyUsageOptions{DigitalSignature: true},
				ExtendedKeyUsage: &privatecapb.KeyUsage_ExtendedKeyUsageOptions{ServerAuth: true}}},
	}}
}

func caConfig(sub *privatecapb.Subject) *privatecapb.CertificateConfig {
	return &privatecapb.CertificateConfig{SubjectConfig: &privatecapb.CertificateConfig_SubjectConfig{Subject: sub},
		X509Config: &privatecapb.X509Parameters{CaOptions: &privatecapb.X509Parameters_CaOptions{IsCa: proto.Bool(true)},
			KeyUsage: &privatecapb.KeyUsage{BaseKeyUsage: &privatecapb.KeyUsage_KeyUsageOptions{CertSign: true, CrlSign: true}}}}
}

func TestKMSKeyCMEKBucketAndIssuanceRules(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	state := filepath.Join(dir, "privateca.json")
	k := kms.NewServer(store.NewMemory())
	s, err := NewServer(state)
	if err != nil {
		t.Fatal(err)
	}
	s.UseKMS(k)
	objs := &memObjects{objs: map[string][]byte{}}
	s.UseStorage(objs, func() string { return "127.0.0.1:9" })
	a := s.API()
	version, encKey := kmsKeys(t, k, kmspb.CryptoKeyVersion_EC_SIGN_P384_SHA384)

	pool := loc + "/caPools/p"
	if _, err := a.CreateCaPool(ctx, &privatecapb.CreateCaPoolRequest{Parent: loc, CaPoolId: "devops", CaPool: &privatecapb.CaPool{Tier: privatecapb.CaPool_DEVOPS,
		EncryptionSpec: &privatecapb.EncryptionSpec{CloudKmsKey: encKey}}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("a DevOps CMEK pool = %v; want INVALID_ARGUMENT", err)
	}
	signKey := strings.TrimSuffix(version, "/cryptoKeyVersions/1")
	wantCode(t, "CMEK with a signing key", func() error {
		_, err := a.CreateCaPool(ctx, &privatecapb.CreateCaPoolRequest{Parent: loc, CaPoolId: "bad", CaPool: &privatecapb.CaPool{Tier: privatecapb.CaPool_ENTERPRISE,
			EncryptionSpec: &privatecapb.EncryptionSpec{CloudKmsKey: signKey}}})
		return err
	}(), codes.InvalidArgument)
	if _, err := a.CreateCaPool(ctx, &privatecapb.CreateCaPoolRequest{Parent: loc, CaPoolId: "p", CaPool: &privatecapb.CaPool{Tier: privatecapb.CaPool_ENTERPRISE,
		EncryptionSpec:    &privatecapb.EncryptionSpec{CloudKmsKey: encKey},
		PublishingOptions: &privatecapb.CaPool_PublishingOptions{PublishCaCert: true, PublishCrl: true},
		IssuancePolicy: &privatecapb.CaPool_IssuancePolicy{AllowRequesterSpecifiedNotBeforeTime: true,
			IdentityConstraints: &privatecapb.CertificateIdentityConstraints{AllowSubjectPassthrough: proto.Bool(true), AllowSubjectAltNamesPassthrough: proto.Bool(true),
				CelExpression: &expr.Expr{Expression: `subject_alt_names.all(san, san.type == DNS && san.value.endsWith(".example.com"))`}},
			PassthroughExtensions: &privatecapb.CertificateExtensionConstraints{KnownExtensions: []privatecapb.CertificateExtensionConstraints_KnownCertificateExtension{
				privatecapb.CertificateExtensionConstraints_BASE_KEY_USAGE, privatecapb.CertificateExtensionConstraints_CA_OPTIONS}}}}}); err != nil {
		t.Fatal(err)
	}
	// A key in another location is refused.
	wantCode(t, "a KMS key in another location", func() error {
		_, err := a.CreateCertificateAuthority(ctx, &privatecapb.CreateCertificateAuthorityRequest{Parent: pool, CertificateAuthorityId: "x",
			CertificateAuthority: &privatecapb.CertificateAuthority{Type: privatecapb.CertificateAuthority_SELF_SIGNED, Lifetime: durationpb.New(48 * time.Hour),
				Config: caConfig(&privatecapb.Subject{CommonName: "x"}), KeySpec: &privatecapb.CertificateAuthority_KeyVersionSpec{
					KeyVersion: &privatecapb.CertificateAuthority_KeyVersionSpec_CloudKmsKeyVersion{CloudKmsKeyVersion: strings.Replace(version, "us-central1", "europe-west1", 1)}}}})
		return err
	}(), codes.InvalidArgument)
	rdn := &privatecapb.Subject{RdnSequence: []*privatecapb.RelativeDistinguishedName{
		{Attributes: []*privatecapb.AttributeTypeAndValue{{AttributeType: &privatecapb.AttributeTypeAndValue_Type{Type: privatecapb.AttributeType_ORGANIZATION}, Value: "CloudBurrow"}}},
		{Attributes: []*privatecapb.AttributeTypeAndValue{{AttributeType: &privatecapb.AttributeTypeAndValue_Type{Type: privatecapb.AttributeType_COMMON_NAME}, Value: "KMS Root"}}},
	}}
	op, err := a.CreateCertificateAuthority(ctx, &privatecapb.CreateCertificateAuthorityRequest{Parent: pool, CertificateAuthorityId: "root",
		CertificateAuthority: &privatecapb.CertificateAuthority{Type: privatecapb.CertificateAuthority_SELF_SIGNED, Lifetime: durationpb.New(48 * time.Hour),
			GcsBucket: "pki-bucket", Config: caConfig(rdn), KeySpec: &privatecapb.CertificateAuthority_KeyVersionSpec{
				KeyVersion: &privatecapb.CertificateAuthority_KeyVersionSpec_CloudKmsKeyVersion{CloudKmsKeyVersion: version}}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = op
	caName := pool + "/certificateAuthorities/root"
	ca, _ := a.GetCertificateAuthority(ctx, &privatecapb.GetCertificateAuthorityRequest{Name: caName})
	root, err := parsePEMCert(ca.GetPemCaCertificates()[0])
	if err != nil {
		t.Fatal(err)
	}
	pk, _ := k.GetPublicKey(ctx, &kmspb.GetPublicKeyRequest{Name: version})
	kpub, _ := parsePublicPEM(pk.GetPem())
	if !kpub.(*ecdsa.PublicKey).Equal(root.PublicKey) || root.SignatureAlgorithm != x509.ECDSAWithSHA384 || root.CheckSignatureFrom(root) != nil {
		t.Fatalf("the root's key is not the KMS version's, or it is not self-signed with it: %v", root.SignatureAlgorithm)
	}
	if root.Subject.String() != "CN=KMS Root,O=CloudBurrow" || len(ca.GetCaCertificateDescriptions()[0].GetSubjectDescription().GetSubject().GetRdnSequence()) != 2 {
		t.Fatalf("the root's subject = %s; want the rdn_sequence O then CN", root.Subject)
	}
	if !strings.HasPrefix(ca.GetAccessUrls().GetCaCertificateAccessUrl(), "http://127.0.0.1:9/pki-bucket/") ||
		len(ca.GetAccessUrls().GetCrlAccessUrls()) != 1 || !strings.HasPrefix(ca.GetAccessUrls().GetCrlAccessUrls()[0], "http://127.0.0.1:9/pki-bucket/") {
		t.Fatalf("access URLs = %v; want the bucket's", ca.GetAccessUrls())
	}
	obj := strings.TrimPrefix(ca.GetAccessUrls().GetCaCertificateAccessUrl(), "http://127.0.0.1:9/")
	if got := string(objs.objs[obj]); strings.TrimSpace(got) != strings.TrimSpace(ca.GetPemCaCertificates()[0]) {
		t.Fatalf("the bucket's ca.crt = %q", got)
	}
	if _, err := a.EnableCertificateAuthority(ctx, &privatecapb.EnableCertificateAuthorityRequest{Name: caName}); err != nil {
		t.Fatal(err)
	}
	// CEL: a SAN outside example.com is refused.
	_, err = a.CreateCertificate(ctx, &privatecapb.CreateCertificateRequest{Parent: pool, CertificateId: "bad",
		Certificate: &privatecapb.Certificate{Lifetime: durationpb.New(time.Hour), CertificateConfig: leafConfig(t, "a", "a.other.org")}})
	wantCode(t, "a SAN the CEL expression refuses", err, codes.InvalidArgument)
	// requested_not_before_time, passthrough (EKU dropped), KMS signature.
	nb := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	c, err := a.CreateCertificate(ctx, &privatecapb.CreateCertificateRequest{Parent: pool, CertificateId: "leaf",
		Certificate: &privatecapb.Certificate{Lifetime: durationpb.New(time.Hour), RequestedNotBeforeTime: timestamppb.New(nb),
			SubjectMode: privatecapb.SubjectRequestMode_RDN_SEQUENCE, CertificateConfig: leafConfig(t, "a", "a.example.com")}})
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := parsePEMCert(c.GetPemCertificate())
	if err := leaf.CheckSignatureFrom(root); err != nil {
		t.Fatalf("the leaf does not verify against the KMS root: %v", err)
	}
	if !leaf.NotBefore.Equal(nb) || !leaf.NotAfter.Equal(nb.Add(time.Hour)) {
		t.Fatalf("validity = %s..%s; want from %s", leaf.NotBefore, leaf.NotAfter, nb)
	}
	if len(leaf.ExtKeyUsage) != 0 || leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0 || c.GetSubjectMode() != privatecapb.SubjectRequestMode_RDN_SEQUENCE {
		t.Fatalf("passthrough: EKU %v KU %v; want EKU dropped, KU kept", leaf.ExtKeyUsage, leaf.KeyUsage)
	}
	_, err = a.CreateCertificate(ctx, &privatecapb.CreateCertificateRequest{Parent: pool, CertificateId: "spiffe",
		Certificate: &privatecapb.Certificate{Lifetime: durationpb.New(time.Hour), SubjectMode: privatecapb.SubjectRequestMode_REFLECTED_SPIFFE, CertificateConfig: leafConfig(t, "a", "a.example.com")}})
	wantCode(t, "REFLECTED_SPIFFE without a SPIFFE identity", err, codes.FailedPrecondition)

	// CMEK: the certificate is not in the clear in the state file.
	b, _ := os.ReadFile(state)
	if strings.Contains(string(b), "a.example.com") || !strings.Contains(string(b), "sealedCertificates") {
		t.Fatalf("the CMEK pool's certificate is in the clear in the state file")
	}
	// A restart reads it back through Cloud KMS.
	s2, err := NewServer(state)
	if err != nil {
		t.Fatal(err)
	}
	s2.UseKMS(k)
	got, err := s2.API().GetCertificate(ctx, &privatecapb.GetCertificateRequest{Name: pool + "/certificates/leaf"})
	if err != nil || got.GetPemCertificate() != c.GetPemCertificate() {
		t.Fatalf("after a restart, Get = %v; want the certificate", err)
	}
	// ... and the KMS-held CA still signs.
	if _, err := s2.API().CreateCertificate(ctx, &privatecapb.CreateCertificateRequest{Parent: pool, CertificateId: "leaf2",
		Certificate: &privatecapb.Certificate{Lifetime: durationpb.New(time.Hour), CertificateConfig: leafConfig(t, "b", "b.example.com")}}); err != nil {
		t.Fatal(err)
	}
	// Without Cloud KMS the certificates stay sealed.
	s3, _ := NewServer(state)
	_, err = s3.API().GetCertificate(ctx, &privatecapb.GetCertificateRequest{Name: pool + "/certificates/leaf"})
	wantCode(t, "Get without Cloud KMS", err, codes.FailedPrecondition)

	// A disabled signing version cannot sign.
	if _, err := k.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: version, State: kmspb.CryptoKeyVersion_DISABLED},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"state"}}}); err != nil {
		t.Fatal(err)
	}
	_, err = a.CreateCertificate(ctx, &privatecapb.CreateCertificateRequest{Parent: pool, CertificateId: "leaf3",
		Certificate: &privatecapb.Certificate{Lifetime: durationpb.New(time.Hour), CertificateConfig: leafConfig(t, "c", "c.example.com")}})
	wantCode(t, "issuing with a disabled KMS version", err, codes.FailedPrecondition)
}

func TestCELAndNotBeforeValidation(t *testing.T) {
	for _, bad := range []string{`subject.common_name ==`, `subject_alt_names.all(a, subject_alt_names.exists(b, b.value == a.value))`, `subject.common_name`} {
		_, err := compileIdentity("x", &privatecapb.CertificateIdentityConstraints{CelExpression: &expr.Expr{Expression: bad}})
		wantCode(t, "compile "+bad, err, codes.InvalidArgument)
	}
	s, _ := NewServer("")
	a := s.API()
	ctx := context.Background()
	if _, err := a.CreateCaPool(ctx, &privatecapb.CreateCaPoolRequest{Parent: loc, CaPoolId: "p", CaPool: &privatecapb.CaPool{Tier: privatecapb.CaPool_DEVOPS}}); err != nil {
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
	_, err := a.CreateCertificate(ctx, &privatecapb.CreateCertificateRequest{Parent: pool,
		Certificate: &privatecapb.Certificate{Lifetime: durationpb.New(time.Hour), RequestedNotBeforeTime: timestamppb.Now(), CertificateConfig: leafConfig(t, "a", "a.example.com")}})
	wantCode(t, "requested_not_before_time without allow_requester_specified_not_before_time", err, codes.InvalidArgument)
	_, err = a.CreateCertificateAuthority(ctx, &privatecapb.CreateCertificateAuthorityRequest{Parent: pool, CertificateAuthorityId: "b",
		CertificateAuthority: &privatecapb.CertificateAuthority{Type: privatecapb.CertificateAuthority_SELF_SIGNED, Lifetime: durationpb.New(48 * time.Hour),
			GcsBucket: "gs://x", Config: caConfig(&privatecapb.Subject{CommonName: "r"}), KeySpec: &privatecapb.CertificateAuthority_KeyVersionSpec{
				KeyVersion: &privatecapb.CertificateAuthority_KeyVersionSpec_Algorithm{Algorithm: privatecapb.CertificateAuthority_EC_P256_SHA256}}}})
	wantCode(t, "gcs_bucket with gs://", err, codes.InvalidArgument)
}
