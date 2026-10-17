//go:build compat

package compat

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"hash/crc32"
	"testing"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	kwp "github.com/tink-crypto/tink-go/v2/kwp/subtle"
	"google.golang.org/grpc/codes"
)

// TestKMSTrustedKeyWrapping (#1521), through the official KMS clients over
// gRPC and REST, as kms v1.35.0's protos and the single-tenant HSM guide
// define trusted key wrapping:
//
//   - an ACTIVE single-tenant HSM instance with key portability, its three
//     generated RSA 2048 approver keys registered (2 of 3), each signing its
//     challenge;
//   - an import job at HSM_SINGLE_TENANT naming the instance in
//     crypto_key_backend (RSA_OAEP_3072_SHA256_AES_256), through which a
//     256-bit wrapping key known on premises is imported into an
//     AES_WRAPPING (AES_256_KWP) key;
//   - an upgrade_key_trust proposal naming that version and one approver's
//     two-factor key, approved with that key's required challenge and a
//     quorum of 2, executed: the version is hsm_trusted;
//   - a symmetric (AES_256_GCM) and an asymmetric (EC_SIGN_P256_SHA256)
//     key, wrapped on premises with AES-KWP (Tink's, RFC 5649) under the
//     trusted key, imported with ImportTrustedKeyWrappedCryptoKeyVersion:
//     RawEncrypt's ciphertext opens locally with the original key, and
//     RawDecrypt opens a locally made one; AsymmetricSign's signature
//     verifies with the original public key;
//   - ExportTrustedKeyWrappedCryptoKeyVersion returns the signing key
//     wrapped under the trusted key, with its CRC32C, which unwraps locally
//     to the PKCS #8 DER imported (the encoding of asymmetric material is
//     UNVERIFIED: Cloud KMS documents it for ordinary import only);
//   - refused: an export with a wrapping key not yet trusted, a wrapped key
//     under another key, trusted_wrapping_enabled on an ENCRYPT_DECRYPT key.
//
// covers: google.cloud.kms.v1.KeyManagementService/ImportTrustedKeyWrappedCryptoKeyVersion, google.cloud.kms.v1.KeyManagementService/ExportTrustedKeyWrappedCryptoKeyVersion
// covers: google.cloud.kms.v1.KeyManagementService/CreateImportJob, google.cloud.kms.v1.KeyManagementService/ImportCryptoKeyVersion
// covers: google.cloud.kms.v1.HsmManagement/CreateSingleTenantHsmInstanceProposal, google.cloud.kms.v1.HsmManagement/ApproveSingleTenantHsmInstanceProposal, google.cloud.kms.v1.HsmManagement/ExecuteSingleTenantHsmInstanceProposal
func TestKMSTrustedKeyWrapping(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	kc := kmsClients(t, h)
	loc := "projects/" + h.Project() + "/locations/us-east1"
	for _, variant := range []string{"grpc", "rest"} {
		t.Run(variant, func(t *testing.T) {
			var hc *kms.HsmManagementClient
			var err error
			if variant == "rest" {
				hc, err = kms.NewHsmManagementRESTClient(ctx, kmsOptions(h, variant)...)
			} else {
				hc, err = kms.NewHsmManagementClient(ctx, kmsOptions(h, variant)...)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer hc.Close()
			c := kc[variant]
			code := func(err error) codes.Code { return kmsCode(variant, err) }
			fatal := func(what string, err error) {
				t.Helper()
				if err != nil {
					t.Fatalf("%s: %v", what, err)
				}
			}
			proposal := func(id string, p *kmspb.SingleTenantHsmInstanceProposal, inst string) *kmspb.SingleTenantHsmInstanceProposal {
				t.Helper()
				op, err := hc.CreateSingleTenantHsmInstanceProposal(ctx, &kmspb.CreateSingleTenantHsmInstanceProposalRequest{Parent: inst,
					SingleTenantHsmInstanceProposalId: id, SingleTenantHsmInstanceProposal: p})
				fatal("propose "+id, err)
				got, err := op.Wait(ctx)
				fatal("propose "+id, err)
				return got
			}
			execute := func(name string) {
				t.Helper()
				op, err := hc.ExecuteSingleTenantHsmInstanceProposal(ctx, &kmspb.ExecuteSingleTenantHsmInstanceProposalRequest{Name: name})
				if err == nil {
					_, err = op.Wait(ctx)
				}
				fatal("execute "+name, err)
			}

			cop, err := hc.CreateSingleTenantHsmInstance(ctx, &kmspb.CreateSingleTenantHsmInstanceRequest{Parent: loc, SingleTenantHsmInstanceId: "trust-" + variant,
				SingleTenantHsmInstance: &kmspb.SingleTenantHsmInstance{KeyPortabilityEnabled: true, QuorumAuth: &kmspb.SingleTenantHsmInstance_QuorumAuth{TotalApproverCount: 3}}})
			fatal("CreateSingleTenantHsmInstance", err)
			inst, err := cop.Wait(ctx)
			fatal("CreateSingleTenantHsmInstance", err)
			a := []hsmApprover{newHsmApprover(t), newHsmApprover(t), newHsmApprover(t)}
			reg := proposal("register", &kmspb.SingleTenantHsmInstanceProposal{Operation: &kmspb.SingleTenantHsmInstanceProposal_RegisterTwoFactorAuthKeys_{
				RegisterTwoFactorAuthKeys: &kmspb.SingleTenantHsmInstanceProposal_RegisterTwoFactorAuthKeys{RequiredApproverCount: 2, TwoFactorPublicKeyPems: []string{a[0].pem, a[1].pem, a[2].pem}}}}, inst.GetName())
			cs := reg.GetQuorumParameters().GetChallenges()
			_, err = hc.ApproveSingleTenantHsmInstanceProposal(ctx, &kmspb.ApproveSingleTenantHsmInstanceProposalRequest{Name: reg.GetName(),
				ApprovalPayload: &kmspb.ApproveSingleTenantHsmInstanceProposalRequest_QuorumReply_{QuorumReply: &kmspb.ApproveSingleTenantHsmInstanceProposalRequest_QuorumReply{
					ChallengeReplies: []*kmspb.ChallengeReply{a[0].sign(t, cs), a[1].sign(t, cs), a[2].sign(t, cs)}}}})
			fatal("approve register", err)
			execute(reg.GetName())

			ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "trust-" + variant})
			fatal("CreateKeyRing", err)
			sthi := func(id string, p kmspb.CryptoKey_CryptoKeyPurpose, alg kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm) *kmspb.CryptoKey {
				t.Helper()
				k, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: id, SkipInitialVersionCreation: true,
					CryptoKey: &kmspb.CryptoKey{Purpose: p, CryptoKeyBackend: inst.GetName(),
						VersionTemplate: &kmspb.CryptoKeyVersionTemplate{ProtectionLevel: kmspb.ProtectionLevel_HSM_SINGLE_TENANT, Algorithm: alg}}})
				fatal("CreateCryptoKey "+id, err)
				return k
			}

			// The wrapping key, known on premises, imported through an
			// HSM_SINGLE_TENANT import job.
			job, err := c.CreateImportJob(ctx, &kmspb.CreateImportJobRequest{Parent: ring.GetName(), ImportJobId: "j", ImportJob: &kmspb.ImportJob{
				ImportMethod: kmspb.ImportJob_RSA_OAEP_3072_SHA256_AES_256, ProtectionLevel: kmspb.ProtectionLevel_HSM_SINGLE_TENANT, CryptoKeyBackend: inst.GetName()}})
			fatal("CreateImportJob", err)
			if job.GetCryptoKeyBackend() != inst.GetName() || job.GetProtectionLevel() != kmspb.ProtectionLevel_HSM_SINGLE_TENANT || job.GetState() != kmspb.ImportJob_ACTIVE {
				t.Fatalf("import job = %v", job)
			}
			b, _ := pem.Decode([]byte(job.GetPublicKey().GetPem()))
			jobPub, err := x509.ParsePKIXPublicKey(b.Bytes)
			fatal("job public key", err)
			kwpWrap := func(kek, key []byte) []byte {
				t.Helper()
				w, err := kwp.NewKWP(kek)
				fatal("KWP", err)
				out, err := w.Wrap(key)
				fatal("KWP wrap", err)
				return out
			}
			kek := make([]byte, 32)
			_, _ = rand.Read(kek)
			ephemeral := make([]byte, 32)
			_, _ = rand.Read(ephemeral)
			wrappedEphemeral, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, jobPub.(*rsa.PublicKey), ephemeral, nil)
			fatal("RSA-OAEP", err)
			wk := sthi("wrap", kmspb.CryptoKey_AES_WRAPPING, kmspb.CryptoKeyVersion_AES_256_KWP)
			wv, err := c.ImportCryptoKeyVersion(ctx, &kmspb.ImportCryptoKeyVersionRequest{Parent: wk.GetName(), ImportJob: job.GetName(),
				Algorithm:          kmspb.CryptoKeyVersion_AES_256_KWP,
				WrappedKeyMaterial: &kmspb.ImportCryptoKeyVersionRequest_RsaAesWrappedKey{RsaAesWrappedKey: append(wrappedEphemeral, kwpWrap(ephemeral, kek)...)}})
			fatal("ImportCryptoKeyVersion (wrapping key)", err)
			if wv.GetHsmTrusted() || wv.GetImportJob() != job.GetName() {
				t.Fatalf("imported wrapping version = %v", wv)
			}

			// Not trusted yet.
			sym := sthi("sym", kmspb.CryptoKey_RAW_ENCRYPT_DECRYPT, kmspb.CryptoKeyVersion_AES_256_GCM)
			symKey := make([]byte, 32)
			_, _ = rand.Read(symKey)
			if _, err := c.ImportTrustedKeyWrappedCryptoKeyVersion(ctx, &kmspb.ImportTrustedKeyWrappedCryptoKeyVersionRequest{Parent: sym.GetName(),
				ImportingKey: wv.GetName(), WrappedKey: kwpWrap(kek, symKey), Algorithm: kmspb.CryptoKeyVersion_AES_256_GCM}); code(err) != codes.FailedPrecondition {
				t.Errorf("import with a wrapping key not yet trusted = %v, want FAILED_PRECONDITION", err)
			}

			// upgrade_key_trust: a[2]'s required challenge, and 2 of 3.
			up := proposal("upgrade", &kmspb.SingleTenantHsmInstanceProposal{Operation: &kmspb.SingleTenantHsmInstanceProposal_UpgradeKeyTrust_{
				UpgradeKeyTrust: &kmspb.SingleTenantHsmInstanceProposal_UpgradeKeyTrust{Name: wv.GetName(), TwoFactorPublicKeyPem: a[2].pem}}}, inst.GetName())
			rq := up.GetRequiredActionQuorumParameters()
			if up.GetUpgradeKeyTrust().GetName() != wv.GetName() || len(rq.GetRequiredChallenges()) != 1 || rq.GetRequiredApproverCount() != 2 {
				t.Fatalf("upgrade_key_trust proposal = %v", up)
			}
			_, err = hc.ApproveSingleTenantHsmInstanceProposal(ctx, &kmspb.ApproveSingleTenantHsmInstanceProposalRequest{Name: up.GetName(),
				ApprovalPayload: &kmspb.ApproveSingleTenantHsmInstanceProposalRequest_RequiredActionQuorumReply_{RequiredActionQuorumReply: &kmspb.ApproveSingleTenantHsmInstanceProposalRequest_RequiredActionQuorumReply{
					RequiredChallengeReplies: []*kmspb.ChallengeReply{a[2].sign(t, rq.GetRequiredChallenges())},
					QuorumChallengeReplies:   []*kmspb.ChallengeReply{a[0].sign(t, rq.GetQuorumChallenges()), a[1].sign(t, rq.GetQuorumChallenges())}}}})
			fatal("approve upgrade_key_trust", err)
			execute(up.GetName())
			if got, err := c.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: wv.GetName()}); err != nil || !got.GetHsmTrusted() {
				t.Fatalf("after upgrade_key_trust = %v, %v", got, err)
			}

			// A symmetric key, wrapped on premises.
			sv, err := c.ImportTrustedKeyWrappedCryptoKeyVersion(ctx, &kmspb.ImportTrustedKeyWrappedCryptoKeyVersionRequest{Parent: sym.GetName(),
				ImportingKey: wv.GetName(), WrappedKey: kwpWrap(kek, symKey), Algorithm: kmspb.CryptoKeyVersion_AES_256_GCM})
			fatal("ImportTrustedKeyWrappedCryptoKeyVersion (AES_256_GCM)", err)
			if !sv.GetTrustedWrappingEnabled() || sv.GetProtectionLevel() != kmspb.ProtectionLevel_HSM_SINGLE_TENANT || sv.GetState() != kmspb.CryptoKeyVersion_ENABLED {
				t.Fatalf("imported symmetric version = %v", sv)
			}
			enc, err := c.RawEncrypt(ctx, &kmspb.RawEncryptRequest{Name: sv.GetName(), Plaintext: []byte("trusted"), AdditionalAuthenticatedData: []byte("aad")})
			fatal("RawEncrypt", err)
			block, _ := aes.NewCipher(symKey)
			gcm, _ := cipher.NewGCMWithTagSize(block, int(enc.GetTagLength()))
			if pt, err := gcm.Open(nil, enc.GetInitializationVector(), enc.GetCiphertext(), []byte("aad")); err != nil || string(pt) != "trusted" {
				t.Errorf("RawEncrypt's ciphertext under the original key = %q, %v", pt, err)
			}
			iv := make([]byte, gcm.NonceSize())
			_, _ = rand.Read(iv)
			dec, err := c.RawDecrypt(ctx, &kmspb.RawDecryptRequest{Name: sv.GetName(), Ciphertext: gcm.Seal(nil, iv, []byte("local"), nil),
				InitializationVector: iv, TagLength: int32(gcm.Overhead())})
			if err != nil || string(dec.GetPlaintext()) != "local" {
				t.Errorf("RawDecrypt of a local ciphertext = %v, %v", dec, err)
			}

			// An asymmetric key, PKCS #8 DER wrapped on premises.
			ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			der, _ := x509.MarshalPKCS8PrivateKey(ec)
			sig := sthi("sig", kmspb.CryptoKey_ASYMMETRIC_SIGN, kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256)
			av, err := c.ImportTrustedKeyWrappedCryptoKeyVersion(ctx, &kmspb.ImportTrustedKeyWrappedCryptoKeyVersionRequest{Parent: sig.GetName(),
				ImportingKey: wk.GetName(), WrappedKey: kwpWrap(kek, der), Algorithm: kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256})
			fatal("ImportTrustedKeyWrappedCryptoKeyVersion (EC_SIGN_P256_SHA256)", err)
			digest := sha256.Sum256([]byte("signed in the HSM"))
			s, err := c.AsymmetricSign(ctx, &kmspb.AsymmetricSignRequest{Name: av.GetName(), Digest: &kmspb.Digest{Digest: &kmspb.Digest_Sha256{Sha256: digest[:]}}})
			fatal("AsymmetricSign", err)
			if !ecdsa.VerifyASN1(&ec.PublicKey, digest[:], s.GetSignature()) {
				t.Error("the signature does not verify with the imported key's own public key")
			}

			// Export it back under the trusted key.
			exp, err := c.ExportTrustedKeyWrappedCryptoKeyVersion(ctx, &kmspb.ExportTrustedKeyWrappedCryptoKeyVersionRequest{Name: av.GetName(), WrappingKey: wv.GetName()})
			fatal("ExportTrustedKeyWrappedCryptoKeyVersion", err)
			if int64(crc32.Checksum(exp.GetWrappedKey(), crc32.MakeTable(crc32.Castagnoli))) != exp.GetWrappedKeyCrc32C().GetValue() {
				t.Error("wrapped_key_crc32c does not match")
			}
			w, _ := kwp.NewKWP(kek)
			if got, err := w.Unwrap(exp.GetWrappedKey()); err != nil || !bytes.Equal(got, der) {
				t.Errorf("the exported key unwraps to something other than the PKCS #8 imported: %v", err)
			}

			// Refused.
			other := sthi("wrap2", kmspb.CryptoKey_AES_WRAPPING, kmspb.CryptoKeyVersion_AES_256_KWP)
			ov, err := c.CreateCryptoKeyVersion(ctx, &kmspb.CreateCryptoKeyVersionRequest{Parent: other.GetName(), CryptoKeyVersion: &kmspb.CryptoKeyVersion{}})
			fatal("CreateCryptoKeyVersion (AES_WRAPPING)", err)
			if _, err := c.ExportTrustedKeyWrappedCryptoKeyVersion(ctx, &kmspb.ExportTrustedKeyWrappedCryptoKeyVersionRequest{Name: av.GetName(), WrappingKey: ov.GetName()}); code(err) != codes.FailedPrecondition {
				t.Errorf("export under a key that is not hsm_trusted = %v, want FAILED_PRECONDITION", err)
			}
			otherKEK := make([]byte, 32)
			_, _ = rand.Read(otherKEK)
			if _, err := c.ImportTrustedKeyWrappedCryptoKeyVersion(ctx, &kmspb.ImportTrustedKeyWrappedCryptoKeyVersionRequest{Parent: sym.GetName(),
				ImportingKey: wv.GetName(), WrappedKey: kwpWrap(otherKEK, symKey), Algorithm: kmspb.CryptoKeyVersion_AES_256_GCM}); code(err) != codes.InvalidArgument {
				t.Errorf("a key wrapped under another key = %v, want INVALID_ARGUMENT", err)
			}
			if _, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "ed", TrustedWrappingEnabled: true,
				CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, CryptoKeyBackend: inst.GetName(),
					VersionTemplate: &kmspb.CryptoKeyVersionTemplate{ProtectionLevel: kmspb.ProtectionLevel_HSM_SINGLE_TENANT}}}); code(err) != codes.InvalidArgument {
				t.Errorf("trusted_wrapping_enabled on ENCRYPT_DECRYPT = %v, want INVALID_ARGUMENT", err)
			}
		})
	}
}
