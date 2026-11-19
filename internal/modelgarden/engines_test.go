package modelgarden

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The pinned images are the ones dependencies.json records.
func TestEngineImagesMatchDependencies(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "dependencies.json"))
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Components struct {
			LocalAI map[string]json.RawMessage `json:"localai"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"modelGardenVLLM": VLLMImage, "modelGardenTEI": TEIImage} {
		var e struct{ Image, Digest, License string }
		if err := json.Unmarshal(d.Components.LocalAI[key], &e); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if got := e.Image + "@" + e.Digest; got != want || e.License != "Apache-2.0" {
			t.Errorf("dependencies.json %s = %s (%s); the engine runs %s", key, got, e.License, want)
		}
	}
}

func TestEngineForTask(t *testing.T) {
	for tag, want := range map[string]string{"text-generation": EngineVLLM, "text2text-generation": EngineVLLM,
		"image-text-to-text": EngineVLLM, "feature-extraction": EngineTEI, "sentence-similarity": EngineTEI} {
		if e, err := EngineFor(tag); err != nil || e.Name != want {
			t.Errorf("EngineFor(%q) = %v, %v; want %s", tag, e.Name, err, want)
		}
	}
	for _, tag := range []string{"", "text-to-image", "automatic-speech-recognition"} {
		if _, err := EngineFor(tag); err == nil {
			t.Errorf("EngineFor(%q) succeeded", tag)
		}
	}
}
