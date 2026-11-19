package privateca

import (
	"context"
	"crypto"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"

	"cloud.google.com/go/kms/apiv1/kmspb"
	"cloud.google.com/go/security/privateca/apiv1/privatecapb"
	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// KMS is the instance's Cloud KMS, in process: a CA whose key_spec names a
// cloud_kms_key_version signs with that version (#1834), and a pool's
// encryption_spec (CMEK) wraps its certificates' data with a key.
type KMS interface {
	GetCryptoKey(context.Context, *kmspb.GetCryptoKeyRequest) (*kmspb.CryptoKey, error)
	GetCryptoKeyVersion(context.Context, *kmspb.GetCryptoKeyVersionRequest) (*kmspb.CryptoKeyVersion, error)
	GetPublicKey(context.Context, *kmspb.GetPublicKeyRequest) (*kmspb.PublicKey, error)
	AsymmetricSign(context.Context, *kmspb.AsymmetricSignRequest) (*kmspb.AsymmetricSignResponse, error)
	Encrypt(context.Context, *kmspb.EncryptRequest) (*kmspb.EncryptResponse, error)
	Decrypt(context.Context, *kmspb.DecryptRequest) (*kmspb.DecryptResponse, error)
}

// UseKMS gives the server Cloud KMS. Without it a CA key held in Cloud KMS
// and a CMEK pool are FAILED_PRECONDITION: Cloud KMS is not served.
func (s *Server) UseKMS(k KMS) { s.kms = k }

var (
	kmsVersionRE = regexp.MustCompile(`^projects/[^/]+/locations/([^/]+)/keyRings/[^/]+/cryptoKeys/[^/]+/cryptoKeyVersions/[^/]+$`)
	kmsKeyRE     = regexp.MustCompile(`^projects/[^/]+/locations/([^/]+)/keyRings/[^/]+/cryptoKeys/[^/]+$`)
)

// kmsAlgorithms are the Cloud KMS signing algorithms a CA key may have:
// "The key's algorithm must be a supported asymmetric signing algorithm",
// RSA_SIGN_PSS and RSA_SIGN_PKCS1 and EC_SIGN_P256 and EC_SIGN_P384, never
// Ed25519 (choosing-key-algorithm), as the CA's SignHashAlgorithm names
// them.
var kmsAlgorithms = map[kmspb.CryptoKeyVersion_CryptoKeyVersionAlgorithm]privatecapb.CertificateAuthority_SignHashAlgorithm{
	kmspb.CryptoKeyVersion_RSA_SIGN_PSS_2048_SHA256:   privatecapb.CertificateAuthority_RSA_PSS_2048_SHA256,
	kmspb.CryptoKeyVersion_RSA_SIGN_PSS_3072_SHA256:   privatecapb.CertificateAuthority_RSA_PSS_3072_SHA256,
	kmspb.CryptoKeyVersion_RSA_SIGN_PSS_4096_SHA256:   privatecapb.CertificateAuthority_RSA_PSS_4096_SHA256,
	kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_2048_SHA256: privatecapb.CertificateAuthority_RSA_PKCS1_2048_SHA256,
	kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_3072_SHA256: privatecapb.CertificateAuthority_RSA_PKCS1_3072_SHA256,
	kmspb.CryptoKeyVersion_RSA_SIGN_PKCS1_4096_SHA256: privatecapb.CertificateAuthority_RSA_PKCS1_4096_SHA256,
	kmspb.CryptoKeyVersion_EC_SIGN_P256_SHA256:        privatecapb.CertificateAuthority_EC_P256_SHA256,
	kmspb.CryptoKeyVersion_EC_SIGN_P384_SHA384:        privatecapb.CertificateAuthority_EC_P384_SHA384,
}

// kmsSigner is a crypto.Signer whose private key is a Cloud KMS
// CryptoKeyVersion: every signature is an AsymmetricSign of the digest.
type kmsSigner struct {
	s       *Server
	version string
	pub     crypto.PublicKey
	alg     privatecapb.CertificateAuthority_SignHashAlgorithm
}

func (k *kmsSigner) Public() crypto.PublicKey { return k.pub }

func (k *kmsSigner) Sign(_ io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
	if k.s.kms == nil {
		return nil, apierror.FailedPrecondition("Cloud KMS is not served by this instance, so %s cannot sign", k.version)
	}
	d := &kmspb.Digest{}
	switch opts.HashFunc() {
	case crypto.SHA256:
		d.Digest = &kmspb.Digest_Sha256{Sha256: digest}
	case crypto.SHA384:
		d.Digest = &kmspb.Digest_Sha384{Sha384: digest}
	case crypto.SHA512:
		d.Digest = &kmspb.Digest_Sha512{Sha512: digest}
	default:
		return nil, errors.New("Cloud KMS signs only SHA-256, SHA-384 and SHA-512 digests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	r, err := k.s.kms.AsymmetricSign(ctx, &kmspb.AsymmetricSignRequest{Name: k.version, Digest: d})
	if err != nil {
		return nil, err
	}
	return r.GetSignature(), nil
}

// signErr is a signing failure: Cloud KMS's own status (a disabled or
// destroyed version is FAILED_PRECONDITION there) when it has one, else
// what fallback says.
func signErr(err error, fallback func(error) error) error {
	if e := apierror.From(err); e.Code != codes.Internal && e.Code != codes.Unknown {
		return e
	}
	return fallback(err)
}

// kmsKey is the signer of a CA whose key is the Cloud KMS version named:
// "The key must be in the same location as your CA pool", "The key
// version that you plan to use must be enabled", and its purpose must be
// ASYMMETRIC_SIGN with a supported algorithm (choosing-key-algorithm).
func (s *Server) kmsKey(ctx context.Context, version, pool string) (*kmsSigner, error) {
	m := kmsVersionRE.FindStringSubmatch(version)
	if m == nil {
		return nil, apierror.InvalidArgument("key_spec.cloud_kms_key_version %q must be projects/*/locations/*/keyRings/*/cryptoKeys/*/cryptoKeyVersions/*", version)
	}
	loc := strings.Split(pool, "/")[3]
	if m[1] != loc {
		return nil, apierror.InvalidArgument("key_spec.cloud_kms_key_version %s must be in the same location as the CA pool (%s)", version, loc)
	}
	if s.kms == nil {
		return nil, apierror.FailedPrecondition("Cloud KMS is not served by this instance: start it with the kms service to hold a CA key in Cloud KMS")
	}
	v, err := s.kms.GetCryptoKeyVersion(ctx, &kmspb.GetCryptoKeyVersionRequest{Name: version})
	if err != nil {
		return nil, err
	}
	if v.GetState() != kmspb.CryptoKeyVersion_ENABLED {
		return nil, apierror.FailedPrecondition("key_spec.cloud_kms_key_version %s is %s: the key version must be enabled", version, v.GetState())
	}
	alg, ok := kmsAlgorithms[v.GetAlgorithm()]
	if !ok {
		return nil, apierror.InvalidArgument("key_spec.cloud_kms_key_version %s has algorithm %s: a CA key must be an ASYMMETRIC_SIGN key of RSA_SIGN_PSS_*_SHA256, RSA_SIGN_PKCS1_*_SHA256, EC_SIGN_P256_SHA256 or EC_SIGN_P384_SHA384", version, v.GetAlgorithm())
	}
	pk, err := s.kms.GetPublicKey(ctx, &kmspb.GetPublicKeyRequest{Name: version})
	if err != nil {
		return nil, err
	}
	pub, err := parsePublicPEM(pk.GetPem())
	if err != nil {
		return nil, apierror.Internal(err, "the public key of %s", version)
	}
	return &kmsSigner{s: s, version: version, pub: pub, alg: alg}, nil
}

func parsePublicPEM(p string) (crypto.PublicKey, error) {
	blk, _ := pem.Decode([]byte(p))
	if blk == nil {
		return nil, errors.New("not PEM")
	}
	return x509.ParsePKIXPublicKey(blk.Bytes)
}

func publicPEM(pub crypto.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})), nil
}

// sigAlgOf is the signature algorithm of a SignHashAlgorithm.
func sigAlgOf(a privatecapb.CertificateAuthority_SignHashAlgorithm) x509.SignatureAlgorithm {
	switch a {
	case privatecapb.CertificateAuthority_RSA_PSS_2048_SHA256, privatecapb.CertificateAuthority_RSA_PSS_3072_SHA256, privatecapb.CertificateAuthority_RSA_PSS_4096_SHA256:
		return x509.SHA256WithRSAPSS
	case privatecapb.CertificateAuthority_RSA_PKCS1_2048_SHA256, privatecapb.CertificateAuthority_RSA_PKCS1_3072_SHA256, privatecapb.CertificateAuthority_RSA_PKCS1_4096_SHA256:
		return x509.SHA256WithRSA
	case privatecapb.CertificateAuthority_EC_P256_SHA256:
		return x509.ECDSAWithSHA256
	case privatecapb.CertificateAuthority_EC_P384_SHA384:
		return x509.ECDSAWithSHA384
	}
	return x509.UnknownSignatureAlgorithm
}
