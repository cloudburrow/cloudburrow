// Modified by CloudBurrow: NewPreHashedDigest added, SHAKE pre-hashing
// removed (PROVENANCE.md).

package slhdsa

import (
	"crypto"
	"hash"
	"io"

	_ "golang.org/x/crypto/sha3"
)

// [PreHash] is a helper for hashing a message before signing.
// It implements the [io.Writer] interface, so the message can be provided
// in chunks before calling the [SignDeterministic], [SignRandomized], or
// [Verify] functions.
// Pre-hash must not be used for generating pure signatures.
type PreHash struct {
	writer interface {
		io.Writer
		Reset()
	}
	size int
	oid  byte
}

// [NewPreHashWithHash] is used to prehash messages using either the SHA2 or
// SHA3 hash functions.
// Returns [ErrPreHash] if the function is not supported.
func NewPreHashWithHash(h crypto.Hash) (*PreHash, error) {
	hash2oid := [...]byte{
		crypto.SHA256:     1,
		crypto.SHA384:     2,
		crypto.SHA512:     3,
		crypto.SHA224:     4,
		crypto.SHA512_224: 5,
		crypto.SHA512_256: 6,
		crypto.SHA3_224:   7,
		crypto.SHA3_256:   8,
		crypto.SHA3_384:   9,
		crypto.SHA3_512:   10,
	}

	if int(h) >= len(hash2oid) {
		return nil, ErrPreHash
	}

	oid := hash2oid[h]
	if oid == 0 {
		return nil, ErrPreHash
	}

	return &PreHash{h.New(), h.Size(), oid}, nil
}

func (ph *PreHash) Reset()                      { ph.writer.Reset() }
func (ph *PreHash) Write(b []byte) (int, error) { return ph.writer.Write(b) }

// BuildMessage returns a [Message] for signing, and resets the writer.
func (ph *PreHash) BuildMessage() (*Message, error) {
	// Source https://csrc.nist.gov/Projects/computer-security-objects-register/algorithm-registration
	const oidLen = 11
	oid := [oidLen]byte{
		0x06, 0x09, 0x60, 0x86, 0x48, 0x01, 0x65, 0x03, 0x04, 0x02, ph.oid,
	}

	msg := make([]byte, oidLen+ph.size)
	copy(msg, oid[:])
	switch f := ph.writer.(type) {
	case hash.Hash:
		msg = f.Sum(msg[:oidLen])
	default:
		return nil, ErrPreHash
	}

	ph.Reset()
	return &Message{msg, 1}, nil
}

// NewPreHashedDigest returns the [Message] that HashSLH-DSA signs for a
// digest the caller already computed with h (FIPS 205, Section 10.2.2,
// Algorithm 23: OID || PH_M), as [PreHash.BuildMessage] does for data it
// hashes itself. It returns [ErrPreHash] if h is not supported or digest is
// not h's size.
//
// Added by CloudBurrow: Cloud KMS's PQ_SIGN_HASH_SLH_DSA_SHA2_128S_SHA256
// signs a SHA-256 digest the client sends (third_party/circl-slhdsa/PROVENANCE.md).
func NewPreHashedDigest(h crypto.Hash, digest []byte) (*Message, error) {
	ph, err := NewPreHashWithHash(h)
	if err != nil {
		return nil, err
	}
	if len(digest) != ph.size {
		return nil, ErrPreHash
	}
	const oidLen = 11
	msg := append([]byte{0x06, 0x09, 0x60, 0x86, 0x48, 0x01, 0x65, 0x03, 0x04, 0x02, ph.oid}, digest...)
	return &Message{msg[:oidLen+ph.size], 1}, nil
}

// [Message] wraps a message for signing.
type Message struct {
	msg       []byte
	isPreHash byte
}

// For pure signatures, use [NewMessage] to pass the message to be signed.
// For pre-hashed signatures, use [PreHash] to hash the message first, and
// then use [PreHash.BuildMessage] to get a [Message] to be signed.
func NewMessage(msg []byte) *Message { return &Message{msg, 0} }

func (m *Message) getMsgPrime(context []byte) ([]byte, error) {
	// See FIPS 205 -- Section 10.2 -- Algorithm 23 and Algorithm 25.
	const MaxContextSize = 255
	if len(context) > MaxContextSize {
		return nil, ErrContext
	}

	return append(append(
		[]byte{m.isPreHash, byte(len(context))}, context...), m.msg...,
	), nil
}
