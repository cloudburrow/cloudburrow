//go:build compat

package compat

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"testing"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/durationpb"
)

// hsmApprover is a quorum member's two-factor key: an RSA 2048 key pair
// generated in the test, as the guide's `openssl genrsa 2048` makes one.
type hsmApprover struct {
	key *rsa.PrivateKey
	pem string
}

func newHsmApprover(t *testing.T) hsmApprover {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return hsmApprover{k, string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))}
}

// sign answers the challenge cs holds for a's key: RSASSA-PKCS1 v1.5 over
// the SHA-256 digest, as ChallengeReply.signed_challenge requires and
// `openssl dgst -sign` produces.
func (a hsmApprover) sign(t *testing.T, cs []*kmspb.Challenge) *kmspb.ChallengeReply {
	t.Helper()
	for _, c := range cs {
		if c.GetPublicKeyPem() == a.pem {
			d := sha256.Sum256(c.GetChallenge())
			sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, d[:])
			if err != nil {
				t.Fatal(err)
			}
			return &kmspb.ChallengeReply{SignedChallenge: sig, PublicKeyPem: a.pem}
		}
	}
	t.Fatalf("no challenge for the key among %d", len(cs))
	return nil
}

// TestKMSSingleTenantHsm (#1469): through the official HsmManagement
// client, over gRPC and REST, a single-tenant HSM instance goes through the
// documented life: created PENDING_TWO_FACTOR_AUTH_REGISTRATION; a
// register_2fa_keys proposal approved by all three generated keys (across
// two requests), executed to ACTIVE; an HSM_SINGLE_TENANT key whose
// crypto_key_backend is the instance encrypts and decrypts; replies that are
// unsigned, wrongly signed or from a key outside the quorum are refused;
// disable (2 of 3) makes the key fail, enable restores it; add_quorum_member
// needs the new key's own signature beside the quorum; remove_quorum_member
// and refresh run; a deleted proposal cannot execute and frees the
// instance for another; delete_sthi deletes it, hidden from List unless
// show_deleted.
//
// covers: google.cloud.kms.v1.HsmManagement/CreateSingleTenantHsmInstance, google.cloud.kms.v1.HsmManagement/GetSingleTenantHsmInstance, google.cloud.kms.v1.HsmManagement/ListSingleTenantHsmInstances
// covers: google.cloud.kms.v1.HsmManagement/CreateSingleTenantHsmInstanceProposal, google.cloud.kms.v1.HsmManagement/ApproveSingleTenantHsmInstanceProposal, google.cloud.kms.v1.HsmManagement/ExecuteSingleTenantHsmInstanceProposal
// covers: google.cloud.kms.v1.HsmManagement/GetSingleTenantHsmInstanceProposal, google.cloud.kms.v1.HsmManagement/ListSingleTenantHsmInstanceProposals, google.cloud.kms.v1.HsmManagement/DeleteSingleTenantHsmInstanceProposal
func TestKMSSingleTenantHsm(t *testing.T) {
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

			cop, err := hc.CreateSingleTenantHsmInstance(ctx, &kmspb.CreateSingleTenantHsmInstanceRequest{Parent: loc, SingleTenantHsmInstanceId: "sthi-" + variant,
				SingleTenantHsmInstance: &kmspb.SingleTenantHsmInstance{QuorumAuth: &kmspb.SingleTenantHsmInstance_QuorumAuth{TotalApproverCount: 3}}})
			if err != nil {
				t.Fatal(err)
			}
			inst, err := cop.Wait(ctx)
			if err != nil || inst.GetState() != kmspb.SingleTenantHsmInstance_PENDING_TWO_FACTOR_AUTH_REGISTRATION ||
				inst.GetUnrefreshedDurationUntilDisable().AsDuration().Hours() != 730*24 {
				t.Fatalf("created = %v, %v", inst, err)
			}
			if _, err := hc.CreateSingleTenantHsmInstance(ctx, &kmspb.CreateSingleTenantHsmInstanceRequest{Parent: loc, SingleTenantHsmInstanceId: "two",
				SingleTenantHsmInstance: &kmspb.SingleTenantHsmInstance{QuorumAuth: &kmspb.SingleTenantHsmInstance_QuorumAuth{TotalApproverCount: 2}}}); code(err) != codes.InvalidArgument {
				t.Errorf("total_approver_count 2 = %v, want INVALID_ARGUMENT", err)
			}
			n := 0
			for it := hc.ListSingleTenantHsmInstances(ctx, &kmspb.ListSingleTenantHsmInstancesRequest{Parent: loc, Filter: "name:sthi-" + variant}); ; n++ {
				if _, err := it.Next(); err == iterator.Done {
					break
				} else if err != nil {
					t.Fatal(err)
				}
			}
			if n != 1 {
				t.Errorf("listed %d instances, want 1", n)
			}

			a := []hsmApprover{newHsmApprover(t), newHsmApprover(t), newHsmApprover(t)}
			propose := func(id string, p *kmspb.SingleTenantHsmInstanceProposal) (*kmspb.SingleTenantHsmInstanceProposal, error) {
				t.Helper()
				op, err := hc.CreateSingleTenantHsmInstanceProposal(ctx, &kmspb.CreateSingleTenantHsmInstanceProposalRequest{Parent: inst.GetName(),
					SingleTenantHsmInstanceProposalId: id, SingleTenantHsmInstanceProposal: p})
				if err != nil {
					return nil, err
				}
				return op.Wait(ctx)
			}
			mustPropose := func(id string, p *kmspb.SingleTenantHsmInstanceProposal) *kmspb.SingleTenantHsmInstanceProposal {
				t.Helper()
				got, err := propose(id, p)
				if err != nil {
					t.Fatalf("proposal %s: %v", id, err)
				}
				return got
			}
			quorum := func(name string, rs ...*kmspb.ChallengeReply) error {
				_, err := hc.ApproveSingleTenantHsmInstanceProposal(ctx, &kmspb.ApproveSingleTenantHsmInstanceProposalRequest{Name: name,
					ApprovalPayload: &kmspb.ApproveSingleTenantHsmInstanceProposalRequest_QuorumReply_{QuorumReply: &kmspb.ApproveSingleTenantHsmInstanceProposalRequest_QuorumReply{ChallengeReplies: rs}}})
				return err
			}
			execute := func(name string) {
				t.Helper()
				op, err := hc.ExecuteSingleTenantHsmInstanceProposal(ctx, &kmspb.ExecuteSingleTenantHsmInstanceProposalRequest{Name: name})
				if err == nil {
					_, err = op.Wait(ctx)
				}
				if err != nil {
					t.Fatalf("execute %s: %v", name, err)
				}
			}
			state := func() kmspb.SingleTenantHsmInstance_State {
				t.Helper()
				got, err := hc.GetSingleTenantHsmInstance(ctx, &kmspb.GetSingleTenantHsmInstanceRequest{Name: inst.GetName()})
				if err != nil {
					t.Fatal(err)
				}
				return got.GetState()
			}
			// M of N with the signatures of the given approvers.
			run := func(id string, op *kmspb.SingleTenantHsmInstanceProposal, by ...hsmApprover) {
				t.Helper()
				p := mustPropose(id, op)
				var rs []*kmspb.ChallengeReply
				for _, x := range by {
					rs = append(rs, x.sign(t, p.GetQuorumParameters().GetChallenges()))
				}
				if err := quorum(p.GetName(), rs...); err != nil {
					t.Fatalf("approve %s: %v", id, err)
				}
				execute(p.GetName())
			}

			reg := mustPropose("register", &kmspb.SingleTenantHsmInstanceProposal{Operation: &kmspb.SingleTenantHsmInstanceProposal_RegisterTwoFactorAuthKeys_{
				RegisterTwoFactorAuthKeys: &kmspb.SingleTenantHsmInstanceProposal_RegisterTwoFactorAuthKeys{RequiredApproverCount: 2, TwoFactorPublicKeyPems: []string{a[0].pem, a[1].pem, a[2].pem}}}})
			cs := reg.GetQuorumParameters().GetChallenges()
			if reg.GetState() != kmspb.SingleTenantHsmInstanceProposal_PENDING || len(cs) != 3 || reg.GetQuorumParameters().GetRequiredApproverCount() != 3 {
				t.Fatalf("register proposal = %v", reg)
			}
			if _, err := propose("other", &kmspb.SingleTenantHsmInstanceProposal{Operation: &kmspb.SingleTenantHsmInstanceProposal_DeleteSingleTenantHsmInstance_{}}); code(err) != codes.FailedPrecondition {
				t.Errorf("a second active proposal = %v, want FAILED_PRECONDITION", err)
			}
			// Never an unsigned or forged approval.
			if err := quorum(reg.GetName(), &kmspb.ChallengeReply{PublicKeyPem: a[0].pem}); code(err) != codes.InvalidArgument {
				t.Errorf("an unsigned reply = %v, want INVALID_ARGUMENT", err)
			}
			wrong := a[0].sign(t, cs)
			wrong.PublicKeyPem = a[1].pem
			if err := quorum(reg.GetName(), wrong); code(err) != codes.InvalidArgument {
				t.Errorf("a reply signed by another key = %v, want INVALID_ARGUMENT", err)
			}
			outsider := newHsmApprover(t)
			forged := a[2].sign(t, cs)
			forged.PublicKeyPem = outsider.pem
			if err := quorum(reg.GetName(), forged); code(err) != codes.InvalidArgument {
				t.Errorf("a key outside the quorum = %v, want INVALID_ARGUMENT", err)
			}
			if err := quorum(reg.GetName(), a[0].sign(t, cs), a[1].sign(t, cs)); err != nil {
				t.Fatal(err)
			}
			got, err := hc.GetSingleTenantHsmInstanceProposal(ctx, &kmspb.GetSingleTenantHsmInstanceProposalRequest{Name: reg.GetName()})
			if err != nil || got.GetState() != kmspb.SingleTenantHsmInstanceProposal_PENDING || len(got.GetQuorumParameters().GetApprovedTwoFactorPublicKeyPems()) != 2 {
				t.Fatalf("after 2 of 3 = %v, %v (register_2fa_keys needs all)", got, err)
			}
			if err := quorum(reg.GetName(), a[2].sign(t, cs)); err != nil {
				t.Fatal(err)
			}
			execute(reg.GetName())
			if st := state(); st != kmspb.SingleTenantHsmInstance_ACTIVE {
				t.Fatalf("after register = %s", st)
			}

			ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "sthi-" + variant})
			if err != nil {
				t.Fatal(err)
			}
			key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k", CryptoKey: &kmspb.CryptoKey{
				Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, CryptoKeyBackend: inst.GetName(),
				VersionTemplate: &kmspb.CryptoKeyVersionTemplate{ProtectionLevel: kmspb.ProtectionLevel_HSM_SINGLE_TENANT}}})
			if err != nil || key.GetCryptoKeyBackend() != inst.GetName() {
				t.Fatalf("HSM_SINGLE_TENANT key = %v, %v", key, err)
			}
			if _, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "nobackend", CryptoKey: &kmspb.CryptoKey{
				Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, VersionTemplate: &kmspb.CryptoKeyVersionTemplate{ProtectionLevel: kmspb.ProtectionLevel_HSM_SINGLE_TENANT}}}); code(err) != codes.InvalidArgument {
				t.Errorf("HSM_SINGLE_TENANT without crypto_key_backend = %v, want INVALID_ARGUMENT", err)
			}
			enc, err := c.Encrypt(ctx, &kmspb.EncryptRequest{Name: key.GetName(), Plaintext: []byte("single tenant")})
			if err != nil || enc.GetProtectionLevel() != kmspb.ProtectionLevel_HSM_SINGLE_TENANT {
				t.Fatalf("Encrypt = %v, %v", enc, err)
			}

			run("disable", &kmspb.SingleTenantHsmInstanceProposal{Operation: &kmspb.SingleTenantHsmInstanceProposal_DisableSingleTenantHsmInstance_{}}, a[0], a[2])
			if st := state(); st != kmspb.SingleTenantHsmInstance_DISABLED {
				t.Fatalf("after disable = %s", st)
			}
			if _, err := c.Decrypt(ctx, &kmspb.DecryptRequest{Name: key.GetName(), Ciphertext: enc.GetCiphertext()}); code(err) != codes.FailedPrecondition {
				t.Errorf("Decrypt in a DISABLED instance = %v, want FAILED_PRECONDITION", err)
			}
			run("enable", &kmspb.SingleTenantHsmInstanceProposal{Operation: &kmspb.SingleTenantHsmInstanceProposal_EnableSingleTenantHsmInstance_{}}, a[1], a[2])
			if dec, err := c.Decrypt(ctx, &kmspb.DecryptRequest{Name: key.GetName(), Ciphertext: enc.GetCiphertext()}); err != nil || string(dec.GetPlaintext()) != "single tenant" {
				t.Fatalf("Decrypt after enable = %v, %v", dec, err)
			}

			d := newHsmApprover(t)
			add := mustPropose("add", &kmspb.SingleTenantHsmInstanceProposal{Operation: &kmspb.SingleTenantHsmInstanceProposal_AddQuorumMember_{
				AddQuorumMember: &kmspb.SingleTenantHsmInstanceProposal_AddQuorumMember{TwoFactorPublicKeyPem: d.pem}}})
			rq := add.GetRequiredActionQuorumParameters()
			if len(rq.GetRequiredChallenges()) != 1 || len(rq.GetQuorumChallenges()) != 3 || rq.GetRequiredApproverCount() != 2 {
				t.Fatalf("add_quorum_member parameters = %v", rq)
			}
			approveRA := func(must, q []*kmspb.ChallengeReply) error {
				_, err := hc.ApproveSingleTenantHsmInstanceProposal(ctx, &kmspb.ApproveSingleTenantHsmInstanceProposalRequest{Name: add.GetName(),
					ApprovalPayload: &kmspb.ApproveSingleTenantHsmInstanceProposalRequest_RequiredActionQuorumReply_{RequiredActionQuorumReply: &kmspb.ApproveSingleTenantHsmInstanceProposalRequest_RequiredActionQuorumReply{
						RequiredChallengeReplies: must, QuorumChallengeReplies: q}}})
				return err
			}
			if err := quorum(add.GetName(), a[0].sign(t, rq.GetQuorumChallenges())); code(err) != codes.InvalidArgument {
				t.Errorf("quorum_reply to required-action parameters = %v, want INVALID_ARGUMENT", err)
			}
			if err := approveRA(nil, []*kmspb.ChallengeReply{a[0].sign(t, rq.GetQuorumChallenges()), a[1].sign(t, rq.GetQuorumChallenges())}); err != nil {
				t.Fatal(err)
			}
			if _, err := hc.ExecuteSingleTenantHsmInstanceProposal(ctx, &kmspb.ExecuteSingleTenantHsmInstanceProposalRequest{Name: add.GetName()}); code(err) != codes.FailedPrecondition {
				t.Errorf("execute without the new key's signature = %v, want FAILED_PRECONDITION", err)
			}
			if err := approveRA([]*kmspb.ChallengeReply{d.sign(t, rq.GetRequiredChallenges())}, nil); err != nil {
				t.Fatal(err)
			}
			execute(add.GetName())
			got2, err := hc.GetSingleTenantHsmInstance(ctx, &kmspb.GetSingleTenantHsmInstanceRequest{Name: inst.GetName()})
			if err != nil || got2.GetQuorumAuth().GetTotalApproverCount() != 4 {
				t.Fatalf("after add = %v, %v", got2, err)
			}

			run("remove", &kmspb.SingleTenantHsmInstanceProposal{Operation: &kmspb.SingleTenantHsmInstanceProposal_RemoveQuorumMember_{
				RemoveQuorumMember: &kmspb.SingleTenantHsmInstanceProposal_RemoveQuorumMember{TwoFactorPublicKeyPem: a[0].pem}}}, d, a[1])
			run("refresh", &kmspb.SingleTenantHsmInstanceProposal{Operation: &kmspb.SingleTenantHsmInstanceProposal_RefreshSingleTenantHsmInstance_{}}, a[1], a[2])

			// A deleted proposal cannot run, and frees the instance.
			gone := mustPropose("dropped", &kmspb.SingleTenantHsmInstanceProposal{Expiration: &kmspb.SingleTenantHsmInstanceProposal_Ttl{Ttl: durationpb.New(3600e9)},
				Operation: &kmspb.SingleTenantHsmInstanceProposal_DisableSingleTenantHsmInstance_{}})
			if err := quorum(gone.GetName(), a[0].sign(t, []*kmspb.Challenge{{PublicKeyPem: a[0].pem}})); code(err) != codes.InvalidArgument {
				t.Errorf("a removed member's reply = %v, want INVALID_ARGUMENT", err)
			}
			if err := hc.DeleteSingleTenantHsmInstanceProposal(ctx, &kmspb.DeleteSingleTenantHsmInstanceProposalRequest{Name: gone.GetName()}); err != nil {
				t.Fatal(err)
			}
			if _, err := hc.ExecuteSingleTenantHsmInstanceProposal(ctx, &kmspb.ExecuteSingleTenantHsmInstanceProposalRequest{Name: gone.GetName()}); code(err) != codes.FailedPrecondition {
				t.Errorf("execute a deleted proposal = %v, want FAILED_PRECONDITION", err)
			}
			count := func(showDeleted bool) int {
				n := 0
				for it := hc.ListSingleTenantHsmInstanceProposals(ctx, &kmspb.ListSingleTenantHsmInstanceProposalsRequest{Parent: inst.GetName(), ShowDeleted: showDeleted}); ; n++ {
					if _, err := it.Next(); err == iterator.Done {
						return n
					} else if err != nil {
						t.Fatal(err)
					}
				}
			}
			if live, all := count(false), count(true); live != 6 || all != 7 {
				t.Errorf("proposals listed = %d, with deleted %d; want 6 and 7", live, all)
			}

			run("disable-again", &kmspb.SingleTenantHsmInstanceProposal{Operation: &kmspb.SingleTenantHsmInstanceProposal_DisableSingleTenantHsmInstance_{}}, a[1], d)
			run("delete", &kmspb.SingleTenantHsmInstanceProposal{Operation: &kmspb.SingleTenantHsmInstanceProposal_DeleteSingleTenantHsmInstance_{}}, a[2], d)
			if st := state(); st != kmspb.SingleTenantHsmInstance_DELETED {
				t.Fatalf("after delete = %s", st)
			}
			hidden := 0
			for it := hc.ListSingleTenantHsmInstances(ctx, &kmspb.ListSingleTenantHsmInstancesRequest{Parent: loc, Filter: "name:sthi-" + variant}); ; hidden++ {
				if _, err := it.Next(); err == iterator.Done {
					break
				} else if err != nil {
					t.Fatal(err)
				}
			}
			if hidden != 0 {
				t.Errorf("a DELETED instance is listed without show_deleted")
			}
		})
	}
}
