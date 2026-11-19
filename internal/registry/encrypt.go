package registry

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Customer-managed encryption (#1897). A repository whose Encryption
// returns a Cipher keeps its own copy of every blob and manifest it holds,
// sealed, under its directory:
//
//	repos/<location>/<project>/<repository>/_blobs/<hex>       sealed content
//	repos/<location>/<project>/<repository>/_blobs/<hex>.size  its plaintext size
//
// and never reads or writes the shared, plaintext blobs/ for them. Every
// read and write asks Encryption first, so a key that can no longer be used
// fails pushes and pulls with ErrKeyUnavailable. Metadata stays readable,
// as Google's does: a manifest's version is still listed, with its size.

// SealedSize is the plaintext size of a sealed blob of r, read without
// its key.
func (s *Store) SealedSize(r Repo, digest string) (int64, bool) {
	b, err := os.ReadFile(s.sealedPath(r, digest) + ".size")
	if err != nil {
		return 0, false
	}
	var n int64
	if _, err := fmt.Sscan(string(b), &n); err != nil {
		return 0, false
	}
	return n, true
}

// Cipher seals and opens one repository's content.
type Cipher interface {
	Seal(plain []byte) ([]byte, error)
	Open(sealed []byte) ([]byte, error)
}

// ErrKeyUnavailable is a repository whose encryption key cannot be used.
var ErrKeyUnavailable = errors.New("the repository's encryption key cannot be used")

// KeyError names why a repository's key cannot be used.
type KeyError struct{ Err error }

func (e *KeyError) Error() string { return ErrKeyUnavailable.Error() + ": " + e.Err.Error() }

// Unwrap makes errors.Is(err, ErrKeyUnavailable) hold.
func (e *KeyError) Unwrap() error { return ErrKeyUnavailable }

// cipherOf is r's Cipher, nil for plaintext. A Repo{} is never encrypted.
func (s *Store) cipherOf(r Repo) (Cipher, error) {
	if s.Encryption == nil || r == (Repo{}) {
		return nil, nil
	}
	c, err := s.Encryption(r)
	if err != nil {
		return nil, &KeyError{err}
	}
	return c, nil
}

func (s *Store) sealedPath(r Repo, digest string) string {
	return filepath.Join(r.dir(s.root), "_blobs", strings.TrimPrefix(digest, "sha256:"))
}

// readSealed opens a sealed blob.
func (s *Store) readSealed(c Cipher, r Repo, digest string) ([]byte, error) {
	if !ValidDigest(digest) {
		return nil, ErrBlobUnknown
	}
	b, err := os.ReadFile(s.sealedPath(r, digest))
	if err != nil {
		return nil, ErrBlobUnknown
	}
	plain, err := c.Open(b)
	if err != nil {
		return nil, &KeyError{err}
	}
	return plain, nil
}

// writeSealed seals b as r's blob digest.
func (s *Store) writeSealed(c Cipher, r Repo, digest string, b []byte) error {
	sealed, err := c.Seal(b)
	if err != nil {
		return &KeyError{err}
	}
	p := s.sealedPath(r, digest)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".put-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(sealed); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	tmp.Close()
	if err := os.WriteFile(p+".size", []byte(fmt.Sprint(len(b))), 0o644); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// RepoBlobSize is BlobSize for one repository's content.
func (s *Store) RepoBlobSize(r Repo, digest string) (int64, error) {
	c, err := s.cipherOf(r)
	if err != nil {
		return 0, err
	}
	if c == nil {
		return s.BlobSize(digest)
	}
	b, err := s.readSealed(c, r, digest)
	if err != nil {
		return 0, err
	}
	return int64(len(b)), nil
}

// ReadSeekCloser is a blob opened for reading.
type ReadSeekCloser interface {
	io.ReadSeeker
	io.Closer
}

type memBlob struct{ *bytes.Reader }

func (memBlob) Close() error { return nil }

// OpenRepoBlob is OpenBlob for one repository's content.
func (s *Store) OpenRepoBlob(r Repo, digest string) (ReadSeekCloser, error) {
	c, err := s.cipherOf(r)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return s.OpenBlob(digest)
	}
	b, err := s.readSealed(c, r, digest)
	if err != nil {
		return nil, err
	}
	return memBlob{bytes.NewReader(b)}, nil
}

// ReadRepoBlob is ReadBlob for one repository's content.
func (s *Store) ReadRepoBlob(r Repo, digest string) ([]byte, error) {
	c, err := s.cipherOf(r)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return s.ReadBlob(digest)
	}
	return s.readSealed(c, r, digest)
}

// PutRepoBlob is PutBlob for one repository's content.
func (s *Store) PutRepoBlob(r Repo, b []byte) (string, error) {
	c, err := s.cipherOf(r)
	if err != nil {
		return "", err
	}
	if c == nil {
		return s.PutBlob(b)
	}
	d := Digest(b)
	return d, s.writeSealed(c, r, d, b)
}

// FinishRepoUpload is FinishUpload for one repository's content.
func (s *Store) FinishRepoUpload(r Repo, id, digest string) error {
	c, err := s.cipherOf(r)
	if err != nil {
		return err
	}
	if c == nil {
		return s.FinishUpload(id, digest)
	}
	p, err := s.uploadPath(id)
	if err != nil {
		return err
	}
	if !ValidDigest(digest) {
		return ErrDigestInvalid
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ErrUploadUnknown
	}
	sum := sha256.Sum256(b)
	if "sha256:"+hex.EncodeToString(sum[:]) != digest {
		return ErrDigestInvalid
	}
	if err := s.writeSealed(c, r, digest, b); err != nil {
		return err
	}
	return os.Remove(p)
}

// MountBlob makes a blob another repository holds one of r's, as a
// cross-repository mount: from's content, opened with its key, is sealed
// with r's (or stored in the shared store when r has none).
func (s *Store) MountBlob(from, r Repo, digest string) error {
	b, err := s.ReadRepoBlob(from, digest)
	if err != nil {
		return err
	}
	if Digest(b) != digest {
		return fmt.Errorf("blob %s does not match its digest", digest)
	}
	_, err = s.PutRepoBlob(r, b)
	return err
}
