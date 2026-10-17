package backupdr

// A backup vault's customer-managed encryption key (#1501): a vault
// created with encryptionConfig.kmsKeyName has a data key of its own,
// 256-bit AES, wrapped (Encrypt) by that Cloud KMS CryptoKey of this
// instance's Cloud KMS and kept only wrapped, as Backup and DR documents
// CMEK for a vault. The key is checked, by unwrapping the data key, before
// each backup into the vault and each restore from it: while the key (its
// primary version, or the version that wrapped the data key) is disabled
// or destroyed, both fail with FAILED_PRECONDITION, as a vault whose key
// is unusable cannot be written or read. The backups themselves are Cloud
// SQL backup runs, dumps the Cloud SQL Admin API keeps; they are not
// encrypted with the key (docs/backupdr.md).

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// KeyService is Cloud KMS: Encrypt and Decrypt with a CryptoKey.
type KeyService interface {
	Encrypt(ctx context.Context, key string, plaintext []byte) ([]byte, error)
	Decrypt(ctx context.Context, key string, ciphertext []byte) ([]byte, error)
}

// UseKMS makes encryptionConfig work, over k: without it, a vault with a
// key is refused, as Cloud KMS is not enabled.
func (s *Server) UseKMS(k KeyService) {
	s.mu.Lock()
	s.kms = k
	s.mu.Unlock()
}

var cryptoKeyRE = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/keyRings/([^/]+)/cryptoKeys/([^/]+)$`)

// vaultKey reads a create's encryptionConfig and wraps a new data key with
// its CryptoKey: "" when the vault has none. loc is the vault's location.
func (s *Server) vaultKey(ctx context.Context, loc string, raw json.RawMessage) (string, []byte, error) {
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "{}" {
		return "", nil, nil
	}
	var ec struct {
		KMSKeyName string `json:"kmsKeyName"`
	}
	if err := json.Unmarshal(raw, &ec); err != nil {
		return "", nil, invalid("backupVault.encryptionConfig: %v", err)
	}
	if ec.KMSKeyName == "" {
		return "", nil, nil
	}
	m := cryptoKeyRE.FindStringSubmatch(ec.KMSKeyName)
	if m == nil {
		return "", nil, invalid("backupVault.encryptionConfig.kmsKeyName must be projects/{project}/locations/{location}/keyRings/{key_ring}/cryptoKeys/{crypto_key}, not %q.", ec.KMSKeyName)
	}
	if region := loc[strings.LastIndex(loc, "/")+1:]; m[2] != region {
		return "", nil, invalid("backupVault.encryptionConfig.kmsKeyName %s must be in the vault's location %s.", ec.KMSKeyName, region)
	}
	s.mu.Lock()
	k := s.kms
	s.mu.Unlock()
	if k == nil {
		return "", nil, precondition("backupVault.encryptionConfig needs Cloud KMS: run up with kms in --services.")
	}
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return "", nil, err
	}
	wrapped, err := k.Encrypt(ctx, ec.KMSKeyName, dek)
	if err != nil {
		return "", nil, keyError(ec.KMSKeyName, err)
	}
	return ec.KMSKeyName, wrapped, nil
}

func keyError(key string, err error) error {
	return precondition("The customer-managed encryption key %s cannot be used: %s", key, err.Error())
}

// checkVaultKeyFor checks the key of the vault name is under (a data
// source or a backup), if it has one: an error when it cannot be used.
func (s *Server) checkVaultKeyFor(name string) error {
	i := strings.Index(name, "/dataSources/")
	if i < 0 {
		return nil
	}
	s.mu.Lock()
	v, k := s.st.Vaults[name[:i]], s.kms
	s.mu.Unlock()
	if v == nil || v.KMSKey == "" {
		return nil
	}
	if k == nil {
		return precondition("Backup vault %s is encrypted with %s, and Cloud KMS is not enabled: run up with kms in --services.", v.Name, v.KMSKey)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := k.Decrypt(ctx, v.KMSKey, v.WrappedKey); err != nil {
		return keyError(v.KMSKey, fmt.Errorf("backup vault %s: %w", v.Name, err))
	}
	return nil
}
