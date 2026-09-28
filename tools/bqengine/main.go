// Command bqengine builds the Linux BigQuery emulator binaries the CLI
// embeds (#1061): goccy/bigquery-emulator at the version
// third_party/bigquery-emulator pins, with CloudBurrow's patches to the SQL
// engine it links (goccy/googlesqlite and goccy/go-googlesql) applied.
//
//	go run ./tools/bqengine -out internal/bigqueryimage/bin
//
// Every source is a Go module at a pinned version, verified by checksum:
// the emulator and all its dependencies by third_party/bigquery-emulator's
// go.sum, and each patched module by the h1: sum sources.json records, both
// as the go command downloads it and as its files sit in the module cache.
// The patched copies are written under third_party/bigquery-emulator/build,
// which that module's go.mod replaces the two modules with, and the patches
// are applied strictly: a hunk that does not match exactly fails the build.
//
// It writes, per architecture, bigquery-emulator-linux-<arch>.gz and, once,
// bigquery-emulator-licenses.txt.gz: the licence and notice files of every
// module linked into the binary, which the image carries. A binary whose
// inputs (the pins, the patches, the go command's version) have not changed
// since the last build is kept, so `make build` stays quick.
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/mod/sumdb/dirhash"
)

// Sources is third_party/bigquery-emulator/sources.json.
type Sources struct {
	// Emulator is the upstream release built; its sum must be go.sum's.
	Emulator Module `json:"emulator"`
	// Patched are the modules replaced by patched copies.
	Patched []Patched `json:"patched"`
	// Tags are the build tags.
	Tags []string `json:"tags"`
	// Version is what the binary's --version prints.
	Version string `json:"version"`
}

// Module is one module at a pinned version.
type Module struct {
	Path    string `json:"path"`
	Version string `json:"version"`
	// Sum is the module's h1: hash, as go.sum records it.
	Sum string `json:"sum"`
	// Commit is the upstream commit the version tag names, for readers;
	// the build verifies Sum.
	Commit string `json:"commit"`
}

// Patched is a module CloudBurrow builds a patched copy of.
type Patched struct {
	Module
	// Dir is where the copy is written, relative to the wrapper module;
	// its go.mod replaces Path at Version with it.
	Dir string `json:"dir"`
	// Patches are applied in order, relative to the wrapper module.
	Patches []string `json:"patches"`
}

// Arches are the node architectures built by default, those the storage
// server is built for.
var defaultArches = "amd64,arm64"

// builderVersion is part of every build's input hash: bump it when a change
// to this tool changes what it writes.
const builderVersion = "1"

