// Package bigqueryimage builds the image the in-cluster BigQuery emulator
// runs (#1061). CloudBurrow no longer pulls goccy/bigquery-emulator's image:
// `make build` and each release compile the emulator's v0.8.1 source, with
// CloudBurrow's patches to the SQL engine it links, from Go modules pinned
// and verified by checksum (tools/bqengine, third_party/bigquery-emulator),
// and the CLI embeds the Linux builds, compressed. `up` writes the one
// matching the cluster's nodes onto the distroless base the storage image
// uses, pinned by digest, builds it with docker and loads it into kind, so
// nothing is downloaded but that base. Its tag is the content hash of the
// binary, the licence bundle and the base, so the same CLI always yields
// the same image, and a rebuild is skipped when the image already exists.
package bigqueryimage

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/elf"
	"embed"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/cloudburrow/cloudburrow/internal/storageimage"
)

// Base is the image the binary is layered on: the storage image's distroless
// static base (dependencies.json, build.storageServerBase), which carries
// CA certificates and time zone data and no shell.
const Base = storageimage.Base

// Repository is the local registry prefix the image is tagged under, one
// kubelet never tries to pull (see internal/images).
const Repository = "dev.local/cloudburrow-bigquery"

// Version is the emulator this CLI's builds are of: upstream's release and
// CloudBurrow's patch level (third_party/bigquery-emulator/sources.json,
// which a test holds it to).
const Version = "v0.8.1-cloudburrow.1"

//go:embed bin/*
var embedded embed.FS

// bins is what this package reads: the embedded directory, or a stand-in a test
// installs with UseBinaries.
var bins fs.FS = embedded

// Arches are the node architectures `make bigquery-binaries` and a release
// embed a build for.
var Arches = storageimage.Arches

// LicencesFile is the embedded bundle of the licence and notice files of
// every module linked into the binary; the image carries it.
const LicencesFile = "bin/bigquery-emulator-licenses.txt.gz"

// ErrNotEmbedded means this CLI was built without the Linux BigQuery
// emulator: a plain `go build` or `go install` rather than `make build` or
// a release. Its error names the fix.
var ErrNotEmbedded = errors.New("this cloudburrow was built without the embedded BigQuery emulator " +
	"(run make build, or make bigquery-binaries before go build, or install a release)")

// NotEmbeddedError is why one architecture's build is unusable. It is
// ErrNotEmbedded, so errors.Is finds it.
type NotEmbeddedError struct {
	Arch   string
	Reason string
}

func (e *NotEmbeddedError) Error() string { return ErrNotEmbedded.Error() + ": " + e.Reason }
func (e *NotEmbeddedError) Unwrap() error { return ErrNotEmbedded }

var machines = map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}

// open returns the embedded build for arch, decompressing as it is read.
// The build is about 200 MB uncompressed, so nothing here holds it in
// memory: the CLI that runs `up` stays running.
func open(arch string) (io.ReadCloser, error) {
	f, err := bins.Open("bin/bigquery-emulator-linux-" + arch + ".gz")
	if err != nil {
		return nil, &NotEmbeddedError{Arch: arch, Reason: fmt.Sprintf("no linux/%s build", arch)}
	}
	zr, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, &NotEmbeddedError{Arch: arch, Reason: fmt.Sprintf("the linux/%s build is not gzip: %v", arch, err)}
	}
	return struct {
		io.Reader
		io.Closer
	}{zr, f}, nil
}

// checkHeader reads an ELF header from r and fails unless it is a Linux
// executable for arch: an image made from anything else would build and
// then fail only when its pod started.
func checkHeader(arch string, hdr []byte) error {
	notEmbedded := func(format string, args ...any) error {
		return &NotEmbeddedError{Arch: arch, Reason: fmt.Sprintf(format, args...)}
	}
	if len(hdr) == 0 {
		return notEmbedded("the linux/%s build is empty", arch)
	}
	if len(hdr) < 20 || !bytes.HasPrefix(hdr, []byte(elf.ELFMAG)) || hdr[elf.EI_DATA] != byte(elf.ELFDATA2LSB) {
		return notEmbedded("the linux/%s build is not an executable", arch)
	}
	typ := elf.Type(binary.LittleEndian.Uint16(hdr[16:]))
	machine := elf.Machine(binary.LittleEndian.Uint16(hdr[18:]))
	if want, ok := machines[arch]; !ok || machine != want || (typ != elf.ET_EXEC && typ != elf.ET_DYN) {
		return notEmbedded("the linux/%s build is a %s %s, not a linux/%s executable", arch, machine, typ, arch)
	}
	return nil
}

// headerSize is the part of an ELF file checkHeader reads.
const headerSize = 64

