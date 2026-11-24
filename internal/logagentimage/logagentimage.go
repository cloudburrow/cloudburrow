// Package logagentimage builds the image of Kubernetes Engine's node
// logging agent, the kube-system/fluentbit-gke DaemonSet (#2100), as
// internal/storageimage builds the storage server's. The image is never
// published or downloaded: the CLI embeds Linux builds of
// cmd/cloudburrow-logging-agent, and the CLI writes the one matching a
// cluster's nodes onto the storage server's distroless base, pinned by
// digest (dependencies.json, build.storageServerBase), builds it with
// docker and loads it into the cluster's kind nodes. Its tag is the content hash of the binary and the
// base, so the same CLI always yields the same image, and a rebuild is
// skipped when the image already exists.
package logagentimage

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

	"github.com/cloudburrow/cloudburrow/internal/storageimage"
)

// Base is the image the binary is layered on: the storage server's.
const Base = storageimage.Base

// Repository is the local registry prefix the image is tagged under, one
// Knative and kubelet never try to pull (see internal/images).
const Repository = "dev.local/cloudburrow-logging-agent"

//go:embed bin/*
var embedded embed.FS

// bins is what Binary reads: the embedded directory, or a stand-in a test
// installs with UseBinaries.
var bins fs.FS = embedded

// Arches are the node architectures `make logging-agent-binaries` and a release
// embed a build for.
var Arches = []string{"amd64", "arm64"}

// ErrNotEmbedded means this CLI was built without the Linux agent
// binaries: a plain `go build` or `go install` rather than `make build` or a
// release. The binaries are never committed, so such a build embeds
// only bin/README.md; its error names the fix.
var ErrNotEmbedded = errors.New("this cloudburrow was built without the embedded Kubernetes Engine logging agent " +
	"(run make build, or make logging-agent-binaries before go build, or install a release)")

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
	b, err := fs.ReadFile(bins, "bin/cloudburrow-logging-agent-linux-"+arch)
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
// (bin/cloudburrow-logging-agent-linux-<arch>), until restore is called. It is for
// tests, which must behave the same whether or not `make logging-agent-binaries`
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

// Dockerfile is the whole image: the binary on the pinned base. It runs
// as root, as GKE's agent does: the container log files it reads are the
// node's root's (the pod spec says so too).
func Dockerfile() string {
	return "FROM " + Base + "\n" +
		"COPY cloudburrow-logging-agent /cloudburrow-logging-agent\n" +
		"ENTRYPOINT [\"/cloudburrow-logging-agent\"]\n"
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
	dir, err := os.MkdirTemp("", "cloudburrow-logging-agent-image-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "cloudburrow-logging-agent"), bin, 0o755); err != nil {
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
