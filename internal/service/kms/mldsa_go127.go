//go:build go1.27

package kms

import (
	"crypto"
	"crypto/mldsa"
	"crypto/x509"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// ML-DSA is Go 1.27's crypto/mldsa (#1946); a CloudBurrow built with Go
// 1.26, which has no crypto/mldsa, refuses the ML-DSA algorithms
// (mldsa_go126.go). Releases and CI's instances are built with Go 1.27.

const mldsaAvailable = true

func mldsaParams(level int) mldsa.Parameters {
	switch level {
	case 44:
		return mldsa.MLDSA44()
	case 65:
		return mldsa.MLDSA65()
	}
	return mldsa.MLDSA87()
}

// mldsaPublic is the FIPS 204 public key of the seed, raw and as a
// SubjectPublicKeyInfo (RFC 9881).
func mldsaPublic(level int, seed []byte) (raw, der []byte, err error) {
	sk, err := mldsa.NewPrivateKey(mldsaParams(level), seed)
	if err != nil {
		return nil, nil, err
	}
	der, err = x509.MarshalPKIXPublicKey(sk.PublicKey())
	if err != nil {
		return nil, nil, err
	}
	return sk.PublicKey().Bytes(), der, nil
}

// mldsaVerify checks a pure ML-DSA signature against the FIPS 204 key.
func mldsaVerify(level int, pub, data, sig []byte) (bool, error) {
	k, err := mldsa.NewPublicKey(mldsaParams(level), pub)
	if err != nil {
		return false, err
	}
	return mldsa.Verify(k, data, sig, nil) == nil, nil
}

// mldsaSign is hedged (randomized) ML-DSA with an empty context, of msg or,
// when mu, of the external μ msg.
func mldsaSign(level int, seed, msg []byte, mu bool) ([]byte, error) {
	sk, err := mldsa.NewPrivateKey(mldsaParams(level), seed)
	if err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "stored ML-DSA key"))
	}
	var opts crypto.SignerOpts
	if mu {
		opts = crypto.MLDSAMu
	}
	sig, err := sk.Sign(nil, msg, opts)
	if err != nil {
		return nil, apierror.Wrap(apierror.Internal(err, "sign with ML-DSA-%d", level))
	}
	return sig, nil
}
