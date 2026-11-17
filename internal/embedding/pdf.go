package embedding

import (
	"bytes"
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
// document_ocr reads the text PDFium finds on each page, which is the text a
// born-digital PDF holds. CloudBurrow runs no OCR engine, so a document whose
// pages hold no text (a scan) is refused with document_ocr rather than
// embedded without the text Google would have read.

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
	for i := 0; i < n; i++ {
		page := requests.Page{ByIndex: &requests.PageByIndex{Document: doc.Document, Index: i}}
		size, err := inst.FPDF_GetPageSizeByIndex(&requests.FPDF_GetPageSizeByIndex{Document: doc.Document, Index: i})
		if err != nil {
			return nil, fmt.Errorf("page %d: %v", i+1, err)
		}
		w := size.Width / 72 * pdfDPI
		h := size.Height / 72 * pdfDPI
		if w <= 0 || h <= 0 {
			return nil, fmt.Errorf("page %d has no area", i+1)
		}
		if px := w * h; px > pdfMaxPixels {
			f := math.Sqrt(pdfMaxPixels / px)
			w, h = w*f, h*f
		}
		r, err := inst.RenderPageInPixels(&requests.RenderPageInPixels{Page: page,
			Width: max(1, int(math.Round(w))), Height: max(1, int(math.Round(h)))})
		if err != nil {
			return nil, fmt.Errorf("rendering page %d: %v", i+1, err)
		}
		src := r.Result.RenderedImage
		if src == nil {
			src = r.Result.Image
		}
		// Copy the pixels: in WebAssembly they live only until Cleanup.
		img := image.NewNRGBA(src.Bounds())
		draw.Draw(img, img.Bounds(), src, src.Bounds().Min, draw.Src)
		r.Cleanup()
		p := PDFPage{Image: img}
		if text {
			t, err := inst.GetPageText(&requests.GetPageText{Page: page})
			if err != nil {
				return nil, fmt.Errorf("reading page %d's text: %v", i+1, err)
			}
			p.Text = strings.TrimSpace(t.Text)
		}
		pages = append(pages, p)
	}
	if text {
		empty := true
		for _, p := range pages {
			if p.Text != "" {
				empty = false
			}
		}
		if empty {
			return nil, fmt.Errorf("document_ocr: the PDF's pages hold no text, and CloudBurrow runs no OCR engine " +
				"to read a scanned page; send it without document_ocr to embed the pages as images")
		}
	}
	return pages, nil
}
