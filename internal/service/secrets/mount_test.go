package secrets

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/k8s"
)

// kubeCMEKStore is a Kubernetes-backed store with a fake Cloud KMS and a
// secret "a" under globalKey holding "hunter2".
func kubeCMEKStore(t *testing.T) (*Store, *fakeKMS, *fakeCluster) {
	t.Helper()
	fake, f := newFakeCluster(), newFakeKMS()
	s := NewStore(NewKubeStore(k8s.NewWith(fake, "", "", "cloudburrow"), "test"))
	s.SetKeyService(func() KeyService { return f })
	if _, err := createWith(NewGRPCServer(s), "a", automaticCMEK(globalKey)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddVersion("p", "a", []byte("hunter2")); err != nil {
		t.Fatal(err)
	}
	return s, f, fake
}

// mounted is the mount Secret's plaintext of version n, and whether it has one.
func mounted(t *testing.T, fake *fakeCluster, n int) (string, bool) {
	t.Helper()
	obj, ok := fake.objects[MountSecretName("p", "a")]
	if !ok {
		return "", false
	}
	data, _ := obj["data"].(map[string]any)
	enc, ok := data[VersionKey(n)].(string)
	if !ok {
		return "", false
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw), true
}

// A workload mounts the plaintext the instance decrypted, from a Secret of
// its own; the stored Secret keeps the ciphertext (#1489).
func TestResolveSecretRefMountsTheDecryptedPayload(t *testing.T) {
	t.Parallel()
	s, _, fake := kubeCMEKStore(t)

	name, key, err := s.ResolveSecretRef("p", "a", "latest")
	if err != nil {
		t.Fatalf("ResolveSecretRef: %v", err)
	}
	if name != MountSecretName("p", "a") || key != VersionKey(1) {
		t.Errorf("resolved to %s/%s", name, key)
	}
	if got, ok := mounted(t, fake, 1); !ok || got != "hunter2" {
		t.Errorf("mounted = %q, %v", got, ok)
	}
	stored := fake.version(t, "p", "a", 1)["data"].(map[string]any)[VersionKey(1)].(string)
	raw, _ := base64.StdEncoding.DecodeString(stored)
	if strings.Contains(string(raw), "hunter2") {
		t.Error("the stored Secret holds the plaintext")
	}
	// The mount Secret is never read as a secret.
	secrets, err := s.ListSecrets("p")
	if err != nil || len(secrets) != 1 {
		t.Errorf("ListSecrets = %d, %v", len(secrets), err)
	}
}

// A key that cannot decrypt refuses the deployment, naming the key; and a
// mounted version loses its plaintext while it cannot be read, and gets it
// back when it can.
func TestMountFollowsTheKeyAndTheVersion(t *testing.T) {
	t.Parallel()
	s, f, fake := kubeCMEKStore(t)

	f.setDisabled(globalKey, true)
	_, _, err := s.ResolveSecretRef("p", "a", "1")
	wantCode(t, err, codes.FailedPrecondition)
	if err != nil && !strings.Contains(err.Error(), globalKey) {
		t.Errorf("error does not name the key: %v", err)
	}
	f.setDisabled(globalKey, false)
	if _, _, err := s.ResolveSecretRef("p", "a", "1"); err != nil {
		t.Fatal(err)
	}

	f.setDisabled(globalKey, true)
	if err := s.RefreshMounts(); err != nil {
		t.Fatal(err)
	}
	if _, ok := mounted(t, fake, 1); ok {
		t.Error("plaintext kept while the key is disabled")
	}
	f.setDisabled(globalKey, false)
	if err := s.RefreshMounts(); err != nil {
		t.Fatal(err)
	}
	if got, ok := mounted(t, fake, 1); !ok || got != "hunter2" {
		t.Errorf("after re-enabling the key, mounted = %q, %v", got, ok)
	}

	// Disabling the version removes it at once; enabling restores it.
	if _, err := s.SetVersionState("p", "a", "1", StateDisabled); err != nil {
		t.Fatal(err)
	}
	if _, ok := mounted(t, fake, 1); ok {
		t.Error("plaintext kept for a disabled version")
	}
	if _, err := s.SetVersionState("p", "a", "1", StateEnabled); err != nil {
		t.Fatal(err)
	}
	if err := s.RefreshMounts(); err != nil {
		t.Fatal(err)
	}
	if _, ok := mounted(t, fake, 1); !ok {
		t.Error("plaintext not restored for a re-enabled version")
	}

	// Destroying removes it for good.
	if _, err := s.DestroyVersion("p", "a", "1"); err != nil {
		t.Fatal(err)
	}
	if err := s.RefreshMounts(); err != nil {
		t.Fatal(err)
	}
	if _, ok := mounted(t, fake, 1); ok {
		t.Error("plaintext kept for a destroyed version")
	}

	// The mount Secret goes with the secret.
	if _, err := NewGRPCServer(s).DeleteSecret(context.Background(), &secretmanagerpb.DeleteSecretRequest{Name: "projects/p/secrets/a"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.objects[MountSecretName("p", "a")]; ok {
		t.Error("the mount Secret outlived its secret")
	}
}

// A version without a customer-managed key is still mounted from the Secret
// that stores it.
func TestResolveSecretRefWithoutAKeyNeedsNoMount(t *testing.T) {
	t.Parallel()
	s, _, fake := kubeCMEKStore(t)
	seedKube(t, s, "p", "plain", "x")
	name, _, err := s.ResolveSecretRef("p", "plain", "1")
	if err != nil || name != VersionSecretName("p", "plain", 1) {
		t.Fatalf("resolved to %q, %v", name, err)
	}
	if _, ok := fake.objects[MountSecretName("p", "plain")]; ok {
		t.Error("a mount Secret for a secret without a key")
	}
}
