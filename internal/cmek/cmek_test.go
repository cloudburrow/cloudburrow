package cmek

import (
	"bytes"
	"context"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/iamcheck"
	"github.com/cloudburrow/cloudburrow/internal/service/kms"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

func newKey(t *testing.T, k *kms.Server, purpose kmspb.CryptoKey_CryptoKeyPurpose) string {
	t.Helper()
	ctx := context.Background()
	ring, err := k.CreateKeyRing(ctx, &kmspb.CreateKeyRingRequest{Parent: "projects/my-proj/locations/us-central1", KeyRingId: "ring"})
	if err != nil {
		t.Fatal(err)
	}
	ck := &kmspb.CryptoKey{Purpose: purpose}
	if purpose == kmspb.CryptoKey_ASYMMETRIC_SIGN {
		ck.VersionTemplate = &kmspb.CryptoKeyVersionTemplate{Algorithm: kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256}
	}
	key, err := k.CreateCryptoKey(ctx, &kmspb.CreateCryptoKeyRequest{Parent: ring.GetName(), CryptoKeyId: "k" + purpose.String(), CryptoKey: ck})
	if err != nil {
		t.Fatal(err)
	}
	return key.GetName()
}

func setState(t *testing.T, k *kms.Server, key string, st kmspb.CryptoKeyVersion_CryptoKeyVersionState) {
	t.Helper()
	if _, err := k.UpdateCryptoKeyVersion(context.Background(), &kmspb.UpdateCryptoKeyVersionRequest{
		CryptoKeyVersion: &kmspb.CryptoKeyVersion{Name: key + "/cryptoKeyVersions/1", State: st},
		UpdateMask:       &fieldmaskpb.FieldMask{Paths: []string{"state"}}}); err != nil {
		t.Fatal(err)
	}
}

// TestSealOpen: a value sealed with a key holds no plaintext, records the
// version, opens while it is enabled and is refused while it is disabled.
func TestSealOpen(t *testing.T) {
	ctx := context.Background()
	srv := kms.NewServer(store.NewMemory())
	key := newKey(t, srv, kmspb.CryptoKey_ENCRYPT_DECRYPT)
	k := Keys{KMS: func() KMS { return InProcess(srv) }}
	agent := ServiceAgent("my-proj", "gcp-sa-logging.iam.gserviceaccount.com")
	if err := k.Usable(ctx, "my-proj", agent, key); err != nil {
		t.Fatal(err)
	}
	plain := []byte("a secret log line")
	s, err := k.Seal(ctx, "my-proj", agent, key, plain, []byte("name"))
	if err != nil {
		t.Fatal(err)
	}
	if s.KeyVersion != key+"/cryptoKeyVersions/1" || s.Key() != key || bytes.Contains(s.Ciphertext, plain) {
		t.Fatalf("sealed %+v", s)
	}
	got, err := k.Open(ctx, "my-proj", agent, s, []byte("name"))
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("open: %q %v", got, err)
	}
	if _, err := k.Open(ctx, "my-proj", agent, s, []byte("other")); err == nil {
		t.Fatal("opened with other additional data")
	}
	setState(t, srv, key, kmspb.CryptoKeyVersion_DISABLED)
	_, err = k.Open(ctx, "my-proj", agent, s, []byte("name"))
	if e := As(err); e == nil || e.Kind != Unusable || e.Key != s.KeyVersion {
		t.Fatalf("open with the version disabled: %v", err)
	}
	if e := As(k.Usable(ctx, "my-proj", agent, key)); e == nil || e.Kind != Unusable {
		t.Fatalf("usable with the primary disabled: %v", e)
	}
}

func TestUsableRefusals(t *testing.T) {
	ctx := context.Background()
	srv := kms.NewServer(store.NewMemory())
	k := Keys{KMS: func() KMS { return InProcess(srv) }}
	if e := As(Keys{}.Usable(ctx, "my-proj", "a", "projects/my-proj/locations/l/keyRings/r/cryptoKeys/k")); e == nil || e.Kind != NoKMS {
		t.Errorf("no KMS: %v", e)
	}
	if e := As(k.Usable(ctx, "my-proj", "a", "projects/my-proj/locations/us-central1/keyRings/r/cryptoKeys/none")); e == nil || e.Kind != NotFound {
		t.Errorf("missing key: %v", e)
	}
	sign := newKey(t, srv, kmspb.CryptoKey_ASYMMETRIC_SIGN)
	if e := As(k.Usable(ctx, "my-proj", "a", sign)); e == nil || e.Kind != Unusable {
		t.Errorf("signing key: %v", e)
	}
	if e := As(CheckName("kms_key_name", "nope", "")); e == nil || e.Kind != Invalid {
		t.Errorf("bad name: %v", e)
	}
	if e := As(CheckName("kms_key_name", "projects/my-proj/locations/eu/keyRings/r/cryptoKeys/k", "us")); e == nil || e.Kind != Invalid {
		t.Errorf("wrong location: %v", e)
	}
}

// TestAgentChecked: with a Checker, the agent must hold the permission,
// and the key's own policy is what it is checked with.
func TestAgentChecked(t *testing.T) {
	ctx := context.Background()
	srv := kms.NewServer(store.NewMemory())
	key := newKey(t, srv, kmspb.CryptoKey_ENCRYPT_DECRYPT)
	agent := ServiceAgent("my-proj", "gcp-sa-speech.iam.gserviceaccount.com")
	check := func(_ context.Context, r iamcheck.Request) (iamcheck.Decision, error) {
		for _, p := range r.Own {
			for _, b := range p.Policy.Bindings {
				for _, m := range b.Members {
					if b.Role == "roles/cloudkms.cryptoKeyEncrypterDecrypter" && m == "serviceAccount:"+r.Signer {
						return iamcheck.Decision{Held: r.Permissions}, nil
					}
				}
			}
		}
		return iamcheck.Decision{}, nil
	}
	k := Keys{KMS: func() KMS { return InProcess(srv) }, Check: check}
	if e := As(k.Usable(ctx, "my-proj", agent, key)); e == nil || e.Kind != PermissionDenied {
		t.Fatalf("without the role: %v", e)
	}
	if _, err := srv.IAMPolicy().SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: key, Policy: &iampb.Policy{
		Bindings: []*iampb.Binding{{Role: "roles/cloudkms.cryptoKeyEncrypterDecrypter", Members: []string{"serviceAccount:" + agent}}}}}); err != nil {
		t.Fatal(err)
	}
	if err := k.Usable(ctx, "my-proj", agent, key); err != nil {
		t.Fatalf("with the role: %v", err)
	}
}
