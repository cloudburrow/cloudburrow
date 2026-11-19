package run

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"github.com/cloudburrow/cloudburrow/internal/registry"
)

// Customer-managed encryption keys (#1897), as
// https://docs.cloud.google.com/artifact-registry/docs/cmek documents them:
// a repository created with kms_key_name has its content encrypted with
// that Cloud KMS key, and while the key version is disabled or destroyed
// the repository's artifacts can be neither read nor written.
//
// This is envelope encryption through the instance's own Cloud KMS: a
// repository's 256-bit data key, made at its first write, is encrypted by
// Cloud KMS Encrypt under kms_key_name and only that wrapped key is kept
// (cmek.json beside the repositories). Every read and write of the
// repository's content first unwraps it with Cloud KMS Decrypt, so it
// fails as soon as the key version that wrapped it is disabled or
// destroyed, and works again once it is enabled or restored. Blobs and
// manifests are sealed with AES-256-GCM in the repository's own directory
// (internal/registry/encrypt.go); package formats' files too.
//
// Choices Google does not document, pinned by unit tests (UNVERIFIED, docs
// re-read 2026-11-19: https://docs.cloud.google.com/artifact-registry/docs/cmek):
//   - the key is not looked up at CreateRepository, only its name and
//     location are checked, as Secret Manager's here; a missing or unusable
//     key fails the first push;
//   - the Docker registry answers 403 DENIED, and the API
//     FAILED_PRECONDITION, naming the key and Cloud KMS's message.

// RepositoryKMS is the part of Cloud KMS a repository's key uses.
type RepositoryKMS interface {
	Encrypt(context.Context, *kmspb.EncryptRequest) (*kmspb.EncryptResponse, error)
	Decrypt(context.Context, *kmspb.DecryptRequest) (*kmspb.DecryptResponse, error)
}

// cmekFile is where the wrapped data keys are saved.
const cmekFile = "artifactregistry-cmek.json"

// cmekEntry is one repository's key and wrapped data key.
type cmekEntry struct {
	Key        string `json:"key"`
	KeyVersion string `json:"keyVersion,omitempty"`
	Wrapped    []byte `json:"wrapped,omitempty"`
}

var cmekState = struct {
	mu   sync.Mutex
	kms  func() RepositoryKMS
	keys map[string]*cmekEntry
}{keys: map[string]*cmekEntry{}}

// UseRepositoryKMS sets the Cloud KMS repositories' keys use: a function,
// as Cloud KMS may start after Cloud Run. With none, a repository with a
// key can be neither written nor read.
func UseRepositoryKMS(f func() RepositoryKMS) {
	cmekState.mu.Lock()
	cmekState.kms = f
	cmekState.mu.Unlock()
}

// errNoRepositoryKMS is a repository with a key on an instance without
// Cloud KMS.
var errNoRepositoryKMS = errors.New("a repository with a customer-managed key needs Cloud KMS, which is not enabled on this " +
	"CloudBurrow instance; start it with --services including kms")

// setRepositoryKey records a new repository's key; "" removes it.
func setRepositoryKey(repoName, key string) {
	cmekState.mu.Lock()
	defer cmekState.mu.Unlock()
	if key == "" {
		if _, ok := cmekState.keys[repoName]; !ok {
			return
		}
		delete(cmekState.keys, repoName)
	} else {
		cmekState.keys[repoName] = &cmekEntry{Key: key}
	}
	saveCMEKLocked()
}

func saveCMEKLocked() {
	st := RegistryStore()
	if st == nil {
		return
	}
	b, err := json.Marshal(cmekState.keys)
	if err != nil {
		return
	}
	_ = st.WriteState(cmekFile, b)
}

// loadCMEK reads the keys a previous run saved; a repository whose key
// was saved before #1897 has no entry yet and gets one here.
func loadCMEK(st *registry.Store, repos map[string]string) {
	m := map[string]*cmekEntry{}
	if b, err := st.ReadState(cmekFile); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	cmekState.mu.Lock()
	defer cmekState.mu.Unlock()
	cmekState.keys = map[string]*cmekEntry{}
	for name, key := range repos {
		if e := m[name]; e != nil && e.Key == key {
			cmekState.keys[name] = e
		} else if key != "" {
			cmekState.keys[name] = &cmekEntry{Key: key}
		}
	}
}

// repositoryCipher is the store's Encryption: a registry.Repo's cipher,
// nil for a repository without a key.
func repositoryCipher(r registry.Repo) (registry.Cipher, error) {
	name := "projects/" + r.Project + "/locations/" + r.Location + "/repositories/" + r.Repository
	cmekState.mu.Lock()
	defer cmekState.mu.Unlock()
	e := cmekState.keys[name]
	if e == nil {
		return nil, nil
	}
	var api RepositoryKMS
	if cmekState.kms != nil {
		api = cmekState.kms()
	}
	if api == nil {
		return nil, errNoRepositoryKMS
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var dek []byte
	if e.Wrapped == nil {
		dek = make([]byte, 32)
		if _, err := rand.Read(dek); err != nil {
			return nil, err
		}
		resp, err := api.Encrypt(ctx, &kmspb.EncryptRequest{Name: e.Key, Plaintext: dek, AdditionalAuthenticatedData: []byte(name)})
		if err != nil {
			return nil, fmt.Errorf("Cloud KMS key %s: %w", e.Key, err)
		}
		e.Wrapped, e.KeyVersion = resp.GetCiphertext(), resp.GetName()
		saveCMEKLocked()
	} else {
		resp, err := api.Decrypt(ctx, &kmspb.DecryptRequest{Name: e.Key, Ciphertext: e.Wrapped, AdditionalAuthenticatedData: []byte(name)})
		if err != nil {
			return nil, fmt.Errorf("Cloud KMS key %s: %w", e.Key, err)
		}
		dek = resp.GetPlaintext()
	}
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcmCipher{gcm}, nil
}

// gcmCipher seals with AES-256-GCM, the nonce first.
type gcmCipher struct{ aead cipher.AEAD }

func (c gcmCipher) Seal(plain []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return c.aead.Seal(nonce, nonce, plain, nil), nil
}

func (c gcmCipher) Open(sealed []byte) ([]byte, error) {
	n := c.aead.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("sealed content is too short")
	}
	return c.aead.Open(nil, sealed[:n], sealed[n:], nil)
}
