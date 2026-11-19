package run

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/registry"
)

// fakeRepoKMS wraps with a fixed XOR and fails while disabled, as Cloud KMS
// fails with a disabled key version.
type fakeRepoKMS struct {
	mu       sync.Mutex
	disabled bool
	calls    int
}

func (f *fakeRepoKMS) crypt(b []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.disabled {
		return nil, status.Error(codes.FailedPrecondition, "key version is not enabled")
	}
	out := make([]byte, len(b))
	for i := range b {
		out[i] = b[i] ^ 0x5a
	}
	return out, nil
}

func (f *fakeRepoKMS) Encrypt(_ context.Context, r *kmspb.EncryptRequest) (*kmspb.EncryptResponse, error) {
	b, err := f.crypt(r.GetPlaintext())
	return &kmspb.EncryptResponse{Name: r.GetName() + "/cryptoKeyVersions/1", Ciphertext: b}, err
}

func (f *fakeRepoKMS) Decrypt(_ context.Context, r *kmspb.DecryptRequest) (*kmspb.DecryptResponse, error) {
	b, err := f.crypt(r.GetCiphertext())
	return &kmspb.DecryptResponse{Plaintext: b}, err
}

func useFakeKMS(t *testing.T) *fakeRepoKMS {
	f := &fakeRepoKMS{}
	UseRepositoryKMS(func() RepositoryKMS { return f })
	t.Cleanup(func() { UseRepositoryKMS(nil) })
	return f
}

// A repository with kms_key_name keeps its content sealed in its own
// directory, never in the shared blobs; while the key cannot be used a
// pull, a push and the API's reads fail, and they work again once it can.
// A generic file is sealed the same way; a repository without a key is
// untouched. Without Cloud KMS a push to it fails.
func TestRepositoryCMEK(t *testing.T) {
	dir := t.TempDir()
	st, err := registry.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	UseRegistry(st)
	t.Cleanup(func() { UseRegistry(nil) })
	kms := useFakeKMS(t)
	a := NewServer(&Knative{}, "t", time.Minute).ArtifactRegistry()
	ctx := context.Background()
	parent := "projects/cmek-unit/locations/us-central1"
	key := parent + "/keyRings/r/cryptoKeys/k"
	for id, f := range map[string]arpb.Repository_Format{"enc": arpb.Repository_DOCKER, "plain": arpb.Repository_DOCKER, "gen": arpb.Repository_GENERIC} {
		r := &arpb.Repository{Format: f, KmsKeyName: key}
		if id == "plain" {
			r.KmsKeyName = ""
		}
		if _, err := a.CreateRepository(ctx, &arpb.CreateRepositoryRequest{Parent: parent, RepositoryId: id, Repository: r}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { RemoveRepository(parent + "/repositories/" + id) })
	}
	srv := httptest.NewServer(RegistryHandler())
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	img, _ := random.Image(4096, 2)
	layers, _ := img.Layers()
	raw, _ := layers[0].Compressed()
	var layer bytes.Buffer
	_, _ = layer.ReadFrom(raw)
	_ = raw.Close()
	if err := crane.Push(img, host+"/cmek-unit/enc/app:v1"); err != nil {
		t.Fatalf("push to the encrypted repository: %v", err)
	}
	ld, _ := layers[0].Digest()
	if _, err := os.Stat(filepath.Join(dir, "blobs", "sha256", strings.TrimPrefix(ld.String(), "sha256:"))); err == nil {
		t.Error("the encrypted repository's layer is in the shared plaintext store")
	}
	sealed, err := os.ReadFile(filepath.Join(dir, "repos", "us-central1", "cmek-unit", "enc", "_blobs", strings.TrimPrefix(ld.String(), "sha256:")))
	if err != nil || bytes.Contains(sealed, layer.Bytes()[:64]) {
		t.Fatalf("sealed layer: %v (contains the plaintext: %v)", err, err == nil)
	}
	if _, err := crane.Pull(host + "/cmek-unit/enc/app:v1"); err != nil {
		t.Errorf("pull with the key enabled: %v", err)
	}
	if _, err := addFile(parent+"/repositories/gen", "tool:1:f.txt", "tool", "1", []byte("generic secret content"), nil); err != nil {
		t.Fatal(err)
	}
	f, _ := fileByID(parent+"/repositories/gen", "tool:1:f.txt")
	if b, err := f.bytes(st); err != nil || string(b) != "generic secret content" {
		t.Errorf("generic file = %q, %v", b, err)
	}
	if _, err := st.ReadBlob(f.Digest); err == nil {
		t.Error("the encrypted generic file is in the shared plaintext store")
	}

	kms.mu.Lock()
	kms.disabled = true
	kms.mu.Unlock()
	if _, err := crane.Pull(host + "/cmek-unit/enc/app:v1"); err == nil || !strings.Contains(err.Error(), "DENIED") {
		t.Errorf("pull with the key disabled = %v; want DENIED", err)
	}
	img2, _ := random.Image(128, 1)
	if err := crane.Push(img2, host+"/cmek-unit/enc/app:v2"); err == nil {
		t.Error("push with the key disabled succeeded")
	}
	// Metadata stays readable, as Google documents.
	if vs, err := a.ListVersions(ctx, &arpb.ListVersionsRequest{Parent: parent + "/repositories/enc/packages/app"}); err != nil || len(vs.GetVersions()) != 1 {
		t.Errorf("ListVersions with the key disabled = %v, %v; want the version", vs, err)
	}
	if _, err := f.bytes(st); !errors.Is(err, registry.ErrKeyUnavailable) {
		t.Errorf("generic file with the key disabled = %v; want ErrKeyUnavailable", err)
	}
	if err := crane.Push(img2, host+"/cmek-unit/plain/app:v1"); err != nil {
		t.Errorf("push to a repository without a key: %v", err)
	}
	kms.mu.Lock()
	kms.disabled = false
	kms.mu.Unlock()
	if _, err := crane.Pull(host + "/cmek-unit/enc/app:v1"); err != nil {
		t.Errorf("pull with the key enabled again: %v", err)
	}

	UseRepositoryKMS(nil)
	if err := crane.Push(img2, host+"/cmek-unit/enc/app:v3"); err == nil {
		t.Error("push without Cloud KMS succeeded")
	}
}
