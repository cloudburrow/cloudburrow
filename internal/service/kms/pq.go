package kms

import (
	"crypto"
	"crypto/ecdh"
	"crypto/mlkem"
	"crypto/rand"
	"crypto/sha3"
	"encoding/pem"
	"errors"

	"cloud.google.com/go/kms/apiv1/kmspb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	slhdsa "github.com/cloudburrow/cloudburrow/third_party/circl-slhdsa"
)

// The post-quantum algorithms (#1946), as the algorithms page
// (https://docs.cloud.google.com/kms/docs/algorithms, read 2026-11-20) and
// resources.proto document them, all at SOFTWARE only:
//
//   - PQ_SIGN_ML_DSA_44/65/87: "pure randomized" ML-DSA (FIPS 204) over
//     data, with an empty context, from Go's crypto/mldsa (Go 1.27;
//     mldsa_go127.go). The _EXTERNAL_MU variants sign the 64-byte μ the
//     client sends in digest.external_mu (RFC 9881's external μ).
//   - PQ_SIGN_SLH_DSA_SHA2_128S: randomized pure SLH-DSA-SHA2-128s (FIPS
//     205) over data; PQ_SIGN_HASH_SLH_DSA_SHA2_128S_SHA256 its pre-hash
//     variant over a SHA-256 digest (HashSLH-DSA, FIPS 205 Algorithm 23).
//     Go has no SLH-DSA, nor has any Google-authored library CloudBurrow may
//     link (Tink's is internal to Tink, and the binary links no Tink:
//     TestTheBinaryDoesNotLinkTink), so it is Cloudflare's CIRCL
//     (BSD-3-Clause), vendored in third_party/circl-slhdsa with one added
//     constructor for a digest the client hashed.
//   - KEM_XWING: X-Wing (draft-connolly-cfrg-xwing-kem-06), ML-KEM-768 and
//     X25519 combined with SHA3-256, from crypto/mlkem, crypto/ecdh and
//     crypto/sha3. The material is the 32-byte X-Wing decapsulation key.
//
// GetPublicKey needs public_key_format for every PQ algorithm
// (resources.proto PublicKeyFormat): NIST_PQC (the FIPS 203/204/205
// encoding) for ML-KEM, ML-DSA and SLH-DSA, XWING_RAW_BYTES for X-Wing, and
// PEM or DER for ML-DSA only ("The PEM and DER formats are only supported
// for ML-DSA keys", retrieve-public-key, read 2026-11-20).

// errMLDSAUnavailable is why a Go 1.26 build refuses ML-DSA (mldsa_go126.go).
var errMLDSAUnavailable = errors.New("ML-DSA needs a CloudBurrow built with Go 1.27, whose crypto/mldsa computes it")

// pqSign reports whether a is a post-quantum signing algorithm.
func (s algSpec) pqSign() bool { return s.mldsa > 0 || s.slhdsa }

// pq reports whether a is a post-quantum algorithm, signing or KEM.
func (s algSpec) pq() bool { return s.pqSign() || s.kem > 0 || s.xwing }

