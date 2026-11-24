package filestoreserver

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"io"
)

// A backup with a customer-managed key (#2192) is kept encrypted: its tar
// stream is cut into chunks of chunkSize bytes, each sealed with the
// backup's 256-bit data key (AES-256-GCM), the nonce the chunk's number
// and whether it is the last, so chunks cannot be reordered, dropped or
// cut off. The data key itself is kept only as Cloud KMS wrapped it.

const chunkSize = 1 << 16

// sealMagic starts an encrypted backup.
var sealMagic = []byte("CBFSENC1")

func nonce(n uint64, last bool) []byte {
	b := make([]byte, 12)
	binary.BigEndian.PutUint64(b[:8], n)
	if last {
		b[8] = 1
	}
	return b
}

func gcmOf(dek []byte) (cipher.AEAD, error) {
	if len(dek) != 32 {
		return nil, errors.New("the data key is not 256 bits")
	}
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// sealingWriter encrypts what is written to it onto w; Close seals the
// last chunk.
type sealingWriter struct {
	w    io.Writer
	aead cipher.AEAD
	buf  []byte
	n    uint64
}

func newSealingWriter(w io.Writer, dek []byte) (*sealingWriter, error) {
	aead, err := gcmOf(dek)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(sealMagic); err != nil {
		return nil, err
	}
	return &sealingWriter{w: w, aead: aead}, nil
}

func (s *sealingWriter) flush(last bool) error {
	out := s.aead.Seal(nil, nonce(s.n, last), s.buf, nil)
	s.n++
	s.buf = s.buf[:0]
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(out)))
	if _, err := s.w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := s.w.Write(out)
	return err
}

func (s *sealingWriter) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		k := min(chunkSize-len(s.buf), len(p))
		s.buf = append(s.buf, p[:k]...)
		p = p[k:]
		if len(s.buf) == chunkSize {
			if err := s.flush(false); err != nil {
				return 0, err
			}
		}
	}
	return total, nil
}

func (s *sealingWriter) Close() error { return s.flush(true) }

// openingReader decrypts an encrypted backup.
type openingReader struct {
	r    io.Reader
	aead cipher.AEAD
	buf  []byte
	n    uint64
	done bool
}

func newOpeningReader(r io.Reader, dek []byte) (*openingReader, error) {
	aead, err := gcmOf(dek)
	if err != nil {
		return nil, err
	}
	magic := make([]byte, len(sealMagic))
	if _, err := io.ReadFull(r, magic); err != nil || string(magic) != string(sealMagic) {
		return nil, errors.New("the backup is not encrypted")
	}
	return &openingReader{r: r, aead: aead}, nil
}

func (o *openingReader) Read(p []byte) (int, error) {
	for len(o.buf) == 0 {
		if o.done {
			return 0, io.EOF
		}
		var hdr [4]byte
		if _, err := io.ReadFull(o.r, hdr[:]); err != nil {
			return 0, errors.New("the encrypted backup is cut short")
		}
		size := binary.BigEndian.Uint32(hdr[:])
		if size > chunkSize+uint32(o.aead.Overhead()) {
			return 0, errors.New("the encrypted backup is damaged")
		}
		ct := make([]byte, size)
		if _, err := io.ReadFull(o.r, ct); err != nil {
			return 0, errors.New("the encrypted backup is cut short")
		}
		// A chunk is the last one when it opens as the last one.
		pt, err := o.aead.Open(nil, nonce(o.n, false), ct, nil)
		if err != nil {
			if pt, err = o.aead.Open(nil, nonce(o.n, true), ct, nil); err != nil {
				return 0, errors.New("the encrypted backup does not open with its key")
			}
			o.done = true
		}
		o.n++
		o.buf = pt
	}
	k := copy(p, o.buf)
	o.buf = o.buf[k:]
	return k, nil
}
