package kms

import (
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"strings"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// EXTERNAL and EXTERNAL_VPC keys (#1469).
//
// On Google an external key's material lives in an external key manager
// (EKM): an EXTERNAL version names it by a key URI the EKM serves over the
// internet, an EXTERNAL_VPC version by a key path on the EkmConnection its
// key's crypto_key_backend names (cloud.google.com/kms/docs/ekm). Google
// publishes no specification or reference server of the protocol Cloud KMS
// speaks to an EKM, so no real EKM can be run here. CloudBurrow instead acts
// as the EKM for every key URI and every connection: what a version names is
// stored and returned as given, and the key material behind it is generated
// and kept by the instance, as for a SOFTWARE key, and never dialled for.
// That is what lets Encrypt, Decrypt, AsymmetricSign and GetPublicKey work
// on external keys locally; nothing leaves the instance.
//
// What follows the docs:
//
//   - The algorithms are the ones the algorithms page lists for both levels:
//     EXTERNAL_SYMMETRIC_ENCRYPTION (AES-256-GCM, the default for
//     ENCRYPT_DECRYPT here) and the EC_SIGN_P256/P384 and RSA_SIGN_PSS/PKCS1
//     signing algorithms ("Limited to symmetric encryption and asymmetric
//     signing"). EXTERNAL_SYMMETRIC_ENCRYPTION is refused at the other levels.
//   - An EXTERNAL key, and an EXTERNAL_VPC key on a MANUAL connection, is
//     created with skip_initial_version_creation (create-external-key), and
//     each version names its key: external_key_uri for EXTERNAL,
//     ekm_connection_key_path for EXTERNAL_VPC ("Set this field instead of
//     external_key_uri when using an EkmConnection"). Asking for an initial
//     version is INVALID_ARGUMENT, since CreateCryptoKey cannot carry the
//     reference. "The version is not created if the key URI is invalid": a
//     URI that is not an absolute https URL with a host is FAILED_PRECONDITION
//     naming EXTERNAL_KEY_URI_INVALID, the type the EKM error reference gives
//     an invalid key URI (an unavailable external key is FAILED_PRECONDITION,
//     the Cloud EKM overview).
//   - An EXTERNAL_VPC key's crypto_key_backend names an EkmConnection in the
//     key's location; when it is unset the location's default connection is
//     used ("new Cloud EKM by VPC keys created in key rings in that location
//     use the indicated EKM connection", create-ekm-connection), and with no
//     default it is INVALID_ARGUMENT. A version's
//     ekm_connection_backend_override names another connection in the
//     location instead.
//   - On a CLOUD_KMS-mode connection (coordinated external keys) the EKM
//     creates the key and returns its path, so versions are created without
//     options, the initial one included. The local EKM answers at once, so a
//     version is ENABLED when created, never PENDING_GENERATION, and its
//     ekm_connection_key_path is "<crypto_space_path>/<random hex>", a path
//     of the instance's own choosing.
//   - external_protection_level_options can be updated on a version
//     (update-external-key-path: "Update an external key path without
//     rotating the key"); its key material stays the same.
//   - Automatic rotation: the rotate-key page documents it for symmetric
//     coordinated external keys only and says nothing of the other external
//     keys, whose new versions need a new URI or path that only their owner
//     knows, so a rotation schedule on those is UNIMPLEMENTED.
//
// Chosen here, and UNVERIFIED: the codes of every refusal; that two versions
// naming the same URI or path get different key material (each version's
// key is generated when it is created, and kept when its reference is
// updated); and the coordinated key path's format.

// externalAlgorithms are the algorithms the algorithms page lists for the
// EXTERNAL and EXTERNAL_VPC protection levels.
var externalAlgorithms = map[algo]bool{
	kmspb.CryptoKeyVersion_EXTERNAL_SYMMETRIC_ENCRYPTION: true,
	kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256:           true,
	kmspb.CryptoKeyVersion_EC_SIGN_P384_SHA384:           true,
	kmspb.CryptoKeyVersion_RSA_SIGN_PSS_2048_SHA256:      true,
	kmspb.CryptoKeyVersion_RSA_SIGN_PSS_3072_SHA256:      true,
	kmspb.CryptoKeyVersion_RSA_SIGN_PSS_4096_SHA256:      true,
	kmspb.CryptoKeyVersion_RSA_SIGN_PSS_4096_SHA512:      true,
	kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_2048_SHA256:    true,
	kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_3072_SHA256:    true,
	kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_4096_SHA256:    true,
	kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_4096_SHA512:    true,
}

func isExternal(pl kmspb.ProtectionLevel) bool {
	return pl == kmspb.ProtectionLevel_EXTERNAL || pl == kmspb.ProtectionLevel_EXTERNAL_VPC
}

// levelAlgorithm resolves a key's algorithm at its protection level: the
// external levels take the algorithms page's list for them, the others
// everything but EXTERNAL_SYMMETRIC_ENCRYPTION.
func levelAlgorithm(level kmspb.ProtectionLevel, p kmspb.CryptoKey_CryptoKeyPurpose, a algo) (algo, error) {
	// AES_WRAPPING keys are trusted wrapping keys, which Google documents in
	// a single-tenant HSM only (trusted.go, #1521).
	if p == kmspb.CryptoKey_AES_WRAPPING && level != kmspb.ProtectionLevel_HSM_SINGLE_TENANT {
		return 0, apierror.Unimplemented("crypto_key.purpose AES_WRAPPING is not implemented at protection level %s: Google documents AES_WRAPPING keys as trusted wrapping keys in a single-tenant HSM (HSM_SINGLE_TENANT) only", level)
	}
	if !isExternal(level) {
		if a == kmspb.CryptoKeyVersion_EXTERNAL_SYMMETRIC_ENCRYPTION {
			return 0, apierror.InvalidArgument("crypto_key.version_template.algorithm %s is only for the EXTERNAL and EXTERNAL_VPC protection levels, not %s", a, level)
		}
		return keyAlgorithm(p, a)
	}
	if a == kmspb.CryptoKeyVersion_CRYPTO_KEY_VERSION_ALGORITHM_UNSPECIFIED && p == kmspb.CryptoKey_ENCRYPT_DECRYPT {
		a = kmspb.CryptoKeyVersion_EXTERNAL_SYMMETRIC_ENCRYPTION
	}
	if p != kmspb.CryptoKey_ENCRYPT_DECRYPT && p != kmspb.CryptoKey_ASYMMETRIC_SIGN {
		return 0, apierror.InvalidArgument("crypto_key.purpose %s is not available at protection level %s: external keys are limited to symmetric encryption and asymmetric signing", p, level)
	}
	got, err := keyAlgorithm(p, a)
	if err != nil {
		return 0, err
	}
	if !externalAlgorithms[got] {
		return 0, apierror.InvalidArgument("crypto_key.version_template.algorithm %s is not available at protection level %s", got, level)
	}
	return got, nil
}

// ExternalAlgorithms lists, sorted, the algorithms of purpose an EXTERNAL or
// EXTERNAL_VPC key can have, for the console's Create key form.
func ExternalAlgorithms(purpose kmspb.CryptoKey_CryptoKeyPurpose) []string {
	var out []string
	for _, a := range Algorithms(purpose) {
		if externalAlgorithms[algo(kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm_value[a])] {
			out = append(out, a)
		}
	}
	if purpose == kmspb.CryptoKey_ENCRYPT_DECRYPT {
		out = append([]string{kmspb.CryptoKeyVersion_EXTERNAL_SYMMETRIC_ENCRYPTION.String()}, out...)
	}
	return out
}

// checkKeyURI is "the version is not created if the key URI is invalid".
func checkKeyURI(uri string) error {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return apierror.FailedPrecondition("external_protection_level_options.external_key_uri %q is invalid (EXTERNAL_KEY_URI_INVALID): it must be an absolute https URL naming the key on the external key manager", uri)
	}
	return nil
}

