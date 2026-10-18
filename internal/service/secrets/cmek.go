package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Customer-managed encryption keys (#1453), as
// docs.cloud.google.com/secret-manager/docs/cmek documents them:
//
//   - automatic replication takes one key, which "must be located in the
//     global Cloud KMS multi-region"; a user-managed replica's key "must be
//     in the same Google Cloud location as the secret version replica it
//     encrypts";
//   - a new version is encrypted with each key's primary version, which its
//     replication_status reports as kms_key_version_name; "an attempt to
//     create a new secret version or access an existing one returns an
//     error" when a key cannot be used, and a destroyed key leaves the data
//     unrecoverable;
//   - the keys can be changed or removed (an UpdateSecret whose mask names
//     replication, `gcloud secrets replication update --set-kms-key` or
//     `--remove-cmek`), but nothing else about the policy can; "existing
//     secret versions are not re-encrypted".
//
// The encryption is envelope encryption through the instance's own Cloud KMS
// Encrypt and Decrypt and nothing else: each version's payload is sealed
// with a fresh 256-bit data key (AES-256-GCM, the version's name as
// additional data), and the data key is encrypted by Cloud KMS under every
// replica's key. Only the sealed payload and the wrapped data keys are
// stored.
//
// Choices Google does not document, pinned by unit tests (UNVERIFIED):
//
//   - a version is readable only while every key it was encrypted with can
//     decrypt; Google routes an access to one replica, and this fails the
//     access when any replica's key cannot be used, so a disabled key is
//     never hidden by another replica;
//   - the error is FAILED_PRECONDITION naming the key and carrying Cloud
//     KMS's own message, for adding and for accessing a version;
//   - the key is not looked up at CreateSecret or UpdateSecret, only its
//     name and location are checked (INVALID_ARGUMENT); a missing key fails
//     the first AddSecretVersion;
//   - the Secret Manager service agent's roles/cloudkms.cryptoKeyEncrypterDecrypter
//     grant is not required: IAM is recorded, never enforced (ADR-0006).
// Docs re-read 2026-10-17, still unanswered: https://docs.cloud.google.com/secret-manager/docs/cmek

// KeyService is the part of Cloud KMS that CMEK uses: Encrypt and Decrypt,
// as the instance's Cloud KMS serves them. *kms.Server is one.
type KeyService interface {
	Encrypt(context.Context, *kmspb.EncryptRequest) (*kmspb.EncryptResponse, error)
	Decrypt(context.Context, *kmspb.DecryptRequest) (*kmspb.DecryptResponse, error)
}

// SetKeyService sets where CMEK goes: a function, since Cloud KMS may start
// after Secret Manager. With none, or one returning nil, a secret with a
// customer-managed key is refused UNIMPLEMENTED, since nothing could
// encrypt its versions.
func (s *Store) SetKeyService(k func() KeyService) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = k
}

// keyService is the configured Cloud KMS, or nil. The caller holds s.mu.
func (s *Store) keyService() KeyService {
	if s.keys == nil {
		return nil
	}
	return s.keys()
}

// errNoKMS refuses CMEK on an instance without Cloud KMS.
var errNoKMS = apierror.Unimplemented("customer_managed_encryption needs Cloud KMS, which is not enabled on this " +
	"CloudBurrow instance; start it with --services including kms")

// ReplicaStatus is one replica's part of a version's replication_status:
// its location ("" under automatic replication) and, when it was encrypted
// with a customer-managed key, the key, the key version Cloud KMS used and
// the data key Cloud KMS encrypted.
type ReplicaStatus struct {
	Location   string `json:"location,omitempty"`
	KMSKey     string `json:"kmsKey,omitempty"`
	KeyVersion string `json:"kmsKeyVersion,omitempty"`
	WrappedKey []byte `json:"wrappedKey,omitempty"`
}

