//go:build go1.27

package kms

import (
	"context"
	"crypto/mldsa"
	"crypto/x509"
	"encoding/pem"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ML-DSA (#1946): each parameter set signs data, verified with the
// NIST_PQC key and with the PEM SubjectPublicKeyInfo; the external-μ
// variants sign a 64-byte μ and refuse data.
func TestMLDSASignatures(t *testing.T) {
	c := client(t)
	ctx := context.Background()
	ring, _ := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "mldsa"})
	data := []byte("post-quantum")
	for a, params := range map[kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm]mldsa.Parameters{
		kmspb.CryptoKeyVersion_PQ_SIGN_ML_DSA_44: mldsa.MLDSA44(),
		kmspb.CryptoKeyVersion_PQ_SIGN_ML_DSA_65: mldsa.MLDSA65(),
		kmspb.CryptoKeyVersion_PQ_SIGN_ML_DSA_87: mldsa.MLDSA87(),
	} {
		v := newKey(t, c, ring.GetName(), a.String()[8:], kmspb.CryptoKey_ASYMMETRIC_SIGN, a)
		pk, err := c.GetPublicKey(ctx, &kmspb.GetPublicKeyRequest{Name: v, PublicKeyFormat: kmspb.PublicKey_NIST_PQC})
		if err != nil || len(pk.GetPublicKey().GetData()) != params.PublicKeySize() {
			t.Fatalf("%s GetPublicKey = %v, %v", a, pk, err)
		}
		pub, err := mldsa.NewPublicKey(params, pk.GetPublicKey().GetData())
		if err != nil {
			t.Fatal(err)
		}
		pemKey, err := c.GetPublicKey(ctx, &kmspb.GetPublicKeyRequest{Name: v, PublicKeyFormat: kmspb.PublicKey_PEM})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := pem.Decode(pemKey.GetPublicKey().GetData())
		parsed, err := x509.ParsePKIXPublicKey(b.Bytes)
		if err != nil || !pub.Equal(parsed) {
			t.Errorf("%s PEM = %v, %v", a, parsed, err)
		}
		sig, err := c.AsymmetricSign(ctx, &kmspb.AsymmetricSignRequest{Name: v, Data: data, DataCrc32C: crc(data)})
		if err != nil || len(sig.GetSignature()) != params.SignatureSize() || !sig.GetVerifiedDataCrc32C() {
			t.Fatalf("%s AsymmetricSign = %v, %v", a, sig, err)
		}
		if err := mldsa.Verify(pub, data, sig.GetSignature(), nil); err != nil {
			t.Errorf("%s: %v", a, err)
		}
	}
	v := newKey(t, c, ring.GetName(), "mu", kmspb.CryptoKey_ASYMMETRIC_SIGN, kmspb.CryptoKeyVersion_PQ_SIGN_ML_DSA_65_EXTERNAL_MU)
	if _, err := c.AsymmetricSign(ctx, &kmspb.AsymmetricSignRequest{Name: v, Data: data}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("external μ with data = %v, want INVALID_ARGUMENT", err)
	}
	if _, err := c.AsymmetricSign(ctx, &kmspb.AsymmetricSignRequest{Name: v, Digest: &kmspb.Digest{Digest: &kmspb.Digest_ExternalMu{ExternalMu: make([]byte, 32)}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a short μ = %v, want INVALID_ARGUMENT", err)
	}
	sig, err := c.AsymmetricSign(ctx, &kmspb.AsymmetricSignRequest{Name: v, Digest: &kmspb.Digest{Digest: &kmspb.Digest_ExternalMu{ExternalMu: make([]byte, 64)}}})
	if err != nil || len(sig.GetSignature()) != mldsa.MLDSA65().SignatureSize() {
		t.Errorf("external μ = %v, %v", sig, err)
	}
}
