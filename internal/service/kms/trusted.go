package kms

import (
	"context"

	"cloud.google.com/go/kms/apiv1/kmspb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Trusted key wrapping in a single-tenant HSM (#1521), as resources.proto,
// service.proto and hsm_management.proto (kms v1.35.0) define it:
//
//   - An AES_WRAPPING key (algorithm AES_256_KWP, "AES key wrap with zero
//     padding algorithm (RFC 5649)") lives in a SingleTenantHsmInstance. Its
//     versions start with hsm_trusted false; an upgrade_key_trust proposal
//     ("Promotes a key with the AES_WRAPPING purpose to a trusted wrapping
//     key"), approved by the instance's quorum and executed, sets it.
//   - trusted_wrapping_enabled ("the key may be wrapped by a trusted key")
//     is set when a version is created or imported, for HSM_SINGLE_TENANT
//     keys of every purpose but ENCRYPT_DECRYPT, and is immutable.
//   - ExportTrustedKeyWrappedCryptoKeyVersion returns such a version's
//     material wrapped with an hsm_trusted AES_256_KWP version, and
//     ImportTrustedKeyWrappedCryptoKeyVersion takes material "pre-wrapped on
//     premises" with one into a new version, which has
//     trusted_wrapping_enabled set.
//
// The wrapping is RFC 5649 AES-KWP under the wrapping version's 32-byte key,
// as AES_256_KWP and the single-tenant guide's aes-256-kwp name. What is
// wrapped is the key material as Cloud KMS's ordinary key import encodes it
// (importing-a-key): the raw bytes of a symmetric (AES, HMAC) key, and the
// PKCS #8 DER private key of an asymmetric one. Google does not state the
// encoding under trusted wrapping, so the asymmetric encoding is UNVERIFIED.
// ML-KEM keys, whose ordinary import is not served either, are refused 501.
//
// CloudBurrow's choices where Google documents none, all UNVERIFIED: these
// operations, upgrade_key_trust included, need the instance's
// key_portability_enabled ("Key portability features are disabled by
// default"), else FAILED_PRECONDITION; the 2FA key upgrade_key_trust names
// must be a quorum member, and signs a required challenge beside the
// quorum's M; an importing_key naming a CryptoKey uses its newest ENABLED
// hsm_trusted version; trusted_wrapping_enabled on CreateCryptoKey with
// skip_initial_version_creation is 501, as which later version gets it is not
// documented; re-import into an existing version (crypto_key_version) is 501,
// as for ImportCryptoKeyVersion.

// trustedWrappingAllowed refuses trusted_wrapping_enabled on a version of k
// where the proto does not support it. The code is UNVERIFIED.
func trustedWrappingAllowed(field string, k cryptoKey) error {
	if k.protectionLevel() != kmspb.ProtectionLevel_HSM_SINGLE_TENANT {
		return apierror.InvalidArgument("%s is only supported for HSM_SINGLE_TENANT keys; CryptoKey %s is %s", field, k.Name, k.protectionLevel())
	}
	if k.purpose() == kmspb.CryptoKey_ENCRYPT_DECRYPT {
		return apierror.InvalidArgument("%s is supported for every purpose but ENCRYPT_DECRYPT; CryptoKey %s is ENCRYPT_DECRYPT", field, k.Name)
	}
	return nil
}

// portableInstance checks that the SingleTenantHsmInstance named holds keys
// for key portability: it is ACTIVE and key_portability_enabled.
func (s *Server) portableInstance(what, name string) error {
	var i hsmInstance
	found, err := s.get(dbKey(hsmInstancePrefix, name), &i)
	if err != nil {
		return apierror.Wrap(err)
	}
	if !found {
		return apierror.Wrap(apierror.FailedPrecondition("%s is in the SingleTenantHsmInstance %s, which does not exist", what, name))
	}
	i = i.effective(s.now())
	if st := i.state(); st != kmspb.SingleTenantHsmInstance_ACTIVE {
		return apierror.Wrap(apierror.FailedPrecondition("%s is in the SingleTenantHsmInstance %s, which is %s: its keys can be used only while it is ACTIVE", what, name, st))
	}
	if !i.Portability {
		return apierror.Wrap(apierror.FailedPrecondition("%s is in the SingleTenantHsmInstance %s, whose key_portability_enabled is false: trusted key wrapping is a key portability feature, set when the instance is created", what, name))
	}
	return nil
}

// wrappingVersion loads an AES_WRAPPING version for upgrade_key_trust or a
// trusted wrap: it must be an AES_256_KWP version of an HSM_SINGLE_TENANT
// key, ENABLED, and hsm_trusted when trusted is set.
func (s *Server) wrappingVersion(field, name string, trusted bool) (keyVersion, cryptoKey, error) {
	keyName, _, err := parseCryptoKeyVersion(field, name)
	if err != nil {
		return keyVersion{}, cryptoKey{}, apierror.Wrap(err)
	}
	var k cryptoKey
	if err := s.load(dbKey(keyPrefix, keyName), &k, "CryptoKey", keyName); err != nil {
		return keyVersion{}, cryptoKey{}, err
	}
	var v keyVersion
	if err := s.load(dbKey(versionPrefix, name), &v, "CryptoKeyVersion", name); err != nil {
		return keyVersion{}, cryptoKey{}, err
	}
	v = v.effective(s.now())
	if k.purpose() != kmspb.CryptoKey_AES_WRAPPING || v.algorithm() != kmspb.CryptoKeyVersion_AES_256_KWP || k.protectionLevel() != kmspb.ProtectionLevel_HSM_SINGLE_TENANT {
		return keyVersion{}, cryptoKey{}, apierror.Wrap(apierror.InvalidArgument("%s %s must be an AES_256_KWP version of an HSM_SINGLE_TENANT AES_WRAPPING key; its key is %s at %s", field, name, k.purpose(), k.protectionLevel()))
	}
	if st := v.state(); st != kmspb.CryptoKeyVersion_ENABLED {
		return keyVersion{}, cryptoKey{}, apierror.Wrap(apierror.FailedPrecondition("%s %s is %s: it must be ENABLED", field, name, st))
	}
	if trusted && !v.HsmTrusted {
		return keyVersion{}, cryptoKey{}, apierror.Wrap(apierror.FailedPrecondition("%s %s is not hsm_trusted: promote it with an upgrade_key_trust proposal on its SingleTenantHsmInstance", field, name))
	}
	return v, k, nil
}

// importingVersion resolves importing_key, "the name of a CryptoKeyVersion or
// a CryptoKey": a key's newest ENABLED hsm_trusted version.
func (s *Server) importingVersion(name string) (keyVersion, cryptoKey, error) {
	if parseCryptoKey("importing_key", name) != nil {
		v, k, err := s.wrappingVersion("importing_key", name, true)
		if err != nil {
			return keyVersion{}, cryptoKey{}, err
		}
		return v, k, s.portableInstance("importing_key "+name, k.Backend)
	}
	var k cryptoKey
	if err := s.load(dbKey(keyPrefix, name), &k, "CryptoKey", name); err != nil {
		return keyVersion{}, cryptoKey{}, err
	}
	for n := k.Next - 1; n >= 1; n-- {
		var v keyVersion
		found, err := s.get(dbKey(versionPrefix, versionName(name, n)), &v)
		if err != nil {
			return keyVersion{}, cryptoKey{}, apierror.Wrap(err)
		}
		if found && v.HsmTrusted && v.effective(s.now()).state() == kmspb.CryptoKeyVersion_ENABLED {
			return s.importingVersion(v.Name)
		}
	}
	return keyVersion{}, cryptoKey{}, apierror.Wrap(apierror.FailedPrecondition("importing_key %s has no ENABLED hsm_trusted version", name))
}

// trustedMaterial is what a trusted wrap carries for v: its material as key
// import encodes it (raw bytes, or PKCS #8 DER for an asymmetric key, which
// is how it is stored).
func trustedMaterial(field string, a algo, material []byte) ([]byte, error) {
	if algorithms[a].kem > 0 {
		return nil, apierror.Unimplemented("%s: trusted key wrapping of %s keys is not implemented: Google documents no encoding of ML-KEM key material for import or trusted wrapping", field, a)
	}
	return material, nil
}

func (s *Server) ExportTrustedKeyWrappedCryptoKeyVersion(_ context.Context, req *kmspb.ExportTrustedKeyWrappedCryptoKeyVersionRequest) (*kmspb.ExportTrustedKeyWrappedCryptoKeyVersionResponse, error) {
	keyName, _, err := parseCryptoKeyVersion("name", req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if req.GetWrappingKey() == "" {
		return nil, apierror.Wrap(apierror.InvalidArgument("wrapping_key is required"))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var k cryptoKey
	if err := s.load(dbKey(keyPrefix, keyName), &k, "CryptoKey", keyName); err != nil {
		return nil, err
	}
	var v keyVersion
	if err := s.load(dbKey(versionPrefix, req.GetName()), &v, "CryptoKeyVersion", req.GetName()); err != nil {
		return nil, err
	}
	v = v.effective(s.now())
	if !v.TrustedWrapping {
		return nil, apierror.Wrap(apierror.FailedPrecondition("CryptoKeyVersion %s does not have trusted_wrapping_enabled: only such a version can be exported", v.Name))
	}
	if st := v.state(); st != kmspb.CryptoKeyVersion_ENABLED {
		return nil, apierror.Wrap(apierror.FailedPrecondition("CryptoKeyVersion %s is %s: only an ENABLED version can be exported", v.Name, st))
	}
	if err := s.portableInstance("CryptoKey "+k.Name, k.Backend); err != nil {
		return nil, err
	}
	w, wk, err := s.wrappingVersion("wrapping_key", req.GetWrappingKey(), true)
	if err != nil {
		return nil, err
	}
	if err := s.portableInstance("wrapping_key "+w.Name, wk.Backend); err != nil {
		return nil, err
	}
	mat, err := trustedMaterial("name", v.algorithm(), v.Material)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	wrapped, err := wrapKWP(w.Material, mat)
	if err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "wrap %s", v.Name))
	}
	return &kmspb.ExportTrustedKeyWrappedCryptoKeyVersionResponse{WrappedKey: wrapped, WrappedKeyCrc32C: crc(wrapped)}, nil
}

