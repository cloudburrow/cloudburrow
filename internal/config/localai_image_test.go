package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A dev build defaults to what `make litert-lm` builds; a release build to
// the image its own release published, by digest (#602).
func TestLocalAIImageDefault(t *testing.T) {
	if got := LocalAIImage(); got != DefaultLocalAIImage {
		t.Errorf("unstamped LocalAIImage() = %q, want %q", got, DefaultLocalAIImage)
	}

	old := releaseLocalAIImage
	t.Cleanup(func() { releaseLocalAIImage = old })
	stamped := PublishedLocalAIRepository + "@sha256:" + strings.Repeat("a", 64)
	releaseLocalAIImage = stamped
	if got := LocalAIImage(); got != stamped {
		t.Errorf("stamped LocalAIImage() = %q, want %q", got, stamped)
	}
}

// The release workflow must stamp the variable this package reads, with the
// repository it pushes to, pinned by digest. A rename on either side would
// otherwise ship release CLIs silently defaulting to the unpublished :local
// tag.
func TestReleaseWorkflowStampsLocalAIImage(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	wf := string(raw)
	for _, want := range []string{
		"github.com/cloudburrow/cloudburrow/internal/config.releaseLocalAIImage=",
		"LOCAL_AI_IMAGE: " + PublishedLocalAIRepository,
		"${LOCAL_AI_IMAGE}@${LOCAL_AI_DIGEST}",
	} {
		if !strings.Contains(wf, want) {
			t.Errorf("release.yml does not contain %q", want)
		}
	}
}

// dependencies.json records the published image, with no digest: each
// release has its own, recorded in its notes and stamped into its CLI.
func TestPublishedLocalAIImageMatchesDependencies(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "dependencies.json"))
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Components map[string]map[string]json.RawMessage `json:"components"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	var img struct {
		Image  string  `json:"image"`
		Digest *string `json:"digest"`
	}
	if err := json.Unmarshal(d.Components["localai"]["runtimeImage"], &img); err != nil {
		t.Fatal(err)
	}
	if img.Image != PublishedLocalAIRepository {
		t.Errorf("dependencies.json localai.runtimeImage.image = %q, want %q", img.Image, PublishedLocalAIRepository)
	}
	if img.Digest != nil {
		t.Errorf("localai.runtimeImage.digest = %q; each release has its own, so none is recorded here", *img.Digest)
	}
}
