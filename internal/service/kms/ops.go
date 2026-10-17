package kms

import (
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/mlkem"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"hash"

	_ "crypto/sha1"   // HMAC_SHA1, OAEP SHA-1
	_ "crypto/sha256" // SHA-224, SHA-256
	_ "crypto/sha512" // SHA-384, SHA-512

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// The cryptographic RPCs of the asymmetric, MAC and raw purposes (#1212),
// computed with Go's standard crypto so that their output verifies, decrypts
// and opens with any standard library, as Google documents (for example a
// signature checked against the GetPublicKey PEM). Every version here is
// SOFTWARE. The codes for a version of the wrong purpose or state are
// UNVERIFIED.
// Docs re-read 2026-10-17, still unanswered: https://raw.githubusercontent.com/googleapis/googleapis/master/google/cloud/kms/v1/service.proto

// wantPurpose refuses a key of a purpose other than want.
func wantPurpose(k cryptoKey, want ...kmspb.CryptoKey_CryptoKeyPurpose) error {
	p := k.purpose()
	for _, w := range want {
		if p == w {
			return nil
		}
	}
	return apierror.Wrap(apierror.FailedPrecondition("CryptoKey %s has purpose %s: this method needs %v", k.Name, p, want))
}

// usableVersion loads the version named by field for a cryptographic
// operation: its key must have one of the purposes, and the version must be
// ENABLED (key-states).
func (s *Server) usableVersion(field, name string, want ...kmspb.CryptoKey_CryptoKeyPurpose) (keyVersion, error) {
	keyName, _, err := parseCryptoKeyVersion(field, name)
	if err != nil {
		return keyVersion{}, apierror.Wrap(err)
	}
	var k cryptoKey
	if err := s.load(dbKey(keyPrefix, keyName), &k, "CryptoKey", keyName); err != nil {
		return keyVersion{}, err
	}
	if err := wantPurpose(k, want...); err != nil {
		return keyVersion{}, err
	}
	if err := s.backendUsable(k); err != nil {
		return keyVersion{}, err
	}
	var v keyVersion
	if err := s.load(dbKey(versionPrefix, name), &v, "CryptoKeyVersion", name); err != nil {
		return keyVersion{}, err
	}
	if st := v.state(); st != kmspb.CryptoKeyVersion_ENABLED {
		return keyVersion{}, apierror.Wrap(apierror.FailedPrecondition("CryptoKeyVersion %s is %s: only an ENABLED version can be used", name, st))
	}
	return v, nil
}

func (v keyVersion) privateKey() (crypto.Signer, error) {
	k, err := x509.ParsePKCS8PrivateKey(v.Material)
	if err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "stored key of %s", v.Name))
	}
	signer, ok := k.(crypto.Signer)
	if !ok {
		return nil, apierror.Wrap(apierror.Internal(nil, "stored key of %s is not a signer", v.Name))
	}
	return signer, nil
}

func checkSize(field string, b []byte) error {
	if len(b) > maxSoftwareBytes {
		return apierror.Wrap(apierror.InvalidArgument("%s is %d bytes: the limit is %d", field, len(b), maxSoftwareBytes))
	}
	return nil
}

func crc(b []byte) *wrapperspb.Int64Value { return wrapperspb.Int64(crc32c(b)) }

// GetPublicKey returns an asymmetric version's public key as PEM, with its
// CRC32C (resources.proto PublicKey), and in public_key when a format is
// asked for.
func (s *Server) GetPublicKey(_ context.Context, req *kmspb.GetPublicKeyRequest) (*kmspb.PublicKey, error) {
	v, err := s.usableVersion("name", req.GetName(), kmspb.CryptoKey_ASYMMETRIC_SIGN, kmspb.CryptoKey_ASYMMETRIC_DECRYPT, kmspb.CryptoKey_KEY_ENCAPSULATION)
	if err != nil {
		return nil, err
	}
	if algorithms[v.algorithm()].kem > 0 {
		return v.kemPublicKey(req.GetPublicKeyFormat())
	}
	priv, err := v.privateKey()
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKIXPublicKey(priv.Public())
	if err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "encode the public key of %s", v.Name))
	}
	p := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	out := &kmspb.PublicKey{
		Pem:             p,
		PemCrc32C:       crc([]byte(p)),
		Algorithm:       v.algorithm(),
		Name:            v.Name,
		ProtectionLevel: v.protectionLevel(),
	}
	switch f := req.GetPublicKeyFormat(); f {
	case kmspb.PublicKey_PUBLIC_KEY_FORMAT_UNSPECIFIED:
	case kmspb.PublicKey_PEM:
		out.PublicKeyFormat = f
		out.PublicKey = &kmspb.ChecksummedData{Data: []byte(p), Crc32CChecksum: crc([]byte(p))}
	case kmspb.PublicKey_DER:
		out.PublicKeyFormat = f
		out.PublicKey = &kmspb.ChecksummedData{Data: der, Crc32CChecksum: crc(der)}
	default:
		// NIST_PQC and XWING_RAW_BYTES are the post-quantum formats.
		return nil, apierror.Wrap(apierror.InvalidArgument("public_key_format %s is not valid for %s", f, v.algorithm()))
	}
	return out, nil
}

