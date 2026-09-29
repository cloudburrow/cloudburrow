package localai

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// EmbeddingGemmaONNXID is the catalogue and API identity of the community
// ONNX conversion. The "-onnx-community" suffix is deliberate: the identity a
// client sees must not read as Google's own model.
const EmbeddingGemmaONNXID = "embeddinggemma-300m-onnx-community"

// RuntimeONNX names ONNX Runtime.
const RuntimeONNX = "onnxruntime"

// ONNXRuntimeCompiled reports whether this binary was built with the onnx
// build tag. It is set by onnx_enabled.go.
var ONNXRuntimeCompiled = false

// Pin fixes an upstream revision and the files taken from it.
type Pin struct {
	// Revision is the full commit SHA on Hugging Face, never a branch name,
	// so the same bytes are fetched however the repository moves.
	Revision string
	// Files are every file the runtime needs, each with its checksum.
	Files []PinnedFile
}

// PinnedFile is one file of a pinned model.
type PinnedFile struct {
	Path   string
	SHA256 string
	Bytes  int64
}

// TotalBytes is the pinned download size.
func (p Pin) TotalBytes() int64 {
	var n int64
	for _, f := range p.Files {
		n += f.Bytes
	}
	return n
}

// URL is the HTTPS location of a pinned file at the pinned revision.
func (p Pin) URL(repo string, f PinnedFile) string {
	return "https://huggingface.co/" + repo + "/resolve/" + p.Revision + "/" + f.Path
}

// PinnedDir is where a pinned model's files are cached, keyed by revision so
// that a re-pin never mixes files from two revisions.
func (c Cache) PinnedDir(m Model) string {
	rev := ""
	if m.Pin != nil {
		rev = m.Pin.Revision
	}
	return filepath.Join(c.Dir, strings.ReplaceAll(m.Repo, "/", "_"), rev)
}

// PinnedPath is where one pinned file is cached.
func (c Cache) PinnedPath(m Model, f PinnedFile) string {
	return filepath.Join(c.PinnedDir(m), filepath.FromSlash(f.Path))
}

// VerifyPinned checks every pinned file against its checksum.
func (c Cache) VerifyPinned(m Model) error {
	if m.Pin == nil {
		return fmt.Errorf("%s has no pin", m.ID)
	}
	for _, f := range m.Pin.Files {
		path := c.PinnedPath(m, f)
		got, err := fileSHA256(path)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("%w: %s (%s)", ErrNotCached, m.ID, f.Path)
			}
			return err
		}
		if got != f.SHA256 {
			return fmt.Errorf("%w: %s: have %s, want %s", ErrChecksumMismatch, f.Path, got, f.SHA256)
		}
	}
	return nil
}

// HTTPGet is the transport FetchPinned uses; tests replace it.
var HTTPGet = func(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return resp.Body, nil
}

// FetchPinned downloads every pinned file over HTTPS at the pinned revision,
// verifying each against its SHA-256 before it is moved into place. Files
// already cached and verified are not fetched again. A gated model is refused.
func (c Cache) FetchPinned(ctx context.Context, m Model, progress func(file string, done int64)) error {
	if m.Pin == nil {
		return fmt.Errorf("%s has no pin", m.ID)
	}
	if m.RequiresCredentials() {
		return fmt.Errorf("%w: %s", ErrGated, m.ID)
	}
	if err := PreflightDisk(c.Dir, m.Pin.TotalBytes()); err != nil {
		return err
	}
	for _, f := range m.Pin.Files {
		dst := c.PinnedPath(m, f)
		if got, err := fileSHA256(dst); err == nil && got == f.SHA256 {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := fetchOne(ctx, m.Pin.URL(m.Repo, f), dst, f, progress); err != nil {
			return fmt.Errorf("fetch %s %s: %w", m.ID, f.Path, err)
		}
	}
	return nil
}

func fetchOne(ctx context.Context, url, dst string, f PinnedFile, progress func(string, int64)) error {
	body, err := HTTPGet(ctx, url)
	if err != nil {
		return err
	}
	defer body.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".partial-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	var r io.Reader = body
	if progress != nil {
		r = &progressReader{r: body, fn: func(n int64) { progress(f.Path, n) }}
	}
	if _, err := io.Copy(tmp, r); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	got, err := fileSHA256(tmpName)
	if err != nil {
		return err
	}
	if got != f.SHA256 {
		return fmt.Errorf("%w: have %s, want %s", ErrChecksumMismatch, got, f.SHA256)
	}
	return os.Rename(tmpName, dst)
}

type progressReader struct {
	r  io.Reader
	n  int64
	fn func(int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.n += int64(n)
	p.fn(p.n)
	return n, err
}
