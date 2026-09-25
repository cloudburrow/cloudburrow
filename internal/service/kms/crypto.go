package kms

import (
	"context"
	"hash/crc32"
	"strings"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// maxSoftwareBytes is the plaintext and AAD limit for SOFTWARE keys
// (service.proto EncryptRequest).
const maxSoftwareBytes = 64 * 1024

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

func crc32c(b []byte) int64 { return int64(crc32.Checksum(b, castagnoli)) }

// checkCRC verifies a request checksum when the client sent one: the wrapper
// is present, even as 0, which is the CRC32C of empty input. A mismatch is
// INVALID_ARGUMENT naming the field (data-integrity guidelines).
func checkCRC(field string, data []byte, sent *wrapperspb.Int64Value) (bool, error) {
	if sent == nil {
		return false, nil
	}
	if sent.GetValue() != crc32c(data) {
		return false, apierror.InvalidArgument("%s does not match the data received: it may have been corrupted in transit", field)
	}
	return true, nil
}

// encryptTarget resolves Encrypt's name to the version to use: a CryptoKey's
// primary, or exactly the CryptoKeyVersion named (service.proto).
func (s *Server) encryptTarget(name string) (keyVersion, error) {
	var keyName string
	var number int
	if parts := strings.Split(name, "/"); len(parts) > 8 {
		k, n, err := parseCryptoKeyVersion("name", name)
		if err != nil {
			return keyVersion{}, apierror.Wrap(err)
		}
		keyName, number = k, n
	} else {
		if err := parseCryptoKey("name", name); err != nil {
			return keyVersion{}, apierror.Wrap(err)
		}
		keyName = name
	}
	var k cryptoKey
	if err := s.load(dbKey(keyPrefix, keyName), &k, "CryptoKey", keyName); err != nil {
		return keyVersion{}, err
	}
	if number == 0 {
		if k.Primary == 0 {
			return keyVersion{}, apierror.Wrap(apierror.FailedPrecondition("CryptoKey %s has no primary version to encrypt with", keyName))
		}
		number = k.Primary
	}
	var v keyVersion
	vn := versionName(keyName, number)
	if err := s.load(dbKey(versionPrefix, vn), &v, "CryptoKeyVersion", vn); err != nil {
		return keyVersion{}, err
	}
	// Only an ENABLED version encrypts (key-states); the code for the others
	// is UNVERIFIED.
	if st := v.state(); st != kmspb.CryptoKeyVersion_ENABLED {
		return keyVersion{}, apierror.Wrap(apierror.FailedPrecondition("CryptoKeyVersion %s is %s: only an ENABLED version can encrypt", vn, st))
	}
	return v, nil
}

// Encrypt seals plaintext under a key's primary version, or a named version,
// in CloudBurrow's own ciphertext format (envelope.go), which is not
// interchangeable with Google's.
func (s *Server) Encrypt(_ context.Context, req *kmspb.EncryptRequest) (*kmspb.EncryptResponse, error) {
	pt, aad := req.GetPlaintext(), req.GetAdditionalAuthenticatedData()
	switch {
	case len(pt) == 0:
		return nil, apierror.Wrap(apierror.InvalidArgument("plaintext is required"))
	case len(pt) > maxSoftwareBytes:
		return nil, apierror.Wrap(apierror.InvalidArgument("plaintext is %d bytes: the limit is %d", len(pt), maxSoftwareBytes))
	case len(aad) > maxSoftwareBytes:
		return nil, apierror.Wrap(apierror.InvalidArgument("additional_authenticated_data is %d bytes: the limit is %d", len(aad), maxSoftwareBytes))
	}
	ptOK, err := checkCRC("plaintext_crc32c", pt, req.GetPlaintextCrc32C())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	aadOK, err := checkCRC("additional_authenticated_data_crc32c", aad, req.GetAdditionalAuthenticatedDataCrc32C())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	v, err := s.encryptTarget(req.GetName())
	if err != nil {
		return nil, err
	}
	_, number, err := parseCryptoKeyVersion("name", v.Name)
	if err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "stored version name %s", v.Name))
	}
	ct, err := seal(v.Material, uint32(number), pt, aad)
	if err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "encrypt with %s", v.Name))
	}
	return &kmspb.EncryptResponse{
		Name:                    v.Name,
		Ciphertext:              ct,
		CiphertextCrc32C:        wrapperspb.Int64(crc32c(ct)),
		VerifiedPlaintextCrc32C: ptOK,
		VerifiedAdditionalAuthenticatedDataCrc32C: aadOK,
		ProtectionLevel: kmspb.ProtectionLevel_SOFTWARE,
	}, nil
}

