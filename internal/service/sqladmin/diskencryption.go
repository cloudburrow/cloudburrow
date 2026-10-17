package sqladmin

// An instance's customer-managed encryption key (#1538):
// diskEncryptionConfiguration.kmsKeyName on instances.insert is a Cloud
// KMS CryptoKey of this instance's Cloud KMS, in the instance's region, as
// Cloud SQL requires ("The Cloud KMS key ring location must match the
// region where you want to create a Cloud SQL instance",
// docs.cloud.google.com/sql/docs/postgres/cmek). The instance gets a data
// key of its own, 256-bit AES, wrapped (Encrypt) by the CryptoKey and kept
// only wrapped; diskEncryptionStatus.kmsKeyVersionName is the version that
// wrapped it. The key is checked, by unwrapping the data key, before the
// instance's server is started (created, restarted, patched, brought back
// by `up`) and whenever the instance is read: while the version is
// disabled or destroyed the start is refused, and the instance answers
// SUSPENDED with suspensionReason KMS_KEY_ISSUE ("If Cloud SQL is unable
// to access the key (such as if you disable the key version), Cloud SQL
// suspends the instance"), and the connectors cannot reach it; it is
// RUNNABLE again once the version is enabled. The server's data volume is
// not encrypted with the key (docs/cloudsql.md).

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"time"
)

// KeyService is Cloud KMS: Encrypt with a CryptoKey's primary version,
// answering the version, and Decrypt.
type KeyService interface {
	Encrypt(ctx context.Context, key string, plaintext []byte) (ciphertext []byte, version string, err error)
	Decrypt(ctx context.Context, key string, ciphertext []byte) ([]byte, error)
}

// UseKMS makes diskEncryptionConfiguration work, over k: without it, an
// instance with a key is refused, as Cloud KMS is not enabled.
func (s *Server) UseKMS(k KeyService) {
	s.emu.Lock()
	s.kms = k
	s.emu.Unlock()
}