func main() {
	dir := flag.String("dir", "third_party/bigquery-emulator", "the wrapper module")
	out := flag.String("out", "internal/bigqueryimage/bin", "where the compressed binaries are written")
	arches := flag.String("arches", defaultArches, "comma-separated linux architectures")
	prepareOnly := flag.Bool("prepare-only", false, "write the patched module copies and stop")
	force := flag.Bool("force", false, "rebuild even when the inputs are unchanged")
	flag.Parse()
	if err := run(context.Background(), *dir, *out, strings.Split(*arches, ","), *prepareOnly, *force); err != nil {
		fmt.Fprintln(os.Stderr, "bqengine:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, dir, out string, arches []string, prepareOnly, force bool) error {
	src, err := loadSources(dir)
	if err != nil {
		return err
	}
	goVersion, err := goOutput(ctx, "", nil, "version")
	if err != nil {
		return err
	}
	inputs, err := inputHash(dir, src, goVersion)
	if err != nil {
		return err
	}
	if prepareOnly {
		return prepare(ctx, dir, src)
	}
	var todo []string
	for _, a := range arches {
		if a = strings.TrimSpace(a); a == "" {
			continue
		}
		if force || !upToDate(binaryPath(out, a), inputs+" "+a) {
			todo = append(todo, a)
		} else {
			fmt.Printf("bqengine: %s is up to date\n", binaryPath(out, a))
		}
	}
	licences := filepath.Join(out, LicencesName)
	licencesDone := !force && upToDate(licences, inputs)
	if len(todo) == 0 && licencesDone {
		return nil
	}
	if err := prepare(ctx, dir, src); err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	for _, a := range todo {
		if err := buildArch(ctx, dir, out, src, a, inputs+" "+a); err != nil {
			return err
		}
	}
	if !licencesDone {
		return writeLicences(ctx, dir, licences, inputs)
	}
	return nil
}

// LicencesName is the licence bundle's file name in the output directory.
const LicencesName = "bigquery-emulator-licenses.txt.gz"

func binaryPath(out, arch string) string {
	return filepath.Join(out, "bigquery-emulator-linux-"+arch+".gz")
}

func loadSources(dir string) (Sources, error) {
	var s Sources
	b, err := os.ReadFile(filepath.Join(dir, "sources.json"))
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("sources.json: %w", err)
	}
	if s.Emulator.Path == "" || s.Emulator.Version == "" || s.Emulator.Sum == "" || s.Version == "" {
		return s, errors.New("sources.json: emulator path, version and sum, and version, are required")
	}
	sum, err := os.ReadFile(filepath.Join(dir, "go.sum"))
	if err != nil {
		return s, err
	}
	// The emulator's sum is go.sum's, unless it is patched itself: go.sum
	// then records none, and download checks the pin instead.
	patchedEmulator := false
	for _, p := range s.Patched {
		if p.Path == s.Emulator.Path {
			patchedEmulator = true
			if p.Version != s.Emulator.Version || p.Sum != s.Emulator.Sum {
				return s, fmt.Errorf("sources.json: %s is patched at %s %s but built at %s %s", p.Path, p.Version, p.Sum, s.Emulator.Version, s.Emulator.Sum)
			}
		}
	}
	if line := s.Emulator.Path + " " + s.Emulator.Version + " " + s.Emulator.Sum; !patchedEmulator && !hasLine(sum, line) {
		return s, fmt.Errorf("go.sum does not record %q, which sources.json pins", line)
	}
	for _, p := range s.Patched {
		if p.Path == "" || p.Version == "" || !strings.HasPrefix(p.Sum, "h1:") || p.Dir == "" || len(p.Patches) == 0 {
			return s, fmt.Errorf("sources.json: %s: path, version, an h1: sum, dir and patches are required", p.Path)
		}
		if filepath.IsAbs(p.Dir) || !strings.HasPrefix(filepath.ToSlash(filepath.Clean(p.Dir)), "build/") {
			return s, fmt.Errorf("sources.json: %s: dir %q must be under build/", p.Path, p.Dir)
		}
	}
	return s, nil
}

func hasLine(b []byte, line string) bool {
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) == line {
			return true
		}
	}
	return false
}

// inputHash is a hash of everything a build depends on that is not already
// pinned by a checksum inside it.
func inputHash(dir string, src Sources, goVersion string) (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "bqengine %s\n%s\n", builderVersion, strings.TrimSpace(goVersion))
	files := []string{"sources.json", "go.mod", "go.sum"}
	for _, p := range src.Patched {
		files = append(files, p.Patches...)
	}
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s %d\n", f, len(b))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func stampPath(p string) string { return p + ".inputs" }

func upToDate(p, inputs string) bool {
	if _, err := os.Stat(p); err != nil {
		return false
	}
	b, err := os.ReadFile(stampPath(p))
	return err == nil && strings.TrimSpace(string(b)) == inputs
}

// prepare writes each patched module's copy: downloaded at its pinned
// version, verified, copied out of the read-only module cache, patched.
func prepare(ctx context.Context, dir string, src Sources) error {
	for _, p := range src.Patched {
		modDir, err := download(ctx, p.Module)
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, p.Dir)
		if err := os.RemoveAll(dst); err != nil {
			return err
		}
		if err := copyTree(modDir, dst); err != nil {
			return fmt.Errorf("copy %s: %w", p.Path, err)
		}
		for _, name := range p.Patches {
			b, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				return err
			}
			if err := applyPatch(dst, b); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			fmt.Printf("bqengine: applied %s to %s@%s\n", name, p.Path, p.Version)
		}
	}
	// The replacements must be in force: a version bump that left the
	// go.mod's versioned replace behind would silently build unpatched.
	for _, p := range src.Patched {
		b, err := goOutput(ctx, dir, nil, "list", "-mod=readonly", "-m", "-json", p.Path)
		if err != nil {
			return err
		}
		var m struct {
			Version string
			Replace *struct{ Path string }
		}
		if err := json.Unmarshal([]byte(b), &m); err != nil {
			return fmt.Errorf("go list -m %s: %w", p.Path, err)
		}
		want := "./" + filepath.ToSlash(filepath.Clean(p.Dir))
		if m.Version != p.Version || m.Replace == nil || m.Replace.Path != want {
			return fmt.Errorf("%s: the build selects %s, replaced by %v; want %s replaced by %s (third_party/bigquery-emulator/go.mod)", p.Path, m.Version, m.Replace, p.Version, want)
		}
	}
	return nil
}

