package storageimage_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/storageimage"
	"github.com/cloudburrow/cloudburrow/internal/storageimage/storageimagetest"
)

// A build with both binaries has both; Binary returns each unchanged.
func TestCheckBothPresent(t *testing.T) {
	storageimagetest.Present(t)
	for arch, err := range storageimage.Check() {
		if err != nil {
			t.Errorf("%s: %v", arch, err)
		}
	}
	b, err := storageimage.Binary("arm64")
	if err != nil || string(b) != string(storageimagetest.ELF("arm64")) {
		t.Errorf("Binary(arm64) = %d bytes, %v", len(b), err)
	}
}

// What a plain `go build ./cmd/cloudburrow` or `go install` embeds since
// #585: the README alone. Every architecture is ErrNotEmbedded, whose text
// names the fix, and Build refuses before it runs docker.
func TestCheckMissing(t *testing.T) {
	storageimagetest.Missing(t)
	for _, arch := range storageimage.Arches {
		err := storageimage.Check()[arch]
		if !errors.Is(err, storageimage.ErrNotEmbedded) || !strings.Contains(err.Error(), "no linux/"+arch+" build") {
			t.Errorf("%s: %v, want ErrNotEmbedded", arch, err)
		}
		for _, fix := range []string{"make build", "make storage-binaries", "release"} {
			if !strings.Contains(err.Error(), fix) {
				t.Errorf("%s: %q does not name %q", arch, err, fix)
			}
		}
	}
	r := &recorder{}
	if _, err := storageimage.Build(context.Background(), r, "amd64"); !errors.Is(err, storageimage.ErrNotEmbedded) || len(r.calls) != 0 {
		t.Errorf("Build = %v after %v, want ErrNotEmbedded and no docker", err, r.calls)
	}
}

// A file that is there but is no Linux executable for its architecture, an
// empty or placeholder file or a build for the other one, is not embedded
// either: an image built from it would fail only when its pod started.
func TestCheckPlaceholders(t *testing.T) {
	for _, tt := range []struct {
		name string
		data []byte
		want string
	}{
		{"empty", nil, "empty"},
		{"placeholder", []byte("placeholder\n"), "not an executable"},
		{"wrong architecture", storageimagetest.ELF("arm64"), "not a linux/amd64 executable"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			storageimagetest.Use(t, storageimagetest.FS(map[string][]byte{
				"cloudburrow-storage-linux-amd64": tt.data,
				"cloudburrow-storage-linux-arm64": storageimagetest.ELF("arm64"),
			}))
			got := storageimage.Check()
			if err := got["amd64"]; !errors.Is(err, storageimage.ErrNotEmbedded) || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("amd64: %v, want ErrNotEmbedded saying %q", err, tt.want)
			}
			if got["arm64"] != nil {
				t.Errorf("arm64: %v", got["arm64"])
			}
		})
	}
	if _, err := storageimage.Binary("riscv64"); !errors.Is(err, storageimage.ErrNotEmbedded) {
		t.Errorf("Binary(riscv64) = %v", err)
	}
}

// The real embed agrees with the files on disk: with no binaries in bin/,
// which is a checkout before `make storage-binaries` and so exactly what
// `go build ./cmd/cloudburrow` compiles in, every architecture is missing;
// with them, as after `make build`, every one passes. So the stand-ins the
// other tests use describe the build this one compiled.
func TestRealEmbedMatchesTheDisk(t *testing.T) {
	for _, arch := range storageimage.Arches {
		_, statErr := os.Stat(filepath.Join("bin", "cloudburrow-storage-linux-"+arch))
		err := storageimage.Check()[arch]
		switch {
		case os.IsNotExist(statErr) && !errors.Is(err, storageimage.ErrNotEmbedded):
			t.Errorf("%s: no file in bin/, but the embed reports %v", arch, err)
		case statErr == nil && err != nil:
			t.Errorf("%s: the build make storage-binaries wrote is refused: %v", arch, err)
		}
	}
}

type recorder struct{ calls []string }

func (r *recorder) Run(_ context.Context, name string, args ...string) (string, error) {
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	return "", nil
}
