package osvscan

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Dir is where a prefetch cache (internal/prefetch's <state dir>/cache)
// keeps the scanner and its database.
type Dir string

// BinaryPath is the pinned scanner's path.
func (d Dir) BinaryPath() string {
	return filepath.Join(string(d), "osv-scanner", Version, "osv-scanner")
}

// DBDir is the directory OSV-Scanner's offline mode reads, through
// OSV_SCANNER_LOCAL_DB_CACHE_DIRECTORY: it looks for
// osv-scalibr/{ecosystem}/all.zip under it.
func (d Dir) DBDir() string {
	return filepath.Join(string(d), "osv-scanner", "db-"+SnapshotDate)
}

// SnapshotPath is one ecosystem archive's path.
func (d Dir) SnapshotPath(s Snapshot) string {
	return filepath.Join(d.DBDir(), "osv-scalibr", s.Ecosystem, "all.zip")
}

// ErrNotPrefetched means the scanner or its database is not in the cache.
var ErrNotPrefetched = errors.New("OSV-Scanner and its database are not prefetched")

// Fetcher downloads what Prefetch stores; Client defaults to
// http.DefaultClient.
type Fetcher struct {
	Client *http.Client
	Logf   func(format string, a ...any)
}

func (f Fetcher) logf(format string, a ...any) {
	if f.Logf != nil {
		f.Logf(format, a...)
	}
}

// Prefetch stores this machine's scanner binary and the pinned snapshots
// of the ecosystems named (every one when none is) under d, skipping what
// is already there and checks out, and returns the bytes it downloaded.
func (f Fetcher) Prefetch(ctx context.Context, d Dir, ecosystems ...string) (int64, error) {
	want := map[string]bool{}
	for _, e := range ecosystems {
		found := false
		for _, s := range Snapshots {
			if strings.EqualFold(s.Ecosystem, e) {
				want[s.Ecosystem], found = true, true
			}
		}
		if !found {
			return 0, fmt.Errorf("no pinned OSV database snapshot for ecosystem %q", e)
		}
	}
	b, err := HostBinary()
	if err != nil {
		return 0, err
	}
	var total int64
	n, err := f.fetch(ctx, BinaryURL(b), d.BinaryPath(), b.SHA256, b.Size, 0o755)
	total += n
	if err != nil {
		return total, err
	}
	for _, s := range Snapshots {
		if s.SHA256 == "" || (len(want) > 0 && !want[s.Ecosystem]) {
			continue
		}
		n, err := f.fetch(ctx, SnapshotURL(s), d.SnapshotPath(s), s.SHA256, s.Size, 0o644)
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func fileSHA256(path string) (string, error) {
	fh, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer fh.Close()
	h := sha256.New()
	if _, err := io.Copy(h, fh); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (f Fetcher) fetch(ctx context.Context, url, path, sum string, size int64, mode os.FileMode) (int64, error) {
	if got, err := fileSHA256(path); err == nil && got == sum {
		f.logf("  osv-scanner: %s already cached\n", path)
		return 0, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, err
	}
	f.logf("  osv-scanner: downloading %s (%d bytes)\n", url, size)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	c := f.Client
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("download %s: %s (a pinned OSV database generation is kept for about a week; move the pin in internal/osvscan/pins.go and dependencies.json)", url, resp.Status)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".download-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), resp.Body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return n, err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sum {
		return n, fmt.Errorf("download %s: sha256 %s, not the pinned %s", url, got, sum)
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return n, err
	}
	return n, os.Rename(tmp.Name(), path)
}

// verified caches Ready's check of a directory: hashing the snapshots
// takes seconds, and they do not change under a running instance.
var verified sync.Map // Dir -> error

// Ready reports whether d holds the pinned scanner and at least one
// pinned snapshot, each matching its SHA-256.
func Ready(d Dir) error {
	if v, ok := verified.Load(d); ok {
		if v == nil {
			return nil
		}
		return v.(error)
	}
	err := ready(d)
	if err == nil {
		verified.Store(d, nil)
	}
	return err
}

func ready(d Dir) error {
	b, err := HostBinary()
	if err != nil {
		return err
	}
	if got, err := fileSHA256(d.BinaryPath()); err != nil || got != b.SHA256 {
		return fmt.Errorf("%w: no OSV-Scanner %s at %s; run `cloudburrow prefetch`", ErrNotPrefetched, Version, d.BinaryPath())
	}
	n := 0
	for _, s := range Snapshots {
		if s.SHA256 == "" {
			continue
		}
		got, err := fileSHA256(d.SnapshotPath(s))
		if err != nil {
			continue
		}
		if got != s.SHA256 {
			return fmt.Errorf("%s has sha256 %s, not the pinned %s; run `cloudburrow prefetch`", d.SnapshotPath(s), got, s.SHA256)
		}
		n++
	}
	if n == 0 {
		return fmt.Errorf("%w: no OSV database snapshot under %s; run `cloudburrow prefetch`", ErrNotPrefetched, d.DBDir())
	}
	return nil
}