func digestBytes(d *kmspb.Digest) (crypto.Hash, []byte) {
	switch x := d.GetDigest().(type) {
	case *kmspb.Digest_Sha256:
		return crypto.SHA256, x.Sha256
	case *kmspb.Digest_Sha384:
		return crypto.SHA384, x.Sha384
	case *kmspb.Digest_Sha512:
		return crypto.SHA512, x.Sha512
	}
	return 0, nil
}

// AsymmetricSign signs a digest, or data the server hashes, with an
// ASYMMETRIC_SIGN version. digest and data are exclusive (service.proto).
// RSA_SIGN_RAW_PKCS1_* and EC_SIGN_ED25519 sign data as given, so they take
// data only (the algorithms page). EC signatures are DER, as Google returns
// them; PSS uses a salt as long as the digest.
func (s *Server) AsymmetricSign(_ context.Context, req *kmspb.AsymmetricSignRequest) (*kmspb.AsymmetricSignResponse, error) {
	data, hasDigest := req.GetData(), req.GetDigest() != nil
	if hasDigest && len(data) > 0 {
		return nil, apierror.Wrap(apierror.InvalidArgument("digest and data cannot both be supplied"))
	}
	if err := checkSize("data", data); err != nil {
		return nil, err
	}
	dataOK, err := checkCRC("data_crc32c", data, req.GetDataCrc32C())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	dh, digest := digestBytes(req.GetDigest())
	digestOK, err := checkCRC("digest_crc32c", digest, req.GetDigestCrc32C())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	v, err := s.usableVersion("name", req.GetName(), kmspb.CryptoKey_ASYMMETRIC_SIGN)
	if err != nil {
		return nil, err
	}
	if err := s.kajAllows(v.Name); err != nil {
		return nil, err
	}
	spec := algorithms[v.algorithm()]
	raw := spec.hash == 0
	switch {
	case raw && hasDigest:
		return nil, apierror.Wrap(apierror.InvalidArgument("%s signs data as given: supply data, not digest", v.algorithm()))
	case raw && len(data) == 0:
		return nil, apierror.Wrap(apierror.InvalidArgument("data is required for %s", v.algorithm()))
	case !raw && hasDigest && dh != spec.hash:
		return nil, apierror.Wrap(apierror.InvalidArgument("digest must be %s for %s", spec.hash, v.algorithm()))
	case !raw && hasDigest && len(digest) != spec.hash.Size():
		return nil, apierror.Wrap(apierror.InvalidArgument("digest is %d bytes: %s needs %d", len(digest), spec.hash, spec.hash.Size()))
	case !raw && !hasDigest:
		if len(data) == 0 {
			return nil, apierror.Wrap(apierror.InvalidArgument("digest or data is required"))
		}
		h := spec.hash.New()
		h.Write(data)
		digest = h.Sum(nil)
	}
	priv, err := v.privateKey()
	if err != nil {
		return nil, err
	}
	var sig []byte
	switch key := priv.(type) {
	case *rsa.PrivateKey:
		switch {
		case raw:
			sig, err = rsa.SignPKCS1v15(rand.Reader, key, 0, data)
		case spec.pss:
			sig, err = rsa.SignPSS(rand.Reader, key, spec.hash, digest, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
		default:
			sig, err = rsa.SignPKCS1v15(rand.Reader, key, spec.hash, digest)
		}
	case *ecdsa.PrivateKey:
		sig, err = ecdsa.SignASN1(rand.Reader, key, digest)
	case ed25519.PrivateKey:
		sig = ed25519.Sign(key, data)
	default:
		return nil, apierror.Wrap(apierror.Internal(nil, "stored key of %s has an unknown type", v.Name))
	}
	if err != nil {
		// Raw PKCS#1 input too long for the modulus is the caller's.
		return nil, apierror.Wrap(apierror.InvalidArgument("sign with %s: %v", v.algorithm(), err))
	}
	return &kmspb.AsymmetricSignResponse{
		Signature:            sig,
		SignatureCrc32C:      crc(sig),
		VerifiedDigestCrc32C: digestOK,
		VerifiedDataCrc32C:   dataOK,
		Name:                 v.Name,
		ProtectionLevel:      v.protectionLevel(),
	}, nil
}

// AsymmetricDecrypt opens RSA-OAEP ciphertext made with the version's public
// key, with the algorithm's hash for both OAEP and MGF1 and an empty label
// (the algorithms page).
func (s *Server) AsymmetricDecrypt(_ context.Context, req *kmspb.AsymmetricDecryptRequest) (*kmspb.AsymmetricDecryptResponse, error) {
	ct := req.GetCiphertext()
	if len(ct) == 0 {
		return nil, apierror.Wrap(apierror.InvalidArgument("ciphertext is required"))
	}
	ctOK, err := checkCRC("ciphertext_crc32c", ct, req.GetCiphertextCrc32C())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	v, err := s.usableVersion("name", req.GetName(), kmspb.CryptoKey_ASYMMETRIC_DECRYPT)
	if err != nil {
		return nil, err
	}
	if err := s.kajAllows(v.Name); err != nil {
		return nil, err
	}
	priv, err := v.privateKey()
	if err != nil {
		return nil, err
	}
	key, ok := priv.(*rsa.PrivateKey)
	if !ok {
		return nil, apierror.Wrap(apierror.Internal(nil, "stored key of %s is not RSA", v.Name))
	}
	pt, err := rsa.DecryptOAEP(algorithms[v.algorithm()].hash.New(), nil, key, ct, nil)
	if err != nil {
		return nil, apierror.Wrap(errBadCiphertext)
	}
	return &kmspb.AsymmetricDecryptResponse{
		Plaintext:                pt,
		PlaintextCrc32C:          crc(pt),
		VerifiedCiphertextCrc32C: ctOK,
		ProtectionLevel:          v.protectionLevel(),
	}, nil
}

func (v keyVersion) mac(data []byte) []byte {
	h := hmac.New(func() hash.Hash { return algorithms[v.algorithm()].hash.New() }, v.Material)
	h.Write(data)
	return h.Sum(nil)
}

// MacSign computes the HMAC of data with a MAC version.
func (s *Server) MacSign(_ context.Context, req *kmspb.MacSignRequest) (*kmspb.MacSignResponse, error) {
	data := req.GetData()
	if err := checkSize("data", data); err != nil {
		return nil, err
	}
	dataOK, err := checkCRC("data_crc32c", data, req.GetDataCrc32C())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	v, err := s.usableVersion("name", req.GetName(), kmspb.CryptoKey_MAC)
	if err != nil {
		return nil, err
	}
	if err := s.kajAllows(v.Name); err != nil {
		return nil, err
	}
	m := v.mac(data)
	return &kmspb.MacSignResponse{Name: v.Name, Mac: m, MacCrc32C: crc(m), VerifiedDataCrc32C: dataOK,
		ProtectionLevel: v.protectionLevel()}, nil
}

// MacVerify checks a MAC in constant time. A MAC that does not match is a
// response with success false, not an error (service.proto);
// verified_success_integrity repeats success, as documented.
func (s *Server) MacVerify(_ context.Context, req *kmspb.MacVerifyRequest) (*kmspb.MacVerifyResponse, error) {
	data, mac := req.GetData(), req.GetMac()
	if err := checkSize("data", data); err != nil {
		return nil, err
	}
	dataOK, err := checkCRC("data_crc32c", data, req.GetDataCrc32C())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	macOK, err := checkCRC("mac_crc32c", mac, req.GetMacCrc32C())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	v, err := s.usableVersion("name", req.GetName(), kmspb.CryptoKey_MAC)
	if err != nil {
		return nil, err
	}
	ok := hmac.Equal(v.mac(data), mac)
	return &kmspb.MacVerifyResponse{Name: v.Name, Success: ok, VerifiedDataCrc32C: dataOK, VerifiedMacCrc32C: macOK,
		VerifiedSuccessIntegrity: ok, ProtectionLevel: v.protectionLevel()}, nil
}

// Raw AES parameters (the raw-encryption page): GCM takes a 12-byte IV the
// server always generates and a 16-byte tag; CBC and CTR take a 16-byte IV,
// the caller's or a generated one. CBC is unpadded: the plaintext is a whole
// number of blocks. Additional authenticated data is GCM's only.
const (
	gcmIVLen  = 12
	gcmTagLen = 16
)

func (v keyVersion) rawMode() string { return algorithms[v.algorithm()].mode }

// RawEncrypt encrypts with a RAW_ENCRYPT_DECRYPT version and returns the
// ciphertext and IV unwrapped, for interoperation with other AES
// implementations.
func (s *Server) RawEncrypt(_ context.Context, req *kmspb.RawEncryptRequest) (*kmspb.RawEncryptResponse, error) {
	pt, aad, iv := req.GetPlaintext(), req.GetAdditionalAuthenticatedData(), req.GetInitializationVector()
	if err := checkSize("plaintext", pt); err != nil {
		return nil, err
	}
	if err := checkSize("additional_authenticated_data", aad); err != nil {
		return nil, err
	}
	ptOK, err := checkCRC("plaintext_crc32c", pt, req.GetPlaintextCrc32C())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	aadOK, err := checkCRC("additional_authenticated_data_crc32c", aad, req.GetAdditionalAuthenticatedDataCrc32C())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	ivOK, err := checkCRC("initialization_vector_crc32c", iv, req.GetInitializationVectorCrc32C())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	v, err := s.usableVersion("name", req.GetName(), kmspb.CryptoKey_RAW_ENCRYPT_DECRYPT)
	if err != nil {
		return nil, err
	}
	if err := s.kajAllows(v.Name); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(v.Material)
	if err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "stored key of %s", v.Name))
	}
	mode := v.rawMode()
	if mode != "gcm" && len(aad) > 0 {
		return nil, apierror.Wrap(apierror.InvalidArgument("additional_authenticated_data is only accepted by AES-GCM, not %s", v.algorithm()))
	}
	var ct []byte
	var tagLen int32
	switch mode {
	case "gcm":
		if len(iv) > 0 {
			return nil, apierror.Wrap(apierror.InvalidArgument("initialization_vector cannot be supplied for %s: the service generates it", v.algorithm()))
		}
		iv = make([]byte, gcmIVLen)
		if _, err := rand.Read(iv); err != nil {
			return nil, apierror.Wrap(apierror.Internal(err, "generate an IV"))
		}
		g, err := cipher.NewGCM(block)
		if err != nil {
			return nil, apierror.Wrap(apierror.Internal(err, "GCM"))
		}
		ct, tagLen = g.Seal(nil, iv, pt, aad), gcmTagLen
	case "cbc", "ctr":
		if len(iv) == 0 {
			iv = make([]byte, aes.BlockSize)
			if _, err := rand.Read(iv); err != nil {
				return nil, apierror.Wrap(apierror.Internal(err, "generate an IV"))
			}
		} else if len(iv) != aes.BlockSize {
			return nil, apierror.Wrap(apierror.InvalidArgument("initialization_vector is %d bytes: %s needs %d", len(iv), v.algorithm(), aes.BlockSize))
		}
		ct = make([]byte, len(pt))
		if mode == "cbc" {
			if len(pt)%aes.BlockSize != 0 {
				return nil, apierror.Wrap(apierror.InvalidArgument("plaintext is %d bytes: %s is unpadded, so it must be a multiple of %d", len(pt), v.algorithm(), aes.BlockSize))
			}
			cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, pt)
		} else {
			cipher.NewCTR(block, iv).XORKeyStream(ct, pt)
		}
	}
	return &kmspb.RawEncryptResponse{
		Ciphertext:                 ct,
		InitializationVector:       iv,
		TagLength:                  tagLen,
		CiphertextCrc32C:           crc(ct),
		InitializationVectorCrc32C: crc(iv),
		VerifiedPlaintextCrc32C:    ptOK,
		VerifiedAdditionalAuthenticatedDataCrc32C: aadOK,
		VerifiedInitializationVectorCrc32C:        ivOK,
		Name:                                      v.Name,
		ProtectionLevel:                           v.protectionLevel(),
	}, nil
}