// keyBackend resolves an EXTERNAL_VPC key's connection: the named one, or
// the location's default; it must exist in the key's location. It returns
// the connection's record.
func (s *Server) keyBackend(field, named, keyRing string) (ekmConnection, error) {
	loc := keyRing[:strings.Index(keyRing, "/keyRings/")]
	if named == "" {
		var cfg ekmConfig
		if _, err := s.get(dbKey(ekmConfigPrefix, loc+"/ekmConfig"), &cfg); err != nil {
			return ekmConnection{}, err
		}
		if cfg.Default == "" {
			return ekmConnection{}, apierror.InvalidArgument("%s is required for an EXTERNAL_VPC key: %s has no default EKM connection", field, loc)
		}
		named = cfg.Default
	}
	if err := parseEkmConnection(field, named); err != nil {
		return ekmConnection{}, err
	}
	if !strings.HasPrefix(named, loc+"/") {
		return ekmConnection{}, apierror.InvalidArgument("%s %s is not in the key's location %s", field, named, loc)
	}
	var c ekmConnection
	found, err := s.get(dbKey(ekmConnectionPrefix, named), &c)
	if err != nil {
		return ekmConnection{}, err
	}
	if !found {
		return ekmConnection{}, apierror.InvalidArgument("%s %s does not exist", field, named)
	}
	return c, nil
}