var cryptoKeyRE = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/keyRings/([^/]+)/cryptoKeys/([^/]+)$`)

// diskEncryption is a body's diskEncryptionConfiguration.
type diskEncryption struct {
	KMSKeyName       string `json:"kmsKeyName"`
	ConfidentialMode bool   `json:"confidentialMode"`
}

func parseDiskEncryption(raw *json.RawMessage) (string, error) {
	if raw == nil {
		return "", nil
	}
	var de diskEncryption
	if err := json.Unmarshal(*raw, &de); err != nil {
		return "", Errorf(http.StatusBadRequest, "Invalid JSON payload received: diskEncryptionConfiguration: %v", err)
	}
	if de.ConfidentialMode {
		return "", unimplemented("diskEncryptionConfiguration.confidentialMode",
			"Confidential Mode is for the Hyperdisk Balanced volumes of C4A machines, which the server here does not run on")
	}
	return de.KMSKeyName, nil
}

// encryptWith gives spec the key named, wrapping a new data key with it:
// the key must be in the instance's region and usable in this instance's
// Cloud KMS.
func (s *Server) encryptWith(ctx context.Context, spec *Spec, key string) error {
	m := cryptoKeyRE.FindStringSubmatch(key)
	if m == nil {
		return Errorf(http.StatusBadRequest, "Invalid request: diskEncryptionConfiguration.kmsKeyName must be "+
			"projects/{project}/locations/{location}/keyRings/{key_ring}/cryptoKeys/{crypto_key}, not %q.", key)
	}
	if m[2] != spec.region() {
		return Errorf(http.StatusBadRequest, "Invalid request: diskEncryptionConfiguration.kmsKeyName %s is in %s; the key must be in "+
			"the instance's region, %s (a multi-region or global key does not work).", key, m[2], spec.region())
	}
	s.emu.RLock()
	k := s.kms
	s.emu.RUnlock()
	if k == nil {
		return Errorf(http.StatusPreconditionFailed, "Invalid request: diskEncryptionConfiguration needs Cloud KMS: "+
			"run `cloudburrow up` with kms in --services.")
	}
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return err
	}
	wrapped, version, err := k.Encrypt(ctx, key, dek)
	if err != nil {
		return keyError(key, err)
	}
	spec.KMSKey, spec.KMSKeyVersion, spec.WrappedKey = key, version, wrapped
	return nil
}

func keyError(key string, err error) error {
	return Errorf(http.StatusBadRequest, "The customer-managed encryption key %s cannot be used: %v", key, err)
}

// keyUsable is nil when spec has no key or its key unwraps the data key,
// and otherwise says why the instance is suspended.
func (s *Server) keyUsable(spec Spec) error {
	if spec.KMSKey == "" {
		return nil
	}
	s.emu.RLock()
	k := s.kms
	s.emu.RUnlock()
	return s.keyUsableWith(k, spec)
}

// keyUsableWith is keyUsable over k, for a caller that holds emu.
func (s *Server) keyUsableWith(k KeyService, spec Spec) error {
	if spec.KMSKey == "" {
		return nil
	}
	if k == nil {
		return Errorf(http.StatusPreconditionFailed, "The instance %s is encrypted with %s, and Cloud KMS is not enabled: "+
			"run `cloudburrow up` with kms in --services.", spec.Name, spec.KMSKey)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := k.Decrypt(ctx, spec.KMSKey, spec.WrappedKey); err != nil {
		return keyError(spec.KMSKey, fmt.Errorf("instance %s: %w", spec.Name, err))
	}
	return nil
}

// replicaKey is a replica's key: its primary's when it is in the primary's
// region, where a different one is refused, and one of its own region's,
// which must be given, in another.
func (s *Server) replicaKey(ctx context.Context, spec *Spec, key string) error {
	s.emu.RLock()
	var primary Spec
	if c := s.writes.byName[spec.MasterInstanceName]; c != nil {
		primary = c.spec
	}
	s.emu.RUnlock()
	if primary.KMSKey == "" {
		if key == "" {
			return nil
		}
		return s.encryptWith(ctx, spec, key)
	}
	if spec.region() == primary.region() {
		if key != "" && key != primary.KMSKey {
			return Errorf(http.StatusBadRequest, "Invalid request: a read replica in its primary's region inherits the primary's key, %s; "+
				"a different diskEncryptionConfiguration.kmsKeyName cannot be assigned to it.", primary.KMSKey)
		}
		spec.KMSKey, spec.KMSKeyVersion, spec.WrappedKey = primary.KMSKey, primary.KMSKeyVersion, primary.WrappedKey
		return nil
	}
	if key == "" {
		return Errorf(http.StatusBadRequest, "Invalid request: the primary is encrypted with a customer-managed key; a read replica "+
			"in another region (%s) needs diskEncryptionConfiguration.kmsKeyName, a key of that region.", spec.region())
	}
	return s.encryptWith(ctx, spec, key)
}

// diskEncryptionJSON adds an instance's key to its answer, and suspends it
// while the key cannot be used.
func (s *Server) diskEncryptionJSON(in map[string]any, spec Spec) {
	if spec.KMSKey == "" {
		return
	}
	in["diskEncryptionConfiguration"] = map[string]any{"kind": "sql#diskEncryptionConfiguration", "kmsKeyName": spec.KMSKey}
	in["diskEncryptionStatus"] = map[string]any{"kind": "sql#diskEncryptionStatus", "kmsKeyVersionName": spec.KMSKeyVersion}
	if s.keyUsable(spec) != nil {
		in["state"] = "SUSPENDED"
		in["suspensionReason"] = []any{"KMS_KEY_ISSUE"}
	}
}

// keyBlocksConnections refuses a connector's call to an instance whose key
// cannot be used: a suspended instance is not reachable.
func (s *Server) keyBlocksConnections(name string) error {
	s.emu.RLock()
	var spec Spec
	if s.writes != nil {
		if c := s.writes.byName[name]; c != nil {
			spec = c.spec
		}
	}
	s.emu.RUnlock()
	if err := s.keyUsable(spec); err != nil {
		return Errorf(http.StatusPreconditionFailed, "The instance %s is SUSPENDED (KMS_KEY_ISSUE): %v", name, err)
	}
	return nil
}
