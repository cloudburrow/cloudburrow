package embedding

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/draw"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"

	"github.com/cloudburrow/cloudburrow/internal/ocr"
)

// PDF for EmbeddingGemma 2 (#1723), as Google documents it for
// gemini-embedding-2's EmbedContent: one PDF of at most 6 pages a request,
// each page "rendered as an image", and with embed_content_config.document_ocr
// the page's text added as text tokens. Pages are rendered with PDFium
// (BSD 3-Clause and Apache 2.0) compiled to WebAssembly and run on wazero,
// through github.com/klippa-app/go-pdfium (MIT), so no cgo is needed. A
// rendered page is an image like any other: the vision encoder's processor
// resizes it to its soft-token budget.
//
// document_ocr reads each page's text: the text layer PDFium finds when the
// page has one (a born-digital PDF), and otherwise (a scan) the text
// Tesseract OCR reads from the page rendered again at 300 DPI
// (internal/ocr, #1733). The text follows the page's image as text tokens,
// counted against the request's token limit like any other text.

// PDFMIMEType is the PDF type.
const PDFMIMEType = "application/pdf"

// IsPDFMIMEType reports whether mt is PDF.
func IsPDFMIMEType(mt string) bool { return baseMIME(mt) == PDFMIMEType }

// MaxPDFPages is the most pages one PDF may have: Google's documented limit
// for gemini-embedding-2.
const MaxPDFPages = 6

// pdfDPI is the resolution pages are rendered at, and pdfMaxPixels the most
// pixels a rendered page has (a larger page is rendered smaller). The vision
// encoder takes at most 2,520 patches of 16x16 pixels (about 645,000
// pixels), so either is ample.
const (
	pdfDPI       = 150
	pdfMaxPixels = 4 << 20
)

// ocrDPI is the resolution a page without a text layer is rendered at for
// OCR, the resolution Tesseract's models are trained for; ocrMaxPixels caps
// it (an A4 page at 300 DPI is 8.7 million pixels).
const (
	ocrDPI       = 300
	ocrMaxPixels = 16 << 20
)

// ocrTimeout bounds the OCR of one PDF.
const ocrTimeout = 5 * time.Minute

// PDFPage is one rendered page and, when asked for, its text.
type PDFPage struct {
	Image image.Image
	Text  string
}

var (
	pdfOnce sync.Once
	pdfPool pdfium.Pool
	pdfErr  error
)

// pdfInstance returns a PDFium instance, starting the WebAssembly runtime
// the first time.
func pdfInstance() (pdfium.Pdfium, error) {
	pdfOnce.Do(func() {
		pdfPool, pdfErr = webassembly.Init(webassembly.Config{MinIdle: 0, MaxIdle: 1, MaxTotal: 2})
	})
	if pdfErr != nil {
		return nil, fmt.Errorf("starting PDFium: %v", pdfErr)
	}
	return pdfPool.GetInstance(2 * time.Minute)
}

// DecodePDF renders each page of a PDF and, with text, reads its text.
func DecodePDF(mimeType string, data []byte, text bool) (pages []PDFPage, err error) {
	if !IsPDFMIMEType(mimeType) {
		return nil, fmt.Errorf("mime_type %q is not supported; send %s", mimeType, PDFMIMEType)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("the PDF data is empty")
	}
	if !bytes.Contains(data[:min(len(data), 1024)], []byte("%PDF-")) {
		return nil, fmt.Errorf("the data is not a PDF")
	}
	inst, err := pdfInstance()
	if err != nil {
		return nil, err
	}
	defer inst.Close()
	defer func() {
		if r := recover(); r != nil {
			pages, err = nil, fmt.Errorf("the data is not a valid PDF: %v", r)
		}
	}()
	doc, err := inst.OpenDocument(&requests.OpenDocument{File: &data})
	if err != nil {
		return nil, fmt.Errorf("the data is not a valid PDF: %v", err)
	}
	defer inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: doc.Document})
	count, err := inst.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: doc.Document})
	if err != nil {
		return nil, fmt.Errorf("the data is not a valid PDF: %v", err)
	}
	n := count.PageCount
	if n == 0 {
		return nil, fmt.Errorf("the PDF has no pages")
	}
	if n > MaxPDFPages {
		return nil, fmt.Errorf("the PDF has %d pages; at most %d pages are embedded", n, MaxPDFPages)
	}
	ctx, cancel := context.WithTimeout(context.Background(), ocrTimeout)
	defer cancel()
	for i := 0; i < n; i++ {
		page := requests.Page{ByIndex: &requests.PageByIndex{Document: doc.Document, Index: i}}
		size, err := inst.FPDF_GetPageSizeByIndex(&requests.FPDF_GetPageSizeByIndex{Document: doc.Document, Index: i})
		if err != nil {
			return nil, fmt.Errorf("page %d: %v", i+1, err)
		}
		img, err := renderPage(inst, page, size.Width, size.Height, pdfDPI, pdfMaxPixels)
		if err != nil {
			return nil, fmt.Errorf("page %d: %v", i+1, err)
		}
		p := PDFPage{Image: img}
		if text {
			t, err := inst.GetPageText(&requests.GetPageText{Page: page})
			if err != nil {
				return nil, fmt.Errorf("reading page %d's text: %v", i+1, err)
			}
			p.Text = strings.TrimSpace(t.Text)
			if p.Text == "" {
				// No text layer: a scanned page, read with OCR.
				hi, err := renderPage(inst, page, size.Width, size.Height, ocrDPI, ocrMaxPixels)
				if err != nil {
					return nil, fmt.Errorf("page %d: %v", i+1, err)
				}
				r, err := ocr.Read(ctx, hi, ocrDPI)
				if err != nil {
					return nil, fmt.Errorf("document_ocr: reading page %d: %v", i+1, err)
				}
				p.Text = r.Text
			}
		}
		pages = append(pages, p)
	}
	return pages, nil
}

// renderPage renders a page of w by h points at dpi, smaller when that would
// exceed maxPixels, and copies the pixels out of WebAssembly memory.
func renderPage(inst pdfium.Pdfium, page requests.Page, wPt, hPt float64, dpi, maxPixels float64) (image.Image, error) {
	w := wPt / 72 * dpi
	h := hPt / 72 * dpi
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("the page has no area")
	}
	if px := w * h; px > maxPixels {
		f := math.Sqrt(maxPixels / px)
		w, h = w*f, h*f
	}
	r, err := inst.RenderPageInPixels(&requests.RenderPageInPixels{Page: page,
		Width: max(1, int(math.Round(w))), Height: max(1, int(math.Round(h)))})
	if err != nil {
		return nil, fmt.Errorf("rendering: %v", err)
	}
	src := r.Result.RenderedImage
	if src == nil {
		src = r.Result.Image
	}
	// Copy the pixels: in WebAssembly they live only until Cleanup.
	img := image.NewNRGBA(src.Bounds())
	draw.Draw(img, img.Bounds(), src, src.Bounds().Min, draw.Src)
	r.Cleanup()
	return img, nil
}
