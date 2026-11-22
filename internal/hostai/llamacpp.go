package hostai

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
)

// LlamaCppTag is the llama.cpp release the host runs: the same build as the
// cluster's ghcr.io/ggml-org/llama.cpp:server image
// (modelgarden.LlamaCppImage), so a model behaves alike on either.
const LlamaCppTag = "b12211"

// LlamaCppArchive is llama.cpp's official macOS arm64 release archive
// (MIT), built with Metal, and LlamaCppSHA256 its digest
// (dependencies.json localai.hostLlamaCpp; GitHub's release asset digest).
const (
	LlamaCppArchive = "https://github.com/ggml-org/llama.cpp/releases/download/" + LlamaCppTag +
		"/llama-" + LlamaCppTag + "-bin-macos-arm64.tar.gz"
	LlamaCppSHA256 = "4da4edb437358d31d55938ad849d262ca31b0e0ffb35e9caedb727a0cd5a8f0f"
	// llamaCppArchiveMax bounds the download (the archive is 16.7 MB).
	llamaCppArchiveMax = 256 << 20
)

// Installer installs the host runtimes under Dir.
type Installer struct {
	Dir string
	// Client fetches the llama.cpp archive; http.DefaultClient when nil.
	Client *http.Client
	// ArchiveURL and ArchiveSHA256 replace the pinned archive (tests).
	ArchiveURL, ArchiveSHA256 string
	// LookPath and Run replace exec.LookPath and running a command
	// (tests).
	LookPath func(string) (string, error)
	Run      func(ctx context.Context, name string, args ...string) (string, error)

	mu sync.Mutex
}

func (in *Installer) client() *http.Client {
	if in.Client != nil {
		return in.Client
	}
	return http.DefaultClient
}

func (in *Installer) archive() (string, string) {
	if in.ArchiveURL != "" {
		return in.ArchiveURL, in.ArchiveSHA256
	}
	return LlamaCppArchive, LlamaCppSHA256
}

// LlamaServer returns the path of llama-server, downloading and unpacking
// the pinned archive the first time: it is fetched to a temporary file,
// checked against its SHA-256, and unpacked into a directory of its own
// that replaces nothing until it is complete.
func (in *Installer) LlamaServer(ctx context.Context, progress func(string)) (string, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	u, sum := in.archive()
	dir := filepath.Join(in.Dir, "llama.cpp-"+LlamaCppTag+"-"+sum[:12])
	bin := filepath.Join(dir, "llama-server")
	if st, err := os.Stat(filepath.Join(dir, ".complete")); err == nil && st.Mode().IsRegular() {
		return bin, nil
	}
	if err := os.MkdirAll(in.Dir, 0o755); err != nil {
		return "", err
	}
	if progress != nil {
		progress("Downloading llama.cpp " + LlamaCppTag + " for macOS (Metal)")
	}
	tmp, err := os.MkdirTemp(in.Dir, ".llama.cpp-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	arc := filepath.Join(tmp, "archive.tar.gz")
	if err := in.download(ctx, u, sum, arc); err != nil {
		return "", err
	}
	out := filepath.Join(tmp, "tree")
	if err := untar(arc, out); err != nil {
		return "", fmt.Errorf("unpack the llama.cpp archive: %w", err)
	}
	if st, err := os.Stat(filepath.Join(out, "llama-server")); err != nil || !st.Mode().IsRegular() {
		return "", fmt.Errorf("the llama.cpp archive has no llama-server")
	}
	if err := os.WriteFile(filepath.Join(out, ".complete"), []byte(sum+"\n"), 0o644); err != nil {
		return "", err
	}
	_ = os.RemoveAll(dir)
	if err := os.Rename(out, dir); err != nil {
		return "", err
	}
	return bin, nil
}

func (in *Installer) download(ctx context.Context, u, sum, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := in.client().Do(req)
	if err != nil {
		return fmt.Errorf("download llama.cpp: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download llama.cpp from %s: HTTP %d", u, resp.StatusCode)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, llamaCppArchiveMax+1))
	if err != nil {
		return fmt.Errorf("download llama.cpp: %w", err)
	}
	if n > llamaCppArchiveMax {
		return fmt.Errorf("the llama.cpp archive is larger than %d bytes", llamaCppArchiveMax)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sum {
		return fmt.Errorf("the llama.cpp archive's SHA-256 is %s, not the pinned %s", got, sum)
	}
	return f.Close()
}

// untar unpacks a gzip tar, dropping its one top-level directory: regular
// files and directories, and symbolic links that stay inside the tree (the
// archive's dylib version links). Anything else, and any path leaving the
// tree, is refused.
func untar(arc, dst string) error {
	f, err := os.Open(arc)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		raw := strings.TrimPrefix(h.Name, "./")
		for _, el := range strings.Split(raw, "/") {
			if el == ".." {
				return fmt.Errorf("unsafe path %q", h.Name)
			}
		}
		name := path.Clean(raw)
		if i := strings.IndexByte(name, '/'); i >= 0 {
			name = name[i+1:]
		} else {
			continue // the top-level directory itself
		}
		if name == "" || name == "." || strings.HasPrefix(name, "../") || name == ".." || path.IsAbs(name) {
			return fmt.Errorf("unsafe path %q", h.Name)
		}
		target := filepath.Join(dst, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, io.LimitReader(tr, h.Size)); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			link := h.Linkname
			resolved := path.Clean(path.Join(path.Dir(name), link))
			if path.IsAbs(link) || resolved == ".." || strings.HasPrefix(resolved, "../") {
				return fmt.Errorf("symbolic link %q leaves the archive", h.Name)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(link, target); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unexpected entry %q (type %c)", h.Name, h.Typeflag)
		}
	}
}

func (in *Installer) lookPath(name string) (string, error) {
	if in.LookPath != nil {
		return in.LookPath(name)
	}
	return exec.LookPath(name)
}

func (in *Installer) run(ctx context.Context, name string, args ...string) (string, error) {
	if in.Run != nil {
		return in.Run(ctx, name, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "PIP_DISABLE_PIP_VERSION_CHECK=1", "PIP_NO_INPUT=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		s := strings.TrimSpace(string(out))
		if len(s) > 1500 {
			s = "…" + s[len(s)-1500:]
		}
		return string(out), fmt.Errorf("%s %s: %w: %s", filepath.Base(name), strings.Join(args[:min(2, len(args))], " "), err, s)
	}
	return string(out), nil
}
