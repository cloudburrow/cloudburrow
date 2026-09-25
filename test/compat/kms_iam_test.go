//go:build compat

package compat

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestKMSIamPolicyIsStoredNotEnforced (#428; ADR-0006 as amended by #421):
// on a key ring and on a crypto key, through the official client against the
// CI instance, a policy round-trips with etags, a stale etag is ABORTED, a
// condition is UNIMPLEMENTED, version 3 without conditions is accepted, and
// TestIamPermissions returns everything asked. Nothing is inherited, and
// nothing is enforced: Encrypt and Decrypt still work for a caller no binding
// names.
//
// covers: google.iam.v1.IAMPolicy/GetIamPolicy, google.iam.v1.IAMPolicy/SetIamPolicy, google.iam.v1.IAMPolicy/TestIamPermissions
//
// unverified: google.iam.v1.IAMPolicy/GetIamPolicy NOT_FOUND: a key ring that does not exist
// unverified: google.iam.v1.IAMPolicy/SetIamPolicy ABORTED: a stale etag
// unverified: google.iam.v1.IAMPolicy/GetIamPolicy UNIMPLEMENTED: an import job, which CloudBurrow does not serve
func TestKMSIamPolicyIsStoredNotEnforced(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	clients := kmsClients(t, h)
	loc := "projects/" + h.Project() + "/locations/global"
	const other = "user:someone-else@example.com"
	since := time.Now()
	// Every case over gRPC and over REST (#429); resources are created over
	// gRPC. Codes over REST are read from the envelope (kmsCode).
	for variant, c := range clients {
		t.Run(variant, func(t *testing.T) {
			g := clients["grpc"]
			ring, err := g.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: loc, KeyRingId: "iam-" + variant})
			if err != nil {
				t.Fatal(err)
			}
			key, err := g.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k",
				CryptoKey: &kmspb.CryptoKey{Purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT}})
			if err != nil {
				t.Fatal(err)
			}
			for kind, res := range map[string]string{"ring": ring.GetName(), "key": key.GetName()} {
				t.Run(kind, func(t *testing.T) {
					empty, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: res})
					if err != nil || len(empty.GetBindings()) != 0 || len(empty.GetEtag()) == 0 {
						t.Fatalf("GetIamPolicy on a new %s = %v, %v", kind, empty, err)
					}
					set, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: res, Policy: &iampb.Policy{Etag: empty.GetEtag(),
						Bindings: []*iampb.Binding{{Role: "roles/cloudkms.cryptoKeyDecrypter", Members: []string{other}}}}})
					if err != nil || bytes.Equal(set.GetEtag(), empty.GetEtag()) {
						t.Fatalf("SetIamPolicy = %v, %v; want a new etag", set, err)
					}
					got, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: res})
					if err != nil || !bytes.Equal(got.GetEtag(), set.GetEtag()) || len(got.GetBindings()) != 1 || got.GetBindings()[0].GetMembers()[0] != other {
						t.Errorf("GetIamPolicy after Set = %v, %v", got, err)
					}
					if _, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: res, Policy: &iampb.Policy{Etag: empty.GetEtag()}}); kmsCode(variant, err) != codes.Aborted {
						t.Errorf("a stale etag = %v, want ABORTED", err)
					}
					_, err = c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: res, Policy: &iampb.Policy{Version: 3,
						Bindings: []*iampb.Binding{{Role: "roles/cloudkms.admin", Members: []string{other}, Condition: &expr.Expr{Expression: "true"}}}}})
					if kmsCode(variant, err) != codes.Unimplemented || !strings.Contains(status.Convert(err).Message(), "condition") {
						t.Errorf("a conditional binding = %v, want UNIMPLEMENTED naming condition", err)
					}
					if _, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: res, Policy: &iampb.Policy{Version: 3,
						Bindings: []*iampb.Binding{{Role: "roles/cloudkms.cryptoKeyDecrypter", Members: []string{other}}}}}); err != nil {
						t.Errorf("a version 3 policy without conditions = %v", err)
					}
					perms := []string{"cloudkms.cryptoKeys.get", "cloudkms.cryptoKeyVersions.useToDecrypt"}
					if r, err := c.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: res, Permissions: perms}); err != nil ||
						strings.Join(r.GetPermissions(), ",") != strings.Join(perms, ",") {
						t.Errorf("TestIamPermissions = %v, %v; want every permission", r, err)
					}
				})
			}

			// No inheritance: each has its own policy.
			rp, _ := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: ring.GetName()})
			kp, _ := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: key.GetName()})
			if bytes.Equal(rp.GetEtag(), kp.GetEtag()) {
				t.Error("the ring and the key share a policy")
			}

			// ResourceIAM builds a gRPC handle from the client's connection, so it
			// exists for the gRPC client only.
			if variant == "grpc" {
				handle := c.ResourceIAM(key.GetName())
				pol, err := handle.Policy(ctx)
				if err != nil {
					t.Fatalf("ResourceIAM.Policy: %v", err)
				}
				pol.Add("user:added@example.com", "roles/cloudkms.viewer")
				if err := handle.SetPolicy(ctx, pol); err != nil {
					t.Fatalf("ResourceIAM.SetPolicy: %v", err)
				}
				if back, err := handle.Policy(ctx); err != nil || !back.HasRole("user:added@example.com", "roles/cloudkms.viewer") {
					t.Errorf("ResourceIAM read back = %v, %v", back, err)
				}
			}

			// Not enforced: both policies bind someone else, and the caller still
			// encrypts and decrypts.
			enc, err := c.Encrypt(ctx, &kmspb.EncryptRequest{Name: key.GetName(), Plaintext: []byte("not-enforced")})
			if err != nil {
				t.Fatalf("Encrypt under a policy naming someone else: %v", err)
			}
			if dec, err := c.Decrypt(ctx, &kmspb.DecryptRequest{Name: key.GetName(), Ciphertext: enc.GetCiphertext()}); err != nil || string(dec.GetPlaintext()) != "not-enforced" {
				t.Errorf("Decrypt under a policy naming someone else = %v, %v", dec, err)
			}

			// Missing resources and resources CloudBurrow does not serve.
			if _, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: loc + "/keyRings/absent"}); kmsCode(variant, err) != codes.NotFound {
				t.Errorf("GetIamPolicy on a missing ring = %v, want NOT_FOUND", err)
			}
			if r, err := c.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: loc + "/keyRings/absent",
				Permissions: []string{"cloudkms.keyRings.get"}}); err != nil || len(r.GetPermissions()) != 0 {
				t.Errorf("TestIamPermissions on a missing ring = %v, %v; want an empty set (cloudkms_v1.yaml:59-64)", r, err)
			}
			if _, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: ring.GetName() + "/importJobs/j"}); kmsCode(variant, err) != codes.Unimplemented ||
				!strings.Contains(status.Convert(err).Message(), "import jobs") {
				t.Errorf("GetIamPolicy on an import job = %v, want UNIMPLEMENTED naming import jobs", err)
			}
		})
	}

	// The JSON calls are in the event log under kms, path and code only.
	if os.Getenv(EnvControl) == "" {
		return
	}
	seen := false
	for _, e := range events(t, h, h.Endpoint(EnvControl), "kms", since) {
		if strings.HasSuffix(e.Target, ":setIamPolicy") {
			seen = true
			for k, v := range e.Detail {
				if strings.Contains(v, other) || strings.Contains(k, "body") {
					t.Errorf("an IAM event records the body: %v", e.Detail)
				}
			}
		}
	}
	if !seen {
		t.Error("no :setIamPolicy JSON request in /admin/events for kms")
	}
}
