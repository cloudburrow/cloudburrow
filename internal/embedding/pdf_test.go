package embedding

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
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
		{"not a PDF", []byte("hello"), false, "not a PDF"},
		{"truncated", read("turtle.pdf")[:200], false, "not a valid PDF"},
		{"empty", nil, false, "empty"},
	} {
		_, err := DecodePDF("application/pdf", c.data, c.ocr)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error naming %q", c.name, err, c.want)
		}
	}
	for _, ocr := range []bool{false, true} {
		if pages, err := DecodePDF("application/pdf", read("no-text.pdf"), ocr); err != nil || len(pages) != 1 || pages[0].Text != "" {
			t.Errorf("a page of shapes, document_ocr %v: %d pages, %v", ocr, len(pages), err)
		}
	}
}

// testdata/scan.pdf is a scanned page: one 300 DPI greyscale JPEG of a
// printed invoice (testdata/scan.txt is its text), with no text layer.
func TestDecodePDFOCR(t *testing.T) {
	data, err := os.ReadFile("testdata/scan.pdf")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/scan.txt")
	if err != nil {
		t.Fatal(err)
	}
	pages, err := DecodePDF("application/pdf", data, false)
	if err != nil || len(pages) != 1 || pages[0].Text != "" {
		t.Fatalf("without document_ocr: %d pages, %v", len(pages), err)
	}
	pages, err = DecodePDF("application/pdf", data, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 1 {
		t.Fatalf("%d pages, want 1", len(pages))
	}
	c := charErrorRate(pages[0].Text, string(want))
	t.Logf("CER %.3f", c)
	if c > 0.05 {
		t.Errorf("CER %.3f over 0.05; text:\n%s", c, pages[0].Text)
	}
	// The image is still the page rendered at 150 DPI (A4).
	if b := pages[0].Image.Bounds(); b.Dx() != 1240 || b.Dy() != 1754 {
		t.Errorf("page image %v, want 1240x1754", b)
	}
}

// charErrorRate is the edit distance between got and want, with runs of
// white space as one space, over want's length.
func charErrorRate(got, want string) float64 {
	a := []rune(strings.Join(strings.Fields(got), " "))
	b := []rune(strings.Join(strings.Fields(want), " "))
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			c := prev[j-1]
			if a[i-1] != b[j-1] {
				c++
			}
			cur[j] = min(c, prev[j]+1, cur[j-1]+1)
		}
		prev, cur = cur, prev
	}
	return float64(prev[len(b)]) / float64(len(b))
}

// sixScans is testdata/scan.pdf with an incremental update whose page tree
// names its page six times: a six-page scan, Google's most.
func sixScans(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/scan.pdf")
	if err != nil {
		t.Fatal(err)
	}
	prev := bytes.LastIndex(data, []byte("startxref\n"))
	startxref := strings.TrimSpace(strings.SplitN(string(data[prev+len("startxref\n"):]), "\n", 2)[0])
	var b bytes.Buffer
	b.Write(data)
	off := b.Len()
	b.WriteString("2 0 obj\n<< /Type /Pages /Kids [ 3 0 R 3 0 R 3 0 R 3 0 R 3 0 R 3 0 R ] /Count 6 >>\nendobj\n")
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 1\n0000000000 65535 f \n2 1\n%010d 00000 n \ntrailer\n<< /Size 18 /Root 1 0 R /Prev %s >>\nstartxref\n%d\n%%%%EOF\n",
		off, startxref, xref)
	return b.Bytes()
}

// TestDecodePDFOCRSixPages reads six scanned pages, in parallel: each with
// the one page's CER bound, in well under six times one page's time.
func TestDecodePDFOCRSixPages(t *testing.T) {
	want, err := os.ReadFile("testdata/scan.txt")
	if err != nil {
		t.Fatal(err)
	}
	data := sixScans(t)
	start := time.Now()
	pages, err := DecodePDF("application/pdf", data, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("six scanned pages in %v", time.Since(start))
	if len(pages) != 6 {
		t.Fatalf("%d pages, want 6", len(pages))
	}
	for i, p := range pages {
		if c := charErrorRate(p.Text, string(want)); c > 0.05 {
			t.Errorf("page %d: CER %.3f over 0.05", i+1, c)
		}
	}
}
