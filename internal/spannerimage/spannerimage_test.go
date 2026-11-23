package spannerimage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestPatchHashMatchesThePatchSeries holds PatchHash to the patches
// sources.json lists, in order, as scripts/spanner-emulator-image.sh hashes
// them: a changed patch must change the image's tag.
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

type fakeRunner struct {
	out string
	err error
}

func (f fakeRunner) Run(context.Context, string, ...string) (string, error) { return f.out, f.err }

func TestAvailable(t *testing.T) {
	ctx := context.Background()
	if !Available(ctx, fakeRunner{out: "linux/arm64\n"}, "arm64") {
		t.Error("an arm64 image for an arm64 node: want available")
	}
	if Available(ctx, fakeRunner{out: "linux/amd64\n"}, "arm64") {
		t.Error("an amd64 image for an arm64 node: want unavailable")
	}
	if Available(ctx, fakeRunner{err: errors.New("No such image")}, "arm64") {
		t.Error("no image: want unavailable")
	}
}
