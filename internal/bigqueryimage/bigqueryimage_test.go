package bigqueryimage_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/bigqueryimage"
	"github.com/cloudburrow/cloudburrow/internal/bigqueryimage/bigqueryimagetest"
	"github.com/cloudburrow/cloudburrow/internal/storageimage"
	"github.com/cloudburrow/cloudburrow/internal/storageimage/storageimagetest"
)

// Version is the one third_party/bigquery-emulator/sources.json builds.
func TestVersionMatchesSources(t *testing.T) {
	raw, err := os.ReadFile("../../third_party/bigquery-emulator/sources.json")
	if err != nil {
		t.Fatal(err)
	}
	var s struct{ Version string }
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	if s.Version != bigqueryimage.Version {
		t.Errorf("Version = %s; sources.json builds %s", bigqueryimage.Version, s.Version)
	}
}

// The image is the storage image's pinned base plus the binary and its
// licences, run as a non-root user, and its tag is a content hash.
func TestDockerfileAndTag(t *testing.T) {
	d := bigqueryimage.Dockerfile()
	for _, want := range []string{"FROM " + storageimage.Base + "\n", "COPY licenses.txt ", "USER 65532:65532\n", `ENTRYPOINT ["/bigquery-emulator"]`} {
		if !strings.Contains(d, want) {
			t.Errorf("Dockerfile lacks %q:\n%s", want, d)
		}
	}
	a := bigqueryimage.Tag([]byte("one"), []byte("lic"))
	if a != bigqueryimage.Tag([]byte("one"), []byte("lic")) || !strings.HasPrefix(a, bigqueryimage.Repository+":") {
		t.Errorf("Tag = %s", a)
	}
	for _, other := range []string{bigqueryimage.Tag([]byte("two"), []byte("lic")), bigqueryimage.Tag([]byte("one"), []byte("other"))} {
		if other == a {
			t.Errorf("a different binary or licence bundle has the same tag %s", a)
		}
	}
}

func TestCheck(t *testing.T) {
	gz := bigqueryimagetest.Gzip
	elf := storageimagetest.ELF
	lic := gz([]byte("licences\n"))
	for _, tt := range []struct {
		name  string
		files map[string][]byte
		want  string // "" for present
	}{
		{"present", bigqueryimagetest.Files(), ""},
		{"missing", nil, "no linux/amd64 build"},
		{"not gzip", map[string][]byte{"bigquery-emulator-linux-amd64.gz": elf("amd64"), "bigquery-emulator-licenses.txt.gz": lic}, "not gzip"},
		{"empty", map[string][]byte{"bigquery-emulator-linux-amd64.gz": gz(nil), "bigquery-emulator-licenses.txt.gz": lic}, "is empty"},
		{"placeholder", map[string][]byte{"bigquery-emulator-linux-amd64.gz": gz([]byte("placeholder\n")), "bigquery-emulator-licenses.txt.gz": lic}, "not an executable"},
		{"wrong arch", map[string][]byte{"bigquery-emulator-linux-amd64.gz": gz(elf("arm64")), "bigquery-emulator-licenses.txt.gz": lic}, "not a linux/amd64 executable"},
		{"no licences", map[string][]byte{"bigquery-emulator-linux-amd64.gz": gz(elf("amd64"))}, "no licence bundle"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			bigqueryimagetest.Use(t, bigqueryimagetest.FS(tt.files))
			err := bigqueryimage.Check()["amd64"]
			switch {
			case tt.want == "" && err != nil:
				t.Errorf("Check = %v, want present", err)
			case tt.want != "" && (!errors.Is(err, bigqueryimage.ErrNotEmbedded) || !strings.Contains(err.Error(), tt.want)):
				t.Errorf("Check = %v, want ErrNotEmbedded naming %q", err, tt.want)
			}
		})
	}
}

type recordingRunner struct {
	calls  []string
	exists bool
	// binary is what the build context held when docker build ran.
	binary []byte
}

func (r *recordingRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	if len(args) > 1 && args[0] == "image" && args[1] == "inspect" && !r.exists {
		return "", errors.New("no such image")
	}
	if len(args) > 0 && args[0] == "build" {
		b, err := os.ReadFile(args[len(args)-1] + "/bigquery-emulator")
		if err != nil {
			return "", err
		}
		r.binary = b
		if d, err := os.ReadFile(args[len(args)-1] + "/Dockerfile"); err != nil || string(d) != bigqueryimage.Dockerfile() {
			return "", errors.New("the build context has no Dockerfile")
		}
	}
	return "", nil
}

// Build decompresses the embedded build into the context, tags it by
// content, builds once, and reuses an image that exists.
func TestBuild(t *testing.T) {
	bigqueryimagetest.Present(t)
	r := &recordingRunner{}
	tag, err := bigqueryimage.Build(context.Background(), r, "arm64")
	if err != nil || len(r.calls) != 2 || !strings.Contains(r.calls[1], "docker build --platform linux/arm64 -t "+tag) {
		t.Fatalf("Build = %s, %v; calls %v", tag, err, r.calls)
	}
	if want := bigqueryimage.Tag(storageimagetest.ELF("arm64"), []byte("licences\n")); tag != want || string(r.binary) != string(storageimagetest.ELF("arm64")) {
		t.Errorf("Build tagged %s (want %s) from %d bytes", tag, want, len(r.binary))
	}
	if got, err := bigqueryimage.TagFor("arm64"); err != nil || got != tag {
		t.Errorf("TagFor = %s, %v; Build tagged %s", got, err, tag)
	}
	again := &recordingRunner{exists: true}
	if _, err := bigqueryimage.Build(context.Background(), again, "arm64"); err != nil || len(again.calls) != 1 {
		t.Errorf("a second Build rebuilt: %v %v", again.calls, err)
	}
}

// A CLI built without the binaries says how to get them and runs nothing.
func TestBuildNotEmbedded(t *testing.T) {
	bigqueryimagetest.Missing(t)
	r := &recordingRunner{}
	if _, err := bigqueryimage.Build(context.Background(), r, "amd64"); !errors.Is(err, bigqueryimage.ErrNotEmbedded) || !strings.Contains(err.Error(), "make build") || len(r.calls) != 0 {
		t.Errorf("Build = %v, calls %v", err, r.calls)
	}
}

// The real embedded files, when `make bigquery-binaries` has run, are whole
// Linux builds: every byte decompresses and the header is right.
func TestEmbeddedBuilds(t *testing.T) {
	for _, arch := range bigqueryimage.Arches {
		if err := bigqueryimage.Check()[arch]; err != nil {
			if !errors.Is(err, bigqueryimage.ErrNotEmbedded) {
				t.Fatal(err)
			}
			t.Skipf("no embedded build in this CLI (make bigquery-binaries): %v", err)
		}
		if _, err := bigqueryimage.TagFor(arch); err != nil {
			t.Errorf("%s: %v", arch, err)
		}
	}
}
