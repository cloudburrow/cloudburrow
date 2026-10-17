package kms

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"testing"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

// activeInstance makes an ACTIVE instance with three registered approvers
// (2 of 3), key portability as asked.
func activeInstance(t *testing.T, s *Server, loc, id string, portable bool) (string, []approver) {
	t.Helper()
	ctx := context.Background()
	h := s.HsmManagement()
	if _, err := h.CreateSingleTenantHsmInstance(ctx, &kmspb.CreateSingleTenantHsmInstanceRequest{Parent: loc, SingleTenantHsmInstanceId: id,
		SingleTenantHsmInstance: &kmspb.SingleTenantHsmInstance{KeyPortabilityEnabled: portable, QuorumAuth: &kmspb.SingleTenantHsmInstance_QuorumAuth{TotalApproverCount: 3}}}); err != nil {
		t.Fatal(err)
	}
	inst := loc + "/singleTenantHsmInstances/" + id
	a := []approver{newApprover(t), newApprover(t), newApprover(t)}
	op, err := h.CreateSingleTenantHsmInstanceProposal(ctx, &kmspb.CreateSingleTenantHsmInstanceProposalRequest{Parent: inst, SingleTenantHsmInstanceProposalId: "reg",
		SingleTenantHsmInstanceProposal: &kmspb.SingleTenantHsmInstanceProposal{Operation: &kmspb.SingleTenantHsmInstanceProposal_RegisterTwoFactorAuthKeys_{
			RegisterTwoFactorAuthKeys: &kmspb.SingleTenantHsmInstanceProposal_RegisterTwoFactorAuthKeys{RequiredApproverCount: 2, TwoFactorPublicKeyPems: []string{a[0].pem, a[1].pem, a[2].pem}}}}})
	if err != nil {
		t.Fatal(err)
	}
	p := &kmspb.SingleTenantHsmInstanceProposal{}
	if err := op.GetResponse().UnmarshalTo(p); err != nil {
		t.Fatal(err)
	}
	cs := p.GetQuorumParameters().GetChallenges()
	if _, err := h.ApproveSingleTenantHsmInstanceProposal(ctx, &kmspb.ApproveSingleTenantHsmInstanceProposalRequest{Name: p.GetName(),
		ApprovalPayload: &kmspb.ApproveSingleTenantHsmInstanceProposalRequest_QuorumReply_{QuorumReply: &kmspb.ApproveSingleTenantHsmInstanceProposalRequest_QuorumReply{
			ChallengeReplies: []*kmspb.ChallengeReply{a[0].reply(t, cs), a[1].reply(t, cs), a[2].reply(t, cs)}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ExecuteSingleTenantHsmInstanceProposal(ctx, &kmspb.ExecuteSingleTenantHsmInstanceProposalRequest{Name: p.GetName()}); err != nil {
		t.Fatal(err)
	}
	return inst, a
}

func TestTrustedKeyWrapping(t *testing.T) {
	ctx := context.Background()
	s := NewServer(store.NewMemory())
	h := s.HsmManagement()
	const loc = "projects/demo-project/locations/us-east1"
	code := func(err error) codes.Code { return status.Code(err) }
	inst, a := activeInstance(t, s, loc, "i", true)
	ring, err := s.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "r"})
	if err != nil {
		t.Fatal(err)
	}
	sthi := func(p kmspb.CryptoKey_CryptoKeyPurpose, alg algo) *kmspb.CryptoKey {
		return &kmspb.CryptoKey{Purpose: p, CryptoKeyBackend: inst,
			VersionTemplate: &kmspb.CryptoKeyVersionTemplate{ProtectionLevel: kmspb.ProtectionLevel_HSM_SINGLE_TENANT, Algorithm: alg}}
	}
	// AES_WRAPPING is served at HSM_SINGLE_TENANT only.
	if _, err := s.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "sw", CryptoKey: &kmspb.CryptoKey{
		Purpose: kmspb.CryptoKey_AES_WRAPPING, VersionTemplate: &kmspb.CryptoKeyVersionTemplate{Algorithm: kmspb.CryptoKeyVersion_AES_256_KWP}}}); code(err) != codes.Unimplemented {
		t.Errorf("SOFTWARE AES_WRAPPING = %v", err)
	}
	wk, err := s.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "wrap", CryptoKey: sthi(kmspb.CryptoKey_AES_WRAPPING, kmspb.CryptoKeyVersion_AES_256_KWP)})
	if err != nil {
		t.Fatal(err)
	}
	wv := wk.GetName() + "/cryptoKeyVersions/1"
	// trusted_wrapping_enabled is refused for ENCRYPT_DECRYPT.
	if _, err := s.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "ed", TrustedWrappingEnabled: true,
		CryptoKey: sthi(kmspb.CryptoKey_ENCRYPT_DECRYPT, 0)}); code(err) != codes.InvalidArgument {
		t.Errorf("trusted ENCRYPT_DECRYPT = %v", err)
	}
	raw, err := s.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "raw", TrustedWrappingEnabled: true,
		CryptoKey: sthi(kmspb.CryptoKey_RAW_ENCRYPT_DECRYPT, kmspb.CryptoKeyVersion_AES_256_GCM)})
	if err != nil {
		t.Fatal(err)
	}
	rv, _ := s.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: raw.GetName() + "/cryptoKeyVersions/1"})
	if !rv.GetTrustedWrappingEnabled() {
		t.Fatalf("trusted version = %v", rv)
	}
	// Not yet trusted: export is refused.
	if _, err := s.ExportTrustedKeyWrappedCryptoKeyVersion(ctx, &kmspb.ExportTrustedKeyWrappedCryptoKeyVersionRequest{Name: rv.GetName(), WrappingKey: wv}); code(err) != codes.FailedPrecondition {
		t.Errorf("export with an untrusted wrapping key = %v", err)
	}
	// upgrade_key_trust: M of N and the named key's own signature.
	op, err := h.CreateSingleTenantHsmInstanceProposal(ctx, &kmspb.CreateSingleTenantHsmInstanceProposalRequest{Parent: inst, SingleTenantHsmInstanceProposalId: "up",
		SingleTenantHsmInstanceProposal: &kmspb.SingleTenantHsmInstanceProposal{Operation: &kmspb.SingleTenantHsmInstanceProposal_UpgradeKeyTrust_{
			UpgradeKeyTrust: &kmspb.SingleTenantHsmInstanceProposal_UpgradeKeyTrust{Name: wv, TwoFactorPublicKeyPem: a[2].pem}}}})
	if err != nil {
		t.Fatal(err)
	}
	p := &kmspb.SingleTenantHsmInstanceProposal{}
	if err := op.GetResponse().UnmarshalTo(p); err != nil {
		t.Fatal(err)
	}
	rq := p.GetRequiredActionQuorumParameters()
	if p.GetUpgradeKeyTrust().GetName() != wv || len(rq.GetRequiredChallenges()) != 1 || rq.GetRequiredApproverCount() != 2 {
		t.Fatalf("upgrade proposal = %v", p)
	}
	if _, err := h.ApproveSingleTenantHsmInstanceProposal(ctx, &kmspb.ApproveSingleTenantHsmInstanceProposalRequest{Name: p.GetName(),
		ApprovalPayload: &kmspb.ApproveSingleTenantHsmInstanceProposalRequest_RequiredActionQuorumReply_{RequiredActionQuorumReply: &kmspb.ApproveSingleTenantHsmInstanceProposalRequest_RequiredActionQuorumReply{
			RequiredChallengeReplies: []*kmspb.ChallengeReply{a[2].reply(t, rq.GetRequiredChallenges())},
			QuorumChallengeReplies:   []*kmspb.ChallengeReply{a[0].reply(t, rq.GetQuorumChallenges()), a[1].reply(t, rq.GetQuorumChallenges())}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ExecuteSingleTenantHsmInstanceProposal(ctx, &kmspb.ExecuteSingleTenantHsmInstanceProposalRequest{Name: p.GetName()}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: wv}); !got.GetHsmTrusted() {
		t.Fatalf("after upgrade = %v", got)
	}
	// Export, and import the wrapped material into another key.
	exp, err := s.ExportTrustedKeyWrappedCryptoKeyVersion(ctx, &kmspb.ExportTrustedKeyWrappedCryptoKeyVersionRequest{Name: rv.GetName(), WrappingKey: wv})
	if err != nil || exp.GetWrappedKeyCrc32C().GetValue() != crc32c(exp.GetWrappedKey()) {
		t.Fatalf("export = %v, %v", exp, err)
	}
	other, err := s.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "other", SkipInitialVersionCreation: true,
		CryptoKey: sthi(kmspb.CryptoKey_RAW_ENCRYPT_DECRYPT, kmspb.CryptoKeyVersion_AES_256_GCM)})
	if err != nil {
		t.Fatal(err)
	}
	iv, err := s.ImportTrustedKeyWrappedCryptoKeyVersion(ctx, &kmspb.ImportTrustedKeyWrappedCryptoKeyVersionRequest{Parent: other.GetName(),
		ImportingKey: wk.GetName(), WrappedKey: exp.GetWrappedKey(), Algorithm: kmspb.CryptoKeyVersion_AES_256_GCM})
	if err != nil || !iv.GetTrustedWrappingEnabled() || iv.GetImportTime() == nil {
		t.Fatalf("import = %v, %v", iv, err)
	}
	enc, err := s.RawEncrypt(ctx, &kmspb.RawEncryptRequest{Name: rv.GetName(), Plaintext: []byte("portable")})
	if err != nil {
		t.Fatal(err)
	}
	dec, err := s.RawDecrypt(ctx, &kmspb.RawDecryptRequest{Name: iv.GetName(), Ciphertext: enc.GetCiphertext(), InitializationVector: enc.GetInitializationVector()})
	if err != nil || string(dec.GetPlaintext()) != "portable" {
		t.Fatalf("decrypt with the imported copy = %v, %v", dec, err)
	}
	// An asymmetric key, PKCS #8 DER wrapped on premises.
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(ec)
	var stored keyVersion
	if _, err := s.get(dbKey(versionPrefix, wv), &stored); err != nil {
		t.Fatal(err)
	}
	wrapMat := stored.Material
	wrapped, _ := wrapKWP(wrapMat, der)
	sig, err := s.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "sig", SkipInitialVersionCreation: true,
		CryptoKey: sthi(kmspb.CryptoKey_ASYMMETRIC_SIGN, kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256)})
	if err != nil {
		t.Fatal(err)
	}
	sv, err := s.ImportTrustedKeyWrappedCryptoKeyVersion(ctx, &kmspb.ImportTrustedKeyWrappedCryptoKeyVersionRequest{Parent: sig.GetName(),
		ImportingKey: wv, WrappedKey: wrapped, Algorithm: kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256})
	if err != nil {
		t.Fatal(err)
	}
	back, err := s.ExportTrustedKeyWrappedCryptoKeyVersion(ctx, &kmspb.ExportTrustedKeyWrappedCryptoKeyVersionRequest{Name: sv.GetName(), WrappingKey: wv})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := unwrapKWP(wrapMat, back.GetWrappedKey()); err != nil || !bytes.Equal(got, der) {
		t.Errorf("exported material differs from the imported PKCS #8: %v", err)
	}
	if _, err := s.ImportTrustedKeyWrappedCryptoKeyVersion(ctx, &kmspb.ImportTrustedKeyWrappedCryptoKeyVersionRequest{Parent: sig.GetName(),
		ImportingKey: wv, WrappedKey: []byte("0123456789abcdef01234567"), Algorithm: kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256}); code(err) != codes.InvalidArgument {
		t.Errorf("garbage wrapped_key = %v", err)
	}
	if _, err := s.ImportTrustedKeyWrappedCryptoKeyVersion(ctx, &kmspb.ImportTrustedKeyWrappedCryptoKeyVersionRequest{Parent: sig.GetName(),
		ImportingKey: wv, WrappedKey: wrapped, Algorithm: kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256, CryptoKeyVersion: sv.GetName()}); code(err) != codes.Unimplemented {
		t.Errorf("re-import = %v", err)
	}

	// An HSM_SINGLE_TENANT import job.
	if _, err := s.CreateImportJob(ctx, &kmspb.CreateImportJobRequest{Parent: ring.GetName(), ImportJobId: "nob", ImportJob: &kmspb.ImportJob{
		ImportMethod: kmspb.ImportJob_RSA_OAEP_3072_SHA256_AES_256, ProtectionLevel: kmspb.ProtectionLevel_HSM_SINGLE_TENANT}}); code(err) != codes.InvalidArgument {
		t.Errorf("job without crypto_key_backend = %v", err)
	}
	job, err := s.CreateImportJob(ctx, &kmspb.CreateImportJobRequest{Parent: ring.GetName(), ImportJobId: "j", ImportJob: &kmspb.ImportJob{
		ImportMethod: kmspb.ImportJob_RSA_OAEP_3072_SHA256_AES_256, ProtectionLevel: kmspb.ProtectionLevel_HSM_SINGLE_TENANT, CryptoKeyBackend: inst}})
	if err != nil || job.GetCryptoKeyBackend() != inst || job.GetState() != kmspb.ImportJob_ACTIVE {
		t.Fatalf("job = %v, %v", job, err)
	}
	ec2, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der2, _ := x509.MarshalPKCS8PrivateKey(ec2)
	w2, err := WrapForImport(job.GetPublicKey().GetPem(), job.GetImportMethod(), der2)
	if err != nil {
		t.Fatal(err)
	}
	jv, err := s.ImportCryptoKeyVersion(ctx, &kmspb.ImportCryptoKeyVersionRequest{Parent: sig.GetName(), ImportJob: job.GetName(),
		Algorithm: kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256, WrappedKeyMaterial: &kmspb.ImportCryptoKeyVersionRequest_RsaAesWrappedKey{RsaAesWrappedKey: w2},
		TrustedWrappingEnabled: true})
	if err != nil || !jv.GetTrustedWrappingEnabled() || jv.GetProtectionLevel() != kmspb.ProtectionLevel_HSM_SINGLE_TENANT {
		t.Fatalf("import through the job = %v, %v", jv, err)
	}

	// Without key portability, upgrade_key_trust is refused.
	inst2, b := activeInstance(t, s, loc, "noport", false)
	wk2, err := s.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "wrap2", CryptoKey: &kmspb.CryptoKey{
		Purpose: kmspb.CryptoKey_AES_WRAPPING, CryptoKeyBackend: inst2,
		VersionTemplate: &kmspb.CryptoKeyVersionTemplate{ProtectionLevel: kmspb.ProtectionLevel_HSM_SINGLE_TENANT, Algorithm: kmspb.CryptoKeyVersion_AES_256_KWP}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.CreateSingleTenantHsmInstanceProposal(ctx, &kmspb.CreateSingleTenantHsmInstanceProposalRequest{Parent: inst2, SingleTenantHsmInstanceProposalId: "up",
		SingleTenantHsmInstanceProposal: &kmspb.SingleTenantHsmInstanceProposal{Operation: &kmspb.SingleTenantHsmInstanceProposal_UpgradeKeyTrust_{
			UpgradeKeyTrust: &kmspb.SingleTenantHsmInstanceProposal_UpgradeKeyTrust{Name: wk2.GetName() + "/cryptoKeyVersions/1", TwoFactorPublicKeyPem: b[0].pem}}}}); code(err) != codes.FailedPrecondition {
		t.Errorf("upgrade without key portability = %v", err)
	}
	// A key in another instance is not this instance's to upgrade.
	if _, err := h.CreateSingleTenantHsmInstanceProposal(ctx, &kmspb.CreateSingleTenantHsmInstanceProposalRequest{Parent: inst, SingleTenantHsmInstanceProposalId: "up2",
		SingleTenantHsmInstanceProposal: &kmspb.SingleTenantHsmInstanceProposal{Operation: &kmspb.SingleTenantHsmInstanceProposal_UpgradeKeyTrust_{
			UpgradeKeyTrust: &kmspb.SingleTenantHsmInstanceProposal_UpgradeKeyTrust{Name: wk2.GetName() + "/cryptoKeyVersions/1", TwoFactorPublicKeyPem: a[0].pem}}}}); code(err) != codes.InvalidArgument {
		t.Errorf("upgrade of another instance's key = %v", err)
	}
}
