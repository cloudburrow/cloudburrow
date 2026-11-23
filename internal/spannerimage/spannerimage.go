// Package spannerimage builds the image the in-cluster Spanner emulator runs
// from CloudBurrow's patched build of Google's Cloud Spanner emulator
// (#2029), as internal/bigqueryimage does for the BigQuery emulator.
//
// scripts/spanner-emulator-binaries.sh (`make spanner-binaries`, and the
// release's spanner-emulator job) compiles the emulator's source at the tag
// third_party/spanner-emulator/sources.json pins, with CloudBurrow's patches
// (a GQL RETURN ... ORDER BY's order, PROPERTY_EXISTS and the other
// documented graph predicates, COUNT of a group variable, ANY CHEAPEST), and
// the CLI embeds one archive per Linux architecture, named by Version. `up`
// writes the one matching the cluster's nodes onto the distroless base
// upstream's own image uses, pinned by digest, builds it with docker and
// loads it into kind. Its tag is the content hash of the archive and the
// Dockerfile, so the same CLI always yields the same image.
//
// A CLI built without the archive (a plain `go build`, or `make build`,
// which does not run the hours-long C++ build) runs Google's prebuilt
// image instead, with a warning naming what it gets wrong.
package spannerimage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/elf"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/cloudburrow/cloudburrow/internal/dockergc"
	"github.com/cloudburrow/cloudburrow/internal/storageimage"
)

// Upstream is the emulator release the patches apply to.
const Upstream = "1.5.58"

// PatchHash is the first 12 hex digits of the SHA-256 of the patches'
// bytes, concatenated in sources.json's order (emulator's, then
// GoogleSQL's), as the build script computes it.
const PatchHash = "7c2530f5a64e"

// Version names a build: the upstream release and the patches.
const Version = Upstream + "-cb." + PatchHash

// Base is the image the build is layered on: the distroless base upstream's
// release stage uses (glibc, no shell), pinned by digest
// (dependencies.json, build.spannerEmulatorBase).
const Base = "gcr.io/distroless/cc-debian12@sha256:e5d81ddde149641e2a9ba55be4545bc125c67de07508b03ba4c22e6eb0ded5aa"

// Repository is the local registry prefix the image is tagged under, one
// kubelet never tries to pull (see internal/images).
const Repository = "dev.local/cloudburrow-spanner-emulator"

// Arches are the node architectures a release embeds a build for.
var Arches = storageimage.Arches

// Files are what an archive holds, all of it: the emulator, the gateway
// that starts it, and the licence bundle. The emulator links libstdc++
// statically (third_party/spanner-emulator/Dockerfile.tail), where
// upstream's image carries the shared library.
var Files = []string{"emulator_main", "gateway_main", "licenses.txt.gz"}

//go:embed bin/*
var embedded embed.FS

var bins fs.FS = embedded

// UseBinaries makes this package read fsys, laid out like the embedded
// directory, until restore is called. It is for tests.
func UseBinaries(fsys fs.FS) (restore func()) {
	prev := bins
	bins = fsys
	return func() { bins = prev }
}

// ArchiveName is the embedded archive for arch.
func ArchiveName(arch string) string {
	return "spanner-emulator-" + Version + "-linux-" + arch + ".tar.gz"
}

// ErrNotEmbedded means this CLI has no patched build for an architecture.
var ErrNotEmbedded = errors.New("this cloudburrow embeds no patched Spanner emulator " +
	"(install a release, or run make spanner-binaries before go build)")

// NotEmbeddedError is why one architecture's build is unusable.
type NotEmbeddedError struct {
	Arch   string
	Reason string
}

func (e *NotEmbeddedError) Error() string { return ErrNotEmbedded.Error() + ": " + e.Reason }
func (e *NotEmbeddedError) Unwrap() error { return ErrNotEmbedded }

var machines = map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}