// RawDecrypt reverses RawEncrypt.
func (s *Server) RawDecrypt(_ context.Context, req *kmspb.RawDecryptRequest) (*kmspb.RawDecryptResponse, error) {
	ct, aad, iv := req.GetCiphertext(), req.GetAdditionalAuthenticatedData(), req.GetInitializationVector()
	if len(ct) == 0 {
		return nil, apierror.Wrap(apierror.InvalidArgument("ciphertext is required"))
	}
	if len(iv) == 0 {
		return nil, apierror.Wrap(apierror.InvalidArgument("initialization_vector is required"))
	}
	if err := checkSize("additional_authenticated_data", aad); err != nil {
		return nil, err
	}
	ctOK, err := checkCRC("ciphertext_crc32c", ct, req.GetCiphertextCrc32C())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	aadOK, err := checkCRC("additional_authenticated_data_crc32c", aad, req.GetAdditionalAuthenticatedDataCrc32C())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	ivOK, err := checkCRC("initialization_vector_crc32c", iv, req.GetInitializationVectorCrc32C())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	v, err := s.usableVersion("name", req.GetName(), kmspb.CryptoKey_RAW_ENCRYPT_DECRYPT)
	if err != nil {
		return nil, err
	}
	if err := s.kajAllows(v.Name); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(v.Material)
	if err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "stored key of %s", v.Name))
	}
	mode := v.rawMode()
	if mode != "gcm" && len(aad) > 0 {
		return nil, apierror.Wrap(apierror.InvalidArgument("additional_authenticated_data is only accepted by AES-GCM, not %s", v.algorithm()))
	}
	var pt []byte
	switch mode {
	case "gcm":
		if tl := req.GetTagLength(); tl != 0 && tl != gcmTagLen {
			return nil, apierror.Wrap(apierror.InvalidArgument("tag_length %d is not supported: AES-GCM tags here are %d bytes", tl, gcmTagLen))
		}
		if len(iv) != gcmIVLen {
			return nil, apierror.Wrap(apierror.InvalidArgument("initialization_vector is %d bytes: %s needs %d", len(iv), v.algorithm(), gcmIVLen))
		}
		g, err := cipher.NewGCM(block)
		if err != nil {
			return nil, apierror.Wrap(apierror.Internal(err, "GCM"))
		}
		if pt, err = g.Open(nil, iv, ct, aad); err != nil {
			return nil, apierror.Wrap(errBadCiphertext)
		}
	case "cbc", "ctr":
		if len(iv) != aes.BlockSize {
			return nil, apierror.Wrap(apierror.InvalidArgument("initialization_vector is %d bytes: %s needs %d", len(iv), v.algorithm(), aes.BlockSize))
		}
		pt = make([]byte, len(ct))
		if mode == "cbc" {
			if len(ct)%aes.BlockSize != 0 {
				return nil, apierror.Wrap(apierror.InvalidArgument("ciphertext is %d bytes: %s needs a multiple of %d", len(ct), v.algorithm(), aes.BlockSize))
			}
			cipher.NewCBCDecrypter(block, iv).CryptBlocks(pt, ct)
		} else {
			cipher.NewCTR(block, iv).XORKeyStream(pt, ct)
		}
	}
	return &kmspb.RawDecryptResponse{
		Plaintext:                pt,
		PlaintextCrc32C:          crc(pt),
		ProtectionLevel:          v.protectionLevel(),
		VerifiedCiphertextCrc32C: ctOK,
		VerifiedAdditionalAuthenticatedDataCrc32C: aadOK,
		VerifiedInitializationVectorCrc32C:        ivOK,
	}, nil
}

