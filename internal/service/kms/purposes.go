package kms

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/mlkem"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"sort"

	"cloud.google.com/go/kms/apiv1/kmspb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Key purposes and their algorithms (#1212). The table is resources.proto's
// CryptoKeyVersionAlgorithm, each value under the purpose its comment and the
// algorithms page (cloud.google.com/kms/docs/algorithms) assign it. Every
// algorithm here is computed with Go's standard crypto; key material is
// stored unencrypted, as for symmetric keys: CloudBurrow's KMS is not a
// security boundary (docs/compatibility.md, ADR-0004).

type algo = kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm

// algSpec describes one implemented algorithm.
type algSpec struct {
	purpose kmspb.CryptoKey_CryptoKeyPurpose
	// rsaBits or curve or keyLen sizes the material.
	rsaBits int
	curve   elliptic.Curve
	ed25519 bool
	keyLen  int
	// hash is the digest the algorithm signs, MACs or uses in OAEP; zero
	// for raw RSA PKCS#1, Ed25519 and AES.
	hash crypto.Hash
	pss  bool
	// mode is the AES mode of a RAW_ENCRYPT_DECRYPT key: gcm, cbc or ctr.
	mode string
	// kem is the ML-KEM parameter set (768 or 1024) of a KEY_ENCAPSULATION
	// key, whose material is the 64-byte FIPS 203 seed.
	kem int
}

