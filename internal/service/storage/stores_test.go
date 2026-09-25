package storage

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// failingReader yields n bytes, then fails, as a dropped client connection does.
type failingReader struct{ n int }

func (r *failingReader) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, errors.New("connection reset")
	}
	k := min(len(p), r.n)
	for i := range p[:k] {
		p[i] = 'x'
	}
	r.n -= k
	return k, nil
}

// A write that fails before its rename leaves nothing visible, and a crash's
// leftover temporary file is removed when the store is reopened.
func TestBlobStoreAtomicFinalize(t *testing.T) {
	root := t.TempDir()
	s, err := OpenFileBlobStore(root, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(&failingReader{n: 1 << 20}); err == nil {
		t.Fatal("a failed stream reported success")
	}
	if n := countFiles(t, filepath.Join(root, "blobs")); n != 0 {
		t.Errorf("%d blobs visible after a failed write", n)
	}
	// A crash mid-write: a temporary file with no rename.
	if err := os.WriteFile(filepath.Join(root, "tmp", "upload-crashed"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFileBlobStore(root, Limits{}); err != nil {
		t.Fatal(err)
	}
	if n := countFiles(t, filepath.Join(root, "tmp")); n != 0 {
		t.Errorf("%d temporary files survived reopening", n)
	}
	b, err := s.Write(strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.Open(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(f)
	f.Close()
	if string(got) != "hello" || b.Size != 5 || b.CRC32C == 0 || len(b.MD5) != 16 {
		t.Errorf("stored %q as %+v", got, b)
	}
	if err := s.Collect(func(string) bool { return false }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(b.ID); !errors.Is(err, ErrNoBlob) {
		t.Errorf("an unreferenced blob survived collection: %v", err)
	}
}

func countFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return nil
	})
	return n
}

// Object names are keys, never paths: whatever the name, the only files are
// blobs named by their hash under the store root.
func FuzzObjectNameNeverTouchesPath(f *testing.F) {
	for _, s := range []string{"../../etc/passwd", "a\x00b", strings.Repeat("n", 1024), "𝔘𝔫𝔦𝔠𝔬𝔡𝔢/../x", "./", "..", "/abs/path", "C:\\win", "%2e%2e%2f"} {
		f.Add(s)
	}
	blobRE := regexp.MustCompile(`^blobs/[0-9a-f]{2}/[0-9a-f]{64}$`)
	f.Fuzz(func(t *testing.T, name string) {
		root := t.TempDir()
		blobs, err := OpenFileBlobStore(filepath.Join(root, "data"), Limits{})
		if err != nil {
			t.Fatal(err)
		}
		meta, err := OpenLogMetaStore(filepath.Join(root, "meta"))
		if err != nil {
			t.Fatal(err)
		}
		defer meta.Close()
		b, err := blobs.Write(strings.NewReader("payload for " + name))
		if err != nil {
			t.Fatal(err)
		}
		if err := meta.Update(func(tx Tx) error { tx.Put("object/bkt/"+name, []byte(b.ID)); return nil }); err != nil {
			t.Fatal(err)
		}
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			switch {
			case strings.HasPrefix(rel, "data/"):
				if !blobRE.MatchString(strings.TrimPrefix(rel, "data/")) {
					t.Errorf("name %q produced the file %s", name, rel)
				}
			case rel == "meta/log.jsonl", rel == "meta/snapshot.json":
			default:
				t.Errorf("name %q produced the file %s outside the store layout", name, rel)
			}
			return nil
		})
	})
}

// endless yields n bytes without holding them.
type endless struct{ n int64 }

func (r *endless) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, io.EOF
	}
	k := int64(len(p))
	if k > r.n {
		k = r.n
	}
	for i := range p[:k] {
		p[i] = byte(i)
	}
	r.n -= k
	return int(k), nil
}

// A 100 MiB object streams through bounded memory.
func TestBlobStoreStreamsWithoutBuffering(t *testing.T) {
	s, err := OpenFileBlobStore(t.TempDir(), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	b, err := s.Write(&endless{n: 100 << 20})
	runtime.ReadMemStats(&after)
	if err != nil || b.Size != 100<<20 {
		t.Fatalf("Write = %+v, %v", b, err)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 8<<20 {
		t.Errorf("a 100 MiB write allocated %d MiB; want it streamed", alloc>>20)
	}
}

// Past the object limit or the quota, a write fails and stores nothing.
func TestBlobStoreQuotaRefuses(t *testing.T) {
	for name, open := range map[string]func(Limits) (BlobStore, error){
		"file":   func(l Limits) (BlobStore, error) { return OpenFileBlobStore(t.TempDir(), l) },
		"memory": func(l Limits) (BlobStore, error) { return NewMemBlobStore(l), nil },
	} {
		s, err := open(Limits{MaxObjectBytes: 1 << 20, QuotaBytes: 3 << 19})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Write(&endless{n: 2 << 20}); !errors.Is(err, ErrObjectTooLarge) {
			t.Errorf("%s: an object over the maximum = %v", name, err)
		}
		if _, err := s.Write(bytes.NewReader(bytes.Repeat([]byte{1}, 1<<20))); err != nil {
			t.Fatalf("%s: a first 1 MiB object: %v", name, err)
		}
		_, err = s.Write(bytes.NewReader(bytes.Repeat([]byte{2}, 1<<20)))
		if !errors.Is(err, ErrQuota) {
			t.Errorf("%s: a write past the quota = %v", name, err)
		}
		if st, reason, ok := storeStatus(err); !ok || st != 507 || reason != "insufficientStorage" {
			t.Errorf("%s: quota maps to %d %s", name, st, reason)
		}
	}
	if st, _, _ := storeStatus(ErrObjectTooLarge); st != 413 {
		t.Errorf("too large maps to %d, want 413", st)
	}
}

// The log store keeps committed transactions across a reopen, all or nothing,
// drops a torn tail, and compacts without losing anything.
func TestMetadataStoreSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenLogMetaStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(tx Tx) error {
		tx.Put("bucket/a", []byte("1"))
		tx.Put("object/a/x", []byte("gen1"))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	failed := errors.New("abandoned")
	if err := s.Update(func(tx Tx) error { tx.Put("bucket/b", []byte("2")); return failed }); !errors.Is(err, failed) {
		t.Fatal(err)
	}
	if err := s.Update(func(tx Tx) error { tx.Delete("object/a/x"); tx.Put("object/a/y", []byte("gen2")); return nil }); err != nil {
		t.Fatal(err)
	}
	s.Close()
	// A crash mid-record: a torn last line.
	f, _ := os.OpenFile(filepath.Join(dir, "log.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"ops":[{"k":"bucket/torn","v":"eA=="}],"crc":12`)
	f.Close()

	s, err = OpenLogMetaStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	check := func(when string) {
		t.Helper()
		_ = s.View(func(tx Tx) error {
			if got := strings.Join(tx.List(""), ","); got != "bucket/a,object/a/y" {
				t.Errorf("%s: keys = %s", when, got)
			}
			if v, ok := tx.Get("object/a/y"); !ok || string(v) != "gen2" {
				t.Errorf("%s: object/a/y = %q, %v", when, v, ok)
			}
			return nil
		})
	}
	check("after reopen")
	// The next write lands after the torn tail was cut.
	if err := s.Update(func(tx Tx) error { tx.Put("bucket/c", []byte("3")); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(tx Tx) error { tx.Delete("bucket/c"); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.compact(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenLogMetaStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	check("after compaction and reopen")
}
