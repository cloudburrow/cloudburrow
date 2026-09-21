package localai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Errors callers are expected to distinguish.
var (
	// ErrInsufficientDisk means the artifact will not fit.
	ErrInsufficientDisk = errors.New("insufficient disk space")
	// ErrChecksumMismatch means a cached artifact is not what was expected.
	ErrChecksumMismatch = errors.New("artifact checksum mismatch")
	// ErrNotCached means the artifact has not been downloaded.
	ErrNotCached = errors.New("model is not cached")
)

// Cache stores downloaded artifacts.
//
// Artifacts are large and slow to fetch, so the cache is content-verified
// rather than trusted: a truncated download that merely exists on disk would
// otherwise fail much later, inside a runtime, as a corrupt-model error.
type Cache struct {
	// Dir is the cache root.
	Dir string
}

// Path returns where an artifact is cached.
func (c Cache) Path(m Model) string {
	return filepath.Join(c.Dir, strings.ReplaceAll(m.Repo, "/", "_"), m.Artifact)
}

// Status describes what is cached for a model.
type Status struct {
	Model    Model
	Cached   bool
	Path     string
	Bytes    int64
	Verified bool
	// Problem explains an unusable cache entry.
	Problem string
}

// Status reports the cache state for a model, verifying the artifact when a
// checksum is known.
func (c Cache) Status(m Model, wantSHA256 string) Status {
	s := Status{Model: m, Path: c.Path(m)}
	info, err := os.Stat(s.Path)
	if err != nil {
		return s
	}
	s.Cached = true
	s.Bytes = info.Size()

	if wantSHA256 == "" {
		// No pinned checksum means no verification is possible. Saying so is
		// better than reporting Verified and meaning "we did not check".
		s.Problem = "no pinned checksum for this artifact, so its contents are unverified"
		return s
	}
	got, err := fileSHA256(s.Path)
	if err != nil {
		s.Problem = "could not read the cached artifact: " + err.Error()
		return s
	}
	if got != wantSHA256 {
		s.Problem = fmt.Sprintf("%v: have %s, want %s", ErrChecksumMismatch, got[:16]+"...", wantSHA256[:16]+"...")
		return s
	}
	s.Verified = true
	return s
}

// Remove deletes a cached artifact, which is how a corrupt download is
// recovered from.
func (c Cache) Remove(m Model) error {
	err := os.Remove(c.Path(m))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// PreflightDisk checks that an artifact will fit before anything is fetched.
//
// Checking first matters because these artifacts are gigabytes: filling a
// developer's disk and failing at 98% is a much worse outcome than refusing up
// front.
func PreflightDisk(dir string, needBytes int64) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("prepare cache directory: %w", err)
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		// Unable to check is not the same as insufficient. Proceeding is the
		// right call; claiming there is room would not be.
		return nil
	}
	available := int64(st.Bavail) * int64(st.Bsize)
	// A margin, because filling a disk completely breaks far more than this.
	const margin = 2 << 30
	if available < needBytes+margin {
		return fmt.Errorf("%w: %s needs %.1f GiB plus a %.0f GiB margin, but only %.1f GiB is free",
			ErrInsufficientDisk, dir,
			float64(needBytes)/(1<<30), float64(margin)/(1<<30), float64(available)/(1<<30))
	}
	return nil
}

// Fetcher retrieves an artifact. Injected so acquisition logic is testable
// without downloading gigabytes.
type Fetcher interface {
	// Fetch writes the artifact to dst, reporting progress.
	Fetch(ctx context.Context, m Model, dst io.Writer, progress func(done int64)) error
}

// Acquirer downloads and verifies artifacts.
type Acquirer struct {
	Cache   Cache
	Fetcher Fetcher
	// Token authorises gated downloads. Without it, a gated model is refused
	// rather than attempted.
	Token string
}

// Acquire fetches a model into the cache if it is not already there.
//
// It refuses a gated model without credentials rather than attempting a
// download that would fail with an opaque 401 after a long wait.
func (a *Acquirer) Acquire(ctx context.Context, m Model, wantSHA256 string, progress func(int64)) error {
	if st := a.Cache.Status(m, wantSHA256); st.Cached && (st.Verified || wantSHA256 == "") {
		return nil
	}

	if m.RequiresCredentials() && a.Token == "" {
		return fmt.Errorf("%w: %s requires accepting the %s licence at https://huggingface.co/%s "+
			"and a token; set HUGGINGFACE_TOKEN", ErrGated, m.ID, m.License, m.Repo)
	}

	dst := a.Cache.Path(m)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	// Download to a temporary file and rename, so an interrupted fetch never
	// leaves a partial artifact that looks cached.
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".partial-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := a.Fetcher.Fetch(ctx, m, tmp, progress); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("fetch %s: %w", m.ID, err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if wantSHA256 != "" {
		got, err := fileSHA256(tmpName)
		if err != nil {
			return err
		}
		if got != wantSHA256 {
			return fmt.Errorf("%w for %s: have %s, want %s", ErrChecksumMismatch, m.ID, got, wantSHA256)
		}
	}
	return os.Rename(tmpName, dst)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