const (
	xwingSeedSize       = 32
	xwingPublicKeySize  = mlkem.EncapsulationKeySize768 + 32
	xwingCiphertextSize = mlkem.CiphertextSize768 + 32
	xwingLabel          = `\.//^\`
	externalMuSize      = 64
)

// generatePQ makes new material for a PQ signing or X-Wing algorithm: the
// 32-byte ML-DSA or X-Wing seed, or the SLH-DSA private key (FIPS 205
// encoding, 64 bytes for SHA2-128s).
func generatePQ(s algSpec) ([]byte, error) {
	if s.slhdsa {
		_, priv, err := slhdsa.GenerateKey(rand.Reader, slhdsa.SHA2_128s)
		if err != nil {
			return nil, err
		}
		return priv.MarshalBinary()
	}
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	return seed, nil
}

func (v keyVersion) slhKey() (*slhdsa.PrivateKey, error) {
	k := slhdsa.PrivateKey{ID: slhdsa.SHA2_128s}
	if err := k.UnmarshalBinary(v.Material); err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "stored key of %s", v.Name))
	}
	return &k, nil
}

// pqPublicKey answers GetPublicKey for a PQ signing or X-Wing version.
func (v keyVersion) pqPublicKey(f kmspb.PublicKey_PublicKeyFormat) (*kmspb.PublicKey, error) {
	spec := algorithms[v.algorithm()]
	a := v.algorithm()
	if f == kmspb.PublicKey_PUBLIC_KEY_FORMAT_UNSPECIFIED {
		return nil, apierror.Wrap(apierror.InvalidArgument("public_key_format is required for the post-quantum algorithm %s", a))
	}
	out := &kmspb.PublicKey{Algorithm: a, Name: v.Name, ProtectionLevel: v.protectionLevel(), PublicKeyFormat: f}
	var data []byte
	switch {
	case spec.xwing:
		if f != kmspb.PublicKey_XWING_RAW_BYTES {
			return nil, apierror.Wrap(apierror.InvalidArgument("public_key_format must be XWING_RAW_BYTES for %s, not %s", a, f))
		}
		pk, err := xwingPublic(v.Material)
		if err != nil {
			return nil, apierror.Wrap(apierror.Internal(err, "stored key of %s", v.Name))
		}
		data = pk
	case spec.slhdsa:
		if f != kmspb.PublicKey_NIST_PQC {
			return nil, apierror.Wrap(apierror.InvalidArgument("public_key_format must be NIST_PQC for %s, not %s", a, f))
		}
		k, err := v.slhKey()
		if err != nil {
			return nil, err
		}
		pub := k.PublicKey()
		b, err := pub.MarshalBinary()
		if err != nil {
			return nil, apierror.Wrap(apierror.Internal(err, "encode the public key of %s", v.Name))
		}
		data = b
	case spec.mldsa > 0:
		raw, der, err := mldsaPublic(spec.mldsa, v.Material)
		if err != nil {
			return nil, apierror.Wrap(apierror.Internal(err, "stored key of %s", v.Name))
		}
		switch f {
		case kmspb.PublicKey_NIST_PQC:
			data = raw
		case kmspb.PublicKey_DER:
			data = der
		case kmspb.PublicKey_PEM:
			p := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
			out.Pem, out.PemCrc32C = p, crc([]byte(p))
			data = []byte(p)
		default:
			return nil, apierror.Wrap(apierror.InvalidArgument("public_key_format %s is not valid for %s", f, a))
		}
	}
	out.PublicKey = &kmspb.ChecksummedData{Data: data, Crc32CChecksum: crc(data)}
	return out, nil
}

// signPQ answers AsymmetricSign for a PQ version. The pure algorithms sign
// data and refuse a digest; the external-μ ones take digest.external_mu
// and the pre-hash SLH-DSA a SHA-256 digest ("The pure algorithms take raw
// data as input, while the pre-hash and external-μ (external-mu)
// algorithms accept a hash value derived from the data", the algorithms
// page). The pre-hash variant also takes data, which it hashes, as the
// classical digest algorithms do here.
func (v keyVersion) signPQ(data []byte, d *kmspb.Digest) ([]byte, error) {
	spec := algorithms[v.algorithm()]
	a := v.algorithm()
	hasDigest := d != nil
	switch {
	case spec.externalMu:
		mu := d.GetExternalMu()
		if !hasDigest || len(data) > 0 || mu == nil {
			return nil, apierror.Wrap(apierror.InvalidArgument("%s signs an externally computed μ: supply digest.external_mu, not data", a))
		}
		if len(mu) != externalMuSize {
			return nil, apierror.Wrap(apierror.InvalidArgument("digest.external_mu is %d bytes: %s needs %d", len(mu), a, externalMuSize))
		}
		return mldsaSign(spec.mldsa, v.Material, mu, true)
	case spec.slhdsa && spec.hash != 0:
		var digest []byte
		if hasDigest {
			h, b := digestBytes(d)
			if h != crypto.SHA256 {
				return nil, apierror.Wrap(apierror.InvalidArgument("digest must be SHA256 for %s", a))
			}
			if len(b) != crypto.SHA256.Size() {
				return nil, apierror.Wrap(apierror.InvalidArgument("digest is %d bytes: SHA256 needs %d", len(b), crypto.SHA256.Size()))
			}
			digest = b
		} else {
			if len(data) == 0 {
				return nil, apierror.Wrap(apierror.InvalidArgument("digest or data is required"))
			}
			h := crypto.SHA256.New()
			h.Write(data)
			digest = h.Sum(nil)
		}
		k, err := v.slhKey()
		if err != nil {
			return nil, err
		}
		msg, err := slhdsa.NewPreHashedDigest(crypto.SHA256, digest)
		if err != nil {
			return nil, apierror.Wrap(apierror.Internal(err, "pre-hash for %s", v.Name))
		}
		return slhSign(k, msg)
	}
	if hasDigest {
		return nil, apierror.Wrap(apierror.InvalidArgument("%s signs data as given: supply data, not digest", a))
	}
	if len(data) == 0 {
		return nil, apierror.Wrap(apierror.InvalidArgument("data is required for %s", a))
	}
	if spec.slhdsa {
		k, err := v.slhKey()
		if err != nil {
			return nil, err
		}
		return slhSign(k, slhdsa.NewMessage(data))
	}
	return mldsaSign(spec.mldsa, v.Material, data, false)
}

func slhSign(k *slhdsa.PrivateKey, msg *slhdsa.Message) ([]byte, error) {
	sig, err := slhdsa.SignRandomized(k, rand.Reader, msg, nil)
	if err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "sign with SLH-DSA"))
	}
	return sig, nil
}

// xwingExpand derives the ML-KEM-768 seed and the X25519 private key from
// an X-Wing decapsulation key (draft-connolly-cfrg-xwing-kem-06,
// expandDecapsulationKey).
func xwingExpand(seed []byte) (*mlkem.DecapsulationKey768, *ecdh.PrivateKey, error) {
	if len(seed) != xwingSeedSize {
		return nil, nil, apierror.Internal(nil, "an X-Wing key is %d bytes, not %d", xwingSeedSize, len(seed))
	}
	expanded := sha3.SumSHAKE256(seed, 96)
	dk, err := mlkem.NewDecapsulationKey768(expanded[:64])
	if err != nil {
		return nil, nil, err
	}
	sk, err := ecdh.X25519().NewPrivateKey(expanded[64:96])
	if err != nil {
		return nil, nil, err
	}
	return dk, sk, nil
}

// xwingPublic is the X-Wing encapsulation key, pk_M || pk_X (1216 bytes).
func xwingPublic(seed []byte) ([]byte, error) {
	dk, sk, err := xwingExpand(seed)
	if err != nil {
		return nil, err
	}
	return append(dk.EncapsulationKey().Bytes(), sk.PublicKey().Bytes()...), nil
}

// xwingKey decapsulates X-Wing ciphertexts, ct_M || ct_X (1120 bytes).
type xwingKey struct{ seed []byte }

func (k xwingKey) Decapsulate(ct []byte) ([]byte, error) {
	if len(ct) != xwingCiphertextSize {
		return nil, apierror.InvalidArgument("an X-Wing ciphertext is %d bytes, not %d", xwingCiphertextSize, len(ct))
	}
	dk, sk, err := xwingExpand(k.seed)
	if err != nil {
		return nil, err
	}
	ssM, err := dk.Decapsulate(ct[:mlkem.CiphertextSize768])
	if err != nil {
		return nil, err
	}
	ctX := ct[mlkem.CiphertextSize768:]
	peer, err := ecdh.X25519().NewPublicKey(ctX)
	if err != nil {
		return nil, err
	}
	ssX, err := sk.ECDH(peer)
	if err != nil {
		// An all-zero X25519 output: reject, as Go's ECDH does.
		return nil, err
	}
	h := sha3.New256()
	h.Write(ssM)
	h.Write(ssX)
	h.Write(ctX)
	h.Write(sk.PublicKey().Bytes())
	h.Write([]byte(xwingLabel))
	return h.Sum(nil), nil
}