var algorithms = map[algo]algSpec{
	kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION: {purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, keyLen: 32},
	// EXTERNAL_SYMMETRIC_ENCRYPTION is AES-256-GCM (the algorithms page),
	// computed by the instance acting as the EKM, and only at the external
	// protection levels (external.go, #1469).
	kmspb.CryptoKeyVersion_EXTERNAL_SYMMETRIC_ENCRYPTION: {purpose: kmspb.CryptoKey_ENCRYPT_DECRYPT, keyLen: 32},

	kmspb.CryptoKeyVersion_RSA_SIGN_PSS_2048_SHA256:   {purpose: kmspb.CryptoKey_ASYMMETRIC_SIGN, rsaBits: 2048, hash: crypto.SHA256, pss: true},
	kmspb.CryptoKeyVersion_RSA_SIGN_PSS_3072_SHA256:   {purpose: kmspb.CryptoKey_ASYMMETRIC_SIGN, rsaBits: 3072, hash: crypto.SHA256, pss: true},
	kmspb.CryptoKeyVersion_RSA_SIGN_PSS_4096_SHA256:   {purpose: kmspb.CryptoKey_ASYMMETRIC_SIGN, rsaBits: 4096, hash: crypto.SHA256, pss: true},
	kmspb.CryptoKeyVersion_RSA_SIGN_PSS_4096_SHA512:   {purpose: kmspb.CryptoKey_ASYMMETRIC_SIGN, rsaBits: 4096, hash: crypto.SHA512, pss: true},
	kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_2048_SHA256: {purpose: kmspb.CryptoKey_ASYMMETRIC_SIGN, rsaBits: 2048, hash: crypto.SHA256},
	kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_3072_SHA256: {purpose: kmspb.CryptoKey_ASYMMETRIC_SIGN, rsaBits: 3072, hash: crypto.SHA256},
	kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_4096_SHA256: {purpose: kmspb.CryptoKey_ASYMMETRIC_SIGN, rsaBits: 4096, hash: crypto.SHA256},
	kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_4096_SHA512: {purpose: kmspb.CryptoKey_ASYMMETRIC_SIGN, rsaBits: 4096, hash: crypto.SHA512},
	kmspb.CryptoKeyVersion_RSA_SIGN_RAW_PKCS1_2048:    {purpose: kmspb.CryptoKey_ASYMMETRIC_SIGN, rsaBits: 2048},
	kmspb.CryptoKeyVersion_RSA_SIGN_RAW_PKCS1_3072:    {purpose: kmspb.CryptoKey_ASYMMETRIC_SIGN, rsaBits: 3072},
	kmspb.CryptoKeyVersion_RSA_SIGN_RAW_PKCS1_4096:    {purpose: kmspb.CryptoKey_ASYMMETRIC_SIGN, rsaBits: 4096},
	kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256:        {purpose: kmspb.CryptoKey_ASYMMETRIC_SIGN, curve: elliptic.P256(), hash: crypto.SHA256},
	kmspb.CryptoKeyVersion_EC_SIGN_P384_SHA384:        {purpose: kmspb.CryptoKey_ASYMMETRIC_SIGN, curve: elliptic.P384(), hash: crypto.SHA384},
	kmspb.CryptoKeyVersion_EC_SIGN_ED25519:            {purpose: kmspb.CryptoKey_ASYMMETRIC_SIGN, ed25519: true},

	kmspb.CryptoKeyVersion_RSA_DECRYPT_OAEP_2048_SHA256: {purpose: kmspb.CryptoKey_ASYMMETRIC_DECRYPT, rsaBits: 2048, hash: crypto.SHA256},
	kmspb.CryptoKeyVersion_RSA_DECRYPT_OAEP_3072_SHA256: {purpose: kmspb.CryptoKey_ASYMMETRIC_DECRYPT, rsaBits: 3072, hash: crypto.SHA256},
	kmspb.CryptoKeyVersion_RSA_DECRYPT_OAEP_4096_SHA256: {purpose: kmspb.CryptoKey_ASYMMETRIC_DECRYPT, rsaBits: 4096, hash: crypto.SHA256},
	kmspb.CryptoKeyVersion_RSA_DECRYPT_OAEP_4096_SHA512: {purpose: kmspb.CryptoKey_ASYMMETRIC_DECRYPT, rsaBits: 4096, hash: crypto.SHA512},
	kmspb.CryptoKeyVersion_RSA_DECRYPT_OAEP_2048_SHA1:   {purpose: kmspb.CryptoKey_ASYMMETRIC_DECRYPT, rsaBits: 2048, hash: crypto.SHA1},
	kmspb.CryptoKeyVersion_RSA_DECRYPT_OAEP_3072_SHA1:   {purpose: kmspb.CryptoKey_ASYMMETRIC_DECRYPT, rsaBits: 3072, hash: crypto.SHA1},
	kmspb.CryptoKeyVersion_RSA_DECRYPT_OAEP_4096_SHA1:   {purpose: kmspb.CryptoKey_ASYMMETRIC_DECRYPT, rsaBits: 4096, hash: crypto.SHA1},

	// HMAC keys are as long as the hash's output (the key-import page's
	// table of HMAC key lengths).
	kmspb.CryptoKeyVersion_HMAC_SHA1:   {purpose: kmspb.CryptoKey_MAC, hash: crypto.SHA1, keyLen: 20},
	kmspb.CryptoKeyVersion_HMAC_SHA224: {purpose: kmspb.CryptoKey_MAC, hash: crypto.SHA224, keyLen: 28},
	kmspb.CryptoKeyVersion_HMAC_SHA256: {purpose: kmspb.CryptoKey_MAC, hash: crypto.SHA256, keyLen: 32},
	kmspb.CryptoKeyVersion_HMAC_SHA384: {purpose: kmspb.CryptoKey_MAC, hash: crypto.SHA384, keyLen: 48},
	kmspb.CryptoKeyVersion_HMAC_SHA512: {purpose: kmspb.CryptoKey_MAC, hash: crypto.SHA512, keyLen: 64},

	kmspb.CryptoKeyVersion_AES_128_GCM: {purpose: kmspb.CryptoKey_RAW_ENCRYPT_DECRYPT, keyLen: 16, mode: "gcm"},
	kmspb.CryptoKeyVersion_AES_256_GCM: {purpose: kmspb.CryptoKey_RAW_ENCRYPT_DECRYPT, keyLen: 32, mode: "gcm"},
	kmspb.CryptoKeyVersion_AES_128_CBC: {purpose: kmspb.CryptoKey_RAW_ENCRYPT_DECRYPT, keyLen: 16, mode: "cbc"},
	kmspb.CryptoKeyVersion_AES_256_CBC: {purpose: kmspb.CryptoKey_RAW_ENCRYPT_DECRYPT, keyLen: 32, mode: "cbc"},
	kmspb.CryptoKeyVersion_AES_128_CTR: {purpose: kmspb.CryptoKey_RAW_ENCRYPT_DECRYPT, keyLen: 16, mode: "ctr"},
	kmspb.CryptoKeyVersion_AES_256_CTR: {purpose: kmspb.CryptoKey_RAW_ENCRYPT_DECRYPT, keyLen: 32, mode: "ctr"},

	kmspb.CryptoKeyVersion_ML_KEM_768:  {purpose: kmspb.CryptoKey_KEY_ENCAPSULATION, kem: 768},
	kmspb.CryptoKeyVersion_ML_KEM_1024: {purpose: kmspb.CryptoKey_KEY_ENCAPSULATION, kem: 1024},
}

