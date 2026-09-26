//go:build compat

package compat

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/api/iterator"
)

// TestTerraformKMS (#425): a key ring, a crypto key and a crypto key version
// apply through `cloudburrow terraform` with the official hashicorp/google
// provider, with a key ring iam_member, a crypto key iam_member and a crypto
// key iam_binding (#430; stored, never enforced, ADR-0006), plan clean, take a label change as PATCH ?updateMask=labels, and
// destroy, all with egress blocked. Destroy does what the provider does on
// Google (v8.4.0): the ring is only dropped from state, and the key is not
// deleted but has every version destroyed; the evidence is the SDK read, not
// Terraform's exit code, because the version resource discards the error of
// its :destroy call.
func TestTerraformKMS(t *testing.T) {
	h := New(t)
	if _, err := exec.LookPath("terraform"); err != nil {
		t.Skip("terraform is not on PATH")
	}
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	c := kmsClients(t, h)["grpc"] // skips without CLOUDBURROW_TEST_KMS
	ctx := h.Context()
	flags := strings.Fields(os.Getenv(EnvCLIArgs))
	control := h.Endpoint(EnvControl)
	dir := t.TempDir()
	member := "serviceAccount:tf-kms@" + h.Project() + ".iam.gserviceaccount.com"
	ringID := "tf-" + h.Project()
	ring := "projects/" + h.Project() + "/locations/us-central1/keyRings/" + ringID
	key := ring + "/cryptoKeys/k"

	write := func(label string) {
		t.Helper()
		module := fmt.Sprintf(`terraform {
  required_providers {
    google = { source = "hashicorp/google", version = "~> 8.0" }
  }
}
resource "google_kms_key_ring" "kr" {
  project  = %q
  name     = %q
  location = "us-central1"
}
resource "google_kms_crypto_key" "k" {
  name     = "k"
  key_ring = google_kms_key_ring.kr.id
  labels   = { env = %q }
}
resource "google_kms_crypto_key_version" "v" {
  crypto_key = google_kms_crypto_key.k.id
}
# Stored, never enforced (ADR-0006).
resource "google_kms_key_ring_iam_member" "rm" {
  key_ring_id = google_kms_key_ring.kr.id
  role        = "roles/cloudkms.viewer"
  member      = %q
}
# Stored, never enforced (ADR-0006).
resource "google_kms_crypto_key_iam_member" "km" {
  crypto_key_id = google_kms_crypto_key.k.id
  role          = "roles/cloudkms.cryptoKeyEncrypterDecrypter"
  member        = %q
}
# Stored, never enforced (ADR-0006). A different role from the iam_member,
# so the two do not fight.
resource "google_kms_crypto_key_iam_binding" "kb" {
  crypto_key_id = google_kms_crypto_key.k.id
  role          = "roles/cloudkms.viewer"
  members       = [%q]
}
`, h.Project(), ringID, label, member, member, member)
		if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(module), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tf := func(args ...string) (string, error) {
		t.Helper()
		cmd := exec.Command(cli, append(append(append([]string{"terraform"}, flags...), "--"), args...)...)
		cmd.Dir = dir
		if args[0] != "init" {
			cmd.Env = noGoogleEgress()
		}
		b, err := cmd.CombinedOutput()
		if left, _ := filepath.Glob(filepath.Join(dir, "cloudburrow_providers*")); len(left) != 0 {
			t.Fatalf("the wrapper left %v behind after %s", left, args[0])
		}
		return string(b), err
	}
	run := func(args ...string) {
		t.Helper()
		out, err := tf(args...)
		if err != nil {
			t.Fatalf("cloudburrow terraform %s: %v\n%s", strings.Join(args, " "), err, lastLines(out, 30))
		}
		t.Logf("terraform %s:\n%s", args[0], lastLines(out, 6))
	}
	cleanPlan := func(when string) {
		t.Helper()
		if out, err := tf("plan", "-detailed-exitcode", "-input=false", "-no-color"); err != nil {
			t.Errorf("plan %s is not clean (%v):\n%s", when, err, lastLines(out, 40))
		}
	}

	write("local")
	run("init", "-input=false", "-no-color")
	run("apply", "-auto-approve", "-input=false", "-no-color")

	if _, err := c.GetKeyRing(ctx, &kmspb.GetKeyRingRequest{Name: ring}); err != nil {
		t.Fatalf("the applied key ring: %v", err)
	}
	k, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key})
	if err != nil {
		t.Fatalf("the applied key: %v", err)
	}
	if k.GetLabels()["env"] != "local" || k.GetLabels()["goog-terraform-provisioned"] != "true" ||
		k.GetPrimary().GetName() != key+"/cryptoKeyVersions/1" || k.GetPrimary().GetState() != kmspb.CryptoKeyVersion_ENABLED {
		t.Errorf("the applied key = labels %v, primary %v %v", k.GetLabels(), k.GetPrimary().GetName(), k.GetPrimary().GetState())
	}
	if v, err := c.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: key + "/cryptoKeyVersions/2"}); err != nil ||
		v.GetState() != kmspb.CryptoKeyVersion_ENABLED {
		t.Errorf("the applied version 2 = %v, %v; want ENABLED", v.GetState(), err)
	}
	// The IAM resources read back through the official client (#430).
	roles := func(res string) map[string][]string {
		t.Helper()
		p, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: res})
		if err != nil {
			t.Fatalf("GetIamPolicy %s: %v", res, err)
		}
		out := map[string][]string{}
		for _, b := range p.GetBindings() {
			out[b.GetRole()] = b.GetMembers()
		}
		return out
	}
	if r := roles(ring); len(r) != 1 || strings.Join(r["roles/cloudkms.viewer"], ",") != member {
		t.Errorf("the ring's policy after apply = %v; want viewer → %s", r, member)
	}
	if r := roles(key); len(r) != 2 || strings.Join(r["roles/cloudkms.cryptoKeyEncrypterDecrypter"], ",") != member ||
		strings.Join(r["roles/cloudkms.viewer"], ",") != member {
		t.Errorf("the key's policy after apply = %v; want encrypterDecrypter and viewer → %s", r, member)
	}
	cleanPlan("right after apply")

	patchesBefore := time.Now()
	write("changed")
	run("apply", "-auto-approve", "-input=false", "-no-color")
	if k, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key}); err != nil || k.GetLabels()["env"] != "changed" {
		t.Errorf("after the label change: %v, %v", k.GetLabels(), err)
	}
	sawPatch := false
	for _, e := range events(t, h, control, "kms", patchesBefore) {
		if strings.HasPrefix(e.Target, "PATCH /v1/"+key) {
			sawPatch = true
		}
	}
	if !sawPatch {
		t.Error("the label change sent no PATCH to the key")
	}
	cleanPlan("after the label change")

	destroyBefore := time.Now()
	run("destroy", "-auto-approve", "-input=false", "-no-color")
	// The wrapper prints its own notices ("cloudburrow terraform: ...") on
	// stderr, which tf captures with the output; only Terraform's lines count.
	out, err := tf("state", "list")
	var state []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "cloudburrow terraform:") {
			state = append(state, l)
		}
	}
	if err != nil || len(state) != 0 {
		t.Errorf("state after destroy = %q, %v; want empty", state, err)
	}
	if _, err := c.GetKeyRing(ctx, &kmspb.GetKeyRingRequest{Name: ring}); err != nil {
		t.Errorf("the key ring is gone after destroy, which the provider never deletes: %v", err)
	}
	if _, err := c.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key}); err != nil {
		t.Errorf("the key is gone after destroy, which the provider never deletes: %v", err)
	}
	for _, res := range []string{ring, key} {
		for role, members := range roles(res) {
			if strings.Contains(strings.Join(members, ","), member) {
				t.Errorf("%s still binds %s to %s after destroy", res, role, member)
			}
		}
	}
	it := c.ListCryptoKeyVersions(ctx, &kmspb.ListCryptoKeyVersionsRequest{Parent: key})
	n := 0
	for {
		v, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		n++
		if v.GetState() != kmspb.CryptoKeyVersion_DESTROY_SCHEDULED {
			t.Errorf("%s after destroy = %v, want DESTROY_SCHEDULED", v.GetName(), v.GetState())
		} else if d := time.Until(v.GetDestroyTime().AsTime()); d < 29*24*time.Hour || d > 31*24*time.Hour {
			t.Errorf("%s destroy_time is %v away, want about 30 days", v.GetName(), d)
		}
	}
	if n < 2 {
		t.Errorf("%d versions after destroy, want at least 2", n)
	}
	var destroys, deletes int
	for _, e := range events(t, h, control, "kms", destroyBefore) {
		switch {
		case strings.HasSuffix(e.Target, ":destroy"):
			destroys++
		case strings.HasPrefix(e.Target, "DELETE "):
			deletes++
		}
	}
	if destroys == 0 || deletes != 0 {
		t.Errorf("destroy sent %d :destroy and %d DELETE requests; want some and none", destroys, deletes)
	}
}
