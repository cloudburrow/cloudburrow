package localai

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

func TestTessdataScriptsArePinned(t *testing.T) {
	p := TessdataFastScripts.Pin
	if len(p.Files) != 36 || !strings.Contains(p.BaseURL, p.Revision) {
		t.Fatalf("%d script models at %s; want 36 at revision %s", len(p.Files), p.BaseURL, p.Revision)
	}
	for _, f := range p.Files {
		if len(f.SHA256) != 64 || f.Bytes <= 0 || !strings.HasPrefix(f.Path, "script/") {
			t.Errorf("incomplete pin: %+v", f)
		}
	}
	f, ok := TessdataScript("Cyrillic")
	if !ok || TessdataFastScripts.Pin.URL(TessdataFastScripts.Repo, f) !=
		"https://raw.githubusercontent.com/tesseract-ocr/tessdata_fast/65727574dfcd264acbb0c3e07860e4e9e9b22185/script/Cyrillic.traineddata" {
		t.Errorf("Cyrillic: %+v, %v", f, ok)
	}
	if _, ok := TessdataScript("Latin"); ok {
		t.Error("script/Latin is pinned; eng reads Latin")
	}
}

// TestFetchPinnedFile fetches one file, keeps it, and refuses one whose
// bytes do not match the pin.
func TestFetchPinnedFile(t *testing.T) {
	body := "traineddata"
	m := Model{ID: "t", Repo: "r", Pin: &Pin{Revision: "abc", BaseURL: "https://example.invalid/abc",
		Files: []PinnedFile{{Path: "script/X.traineddata", Bytes: int64(len(body)),
			SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(body)))}}}}
	gets := 0
	old := HTTPGet
	t.Cleanup(func() { HTTPGet = old })
	HTTPGet = func(_ context.Context, url string) (io.ReadCloser, error) {
		gets++
		if url != "https://example.invalid/abc/script/X.traineddata" {
			t.Errorf("GET %s", url)
		}
		return io.NopCloser(strings.NewReader(body)), nil
	}
	c := Cache{Dir: t.TempDir()}
	for range 2 {
		p, err := c.FetchPinnedFile(context.Background(), m, m.Pin.Files[0])
		if err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(p); string(b) != body {
			t.Fatalf("%s holds %q", p, b)
		}
	}
	if gets != 1 {
		t.Errorf("%d GETs; want the cached file used the second time", gets)
	}
	body = "tampered!!!"
	m.Pin.Files[0].Path = "script/Y.traineddata"
	HTTPGet = func(context.Context, string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(body)), nil
	}
	if _, err := c.FetchPinnedFile(context.Background(), m, m.Pin.Files[0]); !errors.Is(err, ErrChecksumMismatch) {
		t.Errorf("a file that does not match its pin: %v; want ErrChecksumMismatch", err)
	}
}
