package isoconformance

import (
	"encoding/json"
	"os"
	"testing"
)

// TestDependenciesRecordThePins: dependencies.json's
// components.testOracles.iso14496_26Conformance lists exactly Pins, with the
// same zips, sizes and checksums.
func TestDependenciesRecordThePins(t *testing.T) {
	b, err := os.ReadFile("../../dependencies.json")
	if err != nil {
		t.Fatal(err)
	}
	var inv struct {
		Components struct {
			TestOracles struct {
				ISO struct {
					Source   string           `json:"source"`
					ZipBytes map[string]int64 `json:"zipBytes"`
					Files    []struct {
						Zip    string `json:"zip"`
						Member string `json:"member"`
						Bytes  int64  `json:"bytes"`
						SHA256 string `json:"sha256"`
						MD5    string `json:"md5"`
					} `json:"files"`
				} `json:"iso14496_26Conformance"`
			} `json:"testOracles"`
		} `json:"components"`
	}
	if err := json.Unmarshal(b, &inv); err != nil {
		t.Fatal(err)
	}
	iso := inv.Components.TestOracles.ISO
	if iso.Source != BaseURL {
		t.Errorf("source %q, want %q", iso.Source, BaseURL)
	}
	if len(iso.Files) != len(Pins) {
		t.Errorf("dependencies.json lists %d files, Pins %d", len(iso.Files), len(Pins))
	}
	for _, f := range iso.Files {
		got := Pin{f.Zip, iso.ZipBytes[f.Zip], f.Member, f.Bytes, f.SHA256, f.MD5}
		if want := Pins[got.Name()]; got != want {
			t.Errorf("dependencies.json has %+v, Pins %+v", got, want)
		}
	}
}

// TestFileSkipsWhenNotCached: with downloads off and an empty cache, File
// skips rather than failing or reaching the network.
func TestFileSkipsWhenNotCached(t *testing.T) {
	t.Setenv(EnvDownloads, "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	ok := t.Run("fetch", func(t *testing.T) {
		File(t, "al_sbr_ps_06.mp4")
		t.Error("File returned without the file cached")
	})
	if !ok {
		t.Fatal("File failed instead of skipping")
	}
}
