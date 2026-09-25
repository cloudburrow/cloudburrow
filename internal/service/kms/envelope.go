package kms

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
)

// Symmetric ciphertext is CloudBurrow's own format, since Google does not
// publish Cloud KMS's (service.proto DecryptRequest.ciphertext):
//
//	format (1 byte, 0x01) || version number (uint32, big-endian) || nonce (12) || AES-256-GCM ciphertext and tag
//
// The version number lets Decrypt find the version without trying keys. The
// 5-byte header is authenticated with the caller's AAD, so rewriting it makes
// the ciphertext fail to open. Crypto is the Go standard library only (#410).

const (
	envelopeFormat = 0x01
	headerLen      = 1 + 4
	nonceLen       = 12
	tagLen         = 16
)

// errOpen is every failure to open a ciphertext. It carries no input: not the
// ciphertext, the AAD, the plaintext or the key.
var errOpen = errors.New("ciphertext is invalid, was produced by another key, or its additional authenticated data does not match")

func header(version uint32) []byte {
	h := make([]byte, headerLen)
	h[0] = envelopeFormat
	binary.BigEndian.PutUint32(h[1:], version)
	return h
}

func gcm(material []byte) (cipher.AEAD, error) {
	if len(material) != 32 {
		return nil, fmt.Errorf("key material is %d bytes, want 32", len(material))
	}
	block, err := aes.NewCipher(material)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// authData is what GCM authenticates: the fixed-length header then the
// caller's AAD. The header's fixed length keeps the two unambiguous.
func authData(h, aad []byte) []byte {
	return append(append(make([]byte, 0, len(h)+len(aad)), h...), aad...)
}

// seal encrypts plaintext under a version's material, with a fresh random
// nonce every time.
func seal(material []byte, version uint32, plaintext, aad []byte) ([]byte, error) {
	aead, err := gcm(material)
	if err != nil {
		return nil, err
	}
	h := header(version)
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	out := append(append(make([]byte, 0, headerLen+nonceLen+len(plaintext)+tagLen), h...), nonce...)
	return aead.Seal(out, nonce, plaintext, authData(h, aad)), nil
}

// parseVersion returns the version a ciphertext names, without any key.
func parseVersion(ciphertext []byte) (uint32, error) {
	if len(ciphertext) < headerLen+nonceLen+tagLen || ciphertext[0] != envelopeFormat {
		return 0, errOpen
	}
	v := binary.BigEndian.Uint32(ciphertext[1:headerLen])
	if v == 0 {
		return 0, errOpen
	}
	return v, nil
}

// open decrypts a ciphertext sealed for version under material. Any failure
// is errOpen.
func open(material []byte, version uint32, ciphertext, aad []byte) ([]byte, error) {
	got, err := parseVersion(ciphertext)
	if err != nil || got != version {
		return nil, errOpen
	}
	aead, err := gcm(material)
	if err != nil {
		return nil, errOpen
	}
	h := ciphertext[:headerLen]
	nonce := ciphertext[headerLen : headerLen+nonceLen]
	pt, err := aead.Open(nil, nonce, ciphertext[headerLen+nonceLen:], authData(h, aad))
	if err != nil {
		return nil, errOpen
	}
	return pt, nil
}