// GenerateRandomBytes returns 8 to 1024 bytes from Go's crypto/rand.
// service.proto says only the HSM protection level is supported, so HSM is
// what is accepted, as on Google; no HSM is involved locally. Another level
// is INVALID_ARGUMENT (UNVERIFIED).
// Docs re-read 2026-10-17, still unanswered: https://raw.githubusercontent.com/googleapis/googleapis/master/google/cloud/kms/v1/service.proto
func (s *Server) GenerateRandomBytes(_ context.Context, req *kmspb.GenerateRandomBytesRequest) (*kmspb.GenerateRandomBytesResponse, error) {
	if err := parseLocation("location", req.GetLocation()); err != nil {
		return nil, apierror.Wrap(err)
	}
	if n := req.GetLengthBytes(); n < 8 || n > 1024 {
		return nil, apierror.Wrap(apierror.InvalidArgument("length_bytes %d is out of range: it must be from 8 to 1024", n))
	}
	if pl := req.GetProtectionLevel(); pl != kmspb.ProtectionLevel_HSM {
		return nil, apierror.Wrap(apierror.InvalidArgument("protection_level %s is not supported: only HSM is", pl))
	}
	b := make([]byte, req.GetLengthBytes())
	if _, err := rand.Read(b); err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "generate random bytes"))
	}
	return &kmspb.GenerateRandomBytesResponse{Data: b, DataCrc32C: crc(b)}, nil
}

