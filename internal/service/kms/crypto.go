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
