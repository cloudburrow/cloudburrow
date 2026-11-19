package privateca

import (
	"bytes"
	"context"
	"strings"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/security/privateca/apiv1/privatecapb"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// A pool's encryption_spec (CMEK): "When EncryptionSpec is provided, the
// Subject, SubjectAltNames, and the PEM-encoded certificate fields will be
// encrypted at rest." Each certificate of such a pool is kept in the state
// file encrypted with the pool's Cloud KMS key, never in the clear; "If the
// key is inaccessible (e.g., disabled, destroyed, or permissions revoked),
// certificate issuance and management operations requiring decryption will
// fail" (creating-ca-pool).

// sealedCert is a certificate as the state file keeps it in a CMEK pool:
// the Certificate's JSON, encrypted with Key.
type sealedCert struct {
	Key        string `json:"key"`
	Ciphertext []byte `json:"ciphertext"`
	// plain is the JSON it seals, so an unchanged certificate is not
	// encrypted again at every save.
	plain []byte
}

// checkEncryption validates a pool's encryption_spec: "you must provide the
// resource name of a symmetric Cloud KMS key that's located in the same
// Google Cloud region as your CA pool", and only the Enterprise tier
// supports customer-managed keys (tiers).
func (s *Server) checkEncryption(ctx context.Context, pool string, tier privatecapb.CaPool_Tier, spec *privatecapb.EncryptionSpec) error {
	key := spec.GetCloudKmsKey()
	if key == "" {
		return nil
	}
	m := kmsKeyRE.FindStringSubmatch(key)
	if m == nil {
		return apierror.InvalidArgument("encryption_spec.cloud_kms_key %q must be projects/*/locations/*/keyRings/*/cryptoKeys/*", key)
	}
	if loc := strings.Split(pool, "/")[3]; m[1] != loc {
		return apierror.InvalidArgument("encryption_spec.cloud_kms_key %s must be in the same location as the CA pool (%s)", key, loc)
	}
	if tier != privatecapb.CaPool_ENTERPRISE {
		return apierror.InvalidArgument("encryption_spec needs a CA pool in the ENTERPRISE tier: the DevOps tier does not support customer-managed encryption keys")
	}
	if s.kms == nil {
		return apierror.FailedPrecondition("Cloud KMS is not served by this instance: start it with the kms service to use encryption_spec")
	}
	k, err := s.kms.GetCryptoKey(ctx, &kmspb.GetCryptoKeyRequest{Name: key})
	if err != nil {
		return err
	}
	if k.GetPurpose() != kmspb.CryptoKey_ENCRYPT_DECRYPT {
		return apierror.InvalidArgument("encryption_spec.cloud_kms_key %s is a %s key: CMEK needs a symmetric ENCRYPT_DECRYPT key", key, k.GetPurpose())
	}
	return nil
}

// cmekKey is the Cloud KMS key a certificate's pool encrypts with, or "".
// The caller holds mu.
func (s *Server) cmekKey(certName string) string {
	pool, _, _ := strings.Cut(certName, "/certificates/")
	if p, ok := s.pools[pool]; ok {
		return p.Pool.GetEncryptionSpec().GetCloudKmsKey()
	}
	return ""
}

// seal encrypts a certificate for the state file, reusing the previous
// ciphertext when nothing changed. The caller holds mu.
func (s *Server) seal(name string, c *privatecapb.Certificate, key string) (*sealedCert, error) {
	plain, err := protojson.Marshal(c)
	if err != nil {
		return nil, err
	}
	if prev := s.sealedCache[name]; prev != nil && prev.Key == key && bytes.Equal(prev.plain, plain) {
		return prev, nil
	}
	if s.kms == nil {
		return nil, apierror.FailedPrecondition("Cloud KMS is not served by this instance, so the certificate cannot be encrypted with %s", key)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	r, err := s.kms.Encrypt(ctx, &kmspb.EncryptRequest{Name: key, Plaintext: plain, AdditionalAuthenticatedData: []byte(name)})
	if err != nil {
		return nil, err
	}
	out := &sealedCert{Key: key, Ciphertext: r.GetCiphertext(), plain: plain}
	s.sealedCache[name] = out
	return out, nil
}

// unseal decrypts the certificates loaded encrypted, once Cloud KMS can;
// those it cannot stay sealed. The caller holds mu.
func (s *Server) unseal() {
	if len(s.sealed) == 0 || s.kms == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for n, sc := range s.sealed {
		r, err := s.kms.Decrypt(ctx, &kmspb.DecryptRequest{Name: sc.Key, Ciphertext: sc.Ciphertext, AdditionalAuthenticatedData: []byte(n)})
		if err != nil {
			continue
		}
		c := &privatecapb.Certificate{}
		if protojson.Unmarshal(r.GetPlaintext(), c) != nil {
			continue
		}
		s.certs[n] = c
		sc.plain = r.GetPlaintext()
		s.sealedCache[n] = sc
		delete(s.sealed, n)
	}
}

// sealedErr is the error of a certificate whose pool's key cannot decrypt
// it.
func sealedErr(name, key string) error {
	return apierror.FailedPrecondition("Certificate %s cannot be decrypted: its CA pool's Cloud KMS key %s is not available (disabled, destroyed or not served)", name, key)
}