// kemKey is an ML-KEM version's decapsulation key, from its stored seed.
func (v keyVersion) kemKey() (decapsulator, error) {
	var (
		dk  decapsulator
		err error
	)
	if algorithms[v.algorithm()].kem == 1024 {
		dk, err = mlkem.NewDecapsulationKey1024(v.Material)
	} else {
		dk, err = mlkem.NewDecapsulationKey768(v.Material)
	}
	if err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "stored key of %s", v.Name))
	}
	return dk, nil
}

type decapsulator interface {
	Decapsulate(ciphertext []byte) ([]byte, error)
}

// kemPublicKey returns an ML-KEM version's encapsulation key. resources.proto
// requires public_key_format for post-quantum algorithms, and NIST_PQC is the
// FIPS 203 encoding; whether Google also serves ML-KEM keys as PEM or DER is
// UNVERIFIED, so those are refused.
// Docs re-read 2026-10-17, still unanswered: https://raw.githubusercontent.com/googleapis/googleapis/master/google/cloud/kms/v1/resources.proto
func (v keyVersion) kemPublicKey(f kmspb.PublicKey_PublicKeyFormat) (*kmspb.PublicKey, error) {
	if f != kmspb.PublicKey_NIST_PQC {
		return nil, apierror.Wrap(apierror.InvalidArgument("public_key_format must be NIST_PQC for %s, not %s", v.algorithm(), f))
	}
	dk, err := v.kemKey()
	if err != nil {
		return nil, err
	}
	var ek []byte
	switch k := dk.(type) {
	case *mlkem.DecapsulationKey768:
		ek = k.EncapsulationKey().Bytes()
	case *mlkem.DecapsulationKey1024:
		ek = k.EncapsulationKey().Bytes()
	}
	return &kmspb.PublicKey{Algorithm: v.algorithm(), Name: v.Name, ProtectionLevel: v.protectionLevel(),
		PublicKeyFormat: f, PublicKey: &kmspb.ChecksummedData{Data: ek, Crc32CChecksum: crc(ek)}}, nil
}

