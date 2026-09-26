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
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
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
var bins embed.FS

// ErrNotEmbedded means this CLI was built without the Linux storage
// binaries: a plain `go build` rather than `make build` or a release.
var ErrNotEmbedded = errors.New("this cloudburrow was built without the embedded storage server (run make build, or use a release)")

// Binary returns the embedded Linux build for a node architecture.
func Binary(arch string) ([]byte, error) {
	b, err := bins.ReadFile("bin/cloudburrow-storage-linux-" + arch)
	if err != nil {
		return nil, fmt.Errorf("%w: no linux/%s build", ErrNotEmbedded, arch)
	}
	return b, nil
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
