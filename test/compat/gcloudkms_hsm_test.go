//go:build compat

package compat

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
)

// TestGcloudKMSSingleTenantHsm (#1469): gcloud kms single-tenant-hsm,
// configured by `cloudburrow gcloud-setup` alone, follows
// create-manage-single-tenant-hsm: create an instance with
// --total-approver-count 3, describe and list it; a register_2fa_keys
// proposal from three PEM files, approved with
// --quorum-challenge-replies of challenges signed by those keys, then
// executed; a key created --protection-level hsm-single-tenant with the
// instance as --crypto-key-backend, which encrypts and decrypts; then a
// refresh_sthi proposal listed and deleted. The official clients read the
// challenges and check each step.
func TestGcloudKMSSingleTenantHsm(t *testing.T) {
	h := New(t)
	run, project := gcloudKMS(t, h)
	ctx := h.Context()
	hc, err := kms.NewHsmManagementClient(ctx, kmsOptions(h, "grpc")...)
	if err != nil {
		t.Fatal(err)
	}
	defer hc.Close()
	gc := func(args ...string) string {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatalf("gcloud %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	const loc = "us-east4"
	// The CLI's project is the instance's own, shared by every run: the
	// names carry this run's project ID.
	id := "gc-sthi-" + h.Project()
	inst := "projects/" + project + "/locations/" + loc + "/singleTenantHsmInstances/" + id
	gc("kms", "single-tenant-hsm", "create", "--location", loc, "--total-approver-count", "3", "--single-tenant-hsm-instance-id", id)
	if out := gc("kms", "single-tenant-hsm", "describe", inst, "--format", "value(state)"); strings.TrimSpace(out) != "PENDING_TWO_FACTOR_AUTH_REGISTRATION" {
		t.Fatalf("describe = %q", out)
	}
	if out := gc("kms", "single-tenant-hsm", "list", "--location", loc, "--format", "value(name)"); !strings.Contains(out, inst) {
		t.Errorf("list = %q", out)
	}

	dir := t.TempDir()
	a := []hsmApprover{newHsmApprover(t), newHsmApprover(t), newHsmApprover(t)}
	var pemFiles []string
	for i, x := range a {
		f := filepath.Join(dir, fmt.Sprintf("rsapub%d.pem", i))
		if err := os.WriteFile(f, []byte(x.pem), 0o600); err != nil {
			t.Fatal(err)
		}
		pemFiles = append(pemFiles, f)
	}
	// replies signs the proposal's challenges with by and writes the
	// --*-challenge-replies list of (signed file, public key file).
	replies := func(id string, cs []*kmspb.Challenge, by ...int) string {
		t.Helper()
		var tuples []string
		for _, i := range by {
			r := a[i].sign(t, cs)
			f := filepath.Join(dir, fmt.Sprintf("%s-signed-%d.bin", id, i))
			if err := os.WriteFile(f, r.GetSignedChallenge(), 0o600); err != nil {
				t.Fatal(err)
			}
			tuples = append(tuples, fmt.Sprintf("('%s','%s')", f, pemFiles[i]))
		}
		return "[" + strings.Join(tuples, ",") + "]"
	}
	gc("kms", "single-tenant-hsm", "proposal", "create", inst, "--operation-type", "register_2fa_keys",
		"--single-tenant-hsm-instance-proposal-id", "register", "--required-approver-count", "2",
		"--two-factor-public-key-pems", strings.Join(pemFiles, ","))
	prop := inst + "/proposals/register"
	if out := gc("kms", "single-tenant-hsm", "proposal", "describe", prop, "--format", "value(state)"); strings.TrimSpace(out) != "PENDING" {
		t.Fatalf("proposal describe = %q", out)
	}
	p, err := hc.GetSingleTenantHsmInstanceProposal(ctx, &kmspb.GetSingleTenantHsmInstanceProposalRequest{Name: prop})
	if err != nil {
		t.Fatal(err)
	}
	gc("kms", "single-tenant-hsm", "proposal", "approve", prop, "--quorum-challenge-replies", replies("register", p.GetQuorumParameters().GetChallenges(), 0, 1, 2))
	gc("kms", "single-tenant-hsm", "proposal", "execute", prop)
	if out := gc("kms", "single-tenant-hsm", "describe", inst, "--format", "value(state,quorumAuth.requiredApproverCount)"); !strings.Contains(out, "ACTIVE") || !strings.Contains(out, "2") {
		t.Fatalf("after register = %q", out)
	}

	ring := "gc-sthi-" + h.Project()
	at := []string{"--keyring", ring, "--location", loc}
	gc("kms", "keyrings", "create", ring, "--location", loc)
	gc(append([]string{"kms", "keys", "create", "k", "--purpose", "encryption", "--protection-level", "hsm-single-tenant", "--crypto-key-backend", inst}, at...)...)
	pt, ct, back := filepath.Join(dir, "p"), filepath.Join(dir, "c"), filepath.Join(dir, "b")
	if err := os.WriteFile(pt, []byte("single tenant via gcloud"), 0o600); err != nil {
		t.Fatal(err)
	}
	gc(append([]string{"kms", "encrypt", "--key", "k", "--plaintext-file", pt, "--ciphertext-file", ct}, at...)...)
	gc(append([]string{"kms", "decrypt", "--key", "k", "--ciphertext-file", ct, "--plaintext-file", back}, at...)...)
	if b, err := os.ReadFile(back); err != nil || string(b) != "single tenant via gcloud" {
		t.Errorf("decrypt = %q, %v", b, err)
	}

	gc("kms", "single-tenant-hsm", "proposal", "create", inst, "--operation-type", "refresh_sthi", "--single-tenant-hsm-instance-proposal-id", "refresh")
	if out := gc("kms", "single-tenant-hsm", "proposal", "list", inst, "--format", "value(name)"); !strings.Contains(out, "proposals/refresh") || !strings.Contains(out, "proposals/register") {
		t.Errorf("proposal list = %q", out)
	}
	gc("kms", "single-tenant-hsm", "proposal", "delete", inst+"/proposals/refresh")
	got, err := hc.GetSingleTenantHsmInstanceProposal(ctx, &kmspb.GetSingleTenantHsmInstanceProposalRequest{Name: inst + "/proposals/refresh"})
	if err != nil || got.GetState() != kmspb.SingleTenantHsmInstanceProposal_DELETED {
		t.Errorf("after delete = %v, %v", got, err)
	}
}
