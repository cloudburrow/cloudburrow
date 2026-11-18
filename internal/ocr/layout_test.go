package ocr

import (
	"context"
	"image"
	"image/png"
	"os"
	"strings"
	"testing"
)

// TestReadLayout: the scanned page's layout has its words, each word's
// symbols spelling it, and boxes inside the page in reading order.
func TestReadLayout(t *testing.T) {
	if testing.Short() {
		t.Skip("reads a page with Tesseract")
	}
	f, err := os.Open("testdata/scan.png")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ReadLayout(context.Background(), img, 300)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Blocks) == 0 {
		t.Fatal("no blocks")
	}
	b := img.Bounds()
	var words []string
	for _, bl := range res.Blocks {
		for _, p := range bl.Paragraphs {
			for _, w := range p.Words {
				var sb strings.Builder
				for _, s := range w.Symbols {
					sb.WriteString(s.Text)
				}
				words = append(words, sb.String())
				tl, br := w.Box[0], w.Box[2]
				if !tl.In(b) || br.X > b.Max.X || br.Y > b.Max.Y || tl.X >= br.X || tl.Y >= br.Y {
					t.Errorf("word %q box %v outside %v", sb.String(), w.Box, b)
				}
				if w.Confidence <= 0 || w.Confidence > 1 {
					t.Errorf("word %q confidence %v", sb.String(), w.Confidence)
				}
			}
		}
	}
	if got := strings.Join(words, " "); !strings.Contains(got, "INVOICE No. 2026-0417") {
		t.Errorf("words: %s", got)
	}
	if _, err := Read(context.Background(), image.NewGray(image.Rect(0, 0, 0, 0)), 300); err != nil {
		t.Fatal(err)
	}
}

func TestUnturn(t *testing.T) {
	// A 4×2 page turned 90 degrees counter-clockwise is 2×4: its point
	// (0, 0) was the original's top-right corner (4, 0).
	for turned, want := range map[int]image.Point{0: {0, 0}, 90: {4, 0}, 180: {4, 2}, 270: {0, 2}} {
		if got := unturn(image.Point{}, turned, 4, 2); got != want {
			t.Errorf("turned %d: %v, want %v", turned, got, want)
		}
	}
}