// Decapsulate recovers the shared secret from an ML-KEM ciphertext made with
// the version's encapsulation key.
func (s *Server) Decapsulate(_ context.Context, req *kmspb.DecapsulateRequest) (*kmspb.DecapsulateResponse, error) {
	ct := req.GetCiphertext()
	if len(ct) == 0 {
		return nil, apierror.Wrap(apierror.InvalidArgument("ciphertext is required"))
	}
	ctOK, err := checkCRC("ciphertext_crc32c", ct, req.GetCiphertextCrc32C())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	v, err := s.usableVersion("name", req.GetName(), kmspb.CryptoKey_KEY_ENCAPSULATION)
	if err != nil {
		return nil, err
	}
	if err := s.kajAllows(v.Name); err != nil {
		return nil, err
	}
	dk, err := v.kemKey()
	if err != nil {
		return nil, err
	}
	secret, err := dk.Decapsulate(ct)
	if err != nil {
		// Only a ciphertext of the wrong length fails: ML-KEM rejects
		// implicitly, returning a pseudo-random secret for a bad one.
		return nil, apierror.Wrap(apierror.InvalidArgument("ciphertext is %d bytes, not a %s ciphertext", len(ct), v.algorithm()))
	}
	sum := crc32c(secret)
	return &kmspb.DecapsulateResponse{Name: v.Name, SharedSecret: secret, SharedSecretCrc32C: &sum,
		VerifiedCiphertextCrc32C: ctOK, ProtectionLevel: v.protectionLevel()}, nil
}
