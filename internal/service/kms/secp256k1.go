package kms

import (
	"encoding/asn1"
	"errors"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	k1ecdsa "github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// EC_SIGN_SECP256K1_SHA256 (#1719): "ECDSA on the Secp256k1 Curve with a
// SHA-256 digest" (cloud.google.com/kms/docs/algorithms). The algorithms
// page lists it under the HSM and HSM_SINGLE_TENANT protection levels only,
// not SOFTWARE, EXTERNAL or EXTERNAL_VPC, so a key of it at another level is
// refused (levelAlgorithm). The page also says "secp256k1 curves generate
// signatures in the "normalized" form only (also known as the "lower-S
// form")", with BIP-0062's DER encoding: AsymmetricSign returns an ASN.1
// DER ECDSA-Sig-Value whose s is at most n/2. The digest is SHA-256, or any
// 32 bytes passed as digest.sha256 (create-validate-signatures: "provide a
// Keccak hash value and use the SHA digest algorithm with the same length").
//
// Go's standard library has no secp256k1, so the curve arithmetic is
// github.com/decred/dcrd/dcrec/secp256k1/v4 (approved by the user, #1719),
// whose ecdsa.Sign is RFC 6979 deterministic and always lower-S. The key is
// stored, imported and trusted-wrapped as PKCS #8 DER (RFC 5208 with an RFC
// 5915 ECPrivateKey), and its public key is a SubjectPublicKeyInfo with
// id-ecPublicKey (1.2.840.10045.2.1) and the namedCurve secp256k1
// (1.3.132.0.10, SEC 2 and RFC 5480), the key in uncompressed form.

var (
	oidECPublicKey = asn1.ObjectIdentifier{1, 2, 840, 10045, 2, 1}
	oidSecp256k1   = asn1.ObjectIdentifier{1, 3, 132, 0, 10}
)

// pkcs8 is RFC 5208's PrivateKeyInfo.
type pkcs8 struct {
	Version    int
	Algo       algorithmIdentifier
	PrivateKey []byte
}

type algorithmIdentifier struct {
	Algorithm  asn1.ObjectIdentifier
	Parameters asn1.RawValue `asn1:"optional"`
}

// ecPrivateKey is RFC 5915's ECPrivateKey.
type ecPrivateKey struct {
	Version       int
	PrivateKey    []byte
	NamedCurveOID asn1.ObjectIdentifier `asn1:"optional,explicit,tag:0"`
	PublicKey     asn1.BitString        `asn1:"optional,explicit,tag:1"`
}

// subjectPublicKeyInfo is RFC 5280's.
type subjectPublicKeyInfo struct {
	Algorithm algorithmIdentifier
	PublicKey asn1.BitString
}

func secp256k1Params() asn1.RawValue {
	b, _ := asn1.Marshal(oidSecp256k1)
	return asn1.RawValue{FullBytes: b}
}

// generateSecp256k1 makes a new secp256k1 key as PKCS #8 DER.
func generateSecp256k1() ([]byte, error) {
	k, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		return nil, err
	}
	return marshalSecp256k1(k)
}

func marshalSecp256k1(k *secp256k1.PrivateKey) ([]byte, error) {
	pub := k.PubKey().SerializeUncompressed()
	inner, err := asn1.Marshal(ecPrivateKey{Version: 1, PrivateKey: k.Serialize(), NamedCurveOID: oidSecp256k1,
		PublicKey: asn1.BitString{Bytes: pub, BitLength: 8 * len(pub)}})
	if err != nil {
		return nil, err
	}
	return asn1.Marshal(pkcs8{Algo: algorithmIdentifier{Algorithm: oidECPublicKey, Parameters: secp256k1Params()}, PrivateKey: inner})
}

