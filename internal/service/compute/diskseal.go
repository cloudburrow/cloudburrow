package compute

// A disk with a customer-managed (Cloud KMS) or customer-supplied key is
// encrypted at rest (#2206): while no running instance has it open, its
// storage holds only SealedFile, its contents (a tar stream of the
// volume's files) sealed with the disk's 256-bit data key, and nothing in
// the clear. The data key itself is kept only wrapped: by the Cloud KMS key
// version (Encrypt) for a CMEK disk, by the customer's own key (AES-256-GCM)
// for a CSEK disk, so with the key version disabled or destroyed, or
// without the customer's key, the contents can't be read.
//
// The format, after the 8-byte magic "CBSEAL1\n" and an 8-byte random
// nonce prefix, is a sequence of chunks: a 4-byte big-endian length, then
// that many bytes of AES-256-GCM ciphertext of at most sealChunk bytes of
// plaintext, under the nonce prefix||chunk index, with the additional data
// one byte, 1 for the last chunk and 0 otherwise, so a truncated or
// reordered file fails to open. Only Go's standard library is used.

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// SealedFile is the name, in the root of a disk's storage, of its sealed
// contents.
const SealedFile = ".cloudburrow-sealed"

const (
	sealMagic = "CBSEAL1\n"
	sealChunk = 64 << 10
)

// ErrSealed means a sealed disk's contents could not be opened with the
// key given: the wrong key, or a damaged file.
var ErrSealed = errors.New("the disk's sealed contents can't be opened with its data key")

func sealAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("a disk data key is 32 bytes, not %d", len(key))
	}
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

func sealNonce(prefix []byte, i uint32) []byte {
	n := make([]byte, 12)
	copy(n, prefix)
	binary.BigEndian.PutUint32(n[8:], i)
	return n
}

// Seal writes r's bytes to w sealed with key.
func Seal(w io.Writer, r io.Reader, key []byte) error {
	aead, err := sealAEAD(key)
	if err != nil {
		return err
	}
	prefix := make([]byte, 8)
	if _, err := rand.Read(prefix); err != nil {
		return err
	}
	bw := bufio.NewWriter(w)
	if _, err := bw.WriteString(sealMagic); err != nil {
		return err
	}
	if _, err := bw.Write(prefix); err != nil {
		return err
	}
	cur := make([]byte, sealChunk)
	next := make([]byte, sealChunk)
	n, err := io.ReadFull(r, cur)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return err
	}
	for i := uint32(0); ; i++ {
		if i == ^uint32(0) {
			return errors.New("the disk is too large to seal")
		}
		var m int
		last := n < sealChunk
		if !last {
			m, err = io.ReadFull(r, next)
			if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
				return err
			}
			last = m == 0
		}
		ad := []byte{0}
		if last {
			ad[0] = 1
		}
		ct := aead.Seal(nil, sealNonce(prefix, i), cur[:n], ad)
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(ct)))
		if _, err := bw.Write(l[:]); err != nil {
			return err
		}
		if _, err := bw.Write(ct); err != nil {
			return err
		}
		if last {
			return bw.Flush()
		}
		cur, next, n = next, cur, m
	}
}

// Open writes to w the bytes r holds sealed with key; ErrSealed when key
// is not the one that sealed them, or the file is damaged or truncated.
func Open(w io.Writer, r io.Reader, key []byte) error {
	aead, err := sealAEAD(key)
	if err != nil {
		return err
	}
	br := bufio.NewReader(r)
	head := make([]byte, len(sealMagic)+8)
	if _, err := io.ReadFull(br, head); err != nil || string(head[:len(sealMagic)]) != sealMagic {
		return ErrSealed
	}
	prefix := head[len(sealMagic):]
	buf := make([]byte, sealChunk+aead.Overhead())
	for i := uint32(0); ; i++ {
		var l [4]byte
		if _, err := io.ReadFull(br, l[:]); err != nil {
			return ErrSealed // truncated: the last chunk never came
		}
		n := binary.BigEndian.Uint32(l[:])
		if int(n) > len(buf) {
			return ErrSealed
		}
		if _, err := io.ReadFull(br, buf[:n]); err != nil {
			return ErrSealed
		}
		last := false
		pt, err := aead.Open(nil, sealNonce(prefix, i), buf[:n], []byte{0})
		if err != nil {
			pt, err = aead.Open(nil, sealNonce(prefix, i), buf[:n], []byte{1})
			if err != nil {
				return ErrSealed
			}
			last = true
		}
		if _, err := w.Write(pt); err != nil {
			return err
		}
		if last {
			if _, err := br.ReadByte(); err != io.EOF {
				return ErrSealed
			}
			return nil
		}
	}
}
