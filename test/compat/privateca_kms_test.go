//go:build compat

package compat

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	privateca "cloud.google.com/go/security/privateca/apiv1"
	"cloud.google.com/go/security/privateca/apiv1/privatecapb"
	"google.golang.org/api/option"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// privateCAKMS is the official Cloud KMS client, and a key ring in
// us-central1 with an ASYMMETRIC_SIGN key of alg and an ENCRYPT_DECRYPT
// key: their version 1 and key names.
func privateCAKMS(t *testing.T, h *Harness, ring string, alg kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm) (*kms.KeyManagementClient, string, string) {
	t.Helper()
	ctx := h.Context()
	k, err := kms.NewKeyManagementClient(ctx, option.WithEndpoint(h.Endpoint(EnvKMS)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k.Close() })
	parent := "projects/" + h.Project() + "/locations/us-central1"
	r, err := k.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: parent, KeyRingId: ring})
	if err != nil {
		t.Fatal(err)
	}
	sk, err := k.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: r.GetName(), CryptoKeyId: "ca",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ASYMMETRIC_SIGN, VersionTemplate: &kmspb.CryptoKeyVersionTemplate{Algorithm: alg}}})
	if err != nil {
		t.Fatal(err)
	}
	ek, err := k.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: r.GetName(), CryptoKeyId: "cmek",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	return k, sk.GetName() + "/cryptoKeyVersions/1", ek.GetName()
}

func kmsCAConfig(sub *privatecapb.Subject) *privatecapb.CertificateConfig {
	return &privatecapb.CertificateConfig{SubjectConfig: &privatecapb.CertificateConfig_SubjectConfig{Subject: sub},
		X509Config: &privatecapb.X509Parameters{CaOptions: &privatecapb.X509Parameters_CaOptions{IsCa: proto.Bool(true)},
			KeyUsage: &privatecapb.KeyUsage{BaseKeyUsage: &privatecapb.KeyUsage_KeyUsageOptions{CertSign: true, CrlSign: true}}}}
}

func kmsLeaf(t *testing.T, cn string, dns ...string) *privatecapb.Certificate_Config {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	return &privatecapb.Certificate_Config{Config: &privatecapb.CertificateConfig{
		PublicKey:     &privatecapb.PublicKey{Format: privatecapb.PublicKey_PEM, Key: pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})},
		SubjectConfig: &privatecapb.CertificateConfig_SubjectConfig{Subject: &privatecapb.Subject{CommonName: cn}, SubjectAltName: &privatecapb.SubjectAltNames{DnsNames: dns}},
		X509Config: &privatecapb.X509Parameters{CaOptions: &privatecapb.X509Parameters_CaOptions{IsCa: proto.Bool(false)},
			KeyUsage: &privatecapb.KeyUsage{BaseKeyUsage: &privatecapb.KeyUsage_KeyUsageOptions{DigitalSignature: true},
				ExtendedKeyUsage: &privatecapb.KeyUsage_ExtendedKeyUsageOptions{ServerAuth: true}}},
	}}
}

// TestPrivateCAKMSKeysCMEKAndIssuanceRules (#1834) drives, through the
// official clients over gRPC and REST:
//   - a root CA whose key_spec is a Cloud KMS CryptoKeyVersion of this
//     instance's Cloud KMS: its certificate holds that version's public key
//     and is signed by it, as is every certificate it issues; a version in
//     another location is refused, and a disabled one cannot sign;
//   - a subject given as an rdn_sequence, kept in its order;
//   - a CMEK pool (encryption_spec): a DevOps pool or a signing key is
//     refused, and once the key's version is disabled the pool cannot
//     issue;
//   - requested_not_before_time, refused unless the pool allows it;
//   - subject_mode RDN_SEQUENCE keeping a CSR's RDN order, and
//     REFLECTED_SPIFFE refused for a caller without a SPIFFE identity;
//   - a pool's and a template's identity_constraints.cel_expression, and a
//     template's passthrough_extensions dropping the extended key usage.
//
// covers: google.cloud.security.privateca.v1.CertificateAuthorityService/CreateCaPool, google.cloud.security.privateca.v1.CertificateAuthorityService/CreateCertificateAuthority, google.cloud.security.privateca.v1.CertificateAuthorityService/CreateCertificate, google.cloud.security.privateca.v1.CertificateAuthorityService/CreateCertificateTemplate, google.cloud.security.privateca.v1.CertificateAuthorityService/GetCertificate
func TestPrivateCAKMSKeysCMEKAndIssuanceRules(t *testing.T) {
	h := New(t)
	clients := privateCAClients(t, h)
	for i, transport := range []string{"grpc", "rest"} {
		t.Run(transport, func(t *testing.T) {
			k, version, cmek := privateCAKMS(t, h, fmt.Sprintf("pca-%d-%d", time.Now().UnixNano()%1e6, i), kmspb.CryptoKeyVersion_EC_SIGN_P384_SHA384)
			privateCAKMSFlow(t, h, clients[transport], k, version, cmek)
		})
	}
}