// cmekKeyLocation returns a CryptoKey name's location, or an error naming
// field when it is not projects/*/locations/*/keyRings/*/cryptoKeys/*.
func cmekKeyLocation(field, name string) (string, error) {
	p := strings.Split(name, "/")
	if len(p) != 8 || p[0] != "projects" || p[2] != "locations" || p[4] != "keyRings" || p[6] != "cryptoKeys" ||
		p[1] == "" || p[3] == "" || p[5] == "" || p[7] == "" {
		return "", apierror.InvalidArgument("%s %q must be a Cloud KMS CryptoKey, "+
			"projects/{project}/locations/{location}/keyRings/{key_ring}/cryptoKeys/{crypto_key}", field, name)
	}
	return p[3], nil
}

// checkKMSKeys validates a policy's keys: keys[i] is replica i's ("" for
// none); an automatic policy has one entry. The caller holds s.mu.
func (s *Store) checkKMSKeys(replication string, locations, keys []string) error {
	any := false
	for i, k := range keys {
		if k == "" {
			continue
		}
		any = true
		if replication == "automatic" {
			loc, err := cmekKeyLocation("replication.automatic.customer_managed_encryption.kms_key_name", k)
			if err != nil {
				return err
			}
			if loc != "global" {
				return apierror.InvalidArgument("replication.automatic.customer_managed_encryption.kms_key_name %s "+
					"is in %s; a secret with automatic replication needs a key in the global location", k, loc)
			}
			continue
		}
		loc, err := cmekKeyLocation("replication.user_managed.replicas[].customer_managed_encryption.kms_key_name", k)
		if err != nil {
			return err
		}
		if loc != locations[i] {
			return apierror.InvalidArgument("the key %s of the replica in %s is in %s; "+
				"a replica's key must be in the replica's location", k, locations[i], loc)
		}
	}
	if any && s.keyService() == nil {
		return errNoKMS
	}
	return nil
}

// hasKeys reports whether any entry of keys names a key.
func hasKeys(keys []string) bool {
	for _, k := range keys {
		if k != "" {
			return true
		}
	}
	return false
}

// normalKeys is keys as a Secret stores them: nil when no replica has a key.
func normalKeys(keys []string) []string {
	if !hasKeys(keys) {
		return nil
	}
	return append([]string(nil), keys...)
}

const cmekTimeout = 30 * time.Second

// kmsError is a Cloud KMS failure as Secret Manager reports it.
func kmsError(op, key string, err error) error {
	msg := err.Error()
	if st, ok := status.FromError(err); ok {
		msg = st.Message()
	}
	return apierror.FailedPrecondition("the Cloud KMS key %s cannot %s: %s", key, op, msg)
}

// sealVersion encrypts payload for sec's keys and fills v's Payload and
// Replicas. The caller holds s.mu.
func (s *Store) sealVersion(sec Secret, v *Version, payload []byte) error {
	locations := sec.ReplicaLocations
	if sec.Replication != "user-managed" {
		locations = []string{""}
	}
	v.Replicas = make([]ReplicaStatus, len(locations))
	for i, l := range locations {
		v.Replicas[i].Location = l
	}
	if !hasKeys(sec.KMSKeys) {
		v.Payload = append([]byte(nil), payload...)
		return nil
	}
	kms := s.keyService()
	if kms == nil {
		return errNoKMS
	}
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return apierror.Internal(err, "generate a data key")
	}
	sealed, err := sealPayload(dek, payload, []byte(v.Name))
	if err != nil {
		return apierror.Internal(err, "encrypt the payload")
	}
	ctx, cancel := context.WithTimeout(context.Background(), cmekTimeout)
	defer cancel()
	for i := range v.Replicas {
		if i >= len(sec.KMSKeys) || sec.KMSKeys[i] == "" {
			// A replica with no key holds the data key as it is: its data
			// is Google-managed, so the version stays readable through it
			// only while every key-encrypted replica is too.
			v.Replicas[i].WrappedKey = append([]byte(nil), dek...)
			continue
		}
		key := sec.KMSKeys[i]
		resp, err := kms.Encrypt(ctx, &kmspb.EncryptRequest{Name: key, Plaintext: dek, AdditionalAuthenticatedData: []byte(v.Name)})
		if err != nil {
			return kmsError("encrypt", key, err)
		}
		v.Replicas[i].KMSKey, v.Replicas[i].KeyVersion, v.Replicas[i].WrappedKey = key, resp.GetName(), resp.GetCiphertext()
	}
	v.Payload, v.PayloadSize = sealed, len(payload)
	return nil
}

