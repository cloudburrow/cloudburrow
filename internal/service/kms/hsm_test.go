package kms

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/cloudburrow/cloudburrow/internal/sched"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

type approver struct {
	key *rsa.PrivateKey
	pem string
}

func newApprover(t *testing.T) approver {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return approver{k, string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))}
}

// reply signs the challenge cs has for a, as `openssl dgst -sign` does.
func (a approver) reply(t *testing.T, cs []*kmspb.Challenge) *kmspb.ChallengeReply {
	t.Helper()
	for _, c := range cs {
		if samePem(c.GetPublicKeyPem(), a.pem) {
			d := sha256.Sum256(c.GetChallenge())
			sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, d[:])
			if err != nil {
				t.Fatal(err)
			}
			return &kmspb.ChallengeReply{SignedChallenge: sig, PublicKeyPem: a.pem}
		}
	}
	t.Fatalf("no challenge for the key")
	return nil
}

func TestSingleTenantHsmLifecycle(t *testing.T) {
	ctx := context.Background()
	clock := sched.NewFakeClock(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	s := NewServerWithClock(store.NewMemory(), clock)
	h := s.HsmManagement()
	const l = "projects/demo-project/locations/us-east1"
	code := func(err error) codes.Code { return status.Code(err) }

	if _, err := h.CreateSingleTenantHsmInstance(ctx, &kmspb.CreateSingleTenantHsmInstanceRequest{Parent: l, SingleTenantHsmInstanceId: "i",
		SingleTenantHsmInstance: &kmspb.SingleTenantHsmInstance{QuorumAuth: &kmspb.SingleTenantHsmInstance_QuorumAuth{TotalApproverCount: 2}}}); code(err) != codes.InvalidArgument {
		t.Errorf("N=2: %v", err)
	}
	op, err := h.CreateSingleTenantHsmInstance(ctx, &kmspb.CreateSingleTenantHsmInstanceRequest{Parent: l, SingleTenantHsmInstanceId: "i",
		SingleTenantHsmInstance: &kmspb.SingleTenantHsmInstance{QuorumAuth: &kmspb.SingleTenantHsmInstance_QuorumAuth{TotalApproverCount: 3}}})
	if err != nil || !op.GetDone() {
		t.Fatalf("create = %v, %v", op, err)
	}
	inst := l + "/singleTenantHsmInstances/i"
	got, err := h.GetSingleTenantHsmInstance(ctx, &kmspb.GetSingleTenantHsmInstanceRequest{Name: inst})
	if err != nil || got.GetState() != kmspb.SingleTenantHsmInstance_PENDING_TWO_FACTOR_AUTH_REGISTRATION {
		t.Fatalf("get = %v, %v", got, err)
	}
	a := []approver{newApprover(t), newApprover(t), newApprover(t)}
	propose := func(id string, p *kmspb.SingleTenantHsmInstanceProposal) (*kmspb.SingleTenantHsmInstanceProposal, error) {
		if _, err := h.CreateSingleTenantHsmInstanceProposal(ctx, &kmspb.CreateSingleTenantHsmInstanceProposalRequest{Parent: inst,
			SingleTenantHsmInstanceProposalId: id, SingleTenantHsmInstanceProposal: p}); err != nil {
			return nil, err
		}
		return h.GetSingleTenantHsmInstanceProposal(ctx, &kmspb.GetSingleTenantHsmInstanceProposalRequest{Name: inst + "/proposals/" + id})
	}
	quorum := func(name string, rs ...*kmspb.ChallengeReply) error {
		_, err := h.ApproveSingleTenantHsmInstanceProposal(ctx, &kmspb.ApproveSingleTenantHsmInstanceProposalRequest{Name: name,
			ApprovalPayload: &kmspb.ApproveSingleTenantHsmInstanceProposalRequest_QuorumReply_{QuorumReply: &kmspb.ApproveSingleTenantHsmInstanceProposalRequest_QuorumReply{ChallengeReplies: rs}}})
		return err
	}
	execute := func(name string) error {
		op, err := h.ExecuteSingleTenantHsmInstanceProposal(ctx, &kmspb.ExecuteSingleTenantHsmInstanceProposalRequest{Name: name})
		if err == nil && op.GetError() != nil {
			return status.ErrorProto(op.GetError())
		}
		return err
	}
	// Disabling before registration is refused.
	if _, err := propose("early", &kmspb.SingleTenantHsmInstanceProposal{Operation: &kmspb.SingleTenantHsmInstanceProposal_DisableSingleTenantHsmInstance_{}}); code(err) != codes.FailedPrecondition {
		t.Errorf("disable while pending = %v", err)
	}
	reg, err := propose("reg", &kmspb.SingleTenantHsmInstanceProposal{Operation: &kmspb.SingleTenantHsmInstanceProposal_RegisterTwoFactorAuthKeys_{
		RegisterTwoFactorAuthKeys: &kmspb.SingleTenantHsmInstanceProposal_RegisterTwoFactorAuthKeys{RequiredApproverCount: 2, TwoFactorPublicKeyPems: []string{a[0].pem, a[1].pem, a[2].pem}}}})
	if err != nil || reg.GetState() != kmspb.SingleTenantHsmInstanceProposal_PENDING || reg.GetQuorumParameters().GetRequiredApproverCount() != 3 || len(reg.GetQuorumParameters().GetChallenges()) != 3 {
		t.Fatalf("register proposal = %v, %v", reg, err)
	}
	if _, err := propose("second", &kmspb.SingleTenantHsmInstanceProposal{Operation: &kmspb.SingleTenantHsmInstanceProposal_DeleteSingleTenantHsmInstance_{}}); code(err) != codes.FailedPrecondition {
		t.Errorf("a second active proposal = %v", err)
	}
	cs := reg.GetQuorumParameters().GetChallenges()
	// An unsigned, wrongly signed or foreign reply is refused, and records nothing.
	bad := a[0].reply(t, cs)
	bad.SignedChallenge = append([]byte(nil), bad.SignedChallenge...)
	bad.SignedChallenge[0] ^= 1
	if err := quorum(reg.GetName(), a[1].reply(t, cs), bad); code(err) != codes.InvalidArgument {
		t.Errorf("a bad signature = %v", err)
	}
	if err := quorum(reg.GetName(), &kmspb.ChallengeReply{PublicKeyPem: a[0].pem}); code(err) != codes.InvalidArgument {
		t.Errorf("unsigned = %v", err)
	}
	stranger := newApprover(t)
	forged := a[0].reply(t, cs)
	forged.PublicKeyPem = stranger.pem
	if err := quorum(reg.GetName(), forged); code(err) != codes.InvalidArgument {
		t.Errorf("a stranger's key = %v", err)
	}
	if p, _ := h.GetSingleTenantHsmInstanceProposal(ctx, &kmspb.GetSingleTenantHsmInstanceProposalRequest{Name: reg.GetName()}); len(p.GetQuorumParameters().GetApprovedTwoFactorPublicKeyPems()) != 0 {
		t.Errorf("refused replies were recorded: %v", p)
	}
	// Registration needs all N: two of three leave it PENDING.
	if err := quorum(reg.GetName(), a[0].reply(t, cs), a[1].reply(t, cs)); err != nil {
		t.Fatal(err)
	}
	if err := execute(reg.GetName()); code(err) != codes.FailedPrecondition {
		t.Errorf("execute while PENDING = %v", err)
	}
	if err := quorum(reg.GetName(), a[2].reply(t, cs)); err != nil {
		t.Fatal(err)
	}
	if err := execute(reg.GetName()); err != nil {
		t.Fatal(err)
	}
	got, _ = h.GetSingleTenantHsmInstance(ctx, &kmspb.GetSingleTenantHsmInstanceRequest{Name: inst})
	if got.GetState() != kmspb.SingleTenantHsmInstance_ACTIVE || got.GetQuorumAuth().GetRequiredApproverCount() != 2 || len(got.GetQuorumAuth().GetTwoFactorPublicKeyPems()) != 3 {
		t.Fatalf("after register = %v", got)
	}

	// A key in the instance.
	if _, err := s.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: l, KeyRingId: "r"}); err != nil {
		t.Fatal(err)
	}
	mk := func(id, backend string) (*kmspb.CryptoKey, error) {
		return s.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: l + "/keyRings/r", CryptoKeyId: id, CryptoKey: &kmspb.CryptoKey{
			Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, CryptoKeyBackend: backend,
			VersionTemplate: &kmspb.CryptoKeyVersionTemplate{ProtectionLevel: kmspb.ProtectionLevel_HSM_SINGLE_TENANT}}})
	}
	if _, err := mk("none", ""); code(err) != codes.InvalidArgument {
		t.Errorf("no backend = %v", err)
	}
	if _, err := mk("missing", l+"/singleTenantHsmInstances/nope"); code(err) != codes.InvalidArgument {
		t.Errorf("missing backend = %v", err)
	}
	key, err := mk("k", inst)
	if err != nil || key.GetCryptoKeyBackend() != inst || key.GetPrimary().GetProtectionLevel() != kmspb.ProtectionLevel_HSM_SINGLE_TENANT {
		t.Fatalf("key = %v, %v", key, err)
	}
	if _, err := s.Encrypt(ctx, &kmspb.EncryptRequest{Name: key.GetName(), Plaintext: []byte("x")}); err != nil {
		t.Fatal(err)
	}

	// Disable takes M of N; keys then fail.
	dis, err := propose("dis", &kmspb.SingleTenantHsmInstanceProposal{Expiration: &kmspb.SingleTenantHsmInstanceProposal_Ttl{Ttl: durationpb.New(time.Hour)}, Operation: &kmspb.SingleTenantHsmInstanceProposal_DisableSingleTenantHsmInstance_{}})
	if err != nil || dis.GetQuorumParameters().GetRequiredApproverCount() != 2 {
		t.Fatalf("disable proposal = %v, %v", dis, err)
	}
	if err := quorum(dis.GetName(), a[0].reply(t, dis.GetQuorumParameters().GetChallenges()), a[2].reply(t, dis.GetQuorumParameters().GetChallenges())); err != nil {
		t.Fatal(err)
	}
	if err := execute(dis.GetName()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Encrypt(ctx, &kmspb.EncryptRequest{Name: key.GetName(), Plaintext: []byte("x")}); code(err) != codes.FailedPrecondition {
		t.Errorf("encrypt in a DISABLED instance = %v", err)
	}
	if _, err := mk("k2", inst); code(err) != codes.FailedPrecondition {
		t.Errorf("a key in a DISABLED instance = %v", err)
	}

	// Enable, then add a member: its own signature is required as well.
	en, _ := propose("en", &kmspb.SingleTenantHsmInstanceProposal{Operation: &kmspb.SingleTenantHsmInstanceProposal_EnableSingleTenantHsmInstance_{}})
	if err := quorum(en.GetName(), a[1].reply(t, en.GetQuorumParameters().GetChallenges()), a[2].reply(t, en.GetQuorumParameters().GetChallenges())); err != nil {
		t.Fatal(err)
	}
	if err := execute(en.GetName()); err != nil {
		t.Fatal(err)
	}
	d := newApprover(t)
	add, err := propose("add", &kmspb.SingleTenantHsmInstanceProposal{Operation: &kmspb.SingleTenantHsmInstanceProposal_AddQuorumMember_{
		AddQuorumMember: &kmspb.SingleTenantHsmInstanceProposal_AddQuorumMember{TwoFactorPublicKeyPem: d.pem}}})
	if err != nil {
		t.Fatal(err)
	}
	rq := add.GetRequiredActionQuorumParameters()
	if err := quorum(add.GetName(), a[0].reply(t, rq.GetQuorumChallenges())); code(err) != codes.InvalidArgument {
		t.Errorf("quorum_reply to required-action parameters = %v", err)
	}
	approveRA := func(must, q []*kmspb.ChallengeReply) error {
		_, err := h.ApproveSingleTenantHsmInstanceProposal(ctx, &kmspb.ApproveSingleTenantHsmInstanceProposalRequest{Name: add.GetName(),
			ApprovalPayload: &kmspb.ApproveSingleTenantHsmInstanceProposalRequest_RequiredActionQuorumReply_{RequiredActionQuorumReply: &kmspb.ApproveSingleTenantHsmInstanceProposalRequest_RequiredActionQuorumReply{
				RequiredChallengeReplies: must, QuorumChallengeReplies: q}}})
		return err
	}
	if err := approveRA(nil, []*kmspb.ChallengeReply{a[0].reply(t, rq.GetQuorumChallenges()), a[1].reply(t, rq.GetQuorumChallenges())}); err != nil {
		t.Fatal(err)
	}
	if p, _ := h.GetSingleTenantHsmInstanceProposal(ctx, &kmspb.GetSingleTenantHsmInstanceProposalRequest{Name: add.GetName()}); p.GetState() != kmspb.SingleTenantHsmInstanceProposal_PENDING {
		t.Errorf("approved without the new key: %v", p.GetState())
	}
	if err := approveRA([]*kmspb.ChallengeReply{d.reply(t, rq.GetRequiredChallenges())}, nil); err != nil {
		t.Fatal(err)
	}
	if err := execute(add.GetName()); err != nil {
		t.Fatal(err)
	}
	got, _ = h.GetSingleTenantHsmInstance(ctx, &kmspb.GetSingleTenantHsmInstanceRequest{Name: inst})
	if got.GetQuorumAuth().GetTotalApproverCount() != 4 {
		t.Errorf("after add = %v", got.GetQuorumAuth())
	}

	// Remove a member (N=4 to 3, M=2); a further removal would leave N=2.
	rm, err := propose("rm", &kmspb.SingleTenantHsmInstanceProposal{Operation: &kmspb.SingleTenantHsmInstanceProposal_RemoveQuorumMember_{
		RemoveQuorumMember: &kmspb.SingleTenantHsmInstanceProposal_RemoveQuorumMember{TwoFactorPublicKeyPem: a[0].pem}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := quorum(rm.GetName(), d.reply(t, rm.GetQuorumParameters().GetChallenges()), a[2].reply(t, rm.GetQuorumParameters().GetChallenges())); err != nil {
		t.Fatal(err)
	}
	if err := execute(rm.GetName()); err != nil {
		t.Fatal(err)
	}
	if _, err := propose("rm2", &kmspb.SingleTenantHsmInstanceProposal{Operation: &kmspb.SingleTenantHsmInstanceProposal_RemoveQuorumMember_{
		RemoveQuorumMember: &kmspb.SingleTenantHsmInstanceProposal_RemoveQuorumMember{TwoFactorPublicKeyPem: a[1].pem}}}); code(err) != codes.FailedPrecondition {
		t.Errorf("removing below 3 = %v", err)
	}
	// The removed key can no longer approve.
	ref, _ := propose("ref", &kmspb.SingleTenantHsmInstanceProposal{Operation: &kmspb.SingleTenantHsmInstanceProposal_RefreshSingleTenantHsmInstance_{}})
	if err := quorum(ref.GetName(), &kmspb.ChallengeReply{SignedChallenge: []byte{1}, PublicKeyPem: a[0].pem}); code(err) != codes.InvalidArgument {
		t.Errorf("a removed member = %v", err)
	}
	// Delete the proposal: a new one may be made, and it cannot run.
	if _, err := h.DeleteSingleTenantHsmInstanceProposal(ctx, &kmspb.DeleteSingleTenantHsmInstanceProposalRequest{Name: ref.GetName()}); err != nil {
		t.Fatal(err)
	}
	if err := execute(ref.GetName()); code(err) != codes.FailedPrecondition {
		t.Errorf("execute a deleted proposal = %v", err)
	}
	list, _ := h.ListSingleTenantHsmInstanceProposals(ctx, &kmspb.ListSingleTenantHsmInstanceProposalsRequest{Parent: inst})
	all, _ := h.ListSingleTenantHsmInstanceProposals(ctx, &kmspb.ListSingleTenantHsmInstanceProposalsRequest{Parent: inst, ShowDeleted: true})
	if len(all.GetSingleTenantHsmInstanceProposals()) != len(list.GetSingleTenantHsmInstanceProposals())+1 {
		t.Errorf("show_deleted: %d vs %d", len(all.GetSingleTenantHsmInstanceProposals()), len(list.GetSingleTenantHsmInstanceProposals()))
	}
	// Expiry: an unapproved proposal cannot be approved after its ttl.
	exp, err := propose("exp", &kmspb.SingleTenantHsmInstanceProposal{Expiration: &kmspb.SingleTenantHsmInstanceProposal_Ttl{Ttl: durationpb.New(10 * time.Minute)}, Operation: &kmspb.SingleTenantHsmInstanceProposal_RefreshSingleTenantHsmInstance_{}})
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(11 * time.Minute)
	if err := quorum(exp.GetName(), a[1].reply(t, exp.GetQuorumParameters().GetChallenges())); code(err) != codes.FailedPrecondition {
		t.Errorf("approve after expiry = %v", err)
	}
	if _, err := propose("ttl", &kmspb.SingleTenantHsmInstanceProposal{Expiration: &kmspb.SingleTenantHsmInstanceProposal_Ttl{Ttl: durationpb.New(time.Minute)}, Operation: &kmspb.SingleTenantHsmInstanceProposal_RefreshSingleTenantHsmInstance_{}}); code(err) != codes.InvalidArgument {
		t.Errorf("ttl of a minute = %v", err)
	}
	// An instance not refreshed for 730 days reads as DISABLED.
	clock.Advance(hsmUnrefreshed)
	got, _ = h.GetSingleTenantHsmInstance(ctx, &kmspb.GetSingleTenantHsmInstanceRequest{Name: inst})
	if got.GetState() != kmspb.SingleTenantHsmInstance_DISABLED {
		t.Errorf("unrefreshed = %v", got.GetState())
	}
	if _, err := s.Encrypt(ctx, &kmspb.EncryptRequest{Name: key.GetName(), Plaintext: []byte("x")}); code(err) != codes.FailedPrecondition {
		t.Errorf("encrypt in an unrefreshed instance = %v", err)
	}
	// Delete it.
	del, err := propose("del", &kmspb.SingleTenantHsmInstanceProposal{Operation: &kmspb.SingleTenantHsmInstanceProposal_DeleteSingleTenantHsmInstance_{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := quorum(del.GetName(), a[1].reply(t, del.GetQuorumParameters().GetChallenges()), d.reply(t, del.GetQuorumParameters().GetChallenges())); err != nil {
		t.Fatal(err)
	}
	if err := execute(del.GetName()); err != nil {
		t.Fatal(err)
	}
	insts, _ := h.ListSingleTenantHsmInstances(ctx, &kmspb.ListSingleTenantHsmInstancesRequest{Parent: l})
	withDeleted, _ := h.ListSingleTenantHsmInstances(ctx, &kmspb.ListSingleTenantHsmInstancesRequest{Parent: l, ShowDeleted: true})
	if len(insts.GetSingleTenantHsmInstances()) != 0 || len(withDeleted.GetSingleTenantHsmInstances()) != 1 || withDeleted.GetSingleTenantHsmInstances()[0].GetDeleteTime() == nil {
		t.Errorf("after delete: %v / %v", insts, withDeleted)
	}
	if _, err := propose("upg", &kmspb.SingleTenantHsmInstanceProposal{Operation: &kmspb.SingleTenantHsmInstanceProposal_UpgradeKeyTrust_{}}); code(err) != codes.Unimplemented {
		t.Errorf("upgrade_key_trust = %v", err)
	}
}