func (s *Server) ImportTrustedKeyWrappedCryptoKeyVersion(_ context.Context, req *kmspb.ImportTrustedKeyWrappedCryptoKeyVersionRequest) (*kmspb.CryptoKeyVersion, error) {
	if err := parseCryptoKey("parent", req.GetParent()); err != nil {
		return nil, apierror.Wrap(err)
	}
	if req.GetImportingKey() == "" {
		return nil, apierror.Wrap(apierror.InvalidArgument("importing_key is required"))
	}
	if req.GetCryptoKeyVersion() != "" {
		return nil, apierror.Wrap(apierror.Unimplemented("crypto_key_version is not implemented: re-import into an existing version is not supported"))
	}
	if len(req.GetWrappedKey()) == 0 {
		return nil, apierror.Wrap(apierror.InvalidArgument("wrapped_key is required"))
	}
	a := req.GetAlgorithm()
	if a == kmspb.CryptoKeyVersion_CRYPTO_KEY_VERSION_ALGORITHM_UNSPECIFIED {
		return nil, apierror.Wrap(apierror.InvalidArgument("algorithm is required"))
	}
	ap, ok := algorithmPurpose(a)
	if !ok || a == kmspb.CryptoKeyVersion_EXTERNAL_SYMMETRIC_ENCRYPTION {
		return nil, apierror.Wrap(apierror.Unimplemented("algorithm %s is not implemented for import", a))
	}
	if _, err := trustedMaterial("algorithm", a, nil); err != nil {
		return nil, apierror.Wrap(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var k cryptoKey
	if err := s.load(dbKey(keyPrefix, req.GetParent()), &k, "CryptoKey", req.GetParent()); err != nil {
		return nil, err
	}
	if ap != k.purpose() {
		return nil, apierror.Wrap(apierror.InvalidArgument("algorithm %s is not valid for CryptoKey %s, whose purpose is %s", a, k.Name, k.purpose()))
	}
	// The new version "will have trusted_wrapping_enabled set to true".
	if err := trustedWrappingAllowed("ImportTrustedKeyWrappedCryptoKeyVersion (whose version has trusted_wrapping_enabled)", k); err != nil {
		return nil, apierror.Wrap(err)
	}
	if err := s.portableInstance("CryptoKey "+k.Name, k.Backend); err != nil {
		return nil, err
	}
	w, _, err := s.importingVersion(req.GetImportingKey())
	if err != nil {
		return nil, err
	}
	raw, err := unwrapKWP(w.Material, req.GetWrappedKey())
	if err != nil {
		return nil, apierror.Wrap(apierror.InvalidArgument("wrapped_key could not be unwrapped with %s: it must be the key wrapped with AES-KWP (RFC 5649) under that AES_256_KWP version", w.Name))
	}
	mat, err := importedMaterial(a, raw)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	n := k.Next
	v := keyVersion{Name: versionName(k.Name, n), Created: now, Material: mat, Algorithm: storedAlgorithm(a),
		Imported: now, ProtectionLevel: k.ProtectionLevel, TrustedWrapping: true}
	if err := s.put(dbKey(versionPrefix, v.Name), v); err != nil {
		return nil, apierror.Wrap(err)
	}
	k.Next = n + 1
	if err := s.put(dbKey(keyPrefix, k.Name), k); err != nil {
		return nil, apierror.Wrap(err)
	}
	return s.toVersion(v), nil
}

// checkUpgrade is whether upgrade_key_trust can run on instance i for
// proposal p: p.Version must be an ENABLED AES_256_KWP version of an
// AES_WRAPPING key in i, not yet hsm_trusted, and i must allow key
// portability.
func (s *Server) checkUpgrade(i hsmInstance, p hsmProposal) *apierror.Error {
	v, k, err := s.wrappingVersion("upgrade_key_trust.name", p.Version, false)
	if err != nil {
		return apierror.From(err)
	}
	if k.Backend != i.Name {
		return apierror.InvalidArgument("upgrade_key_trust.name %s is a version of %s, whose crypto_key_backend is %s, not this instance %s", v.Name, k.Name, k.Backend, i.Name)
	}
	if v.HsmTrusted {
		return apierror.FailedPrecondition("upgrade_key_trust.name %s is already hsm_trusted", v.Name)
	}
	if !i.Portability {
		return apierror.FailedPrecondition("SingleTenantHsmInstance %s has key_portability_enabled false: upgrade_key_trust is a key portability feature, set when the instance is created", i.Name)
	}
	member := false
	for _, pem := range i.Pems {
		member = member || samePem(pem, p.MemberPem)
	}
	if !member {
		return apierror.InvalidArgument("upgrade_key_trust.two_factor_public_key_pem is not a quorum member of %s", i.Name)
	}
	return nil
}

// upgradeTrust executes upgrade_key_trust: the version becomes hsm_trusted.
func (s *Server) upgradeTrust(name string) error {
	var v keyVersion
	if err := s.load(dbKey(versionPrefix, name), &v, "CryptoKeyVersion", name); err != nil {
		return err
	}
	v.HsmTrusted = true
	return s.put(dbKey(versionPrefix, name), v)
}
