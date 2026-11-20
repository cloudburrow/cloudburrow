//go:build !go1.27

package kms

import "github.com/cloudburrow/cloudburrow/internal/apierror"

// Go 1.26 has no crypto/mldsa: a CloudBurrow built with it refuses the
// ML-DSA algorithms at create (keyAlgorithm), UNIMPLEMENTED. Releases are
// built with Go 1.27 (mldsa_go127.go).

const mldsaAvailable = false

func mldsaPublic(int, []byte) (raw, der []byte, err error) { return nil, nil, errMLDSAUnavailable }

func mldsaVerify(int, []byte, []byte, []byte) (bool, error) { return false, errMLDSAUnavailable }

func mldsaSign(int, []byte, []byte, bool) ([]byte, error) {
	return nil, apierror.Wrap(apierror.Unimplemented("%v", errMLDSAUnavailable))
}