// download fetches a module at its version through the go command, which
// checks it against the checksum database, and then checks both the
// download's hash and the files in the module cache against the pin.
func download(ctx context.Context, m Module) (string, error) {
	tmp, err := os.MkdirTemp("", "bqengine-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	// Outside any module, and with no GOFLAGS such as -insecure or
	// -modcacherw from the caller's environment.
	b, err := goOutput(ctx, tmp, []string{"GOFLAGS=", "GOWORK=off"}, "mod", "download", "-json", m.Path+"@"+m.Version)
	if err != nil {
		return "", err
	}
	var got struct {
		Dir, Sum, Error string
	}
	if err := json.Unmarshal([]byte(b), &got); err != nil {
		return "", fmt.Errorf("go mod download %s@%s: %w", m.Path, m.Version, err)
	}
	if got.Error != "" {
		return "", fmt.Errorf("go mod download %s@%s: %s", m.Path, m.Version, got.Error)
	}
	if got.Sum != m.Sum {
		return "", fmt.Errorf("%s@%s: downloaded sum %s, pinned %s", m.Path, m.Version, got.Sum, m.Sum)
	}
	onDisk, err := dirhash.HashDir(got.Dir, m.Path+"@"+m.Version, dirhash.Hash1)
	if err != nil {
		return "", err
	}
	if onDisk != m.Sum {
		return "", fmt.Errorf("%s@%s: the module cache's files hash to %s, pinned %s (was %s modified?)", m.Path, m.Version, onDisk, m.Sum, got.Dir)
	}
	return got.Dir, nil
}

// copyTree copies a module's files, which the module cache keeps read-only,
// as writable files.
func copyTree(from, to string) error {
	return filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, p)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: not a regular file", p)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if info.Mode()&0o111 != 0 {
			mode = 0o755
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, mode)
	})
}

// mainPackage is the command built.
func mainPackage(src Sources) string { return src.Emulator.Path + "/cmd/bigquery-emulator" }

func buildEnv(arch string) []string {
	return []string{"CGO_ENABLED=0", "GOOS=linux", "GOARCH=" + arch, "GOFLAGS=-mod=readonly", "GOWORK=off"}
}

func buildArch(ctx context.Context, dir, out string, src Sources, arch, inputs string) error {
	tmp, err := os.MkdirTemp("", "bqengine-build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	bin := filepath.Join(tmp, "bigquery-emulator")
	ldflags := fmt.Sprintf("-s -w -X main.version=%s -X main.revision=%s", src.Version, src.Emulator.Commit)
	args := []string{"build", "-trimpath", "-ldflags", ldflags, "-o", bin}
	if len(src.Tags) > 0 {
		args = append(args, "-tags", strings.Join(src.Tags, ","))
	}
	args = append(args, mainPackage(src))
	fmt.Printf("bqengine: building linux/%s\n", arch)
	if _, err := goOutput(ctx, dir, buildEnv(arch), args...); err != nil {
		return err
	}
	b, err := os.ReadFile(bin)
	if err != nil {
		return err
	}
	if err := checkELF(b, arch); err != nil {
		return err
	}
	return writeGzip(binaryPath(out, arch), b, inputs)
}

var machines = map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}