// check opens arch's build and checks its header, reading only that.
func check(arch string) error {
	r, err := open(arch)
	if err != nil {
		return err
	}
	defer r.Close()
	hdr := make([]byte, headerSize)
	n, err := io.ReadFull(r, hdr)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return &NotEmbeddedError{Arch: arch, Reason: fmt.Sprintf("the linux/%s build cannot be read: %v", arch, err)}
	}
	return checkHeader(arch, hdr[:n])
}

// Licences returns the licence bundle, decompressed.
func Licences() ([]byte, error) {
	f, err := bins.Open(LicencesFile)
	if err != nil {
		return nil, &NotEmbeddedError{Reason: "no licence bundle"}
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, &NotEmbeddedError{Reason: "the licence bundle is not gzip"}
	}
	b, err := io.ReadAll(zr)
	if err != nil || len(b) == 0 {
		return nil, &NotEmbeddedError{Reason: "the licence bundle is not gzip text"}
	}
	return b, nil
}

// Check reports, for each of Arches, whether this CLI embeds a usable build
// and the licence bundle: a nil error means present. It reads each build's
// header only; Build verifies the whole file as it writes it.
func Check() map[string]error {
	out := make(map[string]error, len(Arches))
	_, lerr := Licences()
	for _, a := range Arches {
		err := check(a)
		if err == nil && lerr != nil {
			err = lerr
		}
		out[a] = err
	}
	return out
}

// UseBinaries makes this package read fsys, laid out like the embedded
// directory (bin/bigquery-emulator-linux-<arch>.gz and the licence bundle),
// until restore is called. It is for tests; it is not safe to call while
// another goroutine reads a build.
func UseBinaries(fsys fs.FS) (restore func()) {
	prev := bins
	bins = fsys
	return func() { bins = prev }
}

// newTagHash starts the hash a tag is the prefix of: the Dockerfile and the
// licence bundle; the binary follows.
func newTagHash(licences []byte) hash.Hash {
	h := sha256.New()
	h.Write([]byte(Dockerfile() + "\n"))
	fmt.Fprintf(h, "%d\n", len(licences))
	h.Write(licences)
	return h
}

func tagOf(h hash.Hash) string { return Repository + ":" + hex.EncodeToString(h.Sum(nil))[:16] }

// Tag is the image reference for a binary and licence bundle on Base.
func Tag(binary, licences []byte) string {
	h := newTagHash(licences)
	h.Write(binary)
	return tagOf(h)
}

// copyBuild decompresses arch's build into w and returns its tag, failing
// unless it is whole (gzip's checksum) and a Linux executable for arch.
func copyBuild(arch string, licences []byte, w io.Writer) (string, error) {
	r, err := open(arch)
	if err != nil {
		return "", err
	}
	defer r.Close()
	h := newTagHash(licences)
	hdr := &headBuffer{max: headerSize}
	if _, err := io.Copy(io.MultiWriter(w, h, hdr), r); err != nil {
		return "", &NotEmbeddedError{Arch: arch, Reason: fmt.Sprintf("the linux/%s build cannot be read: %v", arch, err)}
	}
	if err := checkHeader(arch, hdr.b); err != nil {
		return "", err
	}
	return tagOf(h), nil
}

// headBuffer keeps the first max bytes written to it.
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

// TagFor is the tag of this CLI's build for arch.
func TagFor(arch string) (string, error) {
	lic, err := Licences()
	if err != nil {
		return "", err
	}
	return copyBuild(arch, lic, io.Discard)
}

// Runner runs docker; injected for tests.
type Runner = storageimage.Runner

// Path is where the image has the emulator: /bin/bigquery-emulator, as
// upstream's image has it, so anything that starts it by that path (a
// Deployment's command) finds it in either.
const Path = "/bin/bigquery-emulator"

// Dockerfile is the whole image: the binary and its licences on the pinned
// base, as a non-root user, with the emulator as the entrypoint, as
// upstream's image has it, so the Deployment's args are its flags.
func Dockerfile() string {
	return "FROM " + Base + "\n" +
		"COPY bigquery-emulator " + Path + "\n" +
		"COPY licenses.txt /licenses/bigquery-emulator.txt\n" +
		"USER 65532:65532\n" +
		"ENTRYPOINT [\"" + Path + "\"]\n"
}

// Build makes the image for arch unless it exists, and returns its tag.
func Build(ctx context.Context, r Runner, arch string) (string, error) {
	lic, err := Licences()
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "cloudburrow-bigquery-image-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	f, err := os.OpenFile(filepath.Join(dir, "bigquery-emulator"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return "", err
	}
	tag, err := copyBuild(arch, lic, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	if _, err := r.Run(ctx, "docker", "image", "inspect", tag); err == nil {
		return tag, nil
	}
	if err := os.WriteFile(filepath.Join(dir, "licenses.txt"), lic, 0o644); err != nil {
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