// coordinatedPath is the key path the local EKM gives a key it creates on a
// CLOUD_KMS-mode connection.
func coordinatedPath(c ekmConnection) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", apierror.Internal(err, "generate key path")
	}
	return strings.TrimSuffix(c.CryptoSpace, "/") + "/" + hex.EncodeToString(b), nil
}

// externalRef validates a version's external_protection_level_options for k
// and fills v's reference to its external key: a URI for EXTERNAL, a key
// path (given, or the local EKM's for a coordinated connection) for
// EXTERNAL_VPC. The other levels take no options.
func (s *Server) externalRef(k cryptoKey, opts *kmspb.ExternalProtectionLevelOptions, v *keyVersion) error {
	const field = "crypto_key_version.external_protection_level_options"
	uri, path, override := opts.GetExternalKeyUri(), opts.GetEkmConnectionKeyPath(), opts.GetEkmConnectionBackendOverride()
	switch k.protectionLevel() {
	case kmspb.ProtectionLevel_EXTERNAL:
		if path != "" || override != "" {
			return apierror.InvalidArgument("%s: an EXTERNAL version names its key by external_key_uri; ekm_connection_key_path and ekm_connection_backend_override are for EXTERNAL_VPC", field)
		}
		if uri == "" {
			return apierror.InvalidArgument("%s.external_key_uri is required for a version of the EXTERNAL key %s", field, k.Name)
		}
		if err := checkKeyURI(uri); err != nil {
			return err
		}
		v.ExternalKeyURI = uri
	case kmspb.ProtectionLevel_EXTERNAL_VPC:
		if uri != "" {
			return apierror.InvalidArgument("%s: an EXTERNAL_VPC version names its key by ekm_connection_key_path, not external_key_uri", field)
		}
		backend := k.Backend
		if override != "" {
			c, err := s.keyBackend(field+".ekm_connection_backend_override", override, k.Name[:strings.Index(k.Name, "/cryptoKeys/")])
			if err != nil {
				return err
			}
			backend = c.Name
			v.Backend = c.Name
		}
		var c ekmConnection
		if err := s.load(dbKey(ekmConnectionPrefix, backend), &c, "EkmConnection", backend); err != nil {
			return err
		}
		if c.mode() == kmspb.EkmConnection_CLOUD_KMS {
			if path != "" {
				return apierror.Unimplemented("%s.ekm_connection_key_path is not implemented on the coordinated EkmConnection %s: the EKM chooses a coordinated key's path, and Google does not document supplying one", field, c.Name)
			}
			p, err := coordinatedPath(c)
			if err != nil {
				return err
			}
			v.KeyPath = p
			return nil
		}
		if path == "" {
			return apierror.InvalidArgument("%s.ekm_connection_key_path is required for a version of %s, whose EkmConnection %s is MANUAL", field, k.Name, c.Name)
		}
		v.KeyPath = path
	default:
		if uri != "" || path != "" || override != "" {
			return apierror.InvalidArgument("%s is only for EXTERNAL and EXTERNAL_VPC keys, not %s", field, k.protectionLevel())
		}
	}
	return nil
}

// newVersion makes version n of k: its material, generated by the instance
// acting as the EKM for an external key, and the reference opts give it.
func (s *Server) newVersion(k cryptoKey, n int, now time.Time, state kmspb.CryptoKeyVersion_CryptoKeyVersionState, opts *kmspb.ExternalProtectionLevelOptions) (keyVersion, error) {
	v := keyVersion{Name: versionName(k.Name, n), Created: now.UTC(), Algorithm: k.Algorithm, ProtectionLevel: k.ProtectionLevel}
	if state != kmspb.CryptoKeyVersion_ENABLED {
		v.State = state.String()
	}
	if err := s.externalRef(k, opts, &v); err != nil {
		return keyVersion{}, err
	}
	mat, err := newMaterial(k)
	if err != nil {
		return keyVersion{}, err
	}
	v.Material = mat
	return v, nil
}

// manualExternal says whether k's versions each need a reference only the
// caller can give: EXTERNAL keys, and EXTERNAL_VPC keys on a MANUAL
// connection when the key was created.
func (k cryptoKey) manualExternal() bool {
	switch k.protectionLevel() {
	case kmspb.ProtectionLevel_EXTERNAL:
		return true
	case kmspb.ProtectionLevel_EXTERNAL_VPC:
		return !k.Coordinated
	}
	return false
}

// externalOptions renders a version's reference to its external key.
func (v keyVersion) externalOptions() *kmspb.ExternalProtectionLevelOptions {
	if v.ExternalKeyURI == "" && v.KeyPath == "" && v.Backend == "" {
		return nil
	}
	return &kmspb.ExternalProtectionLevelOptions{ExternalKeyUri: v.ExternalKeyURI, EkmConnectionKeyPath: v.KeyPath, EkmConnectionBackendOverride: v.Backend}
}