func privateCAKMSFlow(t *testing.T, h *Harness, c *privateca.CertificateAuthorityClient, k *kms.KeyManagementClient, version, cmek string) {
	ctx := h.Context()
	loc := "projects/" + h.Project() + "/locations/us-central1"
	n := time.Now().UnixNano() % 1e6
	pool := fmt.Sprintf("%s/caPools/kms-%d", loc, n)

	createPool := func(id string, p *privatecapb.CaPool) error {
		op, err := c.CreateCaPool(ctx, &privatecapb.CreateCaPoolRequest{Parent: loc, CaPoolId: id, CaPool: p})
		if err == nil {
			_, err = op.Wait(ctx)
		}
		return err
	}
	err := createPool(fmt.Sprintf("devops-%d", n), &privatecapb.CaPool{Tier: privatecapb.CaPool_DEVOPS, EncryptionSpec: &privatecapb.EncryptionSpec{CloudKmsKey: cmek}})
	wantPrivateCACode(t, "a DevOps pool with encryption_spec", err, codes.InvalidArgument)
	err = createPool(fmt.Sprintf("signkey-%d", n), &privatecapb.CaPool{Tier: privatecapb.CaPool_ENTERPRISE,
		EncryptionSpec: &privatecapb.EncryptionSpec{CloudKmsKey: strings.TrimSuffix(version, "/cryptoKeyVersions/1")}})
	wantPrivateCACode(t, "encryption_spec with an ASYMMETRIC_SIGN key", err, codes.InvalidArgument)
	if err := createPool(fmt.Sprintf("kms-%d", n), &privatecapb.CaPool{Tier: privatecapb.CaPool_ENTERPRISE,
		EncryptionSpec: &privatecapb.EncryptionSpec{CloudKmsKey: cmek},
		IssuancePolicy: &privatecapb.CaPool_IssuancePolicy{AllowRequesterSpecifiedNotBeforeTime: true,
			IdentityConstraints: &privatecapb.CertificateIdentityConstraints{AllowSubjectPassthrough: proto.Bool(true), AllowSubjectAltNamesPassthrough: proto.Bool(true),
				CelExpression: &expr.Expr{Expression: `subject_alt_names.all(san, san.type == DNS && san.value.endsWith(".example.com"))`}}}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ca := pool + "/certificateAuthorities/root"
		if op, err := c.DisableCertificateAuthority(ctx, &privatecapb.DisableCertificateAuthorityRequest{Name: ca}); err == nil {
			_, _ = op.Wait(ctx)
		}
		if op, err := c.DeleteCertificateAuthority(ctx, &privatecapb.DeleteCertificateAuthorityRequest{Name: ca, SkipGracePeriod: true, IgnoreActiveCertificates: true}); err == nil {
			_, _ = op.Wait(ctx)
		}
		if op, err := c.DeleteCaPool(ctx, &privatecapb.DeleteCaPoolRequest{Name: pool}); err == nil {
			_ = op.Wait(ctx)
		}
	})

	createCA := func(id, kv string, sub *privatecapb.Subject) (*privatecapb.CertificateAuthority, error) {
		op, err := c.CreateCertificateAuthority(ctx, &privatecapb.CreateCertificateAuthorityRequest{Parent: pool, CertificateAuthorityId: id,
			CertificateAuthority: &privatecapb.CertificateAuthority{Type: privatecapb.CertificateAuthority_SELF_SIGNED, Lifetime: durationpb.New(72 * time.Hour),
				Config: kmsCAConfig(sub), KeySpec: &privatecapb.CertificateAuthority_KeyVersionSpec{
					KeyVersion: &privatecapb.CertificateAuthority_KeyVersionSpec_CloudKmsKeyVersion{CloudKmsKeyVersion: kv}}}})
		if err != nil {
			return nil, err
		}
		return op.Wait(ctx)
	}
	_, err = createCA("elsewhere", strings.Replace(version, "/us-central1/", "/europe-west1/", 1), &privatecapb.Subject{CommonName: "x"})
	wantPrivateCACode(t, "a Cloud KMS key in another location", err, codes.InvalidArgument)
	ca, err := createCA("root", version, &privatecapb.Subject{RdnSequence: []*privatecapb.RelativeDistinguishedName{
		{Attributes: []*privatecapb.AttributeTypeAndValue{{AttributeType: &privatecapb.AttributeTypeAndValue_Type{Type: privatecapb.AttributeType_COMMON_NAME}, Value: "KMS Root"}}},
		{Attributes: []*privatecapb.AttributeTypeAndValue{{AttributeType: &privatecapb.AttributeTypeAndValue_Type{Type: privatecapb.AttributeType_ORGANIZATION}, Value: "CloudBurrow"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	root, err := parsePEM(ca.GetPemCaCertificates()[0])
	if err != nil {
		t.Fatal(err)
	}
	pk, err := k.GetPublicKey(ctx, &kmspb.GetPublicKeyRequest{Name: version})
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode([]byte(pk.GetPem()))
	kpub, _ := x509.ParsePKIXPublicKey(blk.Bytes)
	if !kpub.(*ecdsa.PublicKey).Equal(root.PublicKey) || root.SignatureAlgorithm != x509.ECDSAWithSHA384 || root.CheckSignatureFrom(root) != nil {
		t.Fatalf("the root's key is not the Cloud KMS version's, or it is not signed by it (%v)", root.SignatureAlgorithm)
	}
	var seq pkix.RDNSequence
	if _, err := asn1.Unmarshal(root.RawSubject, &seq); err != nil || len(seq) != 2 || !seq[0][0].Type.Equal(asn1.ObjectIdentifier{2, 5, 4, 3}) {
		t.Fatalf("the root's subject %s is not the rdn_sequence CN then O", root.Subject)
	}
	if got := ca.GetCaCertificateDescriptions()[0].GetSubjectDescription().GetSubject(); got.GetCommonName() != "KMS Root" || len(got.GetRdnSequence()) != 2 {
		t.Errorf("the root's subject description = %v", got)
	}
	if op, err := c.EnableCertificateAuthority(ctx, &privatecapb.EnableCertificateAuthorityRequest{Name: ca.GetName()}); err != nil {
		t.Fatal(err)
	} else if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}

	issue := func(id string, crt *privatecapb.Certificate) (*privatecapb.Certificate, error) {
		return c.CreateCertificate(ctx, &privatecapb.CreateCertificateRequest{Parent: pool, CertificateId: id, Certificate: crt})
	}
	// The pool's CEL expression.
	_, err = issue("other", &privatecapb.Certificate{Lifetime: durationpb.New(time.Hour), CertificateConfig: kmsLeaf(t, "o", "o.other.org")})
	wantPrivateCACode(t, "a SAN the pool's cel_expression refuses", err, codes.InvalidArgument)
	// REFLECTED_SPIFFE: no caller of a local instance has a SPIFFE identity.
	_, err = issue("spiffe", &privatecapb.Certificate{Lifetime: durationpb.New(time.Hour), SubjectMode: privatecapb.SubjectRequestMode_REFLECTED_SPIFFE,
		CertificateConfig: kmsLeaf(t, "s", "s.example.com")})
	wantPrivateCACode(t, "REFLECTED_SPIFFE", err, codes.FailedPrecondition)

	// A template whose passthrough_extensions let through only the base key
	// usage and CA options, and whose own CEL expression needs one SAN.
	tmplName := fmt.Sprintf("%s/certificateTemplates/kmst-%d", loc, n)
	op, err := c.CreateCertificateTemplate(ctx, &privatecapb.CreateCertificateTemplateRequest{Parent: loc, CertificateTemplateId: fmt.Sprintf("kmst-%d", n),
		CertificateTemplate: &privatecapb.CertificateTemplate{
			IdentityConstraints: &privatecapb.CertificateIdentityConstraints{AllowSubjectPassthrough: proto.Bool(true), AllowSubjectAltNamesPassthrough: proto.Bool(true),
				CelExpression: &expr.Expr{Expression: `subject_alt_names.size() == 1`}},
			PassthroughExtensions: &privatecapb.CertificateExtensionConstraints{KnownExtensions: []privatecapb.CertificateExtensionConstraints_KnownCertificateExtension{
				privatecapb.CertificateExtensionConstraints_BASE_KEY_USAGE, privatecapb.CertificateExtensionConstraints_CA_OPTIONS}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteCertificateTemplate(ctx, &privatecapb.DeleteCertificateTemplateRequest{Name: tmplName}); err == nil {
			_ = op.Wait(ctx)
		}
	})
	_, err = issue("two", &privatecapb.Certificate{Lifetime: durationpb.New(time.Hour), CertificateTemplate: tmplName,
		CertificateConfig: kmsLeaf(t, "t", "a.example.com", "b.example.com")})
	wantPrivateCACode(t, "two SANs under the template's cel_expression", err, codes.InvalidArgument)
	nb := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	got, err := issue("leaf", &privatecapb.Certificate{Lifetime: durationpb.New(2 * time.Hour), CertificateTemplate: tmplName,
		RequestedNotBeforeTime: timestamppb.New(nb), CertificateConfig: kmsLeaf(t, "leaf", "leaf.example.com")})
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := parsePEM(got.GetPemCertificate())
	if err := leaf.CheckSignatureFrom(root); err != nil {
		t.Fatalf("the leaf does not verify against the Cloud KMS-held root: %v", err)
	}
	if !leaf.NotBefore.Equal(nb) || !leaf.NotAfter.Equal(nb.Add(2*time.Hour)) {
		t.Errorf("the leaf is valid %s to %s; want from the requested %s for 2h", leaf.NotBefore, leaf.NotAfter, nb)
	}
	if len(leaf.ExtKeyUsage) != 0 || leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		t.Errorf("passthrough_extensions: EKU %v, KU %v; want the EKU dropped and the KU kept", leaf.ExtKeyUsage, leaf.KeyUsage)
	}
	if g, err := c.GetCertificate(ctx, &privatecapb.GetCertificateRequest{Name: got.GetName()}); err != nil || g.GetPemCertificate() != got.GetPemCertificate() {
		t.Errorf("Get of the CMEK pool's certificate = %v", err)
	}

	// RDN_SEQUENCE keeps a CSR's RDN order: O before CN, which the named
	// fields would reorder.
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	raw, _ := asn1.Marshal(pkix.RDNSequence{
		{{Type: asn1.ObjectIdentifier{2, 5, 4, 10}, Value: "CloudBurrow"}},
		{{Type: asn1.ObjectIdentifier{2, 5, 4, 3}, Value: "rdn.example.com"}},
		{{Type: asn1.ObjectIdentifier{2, 5, 4, 11}, Value: "B"}, {Type: asn1.ObjectIdentifier{2, 5, 4, 11}, Value: "A"}},
	})
	csrDER, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{RawSubject: raw, DNSNames: []string{"rdn.example.com"}}, key)
	got, err = issue("rdn", &privatecapb.Certificate{Lifetime: durationpb.New(time.Hour), SubjectMode: privatecapb.SubjectRequestMode_RDN_SEQUENCE,
		CertificateConfig: &privatecapb.Certificate_PemCsr{PemCsr: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}))}})
	if err != nil {
		t.Fatal(err)
	}
	rl, _ := parsePEM(got.GetPemCertificate())
	if string(rl.RawSubject) != string(raw) || got.GetSubjectMode() != privatecapb.SubjectRequestMode_RDN_SEQUENCE {
		t.Errorf("RDN_SEQUENCE: the certificate's subject %s is not the CSR's sequence", rl.Subject)
	}

	// A disabled CMEK key version: the pool cannot keep a new certificate.
	ck, err := k.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: cmek})
	if err != nil {
		t.Fatal(err)
	}
	disable := func(name string) {
		t.Helper()
		if _, err := k.UpdateCryptoKeyVersion(ctx, &kmspb.UpdateCryptoKeyVersionRequest{
			CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: name, State: kmspb.CryptoKeyVersion_DISABLED},
			UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"state"}}}); err != nil {
			t.Fatal(err)
		}
	}
	disable(ck.GetPrimary().GetName())
	_, err = issue("nocmek", &privatecapb.Certificate{Lifetime: durationpb.New(time.Hour), CertificateConfig: kmsLeaf(t, "n", "n.example.com")})
	wantPrivateCACode(t, "issuing with the CMEK key disabled", err, codes.FailedPrecondition)
	// A disabled signing version cannot sign.
	disable(version)
	_, err = issue("nosign", &privatecapb.Certificate{Lifetime: durationpb.New(time.Hour), CertificateConfig: kmsLeaf(t, "n", "n.example.com")})
	wantPrivateCACode(t, "issuing with the CA's Cloud KMS version disabled", err, codes.FailedPrecondition)
}

