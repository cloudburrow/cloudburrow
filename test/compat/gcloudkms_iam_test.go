//go:build compat

package compat

import (
	"encoding/json"
	"strings"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/kms/apiv1/kmspb"
)

// TestGcloudKMSIam (#431): KMS IAM through real gcloud, configured by
// gcloud-setup alone; stored, never enforced (ADR-0006 as amended by #421).
// gcloud sends version 3 policies and read-modify-writes with the etag, so
// a second add keeps the first. A binding with an IAM Condition is stored
// (#1745), as Google's KMS stores one; gcloud then refuses to add an
// unconditional binding to that policy non-interactively unless it is
// given --condition=None, as it does against Google.
func TestGcloudKMSIam(t *testing.T) {
	h := New(t)
	run, project := gcloudKMS(t, h)
	c := kmsClients(t, h)["grpc"]
	ctx := h.Context()
	ringID := "gci-" + h.Project()
	ring, err := c.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/" + project + "/locations/global", KeyRingId: ringID})
	if err != nil {
		t.Fatal(err)
	}
	key, err := c.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
		CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
	if err != nil {
		t.Fatal(err)
	}
	gc := func(args ...string) string {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatalf("gcloud %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	roles := func(res string) map[string]string {
		t.Helper()
		p, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: res, Options: &iampb.GetPolicyOptions{RequestedPolicyVersion: 3}})
		if err != nil {
			t.Fatalf("GetIamPolicy %s: %v", res, err)
		}
		out := map[string]string{}
		for _, b := range p.GetBindings() {
			role := b.GetRole()
			if c := b.GetCondition(); c != nil {
				role += " if " + c.GetExpression()
			}
			out[role] = strings.Join(b.GetMembers(), ",")
		}
		return out
	}
	const dev = "user:dev@example.com"
	loc := []string{"--location", "global"}
	kr := append([]string{"--keyring", ringID}, loc...)

	gc(append([]string{"kms", "keyrings", "add-iam-policy-binding", ringID, "--member", dev, "--role", "roles/cloudkms.viewer"}, loc...)...)
	gc(append([]string{"kms", "keys", "add-iam-policy-binding", "k", "--member", dev, "--role", "roles/cloudkms.cryptoKeyEncrypterDecrypter"}, kr...)...)
	if r := roles(ring.GetName()); r["roles/cloudkms.viewer"] != dev {
		t.Errorf("the ring's policy = %v; want viewer → %s", r, dev)
	}
	if r := roles(key.GetName()); r["roles/cloudkms.cryptoKeyEncrypterDecrypter"] != dev {
		t.Errorf("the key's policy = %v; want cryptoKeyEncrypterDecrypter → %s", r, dev)
	}

	var got struct {
		Etag     string `json:"etag"`
		Bindings []struct {
			Role    string   `json:"role"`
			Members []string `json:"members"`
		} `json:"bindings"`
	}
	if err := json.Unmarshal([]byte(gc(append([]string{"kms", "keys", "get-iam-policy", "k", "--format=json"}, kr...)...)), &got); err != nil {
		t.Fatalf("get-iam-policy is not JSON: %v", err)
	}
	if got.Etag == "" || len(got.Bindings) != 1 || got.Bindings[0].Role != "roles/cloudkms.cryptoKeyEncrypterDecrypter" {
		t.Errorf("get-iam-policy = %+v; want the binding and an etag", got)
	}

	const expr = `request.time < timestamp("2030-01-01T00:00:00Z")`
	gc(append([]string{"kms", "keys", "add-iam-policy-binding", "k", "--member", dev, "--role", "roles/cloudkms.admin",
		"--condition=expression=" + expr + ",title=t"}, kr...)...)
	if r := roles(key.GetName()); r["roles/cloudkms.admin if "+expr] != dev || r["roles/cloudkms.admin"] != "" {
		t.Errorf("after a conditional add, the key's policy = %v; want admin → %s on the condition only", r, dev)
	}

	// The policy holds a condition now: an unconditional binding needs
	// --condition=None, without which gcloud refuses in non-interactive
	// mode (as against Google).
	const second = "user:second@example.com"
	viewer := append([]string{"kms", "keys", "add-iam-policy-binding", "k", "--member", second, "--role", "roles/cloudkms.viewer"}, kr...)
	if out, err := run(viewer...); err == nil || !strings.Contains(out, "--condition=None") {
		t.Errorf("an unconditional add to a policy with a condition, without --condition = %v:\n%s; want gcloud to ask for --condition=None", err, out)
	}
	gc(append(viewer, "--condition=None")...)
	r := roles(key.GetName())
	if r["roles/cloudkms.cryptoKeyEncrypterDecrypter"] != dev || r["roles/cloudkms.viewer"] != second || r["roles/cloudkms.admin if "+expr] != dev {
		t.Errorf("after a second add, the key's policy = %v; want all three bindings", r)
	}

	gc(append([]string{"kms", "keys", "remove-iam-policy-binding", "k", "--member", dev, "--role", "roles/cloudkms.admin",
		"--condition=expression=" + expr + ",title=t"}, kr...)...)
	if r := roles(key.GetName()); r["roles/cloudkms.admin if "+expr] != "" || r["roles/cloudkms.viewer"] != second {
		t.Errorf("after removing the conditional binding, the key's policy = %v", r)
	}
}
