// Package storageimage builds the image the in-cluster Cloud Storage
// Deployment runs (#514). The image is never published or downloaded: the
// CLI embeds Linux builds of cmd/cloudburrow-storage, and `up` writes the one
// matching the cluster's nodes onto a distroless base pinned by digest
// (dependencies.json, build.storageServerBase), builds it with docker and
// loads it into kind. Its tag is the content hash of the binary and the
// base, so the same CLI always yields the same image, and a rebuild is
// skipped when the image already exists.
package storageimage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Base is the image the binary is layered on: distroless static, which has
// no shell or package manager, pinned by digest. It duplicates
// dependencies.json's build.storageServerBase, the source of truth.
const Base = "gcr.io/distroless/static-debian12@sha256:d75cdd72874d4790092fcb1b058493ecf6bb5bf2b2b897045b00ff01d91843f2"

// Repository is the local registry prefix the image is tagged under, one
// Knative and kubelet never try to pull (see internal/images).
const Repository = "dev.local/cloudburrow-storage"

//go:embed bin/*
var embedded embed.FS

// bins is what Binary reads: the embedded directory, or a stand-in a test
// installs with UseBinaries.
var bins fs.FS = embedded

// Arches are the node architectures `make storage-binaries` and a release
// embed a build for.
var Arches = []string{"amd64", "arm64"}

// ErrNotEmbedded means this CLI was built without the Linux storage
// binaries: a plain `go build` or `go install` rather than `make build` or a
// release. Since #585 the binaries are not committed, so such a build embeds
// only bin/README.md; its error names the fix.
var ErrNotEmbedded = errors.New("this cloudburrow was built without the embedded storage server " +
	"(run make build, or make storage-binaries before go build, or install a release)")

// machines is the ELF machine each architecture's build must be for.
var machines = map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}

// NotEmbeddedError is why one architecture's build is unusable. It is
// ErrNotEmbedded, so errors.Is finds it.
type NotEmbeddedError struct {
	Arch string
	// Reason is the short why: no file, an empty one, not an executable.
	Reason string
}

func (e *NotEmbeddedError) Error() string { return ErrNotEmbedded.Error() + ": " + e.Reason }
func (e *NotEmbeddedError) Unwrap() error { return ErrNotEmbedded }

// Binary returns the embedded Linux build for a node architecture. A file
// that is absent, empty, or not a Linux executable for that architecture
// (a placeholder, or a build for the wrong one) is ErrNotEmbedded: an image
// made from it would build and then fail only when its pod started.
func Binary(arch string) ([]byte, error) {
	notEmbedded := func(format string, args ...any) error {
		return &NotEmbeddedError{Arch: arch, Reason: fmt.Sprintf(format, args...)}
	}
	b, err := fs.ReadFile(bins, "bin/cloudburrow-storage-linux-"+arch)
	if err != nil {
		return nil, notEmbedded("no linux/%s build", arch)
	}
	if len(b) == 0 {
		return nil, notEmbedded("the linux/%s build is empty", arch)
	}
	f, err := elf.NewFile(bytes.NewReader(b))
	if err != nil {
		return nil, notEmbedded("the linux/%s build is not an executable (%d bytes)", arch, len(b))
	}
	if want, ok := machines[arch]; !ok || f.Machine != want || (f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN) {
		return nil, notEmbedded("the linux/%s build is a %s %s, not a linux/%s executable", arch, f.Machine, f.Type, arch)
	}
	return b, nil
}

// Check reports, for each of Arches, whether this CLI embeds a usable build:
// a nil error means present.
func Check() map[string]error {
	out := make(map[string]error, len(Arches))
	for _, a := range Arches {
		_, err := Binary(a)
		out[a] = err
	}
	return out
}

// UseBinaries makes Binary read fsys, laid out like the embedded directory
// (bin/cloudburrow-storage-linux-<arch>), until restore is called. It is for
// tests, which must behave the same whether or not `make storage-binaries`
// has run; it is not safe to call while another goroutine reads a binary.
func UseBinaries(fsys fs.FS) (restore func()) {
	prev := bins
	bins = fsys
	return func() { bins = prev }
}

// Tag is the image reference for a binary on Base.
func Tag(binary []byte) string {
	h := sha256.New()
	h.Write([]byte(Base + "\n"))
	h.Write(binary)
	return Repository + ":" + hex.EncodeToString(h.Sum(nil))[:16]
}

// Runner runs docker; injected for tests.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// Dockerfile is the whole image: the binary on the pinned base, as a
// non-root user.
func Dockerfile() string {
	return "FROM " + Base + "\n" +
		"COPY cloudburrow-storage /cloudburrow-storage\n" +
		"USER 65532:65532\n" +
		"ENTRYPOINT [\"/cloudburrow-storage\"]\n"
}

// Build makes the image for arch unless it exists, and returns its tag.
func Build(ctx context.Context, r Runner, arch string) (string, error) {
	bin, err := Binary(arch)
	if err != nil {
		return "", err
	}
	tag := Tag(bin)
	if _, err := r.Run(ctx, "docker", "image", "inspect", tag); err == nil {
		return tag, nil
	}
	dir, err := os.MkdirTemp("", "cloudburrow-storage-image-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "cloudburrow-storage"), bin, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(Dockerfile()), 0o644); err != nil {
		return "", err
	}
	if out, err := r.Run(ctx, "docker", "build", "--platform", "linux/"+arch, "-t", tag, dir); err != nil {
		return "", fmt.Errorf("build %s: %w\n%s", tag, err, out)
	}
	return tag, nil
}