// algorithmPurpose is the purpose of an implemented algorithm. The documented
// algorithms missing from the table are UNIMPLEMENTED: Go's standard library
// has no secp256k1, ML-DSA, SLH-DSA or X-Wing, and AES_256_KWP
// (AES_WRAPPING) needs trusted wrapping. EXTERNAL_SYMMETRIC_ENCRYPTION is
// served for external keys only (levelAlgorithm). ML-KEM is crypto/mlkem.
func algorithmPurpose(a algo) (kmspb.CryptoKey_CryptoKeyPurpose, bool) {
	if s, ok := algorithms[a]; ok {
		return s.purpose, true
	}
	return 0, false
}

// purposes are the CryptoKey purposes CloudBurrow serves.
var purposes = map[kmspb.CryptoKey_CryptoKeyPurpose]bool{
	kmspb.CryptoKey_ENCRYPT_DECRYPT:     true,
	kmspb.CryptoKey_ASYMMETRIC_SIGN:     true,
	kmspb.CryptoKey_ASYMMETRIC_DECRYPT:  true,
	kmspb.CryptoKey_MAC:                 true,
	kmspb.CryptoKey_RAW_ENCRYPT_DECRYPT: true,
	kmspb.CryptoKey_KEY_ENCAPSULATION:   true,
}

// keyAlgorithm resolves the algorithm a new key's versions get: the
// template's, which resources.proto requires for every purpose but
// ENCRYPT_DECRYPT, where GOOGLE_SYMMETRIC_ENCRYPTION is implied. An algorithm
// of another purpose is INVALID_ARGUMENT; a documented one not computed here
// is UNIMPLEMENTED naming it. The codes are UNVERIFIED.
func keyAlgorithm(p kmspb.CryptoKey_CryptoKeyPurpose, a algo) (algo, error) {
	if a == kmspb.CryptoKeyVersion_CRYPTO_KEY_VERSION_ALGORITHM_UNSPECIFIED {
		if p == kmspb.CryptoKey_ENCRYPT_DECRYPT {
			return kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION, nil
		}
		return 0, apierror.InvalidArgument("crypto_key.version_template.algorithm is required for purpose %s", p)
	}
	ap, ok := algorithmPurpose(a)
	if !ok {
		return 0, apierror.Unimplemented("crypto_key.version_template.algorithm %s is not implemented", a)
	}
	if ap != p {
		return 0, apierror.InvalidArgument("crypto_key.version_template.algorithm %s is not valid for purpose %s", a, p)
	}
	return a, nil
}

// generate makes new material for a: the AES or HMAC key bytes, or the
// private key as PKCS #8 DER.
func generate(a algo) ([]byte, error) {
	s, ok := algorithms[a]
	if !ok {
		return nil, apierror.Internal(nil, "no generator for %s", a)
	}
	var priv any
	var err error
	switch {
	case s.rsaBits > 0:
		priv, err = rsa.GenerateKey(rand.Reader, s.rsaBits)
	case s.curve != nil:
		priv, err = ecdsa.GenerateKey(s.curve, rand.Reader)
	case s.ed25519:
		_, priv, err = ed25519.GenerateKey(rand.Reader)
	case s.kem > 0:
		seed := make([]byte, mlkem.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return nil, apierror.Internal(err, "generate key material")
		}
		return seed, nil
	default:
		b := make([]byte, s.keyLen)
		if _, err := rand.Read(b); err != nil {
			return nil, apierror.Internal(err, "generate key material")
		}
		return b, nil
	}
	if err != nil {
		return nil, apierror.Internal(err, "generate %s key", a)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, apierror.Internal(err, "encode %s key", a)
	}
	return der, nil
}

// algorithmOf reads a stored algorithm name; empty is the symmetric default,
// for records written before other purposes existed.
func algorithmOf(name string) algo {
	if name == "" {
		return kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION
	}
	return algo(kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm_value[name])
}

// purposeOf reads a stored purpose name; empty is ENCRYPT_DECRYPT.
func purposeOf(name string) kmspb.CryptoKey_CryptoKeyPurpose {
	if name == "" {
		return kmspb.CryptoKey_ENCRYPT_DECRYPT
	}
	return kmspb.CryptoKey_CryptoKeyPurpose(kmspb.CryptoKey_CryptoKeyPurpose_value[name])
}

