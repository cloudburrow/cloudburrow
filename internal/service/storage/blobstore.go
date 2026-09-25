package storage

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

// Object bytes live in a blob store, content-addressed by SHA-256 (#489). An
// object name never becomes a file name: a name is only ever a key in the
// metadata store, and the blob a generation points at is named by its hash.

var (
	// ErrObjectTooLarge is a write over the configured maximum object size.
	ErrObjectTooLarge = errors.New("object exceeds the configured maximum size")
	// ErrQuota is a write that would take the store past its disk quota.
	ErrQuota = errors.New("the Cloud Storage disk quota is exhausted")
	// ErrNoBlob is a read of a blob that is not there.
	ErrNoBlob = errors.New("blob not found")
)

// Blob describes stored bytes.
type Blob struct {
	ID     string // hex SHA-256 of the bytes
	Size   int64
	MD5    []byte
	CRC32C uint32
}

// BlobStore keeps object bytes.
type BlobStore interface {
	// Write streams r into the store and returns what it stored. On any error
	// nothing becomes visible.
	Write(r io.Reader) (Blob, error)
	// Open reads a blob.
	Open(id string) (io.ReadSeekCloser, error)
	// Collect deletes every blob keep says is unreferenced.
	Collect(keep func(id string) bool) error
}

// Limits bound what a store accepts; zero means unlimited.
type Limits struct {
	MaxObjectBytes int64
	QuotaBytes     int64
}

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// hashes computes every digest while the bytes stream past.
type hashes struct {
	sha, md hash.Hash
	crc     hash.Hash32
	n       int64
}

func newHashes() *hashes {
	return &hashes{sha: sha256.New(), md: md5.New(), crc: crc32.New(castagnoli)}
}

func (h *hashes) Write(p []byte) (int, error) {
	h.sha.Write(p)
	h.md.Write(p)
	h.crc.Write(p)
	h.n += int64(len(p))
	return len(p), nil
}

func (h *hashes) blob() Blob {
	return Blob{ID: hex.EncodeToString(h.sha.Sum(nil)), Size: h.n, MD5: h.md.Sum(nil), CRC32C: h.crc.Sum32()}
}

// limitFor is how many bytes a write may take: the object limit, and what is
// left of the quota.
func limitFor(l Limits, used int64) (limit int64, errOver error) {
	limit, errOver = -1, nil
	if l.MaxObjectBytes > 0 {
		limit, errOver = l.MaxObjectBytes, ErrObjectTooLarge
	}
	if l.QuotaBytes > 0 {
		if left := l.QuotaBytes - used; limit < 0 || left < limit {
			limit, errOver = max(left, 0), ErrQuota
		}
	}
	return limit, errOver
}

// copyLimited copies r to w, failing with errOver past limit (unless limit is
// negative).
func copyLimited(w io.Writer, r io.Reader, limit int64, errOver error) error {
	if limit < 0 {
		_, err := io.Copy(w, r)
		return err
	}
	n, err := io.Copy(w, io.LimitReader(r, limit+1))
	if err != nil {
		return err
	}
	if n > limit {
		return errOver
	}
	return nil
}

// FileBlobStore keeps blobs as files under a root: blobs/<2 hex>/<64 hex>,
// written in tmp/ and renamed into place after fsync.
type FileBlobStore struct {
	root   string
	limits Limits
	mu     sync.Mutex
	used   int64
}

var blobIDRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// OpenFileBlobStore opens or creates the store at root. Temporary files a
// crash left behind are removed: they were never visible.
func OpenFileBlobStore(root string, limits Limits) (*FileBlobStore, error) {
	for _, d := range []string{"blobs", "tmp"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			return nil, err
		}
	}
	if err := os.RemoveAll(filepath.Join(root, "tmp")); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(root, "tmp"), 0o700); err != nil {
		return nil, err
	}
	s := &FileBlobStore{root: root, limits: limits}
	err := filepath.WalkDir(filepath.Join(root, "blobs"), func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if info, err := d.Info(); err == nil {
			s.used += info.Size()
		}
		return nil
	})
	return s, err
}

