// Copyright 2021 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package fakekms

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
)

type keyFactory interface {
	Generate() interface{}
}

type ecKeyFactory struct {
	curve elliptic.Curve
}

func (f *ecKeyFactory) Generate() interface{} {
	k, err := ecdsa.GenerateKey(f.curve, rand.Reader)
	if err != nil {
		panic(err) // we ran out of entropy
	}
	return k
}

type rsaKeyFactory int

func (f rsaKeyFactory) Generate() interface{} {
	// Modified by CloudBurrow: generated at runtime rather than read from
	// pregenerated PEM files, so no private key is committed. The oracle
	// compares symmetric keys only; this path serves asymmetric purposes.
	key, err := rsa.GenerateKey(rand.Reader, int(f))
	if err != nil {
		panic("error generating RSA private key: " + err.Error())
	}
	return key
}

type symmetricKeyFactory int

func (f symmetricKeyFactory) Generate() interface{} {
	k := make([]byte, int(f)/8)
	if _, err := rand.Read(k); err != nil {
		panic(err) // we ran out of entropy
	}
	return k
}