func parsePEM(s string) (*x509.Certificate, error) {
	blk, _ := pem.Decode([]byte(s))
	if blk == nil {
		return nil, fmt.Errorf("not PEM")
	}
	return x509.ParseCertificate(blk.Bytes)
}

// TestPrivateCAPublishesToGCSBucket (#1834): a CA whose gcs_bucket names a
// bucket of this instance's Cloud Storage publishes its CA certificate and
// CRL there, as Google does: the access URLs are the bucket's objects,
// served by Cloud Storage, and readable through the official Storage
// client; a revocation republishes the CRL; a gs:// name and a missing
// bucket are refused.
//
// covers: google.cloud.security.privateca.v1.CertificateAuthorityService/CreateCertificateAuthority, google.cloud.security.privateca.v1.CertificateAuthorityService/RevokeCertificate
func TestPrivateCAPublishesToGCSBucket(t *testing.T) {
	h := New(t)
	// The sql shard serves Certificate Authority Service and Cloud Storage
	// together, under its own names.
	shardEndpoint(h, envSQLPrivateCA, EnvPrivateCA)
	shardEndpoint(h, envNoPubSubStorage, EnvStorage)
	c := privateCAClients(t, h)["grpc"]
	sc := storageClient(t, h)
	ctx := h.Context()
	bh := bucket(t, h, sc)
	bucketName := bh.BucketName()
	loc := "projects/" + h.Project() + "/locations/us-central1"
	n := time.Now().UnixNano() % 1e6
	pool := fmt.Sprintf("%s/caPools/gcs-%d", loc, n)
	op, err := c.CreateCaPool(ctx, &privatecapb.CreateCaPoolRequest{Parent: loc, CaPoolId: fmt.Sprintf("gcs-%d", n), CaPool: &privatecapb.CaPool{
		Tier: privatecapb.CaPool_ENTERPRISE, PublishingOptions: &privatecapb.CaPool_PublishingOptions{PublishCaCert: true, PublishCrl: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	create := func(id, bucket string) (*privatecapb.CertificateAuthority, error) {
		op, err := c.CreateCertificateAuthority(ctx, &privatecapb.CreateCertificateAuthorityRequest{Parent: pool, CertificateAuthorityId: id,
			CertificateAuthority: &privatecapb.CertificateAuthority{Type: privatecapb.CertificateAuthority_SELF_SIGNED, Lifetime: durationpb.New(72 * time.Hour),
				GcsBucket: bucket, Config: kmsCAConfig(&privatecapb.Subject{CommonName: "Bucket Root"}), KeySpec: &privatecapb.CertificateAuthority_KeyVersionSpec{
					KeyVersion: &privatecapb.CertificateAuthority_KeyVersionSpec_Algorithm{Algorithm: privatecapb.CertificateAuthority_EC_P256_SHA256}}}})
		if err != nil {
			return nil, err
		}
		return op.Wait(ctx)
	}
	t.Cleanup(func() {
		for _, id := range []string{"root"} {
			name := pool + "/certificateAuthorities/" + id
			if op, err := c.DisableCertificateAuthority(ctx, &privatecapb.DisableCertificateAuthorityRequest{Name: name}); err == nil {
				_, _ = op.Wait(ctx)
			}
			if op, err := c.DeleteCertificateAuthority(ctx, &privatecapb.DeleteCertificateAuthorityRequest{Name: name, SkipGracePeriod: true, IgnoreActiveCertificates: true}); err == nil {
				_, _ = op.Wait(ctx)
			}
		}
		if op, err := c.DeleteCaPool(ctx, &privatecapb.DeleteCaPoolRequest{Name: pool}); err == nil {
			_ = op.Wait(ctx)
		}
	})
	_, err = create("prefixed", "gs://"+bucketName)
	wantPrivateCACode(t, "gcs_bucket with a gs:// prefix", err, codes.InvalidArgument)
	_, err = create("missing", bucketName+"-missing")
	wantPrivateCACode(t, "gcs_bucket naming no bucket", err, codes.FailedPrecondition)
	ca, err := create("root", bucketName)
	if err != nil {
		t.Fatal(err)
	}
	storageAddr := os.Getenv(EnvStorage)
	u := ca.GetAccessUrls()
	prefix := "http://" + strings.TrimPrefix(strings.TrimPrefix(storageAddr, "http://"), "https://") + "/" + bucketName + "/"
	if !strings.HasPrefix(u.GetCaCertificateAccessUrl(), prefix) || len(u.GetCrlAccessUrls()) != 1 || !strings.HasPrefix(u.GetCrlAccessUrls()[0], prefix) {
		t.Fatalf("access URLs = %v; want objects under %s", u, prefix)
	}
	get := func(url string) []byte {
		t.Helper()
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", url, resp.StatusCode, b)
		}
		return b
	}
	if got := get(u.GetCaCertificateAccessUrl()); strings.TrimSpace(string(got)) != strings.TrimSpace(ca.GetPemCaCertificates()[0]) {
		t.Errorf("the published CA certificate is not the CA's")
	}
	objName := strings.TrimPrefix(u.GetCrlAccessUrls()[0], prefix)
	r, err := bh.Object(objName).NewReader(ctx)
	if err != nil {
		t.Fatalf("the CRL object %s: %v", objName, err)
	}
	crlPEM, _ := io.ReadAll(r)
	_ = r.Close()
	if blk, _ := pem.Decode(crlPEM); blk == nil || blk.Type != "X509 CRL" {
		t.Fatalf("the CRL object is not a PEM CRL: %q", crlPEM)
	}

	if op, err := c.EnableCertificateAuthority(ctx, &privatecapb.EnableCertificateAuthorityRequest{Name: ca.GetName()}); err != nil {
		t.Fatal(err)
	} else if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	crt, err := c.CreateCertificate(ctx, &privatecapb.CreateCertificateRequest{Parent: pool, CertificateId: "leaf",
		Certificate: &privatecapb.Certificate{Lifetime: durationpb.New(time.Hour), CertificateConfig: kmsLeaf(t, "leaf", "leaf.example.com")}})
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := parsePEM(crt.GetPemCertificate())
	if len(leaf.CRLDistributionPoints) != 1 || leaf.CRLDistributionPoints[0] != u.GetCrlAccessUrls()[0] || len(leaf.IssuingCertificateURL) != 1 {
		t.Errorf("the leaf's CDP %v and AIA %v are not the bucket's objects", leaf.CRLDistributionPoints, leaf.IssuingCertificateURL)
	}
	if _, err := c.RevokeCertificate(ctx, &privatecapb.RevokeCertificateRequest{Name: crt.GetName(), Reason: privatecapb.RevocationReason_KEY_COMPROMISE}); err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(get(u.GetCrlAccessUrls()[0]))
	crl, err := x509.ParseRevocationList(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(crl.RevokedCertificateEntries) != 1 || crl.RevokedCertificateEntries[0].SerialNumber.Cmp(leaf.SerialNumber) != 0 {
		t.Errorf("the republished CRL lists %d certificates; want the revoked leaf", len(crl.RevokedCertificateEntries))
	}
}