// encrypted reports whether v's payload is sealed under customer-managed keys.
func (v Version) encrypted() bool {
	for _, r := range v.Replicas {
		if r.KMSKey != "" {
			return true
		}
	}
	return false
}

// Size is the payload's length in bytes.
func (v Version) Size() int {
	if v.encrypted() {
		return v.PayloadSize
	}
	return len(v.Payload)
}

// openVersion returns v with its plaintext payload, decrypting the data key
// with every replica's key. The caller holds s.mu.
func (s *Store) openVersion(v Version) (Version, error) {
	if !v.encrypted() {
		return v, nil
	}
	kms := s.keyService()
	if kms == nil {
		return Version{}, errNoKMS
	}
	ctx, cancel := context.WithTimeout(context.Background(), cmekTimeout)
	defer cancel()
	var dek []byte
	for _, r := range v.Replicas {
		if r.KMSKey == "" {
			continue
		}
		resp, err := kms.Decrypt(ctx, &kmspb.DecryptRequest{Name: r.KMSKey, Ciphertext: r.WrappedKey, AdditionalAuthenticatedData: []byte(v.Name)})
		if err != nil {
			return Version{}, kmsError("decrypt", r.KMSKey, err)
		}
		dek = resp.GetPlaintext()
	}
	pt, err := openPayload(dek, v.Payload, []byte(v.Name))
	if err != nil {
		return Version{}, apierror.Internal(err, "decrypt secret version %s", v.Name)
	}
	v.Payload = pt
	return v, nil
}

func sealPayload(dek, plaintext, aad []byte) ([]byte, error) {
	aead, err := payloadAEAD(dek)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, plaintext, aad), nil
}

func openPayload(dek, sealed, aad []byte) ([]byte, error) {
	aead, err := payloadAEAD(dek)
	if err != nil {
		return nil, err
	}
	if len(sealed) < aead.NonceSize() {
		return nil, errors.New("sealed payload is too short")
	}
	return aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], aad)
}

func payloadAEAD(dek []byte) (cipher.AEAD, error) {
	if len(dek) != 32 {
		return nil, fmt.Errorf("data key is %d bytes, want 32", len(dek))
	}
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// SetReplicationKeys changes a secret's customer-managed keys (keys[i] is
// replica i's, "" for none), the one part of a replication policy that can
// change. Existing versions keep the keys they were encrypted with.
func (s *Store) SetReplicationKeys(project, id string, keys []string) (Secret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sec, err := s.getSecret(project, id)
	if err != nil {
		return Secret{}, err
	}
	want := 1
	if sec.Replication == "user-managed" {
		want = len(sec.ReplicaLocations)
	}
	if len(keys) != want {
		return Secret{}, apierror.InvalidArgument("the replication policy has %d replicas; %d keys were given", want, len(keys))
	}
	if err := s.checkKMSKeys(sec.Replication, sec.ReplicaLocations, keys); err != nil {
		return Secret{}, err
	}
	sec.KMSKeys = normalKeys(keys)
	sec.Etag = s.etag()
	if err := s.put(secretKey(project, id), sec); err != nil {
		return Secret{}, err
	}
	s.notify(sec, EventSecretUpdate, nil, "")
	return sec, nil
}