func (k cryptoKey) purpose() kmspb.CryptoKey_CryptoKeyPurpose { return purposeOf(k.Purpose) }
func (k cryptoKey) algorithm() algo                           { return algorithmOf(k.Algorithm) }
func (v keyVersion) algorithm() algo                          { return algorithmOf(v.Algorithm) }

// storedName is how a purpose or algorithm is stored: the symmetric defaults
// as empty, so a record reads back the same whichever code wrote it.
func storedPurpose(p kmspb.CryptoKey_CryptoKeyPurpose) string {
	if p == kmspb.CryptoKey_ENCRYPT_DECRYPT {
		return ""
	}
	return p.String()
}

func storedAlgorithm(a algo) string {
	if a == kmspb.CryptoKeyVersion_GOOGLE_SYMMETRIC_ENCRYPTION {
		return ""
	}
	return a.String()
}

// Algorithms lists the implemented algorithms of purpose, by name, sorted,
// for the console's Create key form, at SOFTWARE and HSM; ExternalAlgorithms
// lists the external levels'.
func Algorithms(purpose kmspb.CryptoKey_CryptoKeyPurpose) []string {
	var out []string
	for a, s := range algorithms {
		if s.purpose == purpose && a != kmspb.CryptoKeyVersion_EXTERNAL_SYMMETRIC_ENCRYPTION {
			out = append(out, a.String())
		}
	}
	sort.Strings(out)
	return out
}

// Purposes lists the implemented purposes by name, ENCRYPT_DECRYPT first.
func Purposes() []string {
	return []string{"ENCRYPT_DECRYPT", "ASYMMETRIC_SIGN", "ASYMMETRIC_DECRYPT", "MAC", "RAW_ENCRYPT_DECRYPT", "KEY_ENCAPSULATION"}
}

func parsePublicPEM(p string) (any, error) {
	b, _ := pem.Decode([]byte(p))
	if b == nil {
		return nil, errors.New("not a PEM public key")
	}
	return x509.ParsePKIXPublicKey(b.Bytes)
}

// VerifySignature checks an AsymmetricSign signature of data against the
// GetPublicKey PEM, as a client does with standard crypto (Google's
// create-validate-signatures page). The console's Verify uses it; nothing
// server-side does, because Cloud KMS has no verify method.
func VerifySignature(publicPEM string, a kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm, data, sig []byte) (bool, error) {
	pub, err := parsePublicPEM(publicPEM)
	if err != nil {
		return false, err
	}
	s, ok := algorithms[a]
	if !ok || s.purpose != kmspb.CryptoKey_ASYMMETRIC_SIGN {
		return false, fmt.Errorf("%s is not a signing algorithm", a)
	}
	var digest []byte
	if s.hash != 0 {
		h := s.hash.New()
		h.Write(data)
		digest = h.Sum(nil)
	}
	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		return ecdsa.VerifyASN1(k, digest, sig), nil
	case ed25519.PublicKey:
		return ed25519.Verify(k, data, sig), nil
	case *rsa.PublicKey:
		switch {
		case s.hash == 0:
			return rsa.VerifyPKCS1v15(k, 0, data, sig) == nil, nil
		case s.pss:
			return rsa.VerifyPSS(k, s.hash, digest, sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash}) == nil, nil
		default:
			return rsa.VerifyPKCS1v15(k, s.hash, digest, sig) == nil, nil
		}
	}
	return false, errors.New("unsupported public key type")
}

// EncryptWithPublicKey is RSA-OAEP under the GetPublicKey PEM with the
// algorithm's hash and an empty label, as a client encrypts for
// AsymmetricDecrypt. The console's Encrypt with public key uses it.
func EncryptWithPublicKey(publicPEM string, a kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm, plaintext []byte) ([]byte, error) {
	pub, err := parsePublicPEM(publicPEM)
	if err != nil {
		return nil, err
	}
	s, ok := algorithms[a]
	k, isRSA := pub.(*rsa.PublicKey)
	if !ok || s.purpose != kmspb.CryptoKey_ASYMMETRIC_DECRYPT || !isRSA {
		return nil, fmt.Errorf("%s is not an asymmetric decryption algorithm", a)
	}
	return rsa.EncryptOAEP(s.hash.New(), rand.Reader, k, plaintext, nil)
}