func (s *FileBlobStore) path(id string) string {
	return filepath.Join(s.root, "blobs", id[:2], id)
}

func (s *FileBlobStore) Write(r io.Reader) (Blob, error) {
	s.mu.Lock()
	limit, errOver := limitFor(s.limits, s.used)
	s.mu.Unlock()
	tmp, err := os.CreateTemp(filepath.Join(s.root, "tmp"), "upload-")
	if err != nil {
		return Blob{}, err
	}
	name := tmp.Name()
	defer os.Remove(name) // a no-op once renamed
	h := newHashes()
	err = copyLimited(io.MultiWriter(tmp, h), r, limit, errOver)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return Blob{}, err
	}
	b := h.blob()
	s.mu.Lock()
	defer s.mu.Unlock()
	dst := s.path(b.ID)
	if _, err := os.Stat(dst); err == nil {
		return b, nil // the same bytes are already stored
	}
	if s.limits.QuotaBytes > 0 && s.used+b.Size > s.limits.QuotaBytes {
		return Blob{}, ErrQuota
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return Blob{}, err
	}
	if err := os.Rename(name, dst); err != nil {
		return Blob{}, err
	}
	if d, err := os.Open(filepath.Dir(dst)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	s.used += b.Size
	return b, nil
}

func (s *FileBlobStore) Open(id string) (io.ReadSeekCloser, error) {
	if !blobIDRE.MatchString(id) {
		return nil, fmt.Errorf("%w: %q is not a blob ID", ErrNoBlob, id)
	}
	f, err := os.Open(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNoBlob, id)
	}
	return f, err
}

func (s *FileBlobStore) Collect(keep func(id string) bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return filepath.WalkDir(filepath.Join(s.root, "blobs"), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || keep(d.Name()) {
			return err
		}
		info, ierr := d.Info()
		if rerr := os.Remove(p); rerr != nil {
			return rerr
		}
		if ierr == nil {
			s.used -= info.Size()
		}
		return nil
	})
}

// MemBlobStore keeps blobs in memory, for ephemeral mode and tests.
type MemBlobStore struct {
	limits Limits
	mu     sync.Mutex
	blobs  map[string][]byte
	used   int64
}

// NewMemBlobStore returns an empty in-memory store.
func NewMemBlobStore(limits Limits) *MemBlobStore {
	return &MemBlobStore{limits: limits, blobs: map[string][]byte{}}
}

func (s *MemBlobStore) Write(r io.Reader) (Blob, error) {
	s.mu.Lock()
	limit, errOver := limitFor(s.limits, s.used)
	s.mu.Unlock()
	var buf bytes.Buffer
	h := newHashes()
	if err := copyLimited(io.MultiWriter(&buf, h), r, limit, errOver); err != nil {
		return Blob{}, err
	}
	b := h.blob()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.blobs[b.ID]; !ok {
		if s.limits.QuotaBytes > 0 && s.used+b.Size > s.limits.QuotaBytes {
			return Blob{}, ErrQuota
		}
		s.blobs[b.ID] = buf.Bytes()
		s.used += b.Size
	}
	return b, nil
}

func (s *MemBlobStore) Open(id string) (io.ReadSeekCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.blobs[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoBlob, id)
	}
	return nopCloser{bytes.NewReader(b)}, nil
}

func (s *MemBlobStore) Collect(keep func(id string) bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, b := range s.blobs {
		if !keep(id) {
			delete(s.blobs, id)
			s.used -= int64(len(b))
		}
	}
	return nil
}

type nopCloser struct{ *bytes.Reader }

func (nopCloser) Close() error { return nil }

// storeStatus maps a blob-store limit error to the HTTP status and reason the
// JSON API answers with. An object over the maximum is 413; Google documents
// 413 for an entity too large. A local disk quota has no Google equivalent,
// so 507 insufficientStorage is CloudBurrow's choice.
//
// unverified: the reasons for both
func storeStatus(err error) (status int, reason string, ok bool) {
	switch {
	case errors.Is(err, ErrObjectTooLarge):
		return 413, "entityTooLarge", true
	case errors.Is(err, ErrQuota):
		return 507, "insufficientStorage", true
	}
	return 0, "", false
}