// extract reads arch's archive, writing each of Files into dir when dir is
// not "", and returns its tag. It fails unless the archive holds exactly
// Files, as regular files, with the two programs ELF files for arch.
func extract(arch, dir string) (string, error) {
	bad := func(format string, args ...any) error {
		return &NotEmbeddedError{Arch: arch, Reason: fmt.Sprintf(format, args...)}
	}
	if _, ok := machines[arch]; !ok {
		return "", bad("no build for linux/%s", arch)
	}
	f, err := bins.Open("bin/" + ArchiveName(arch))
	if err != nil {
		return "", bad("no linux/%s build of %s", arch, Version)
	}
	defer f.Close()
	h := sha256.New()
	h.Write([]byte(Dockerfile() + "\n"))
	zr, err := gzip.NewReader(io.TeeReader(f, h))
	if err != nil {
		return "", bad("the linux/%s build is not gzip: %v", arch, err)
	}
	tr := tar.NewReader(zr)
	seen := map[string]bool{}
	want := map[string]bool{}
	for _, name := range Files {
		want[name] = true
	}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", bad("the linux/%s build cannot be read: %v", arch, err)
		}
		if !want[hdr.Name] || seen[hdr.Name] || hdr.Typeflag != tar.TypeReg {
			return "", bad("the linux/%s build has an unexpected entry %q", arch, hdr.Name)
		}
		seen[hdr.Name] = true
		var w io.Writer = io.Discard
		var file *os.File
		if dir != "" {
			file, err = os.OpenFile(filepath.Join(dir, hdr.Name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
			if err != nil {
				return "", err
			}
			w = file
		}
		head := &headBuffer{max: 20}
		_, err = io.Copy(io.MultiWriter(w, head), tr)
		if file != nil {
			if cerr := file.Close(); err == nil {
				err = cerr
			}
		}
		if err != nil {
			return "", bad("the linux/%s build cannot be read: %v", arch, err)
		}
		if hdr.Name != "licenses.txt.gz" {
			if err := checkELF(arch, head.b); err != nil {
				return "", bad("%s: %v", hdr.Name, err)
			}
		}
	}
	for _, name := range Files {
		if !seen[name] {
			return "", bad("the linux/%s build has no %s", arch, name)
		}
	}
	// the rest of the gzip stream, so the hash covers the whole file
	if _, err := io.Copy(io.Discard, zr); err != nil {
		return "", bad("the linux/%s build cannot be read: %v", arch, err)
	}
	if _, err := io.Copy(h, f); err != nil {
		return "", bad("the linux/%s build cannot be read: %v", arch, err)
	}
	return Repository + ":" + Version + "-" + hex.EncodeToString(h.Sum(nil))[:12], nil
}

// checkELF fails unless hdr starts a little-endian ELF file for arch.
func checkELF(arch string, hdr []byte) error {
	if len(hdr) < 20 || !bytes.HasPrefix(hdr, []byte(elf.ELFMAG)) || hdr[elf.EI_DATA] != byte(elf.ELFDATA2LSB) {
		return errors.New("not an ELF file")
	}
	if m := elf.Machine(uint16(hdr[18]) | uint16(hdr[19])<<8); m != machines[arch] {
		return fmt.Errorf("built for %s, not linux/%s", m, arch)
	}
	return nil
}

type headBuffer struct {
	b   []byte
	max int
}

func (h *headBuffer) Write(p []byte) (int, error) {
	if n := h.max - len(h.b); n > 0 {
		h.b = append(h.b, p[:min(n, len(p))]...)
	}
	return len(p), nil
}

// Check reports, for each of Arches, whether this CLI embeds a usable
// build: a nil error means present.
func Check() map[string]error {
	out := make(map[string]error, len(Arches))
	for _, a := range Arches {
		_, err := extract(a, "")
		out[a] = err
	}
	return out
}

// TagFor is the tag of this CLI's build for arch.
func TagFor(arch string) (string, error) { return extract(arch, "") }

// Dockerfile is the whole image, laid out as upstream's release stage lays
// it out, so the Spanner pod's ./gateway_main command runs it.
func Dockerfile() string {
	return "FROM " + Base + "\n" +
		"WORKDIR /\n" +
		"COPY emulator_main gateway_main ./\n" +
		"COPY licenses.txt.gz ./\n" +
		"EXPOSE 9010 9020\n" +
		"CMD [\"./gateway_main\", \"--hostname\", \"0.0.0.0\"]\n"
}

// Runner runs docker; injected for tests.
type Runner = storageimage.Runner

// Build makes the image for arch unless it exists, and returns its tag. A
// new one is labelled for instance (dockergc.InstanceLabel), whose stop or
// delete removes it once no other instance uses it. An error wrapping
// ErrNotEmbedded means this CLI has no build for arch.
func Build(ctx context.Context, r Runner, arch, instance string) (string, error) {
	tag, err := TagFor(arch)
	if err != nil {
		return "", err
	}
	if _, err := r.Run(ctx, "docker", "image", "inspect", tag); err == nil {
		return tag, nil
	}
	dir, err := os.MkdirTemp("", "cloudburrow-spanner-image-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	if _, err := extract(arch, dir); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(Dockerfile()), 0o644); err != nil {
		return "", err
	}
	if out, err := r.Run(ctx, "docker", append(append([]string{"build", "--platform", "linux/" + arch}, dockergc.LabelArgs(instance)...), "-t", tag, dir)...); err != nil {
		return "", fmt.Errorf("build %s: %w\n%s", tag, err, out)
	}
	return tag, nil
}
