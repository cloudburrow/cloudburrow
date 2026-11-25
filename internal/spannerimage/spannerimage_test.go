package spannerimage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/cloudburrow/cloudburrow/internal/storageimage/storageimagetest"
)

// TestPatchHashMatchesThePatchSeries holds PatchHash to the patches
// sources.json lists, in order, as scripts/spanner-emulator-binaries.sh hashes
// them: a changed patch must change the archive's name, so a CLI ignores a
// build of other patches.
func TestPatchHashMatchesThePatchSeries(t *testing.T) {
	dir := filepath.Join("..", "..", "third_party", "spanner-emulator")
	raw, err := os.ReadFile(filepath.Join(dir, "sources.json"))
	if err != nil {
		t.Fatal(err)
	}
	var src struct {
		Emulator struct {
			Tag     string   `json:"tag"`
			Patches []string `json:"patches"`
		} `json:"emulator"`
		GoogleSQL struct {
			Patches []string `json:"patches"`
		} `json:"googlesql"`
	}
	if err := json.Unmarshal(raw, &src); err != nil {
		t.Fatal(err)
	}
	if src.Emulator.Tag != "v"+Upstream {
		t.Errorf("sources.json pins %s, Upstream is %s", src.Emulator.Tag, Upstream)
	}
	h := sha256.New()
	for _, p := range append(src.Emulator.Patches, src.GoogleSQL.Patches...) {
		b, err := os.ReadFile(filepath.Join(dir, p))
		if err != nil {
			t.Fatal(err)
		}
		h.Write(b)
	}
	if got := hex.EncodeToString(h.Sum(nil))[:12]; got != PatchHash {
		t.Errorf("the patches hash to %s; PatchHash is %s", got, PatchHash)
	}
}

// archive is a gzip tar of the named files, in order.
func archive(t *testing.T, files [][2]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f[0], Mode: 0o755, Size: int64(len(f[1])), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(f[1]))
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

func whole(arch string) [][2]string {
	elf := string(storageimagetest.ELF(arch))
	return [][2]string{{"emulator_main", elf}, {"gateway_main", elf}, {"licenses.txt.gz", "licences"}}
}

func use(t *testing.T, files map[string][]byte) {
	t.Helper()
	fsys := fstest.MapFS{"bin/README.md": {Data: []byte("x")}}
	for name, b := range files {
		fsys["bin/"+name] = &fstest.MapFile{Data: b}
	}
	t.Cleanup(UseBinaries(fsys))
}

type recordingRunner struct {
	exists bool
	calls  []string
	files  map[string]string
}

func (r *recordingRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	if args[0] == "image" {
		if r.exists {
			return "[]", nil
		}
		return "", errors.New("No such image")
	}
	dir := args[len(args)-1]
	r.files = map[string]string{}
	for _, f := range append([]string{"Dockerfile"}, Files...) {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return "", err
		}
		r.files[f] = string(b)
	}
	return "", nil
}

// Build writes the archive's files and a Dockerfile laid out as upstream's
// image is, tags the image by content, and reuses one that exists.
func TestBuild(t *testing.T) {
	use(t, map[string][]byte{ArchiveName("arm64"): archive(t, whole("arm64"))})
	r := &recordingRunner{}
	tag, err := Build(context.Background(), r, "arm64", "w1")
	if err != nil || len(r.calls) != 2 || !strings.Contains(r.calls[1], "docker build --platform linux/arm64 --label io.cloudburrow.instance=w1 -t "+tag) {
		t.Fatalf("Build = %s, %v; calls %v", tag, err, r.calls)
	}
	if !strings.HasPrefix(tag, Repository+":"+Version+"-") {
		t.Errorf("tag %s does not name %s", tag, Version)
	}
	for _, want := range []string{"FROM " + Base, `CMD ["./gateway_main", "--hostname", "0.0.0.0"]`} {
		if !strings.Contains(r.files["Dockerfile"], want) {
			t.Errorf("Dockerfile has no %q:\n%s", want, r.files["Dockerfile"])
		}
	}
	if r.files["emulator_main"] != string(storageimagetest.ELF("arm64")) || r.files["licenses.txt.gz"] != "licences" {
		t.Error("the build context does not hold the archive's files")
	}
	again := &recordingRunner{exists: true}
	if got, err := Build(context.Background(), again, "arm64", "w2"); err != nil || got != tag || len(again.calls) != 1 {
		t.Errorf("a second Build = %s, %v; calls %v", got, err, again.calls)
	}
	// another archive, another tag
	other := whole("arm64")
	other[2][1] = "other licences"
	use(t, map[string][]byte{ArchiveName("arm64"): archive(t, other)})
	if got, err := TagFor("arm64"); err != nil || got == tag {
		t.Errorf("TagFor another archive = %s, %v; the first was %s", got, err, tag)
	}
}

// Without a usable archive for the architecture, Build runs nothing and
// says ErrNotEmbedded, which up answers by running Google's image.
func TestBuildNotEmbedded(t *testing.T) {
	stale := strings.Replace(ArchiveName("amd64"), PatchHash, "000000000000", 1)
	cases := map[string]map[string][]byte{
		"none":             {},
		"other patches":    {stale: archive(t, whole("amd64"))},
		"other arch":       {ArchiveName("amd64"): archive(t, whole("arm64"))},
		"missing file":     {ArchiveName("amd64"): archive(t, whole("amd64")[:2])},
		"unexpected entry": {ArchiveName("amd64"): archive(t, append(whole("amd64"), [2]string{"../x", "y"}))},
		"not gzip":         {ArchiveName("amd64"): []byte("not gzip")},
	}
	for name, files := range cases {
		use(t, files)
		r := &recordingRunner{}
		if _, err := Build(context.Background(), r, "amd64", "w1"); !errors.Is(err, ErrNotEmbedded) || len(r.calls) != 0 {
			t.Errorf("%s: Build = %v, calls %v", name, err, r.calls)
		}
		if err := Check()["amd64"]; !errors.Is(err, ErrNotEmbedded) {
			t.Errorf("%s: Check = %v", name, err)
		}
	}
}

// The real embedded archives, when make spanner-binaries or the release
// put them there, are whole builds. CLOUDBURROW_REQUIRE_SPANNER_BUILDS=1
// (the release's build jobs) fails rather than skips without both.
func TestEmbeddedBuilds(t *testing.T) {
	require := os.Getenv("CLOUDBURROW_REQUIRE_SPANNER_BUILDS") == "1"
	for _, arch := range Arches {
		err := Check()[arch]
		switch {
		case err == nil:
		case require:
			t.Errorf("%s: %v", arch, err)
		case errors.Is(err, ErrNotEmbedded):
			t.Logf("%s: no embedded build (make spanner-binaries): %v", arch, err)
		default:
			t.Error(err)
		}
	}
}

// Base is dependencies.json's build.spannerEmulatorBase, the source of truth.
func TestBaseMatchesDependencies(t *testing.T) {
	raw, err := os.ReadFile("../../dependencies.json")
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Components map[string]map[string]json.RawMessage `json:"components"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	var b struct{ Image, Digest string }
	if err := json.Unmarshal(d.Components["build"]["spannerEmulatorBase"], &b); err != nil {
		t.Fatal(err)
	}
	if want := b.Image + "@" + b.Digest; Base != want {
		t.Errorf("Base = %s; dependencies.json has %s", Base, want)
	}
}
