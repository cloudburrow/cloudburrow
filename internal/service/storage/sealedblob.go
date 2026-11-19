package storage

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

// The at-rest format of an object encrypted with a customer-managed key
// (cmek.go, #1938): a 12-byte header, "CBS1" and a random 8-byte nonce
// prefix, then the plaintext in segments of sealSegment bytes (the last
// shorter, or empty for an empty object), each sealed with AES-256-GCM
// under the object's data key, its nonce the prefix and the segment's
// big-endian index, its additional data one byte saying whether it is the
// last. Segments of a fixed size make a range read decrypt only the
// segments it covers; the last-segment flag makes a truncated blob fail
// rather than read short.

const (
	sealMagic   = "CBS1"
	sealHeader  = 12
	sealSegment = 64 << 10
	sealTag     = 16
)

var errSealed = errors.New("the object's encrypted bytes do not open with its data key")

func sealAEAD(dek []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func sealNonce(prefix []byte, i uint32) []byte {
	n := make([]byte, 12)
	copy(n, prefix)
	binary.BigEndian.PutUint32(n[8:], i)
	return n
}

func lastAAD(last bool) []byte {
	if last {
		return []byte{1}
	}
	return []byte{0}
}

// sealReader reads plaintext from src and yields the sealed blob.
type sealReader struct {
	src    *bufio.Reader
	aead   cipher.AEAD
	prefix []byte
	index  uint32
	out    []byte
	buf    []byte
	done   bool
}

func newSealReader(src io.Reader, dek []byte) (io.Reader, error) {
	aead, err := sealAEAD(dek)
	if err != nil {
		return nil, err
	}
	prefix := make([]byte, 8)
	if _, err := rand.Read(prefix); err != nil {
		return nil, err
	}
	r := &sealReader{src: bufio.NewReaderSize(src, sealSegment+1), aead: aead, prefix: prefix, buf: make([]byte, sealSegment)}
	r.out = append([]byte(sealMagic), prefix...)
	return r, nil
}

func (r *sealReader) Read(p []byte) (int, error) {
	for len(r.out) == 0 {
		if r.done {
			return 0, io.EOF
		}
		n, err := io.ReadFull(r.src, r.buf)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return 0, err
		}
		last := n < sealSegment
		if !last {
			if _, perr := r.src.Peek(1); perr == io.EOF {
				last = true
			} else if perr != nil {
				return 0, perr
			}
		}
		r.out = r.aead.Seal(r.out[:0], sealNonce(r.prefix, r.index), r.buf[:n], lastAAD(last))
		r.index++
		r.done = last
	}
	n := copy(p, r.out)
	r.out = r.out[n:]
	return n, nil
}

// sealedReader is the plaintext of a sealed blob of size plaintext bytes.
type sealedReader struct {
	f      io.ReadSeekCloser
	aead   cipher.AEAD
	prefix []byte
	size   int64
	pos    int64
	seg    int64 // index of the segment in plain, -1 for none
	plain  []byte
	ct     []byte
}

func newSealedReader(f io.ReadSeekCloser, dek []byte, size int64) (*sealedReader, error) {
	aead, err := sealAEAD(dek)
	if err != nil {
		return nil, err
	}
	head := make([]byte, sealHeader)
	if _, err := io.ReadFull(f, head); err != nil || string(head[:4]) != sealMagic {
		return nil, errSealed
	}
	return &sealedReader{f: f, aead: aead, prefix: head[4:], size: size, seg: -1, ct: make([]byte, sealSegment+sealTag)}, nil
}

func (r *sealedReader) segments() int64 {
	if r.size == 0 {
		return 1
	}
	return (r.size + sealSegment - 1) / sealSegment
}

func (r *sealedReader) load(i int64) error {
	if r.seg == i {
		return nil
	}
	n := int64(sealSegment)
	if last := r.segments() - 1; i == last {
		n = r.size - last*sealSegment
	}
	if _, err := r.f.Seek(sealHeader+i*(sealSegment+sealTag), io.SeekStart); err != nil {
		return err
	}
	ct := r.ct[:n+sealTag]
	if _, err := io.ReadFull(r.f, ct); err != nil {
		return errSealed
	}
	pt, err := r.aead.Open(r.plain[:0], sealNonce(r.prefix, uint32(i)), ct, lastAAD(i == r.segments()-1))
	if err != nil {
		return errSealed
	}
	r.plain, r.seg = pt, i
	return nil
}

func (r *sealedReader) Read(p []byte) (int, error) {
	if r.pos >= r.size {
		if r.size == 0 && r.seg < 0 {
			// An empty object still checks its one segment opens.
			if err := r.load(0); err != nil {
				return 0, err
			}
		}
		return 0, io.EOF
	}
	i := r.pos / sealSegment
	if err := r.load(i); err != nil {
		return 0, err
	}
	n := copy(p, r.plain[r.pos-i*sealSegment:])
	r.pos += int64(n)
	return n, nil
}

func (r *sealedReader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += r.pos
	case io.SeekEnd:
		offset += r.size
	default:
		return 0, fmt.Errorf("seek: whence %d", whence)
	}
	if offset < 0 {
		return 0, fmt.Errorf("seek: negative position %d", offset)
	}
	r.pos = offset
	return offset, nil
}

func (r *sealedReader) Close() error { return r.f.Close() }
