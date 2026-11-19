package localai

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestLandmarkClassifiersArePinned: six continents, each an archive at
// Kaggle's public download of Google's google/landmarks variation, with
// its SHA-256 and size, and the model file it holds pinned too.
func TestLandmarkClassifiersArePinned(t *testing.T) {
	want := []string{"africa", "asia", "europe", "north-america", "oceania-antarctica", "south-america"}
	if len(LandmarkClassifiers) != len(want) {
		t.Fatalf("%d classifiers", len(LandmarkClassifiers))
	}
	for i, l := range LandmarkClassifiers {
		m := l.Model
		if l.Region != want[i] || !m.Official || m.Publisher != PublisherGoogle || m.License != "apache-2.0" || m.Pin == nil || len(m.Pin.Files) != 1 {
			t.Fatalf("%s: %+v", l.Region, l)
		}
		f := m.Pin.Files[0]
		if len(f.SHA256) != 64 || f.Bytes < 10<<20 || len(l.TFLite.SHA256) != 64 || l.TFLite.Bytes <= f.Bytes {
			t.Errorf("%s: incomplete pin: %+v %+v", l.Region, f, l.TFLite)
		}
		if got := m.Pin.URL(m.Repo, f); got != "https://www.kaggle.com/api/v1/models/google/landmarks/tfLite/classifier-"+l.Region+"-v1/1/download" {
			t.Errorf("%s: URL %s", l.Region, got)
		}
	}
}

// TestExtractLandmarkModel: 1.tflite is taken from the archive only when
// its size and SHA-256 match; another member or a wrong file is refused.
func TestExtractLandmarkModel(t *testing.T) {
	dir := t.TempDir()
	body := []byte("not really a model")
	sum := sha256.Sum256(body)
	archive := func(name string, data []byte) string {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(zw)
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(data)
		_ = tw.Close()
		_ = zw.Close()
		p := filepath.Join(dir, name+".tgz")
		if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	want := PinnedFile{Path: "m.tflite", Bytes: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}
	dst := filepath.Join(dir, "m.tflite")
	if err := extractLandmarkModel(archive("1.tflite", body), dst, want); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); !bytes.Equal(b, body) {
		t.Errorf("extracted %q", b)
	}
	if err := extractLandmarkModel(archive("2.tflite", body), filepath.Join(dir, "x"), want); err == nil {
		t.Error("an archive without 1.tflite was accepted")
	}
	bad := append([]byte(nil), body...)
	bad[0] = 'N'
	if err := extractLandmarkModel(archive("1.tflite", bad), filepath.Join(dir, "y"), want); !errors.Is(err, ErrChecksumMismatch) {
		t.Errorf("a wrong file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "y")); err == nil {
		t.Error("a wrong file was kept")
	}
}
