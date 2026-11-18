package embedding

import (
	"os"
	"strings"
	"testing"
)

// The PDFs in testdata are written by testdata/mkpdf.py (python3 mkpdf.py
// testdata): pages of Helvetica text, and one page of shapes with no text.

func TestDecodePDF(t *testing.T) {
	data, err := os.ReadFile("testdata/two-pages.pdf")
	if err != nil {
		t.Fatal(err)
	}
	pages, err := DecodePDF("application/pdf", data, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 2 {
		t.Fatalf("%d pages, want 2", len(pages))
	}
	for i, p := range pages {
		// US Letter at 150 DPI.
		if b := p.Image.Bounds(); b.Dx() != 1275 || b.Dy() != 1650 {
			t.Errorf("page %d is %v, want 1275x1650", i+1, b)
		}
	}
	// White paper, black text near the top left.
	if r, g, b, _ := pages[0].Image.At(5, 5).RGBA(); r>>8 != 255 || g>>8 != 255 || b>>8 != 255 {
		t.Errorf("the page's corner is %d,%d,%d, want white", r>>8, g>>8, b>>8)
	}
	dark := 0
	for y := 150; y < 260; y++ {
		for x := 120; x < 900; x++ {
			if r, _, _, _ := pages[0].Image.At(x, y).RGBA(); r>>8 < 64 {
				dark++
			}
		}
	}
	if dark < 1000 {
		t.Errorf("%d dark pixels where the first line is, want the text rendered", dark)
	}
	if pages[0].Text != "Cats sleeping on a couch." || !strings.Contains(pages[1].Text, "sea turtle") {
		t.Errorf("texts %q, %q", pages[0].Text, pages[1].Text)
	}

	noText, _ := DecodePDF("application/pdf", data, false)
	if len(noText) != 2 || noText[0].Text != "" {
		t.Errorf("without document_ocr: %d pages, text %q", len(noText), noText[0].Text)
	}
}

func TestDecodePDFRefuses(t *testing.T) {
	read := func(name string) []byte {
		b, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, c := range []struct {
		name string
		data []byte
		ocr  bool
		want string
	}{
		{"seven pages", read("seven-pages.pdf"), false, "7 pages; at most 6"},
		{"scan with document_ocr", read("no-text.pdf"), true, "no OCR engine"},
		{"not a PDF", []byte("hello"), false, "not a PDF"},
		{"truncated", read("turtle.pdf")[:200], false, "not a valid PDF"},
		{"empty", nil, false, "empty"},
	} {
		_, err := DecodePDF("application/pdf", c.data, c.ocr)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error naming %q", c.name, err, c.want)
		}
	}
	if pages, err := DecodePDF("application/pdf", read("no-text.pdf"), false); err != nil || len(pages) != 1 {
		t.Errorf("a page with no text, without document_ocr: %d pages, %v", len(pages), err)
	}
}