// parseSecp256k1 reads a PKCS #8 DER secp256k1 private key: the curve is
// named in the AlgorithmIdentifier's parameters, the ECPrivateKey's, or
// both (OpenSSL writes both), and must be secp256k1 wherever it is named.
func parseSecp256k1(der []byte) (*secp256k1.PrivateKey, error) {
	var p pkcs8
	if rest, err := asn1.Unmarshal(der, &p); err != nil || len(rest) > 0 {
		return nil, errors.New("not PKCS #8 DER")
	}
	if !p.Algo.Algorithm.Equal(oidECPublicKey) {
		return nil, errors.New("not an elliptic curve key")
	}
	named := false
	if len(p.Algo.Parameters.FullBytes) > 0 {
		var oid asn1.ObjectIdentifier
		if _, err := asn1.Unmarshal(p.Algo.Parameters.FullBytes, &oid); err != nil || !oid.Equal(oidSecp256k1) {
			return nil, errors.New("the curve is not secp256k1")
		}
		named = true
	}
	var ec ecPrivateKey
	if rest, err := asn1.Unmarshal(p.PrivateKey, &ec); err != nil || len(rest) > 0 {
		return nil, errors.New("not an RFC 5915 ECPrivateKey")
	}
	if len(ec.NamedCurveOID) > 0 {
		if !ec.NamedCurveOID.Equal(oidSecp256k1) {
			return nil, errors.New("the curve is not secp256k1")
		}
		named = true
	}
	if !named {
		return nil, errors.New("the curve is not named")
	}
	if ec.Version != 1 || len(ec.PrivateKey) != 32 {
		return nil, errors.New("the private key must be 32 bytes")
	}
	var s secp256k1.ModNScalar
	if overflow := s.SetByteSlice(ec.PrivateKey); overflow || s.IsZero() {
		return nil, errors.New("the private key is out of range")
	}
	k := secp256k1.NewPrivateKey(&s)
	if len(ec.PublicKey.Bytes) > 0 {
		pub, err := secp256k1.ParsePubKey(ec.PublicKey.Bytes)
		if err != nil || !pub.IsEqual(k.PubKey()) {
			return nil, errors.New("the public key does not match the private key")
		}
	}
	return k, nil
}

// secp256k1PublicDER is the SubjectPublicKeyInfo GetPublicKey returns.
func secp256k1PublicDER(pub *secp256k1.PublicKey) ([]byte, error) {
	b := pub.SerializeUncompressed()
	return asn1.Marshal(subjectPublicKeyInfo{
		Algorithm: algorithmIdentifier{Algorithm: oidECPublicKey, Parameters: secp256k1Params()},
		PublicKey: asn1.BitString{Bytes: b, BitLength: 8 * len(b)},
	})
}

// parseSecp256k1Public reads a SubjectPublicKeyInfo DER that names
// secp256k1, as a client reads GetPublicKey's PEM.
func parseSecp256k1Public(der []byte) (*secp256k1.PublicKey, error) {
	var spki subjectPublicKeyInfo
	if rest, err := asn1.Unmarshal(der, &spki); err != nil || len(rest) > 0 {
		return nil, errors.New("not a SubjectPublicKeyInfo")
	}
	var oid asn1.ObjectIdentifier
	if !spki.Algorithm.Algorithm.Equal(oidECPublicKey) {
		return nil, errors.New("not an elliptic curve key")
	}
	if _, err := asn1.Unmarshal(spki.Algorithm.Parameters.FullBytes, &oid); err != nil || !oid.Equal(oidSecp256k1) {
		return nil, errors.New("the curve is not secp256k1")
	}
	return secp256k1.ParsePubKey(spki.PublicKey.RightAlign())
}

// signSecp256k1 signs a 32-byte digest, lower-S, DER-encoded.
func signSecp256k1(k *secp256k1.PrivateKey, digest []byte) []byte {
	return k1ecdsa.Sign(k, digest).Serialize()
}

// verifySecp256k1 checks a DER signature of digest. ParseDERSignature
// refuses a signature that is not lower-S, as Google never returns one.
func verifySecp256k1(pub *secp256k1.PublicKey, digest, sig []byte) bool {
	s, err := k1ecdsa.ParseDERSignature(sig)
	return err == nil && s.Verify(digest, pub)
}

// secp256k1Level refuses a protection level the algorithms page does not
// list EC_SIGN_SECP256K1_SHA256 under. The code is UNVERIFIED.
func secp256k1Level(level kmspb.ProtectionLevel) error {
	if level == kmspb.ProtectionLevel_HSM || level == kmspb.ProtectionLevel_HSM_SINGLE_TENANT {
		return nil
	}
	return apierror.InvalidArgument("crypto_key.version_template.algorithm %s is not available at protection level %s: "+
		"Google offers it at the HSM and HSM_SINGLE_TENANT protection levels only", kmspb.CryptoKeyVersion_EC_SIGN_SECP256K1_SHA256, level)
}

// secp256k1Key loads the version's stored key.
func (v keyVersion) secp256k1Key() (*secp256k1.PrivateKey, error) {
	k, err := parseSecp256k1(v.Material)
	if err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "stored key of %s", v.Name))
	}
	return k, nil
}