func checkELF(b []byte, arch string) error {
	f, err := elf.NewFile(bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("the linux/%s build is not an ELF executable: %w", arch, err)
	}
	if want, ok := machines[arch]; !ok || f.Machine != want {
		return fmt.Errorf("the linux/%s build is for %s", arch, f.Machine)
	}
	return nil
}

// writeGzip writes b compressed, then its inputs stamp, each through a
// temporary file, so an interrupted build never leaves a stamp beside a
// partial file.
func writeGzip(path string, b []byte, inputs string) error {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return err
	}
	if _, err := zw.Write(b); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	_ = os.Remove(stampPath(path))
	if err := writeAtomic(path, buf.Bytes()); err != nil {
		return err
	}
	fmt.Printf("bqengine: wrote %s (%d bytes, %d uncompressed)\n", path, buf.Len(), len(b))
	return writeAtomic(stampPath(path), []byte(inputs+"\n"))
}

func writeAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// licenceFile matches the files a module's licence terms are in.
var licenceFile = regexp.MustCompile(`(?i)^(licen[cs]e|copying|notice|patents)([._-].*)?$`)

// writeLicences bundles the licence and notice files at the root of every
// module linked into the linux/amd64 build (the arm64 build links the
// same modules), so the image carries every notice its binary's licences
// require.
func writeLicences(ctx context.Context, dir, path, inputs string) error {
	src, err := loadSources(dir)
	if err != nil {
		return err
	}
	args := []string{"list", "-deps", "-f", "{{with .Module}}{{.Path}}\t{{.Version}}\t{{with .Replace}}{{.Dir}}{{else}}{{.Dir}}{{end}}{{end}}"}
	if len(src.Tags) > 0 {
		args = append(args, "-tags", strings.Join(src.Tags, ","))
	}
	listed, err := goOutput(ctx, dir, buildEnv("amd64"), append(args, mainPackage(src))...)
	if err != nil {
		return err
	}
	type mod struct{ path, version, dir string }
	seen := map[string]mod{}
	for _, l := range strings.Split(listed, "\n") {
		f := strings.Split(l, "\t")
		if len(f) != 3 || f[0] == "" {
			continue
		}
		if !filepath.IsAbs(f[2]) {
			f[2] = filepath.Join(dir, f[2])
		}
		seen[f[0]] = mod{f[0], f[1], f[2]}
	}
	paths := make([]string, 0, len(seen))
	for p := range seen {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "Licences and notices of the Go modules linked into CloudBurrow's build of\n%s %s (%s), with CloudBurrow's patches to\n", src.Emulator.Path, src.Emulator.Version, src.Version)
	for _, p := range src.Patched {
		fmt.Fprintf(&buf, "%s %s", p.Path, p.Version)
		buf.WriteString(" and ")
	}
	buf.Truncate(buf.Len() - len(" and "))
	buf.WriteString(" (see cloudburrow/cloudburrow third_party/bigquery-emulator).\n")
	var missing []string
	for _, p := range paths {
		m := seen[p]
		entries, err := os.ReadDir(m.dir)
		if err != nil {
			return err
		}
		found := false
		for _, e := range entries {
			if e.Type().IsRegular() && licenceFile.MatchString(e.Name()) {
				b, err := os.ReadFile(filepath.Join(m.dir, e.Name()))
				if err != nil {
					return err
				}
				fmt.Fprintf(&buf, "\n==== %s %s: %s ====\n\n", m.path, m.version, e.Name())
				buf.Write(bytes.TrimRight(b, "\n"))
				buf.WriteString("\n")
				found = true
			}
		}
		if !found {
			missing = append(missing, p+" "+m.version)
		}
	}
	if len(missing) > 0 {
		// Some modules keep their licence below the root, or in a parent
		// module; name them rather than fail, so the gap is visible.
		fmt.Fprintf(&buf, "\n==== Modules with no licence file at their root ====\n\n%s\n", strings.Join(missing, "\n"))
		fmt.Printf("bqengine: %d module(s) have no licence file at their root: %s\n", len(missing), strings.Join(missing, ", "))
	}
	return writeGzip(path, buf.Bytes(), inputs)
}

// goOutput runs the go command in dir with extra environment and returns
// its standard output; a failure carries its standard error.
func goOutput(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("go %s: %w\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String(), nil
}