// errBadCiphertext is Decrypt's one answer for every ciphertext that does not
// open, so a response does not tell which check failed. The code is
// UNVERIFIED.
var errBadCiphertext = apierror.InvalidArgument("decryption failed: the ciphertext is invalid, was not produced by this key, or the additional authenticated data does not match")

// Decrypt opens ciphertext produced by Encrypt under this CryptoKey. The
// server picks the version from the ciphertext (service.proto): any ENABLED
// version decrypts, primary or not.
func (s *Server) Decrypt(_ context.Context, req *kmspb.DecryptRequest) (*kmspb.DecryptResponse, error) {
	if err := parseCryptoKey("name", req.GetName()); err != nil {
		return nil, apierror.Wrap(err)
	}
	ct, aad := req.GetCiphertext(), req.GetAdditionalAuthenticatedData()
	switch {
	case len(ct) == 0:
		return nil, apierror.Wrap(apierror.InvalidArgument("ciphertext is required"))
	case len(aad) > maxSoftwareBytes:
		return nil, apierror.Wrap(apierror.InvalidArgument("additional_authenticated_data is %d bytes: the limit is %d", len(aad), maxSoftwareBytes))
	}
	// Integrity first: a corrupted request is reported as corrupted, not as
	// a ciphertext that fails to open.
	if _, err := checkCRC("ciphertext_crc32c", ct, req.GetCiphertextCrc32C()); err != nil {
		return nil, apierror.Wrap(err)
	}
	if _, err := checkCRC("additional_authenticated_data_crc32c", aad, req.GetAdditionalAuthenticatedDataCrc32C()); err != nil {
		return nil, apierror.Wrap(err)
	}
	var k cryptoKey
	if err := s.load(dbKey(keyPrefix, req.GetName()), &k, "CryptoKey", req.GetName()); err != nil {
		return nil, err
	}
	number, err := parseVersion(ct)
	if err != nil {
		return nil, apierror.Wrap(errBadCiphertext)
	}
	var v keyVersion
	found, err := s.get(dbKey(versionPrefix, versionName(k.Name, int(number))), &v)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if !found {
		// A version this key does not have: the ciphertext is not this
		// key's, which is a bad ciphertext rather than a missing resource.
		return nil, apierror.Wrap(errBadCiphertext)
	}
	// State before material: a DESTROYED version has none (key-states,
	// destroy-restore). The code is UNVERIFIED.
	if st := v.state(); st != kmspb.CryptoKeyVersion_ENABLED {
		return nil, apierror.Wrap(apierror.FailedPrecondition("CryptoKeyVersion %s is %s: only an ENABLED version can decrypt", v.Name, st))
	}
	pt, err := open(v.Material, number, ct, aad)
	if err != nil {
		return nil, apierror.Wrap(errBadCiphertext)
	}
	return &kmspb.DecryptResponse{
		Plaintext:       pt,
		PlaintextCrc32C: wrapperspb.Int64(crc32c(pt)),
		UsedPrimary:     int(number) == k.Primary,
		ProtectionLevel: kmspb.ProtectionLevel_SOFTWARE,
	}, nil
}
