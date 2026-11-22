package vertexai

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloud.google.com/go/documentai/apiv1/documentaipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const docAIParentUS = "projects/p1/locations/us"

func newDocAI(t *testing.T, file string) *documentAIService {
	t.Helper()
	s := NewServer(nil)
	s.SetDocumentAIState(file)
	return &documentAIService{s: s}
}

func createProcessor(t *testing.T, d *documentAIService, typ, name string) *documentaipb.Processor {
	t.Helper()
	p, err := d.CreateProcessor(context.Background(), &documentaipb.CreateProcessorRequest{Parent: docAIParentUS,
		Processor: &documentaipb.Processor{Type: typ, DisplayName: name}})
	if err != nil {
		t.Fatalf("CreateProcessor %s: %v", typ, err)
	}
	return p
}

func code(err error) codes.Code { return status.Code(err) }

// TestDocumentAIProcessors: the served types, processors' lifecycle, their
// refusals and their persistence.
func TestDocumentAIProcessors(t *testing.T) {
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), "processors.json")
	d := newDocAI(t, file)
	types, err := d.FetchProcessorTypes(ctx, &documentaipb.FetchProcessorTypesRequest{Parent: docAIParentUS})
	if err != nil || len(types.GetProcessorTypes()) != 5 || types.ProcessorTypes[3].GetType() != "LAYOUT_PARSER_PROCESSOR" ||
		types.ProcessorTypes[4].GetName() != docAIParentUS+"/processorTypes/OCR_PROCESSOR" || !types.ProcessorTypes[4].GetAllowCreation() ||
		types.ProcessorTypes[0].GetType() != "CUSTOM_CLASSIFICATION_PROCESSOR" {
		t.Fatalf("FetchProcessorTypes: %v %v", types, err)
	}
	if _, err := d.FetchProcessorTypes(ctx, &documentaipb.FetchProcessorTypesRequest{Parent: "projects/p1/locations/mars"}); code(err) != codes.InvalidArgument {
		t.Errorf("unserved location: %v", err)
	}
	if pt, err := d.GetProcessorType(ctx, &documentaipb.GetProcessorTypeRequest{Name: docAIParentUS + "/processorTypes/OCR_PROCESSOR"}); err != nil || pt.GetCategory() != "OCR" {
		t.Errorf("GetProcessorType: %v %v", pt, err)
	}
	ocrP := createProcessor(t, d, "OCR_PROCESSOR", "ocr")
	if !strings.HasPrefix(ocrP.GetName(), docAIParentUS+"/processors/") || len(strings.TrimPrefix(ocrP.GetName(), docAIParentUS+"/processors/")) != 16 ||
		ocrP.GetState() != documentaipb.Processor_ENABLED || !strings.HasPrefix(ocrP.GetDefaultProcessorVersion(), ocrP.GetName()+"/processorVersions/pretrained-ocr") {
		t.Errorf("created: %v", ocrP)
	}
	for _, c := range []struct {
		typ, name string
		want      codes.Code
	}{{"FORM_PARSER_PROCESSOR", "f", codes.Unimplemented}, {"NO_SUCH_PROCESSOR", "n", codes.InvalidArgument},
		{"OCR_PROCESSOR", "", codes.InvalidArgument}, {"OCR_PROCESSOR", "ocr", codes.AlreadyExists}} {
		_, err := d.CreateProcessor(ctx, &documentaipb.CreateProcessorRequest{Parent: docAIParentUS,
			Processor: &documentaipb.Processor{Type: c.typ, DisplayName: c.name}})
		if code(err) != c.want {
			t.Errorf("CreateProcessor %s %q: %v, want %v", c.typ, c.name, err, c.want)
		}
	}
	layout := createProcessor(t, d, "LAYOUT_PARSER_PROCESSOR", "layout")
	list, err := d.ListProcessors(ctx, &documentaipb.ListProcessorsRequest{Parent: docAIParentUS, PageSize: 1})
	if err != nil || len(list.GetProcessors()) != 1 || list.GetNextPageToken() == "" {
		t.Fatalf("ListProcessors: %v %v", list, err)
	}
	op, err := d.DisableProcessor(ctx, &documentaipb.DisableProcessorRequest{Name: layout.GetName()})
	if err != nil || !op.GetDone() || op.GetResponse() == nil {
		t.Fatalf("DisableProcessor: %v %v", op, err)
	}
	_, err = d.ProcessDocument(ctx, &documentaipb.ProcessRequest{Name: layout.GetName(),
		Source: &documentaipb.ProcessRequest_RawDocument{RawDocument: &documentaipb.RawDocument{Content: []byte("x"), MimeType: "image/png"}}})
	if code(err) != codes.FailedPrecondition {
		t.Errorf("process on a disabled processor: %v", err)
	}

	// The processors survive a restart.
	d2 := newDocAI(t, file)
	got, err := d2.GetProcessor(ctx, &documentaipb.GetProcessorRequest{Name: layout.GetName()})
	if err != nil || got.GetState() != documentaipb.Processor_DISABLED || got.GetDisplayName() != "layout" {
		t.Fatalf("after reload: %v %v", got, err)
	}
	if op, err := d2.DeleteProcessor(ctx, &documentaipb.DeleteProcessorRequest{Name: ocrP.GetName()}); err != nil || !op.GetDone() {
		t.Fatalf("DeleteProcessor: %v %v", op, err)
	}
	if _, err := d2.GetProcessor(ctx, &documentaipb.GetProcessorRequest{Name: ocrP.GetName()}); code(err) != codes.NotFound {
		t.Errorf("deleted processor: %v", err)
	}
	if _, err := d2.DeleteProcessor(ctx, &documentaipb.DeleteProcessorRequest{Name: ocrP.GetName()}); code(err) != codes.NotFound {
		t.Errorf("second delete: %v", err)
	}
}

// TestDocumentAIProcess: the OCR processor and the layout parser read the
// scanned page with local Tesseract, in Document AI's shapes.
func TestDocumentAIProcess(t *testing.T) {
	ctx := context.Background()
	scan, err := os.ReadFile("../../ocr/testdata/scan.png")
	if err != nil {
		t.Fatal(err)
	}
	d := newDocAI(t, "")
	raw := &documentaipb.ProcessRequest_RawDocument{RawDocument: &documentaipb.RawDocument{Content: scan, MimeType: "image/png"}}
	ocrP := createProcessor(t, d, "OCR_PROCESSOR", "ocr")
	res, err := d.ProcessDocument(ctx, &documentaipb.ProcessRequest{Name: ocrP.GetDefaultProcessorVersion(), Source: raw,
		ProcessOptions: &documentaipb.ProcessOptions{OcrConfig: &documentaipb.OcrConfig{EnableSymbol: true}}})
	if err != nil {
		t.Fatalf("ProcessDocument OCR: %v", err)
	}
	doc := res.GetDocument()
	if !strings.Contains(doc.GetText(), "INVOICE No. 2026-0417") || len(doc.GetPages()) != 1 {
		t.Fatalf("OCR text: %q", doc.GetText())
	}
	pg := doc.Pages[0]
	if pg.GetPageNumber() != 1 || pg.GetDimension().GetUnit() != "pixels" || len(pg.GetBlocks()) == 0 || len(pg.GetParagraphs()) == 0 ||
		len(pg.GetLines()) == 0 || len(pg.GetTokens()) < 10 || len(pg.GetSymbols()) < 10 {
		t.Fatalf("page: %d blocks, %d paragraphs, %d lines, %d tokens, %d symbols", len(pg.GetBlocks()), len(pg.GetParagraphs()),
			len(pg.GetLines()), len(pg.GetTokens()), len(pg.GetSymbols()))
	}
	runes := []rune(doc.GetText())
	slice := func(a *documentaipb.Document_TextAnchor) string {
		s := a.GetTextSegments()[0]
		return string(runes[s.GetStartIndex():s.GetEndIndex()])
	}
	if got := slice(pg.Tokens[0].GetLayout().GetTextAnchor()); strings.TrimSpace(got) != "INVOICE" {
		t.Errorf("first token %q", got)
	}
	if got := slice(pg.Symbols[0].GetLayout().GetTextAnchor()); got != "I" {
		t.Errorf("first symbol %q", got)
	}
	if got := slice(pg.Lines[0].GetLayout().GetTextAnchor()); got != "INVOICE No. 2026-0417\n" {
		t.Errorf("first line %q", got)
	}
	if got := slice(pg.GetLayout().GetTextAnchor()); got != doc.GetText() {
		t.Errorf("the page's anchor is not the whole text")
	}
	v := pg.Tokens[0].GetLayout().GetBoundingPoly()
	if len(v.GetVertices()) != 4 || len(v.GetNormalizedVertices()) != 4 || v.NormalizedVertices[2].GetX() <= 0 || v.NormalizedVertices[2].GetX() > 1 {
		t.Errorf("token box: %v", v)
	}

	layout := createProcessor(t, d, "LAYOUT_PARSER_PROCESSOR", "layout")
	res, err = d.ProcessDocument(ctx, &documentaipb.ProcessRequest{Name: layout.GetName(), Source: raw,
		ProcessOptions: &documentaipb.ProcessOptions{LayoutConfig: &documentaipb.ProcessOptions_LayoutConfig{
			ChunkingConfig: &documentaipb.ProcessOptions_LayoutConfig_ChunkingConfig{ChunkSize: 20, IncludeAncestorHeadings: true}}}})
	if err != nil {
		t.Fatalf("ProcessDocument layout: %v", err)
	}
	doc = res.GetDocument()
	blocks, chunks := doc.GetDocumentLayout().GetBlocks(), doc.GetChunkedDocument().GetChunks()
	if doc.GetText() != "" || len(blocks) < 2 || len(chunks) < 2 || blocks[0].GetBlockId() != "1" ||
		blocks[0].GetTextBlock().GetType() != "paragraph" || blocks[0].GetPageSpan().GetPageStart() != 1 ||
		!strings.HasPrefix(blocks[0].GetTextBlock().GetText(), "INVOICE No. 2026-0417") {
		t.Fatalf("layout: %v", doc)
	}
	all := ""
	for i, c := range chunks {
		if c.GetChunkId() != fmt.Sprintf("c%d", i+1) || len(c.GetSourceBlockIds()) != 0 || c.GetPageSpan().GetPageEnd() != 1 {
			t.Errorf("chunk %d: %v", i, c)
		}
		if n := len(strings.Fields(c.GetContent())); n > 20 {
			t.Errorf("chunk %d has %d words over 20", i, n)
		}
		all += c.GetContent() + "\n\n"
	}
	if !strings.Contains(all, "INVOICE No. 2026-0417") {
		t.Errorf("chunks lose text: %q", all)
	}

	for name, req := range map[string]*documentaipb.ProcessRequest{
		"ocr_config on the layout parser": {Name: layout.GetName(), Source: raw, ProcessOptions: &documentaipb.ProcessOptions{OcrConfig: &documentaipb.OcrConfig{}}},
		"html on the OCR processor":       {Name: ocrP.GetName(), Source: &documentaipb.ProcessRequest_RawDocument{RawDocument: &documentaipb.RawDocument{Content: []byte("<p>x"), MimeType: "text/html"}}},
		"no document":                     {Name: ocrP.GetName()},
		"bad image":                       {Name: ocrP.GetName(), Source: &documentaipb.ProcessRequest_RawDocument{RawDocument: &documentaipb.RawDocument{Content: []byte("nope"), MimeType: "image/png"}}},
		"other version":                   {Name: ocrP.GetName() + "/processorVersions/rc", Source: raw},
		"native pdf parsing":              {Name: ocrP.GetName(), Source: raw, ProcessOptions: &documentaipb.ProcessOptions{OcrConfig: &documentaipb.OcrConfig{EnableNativePdfParsing: true}}},
		"page out of range": {Name: ocrP.GetName(), Source: raw, ProcessOptions: &documentaipb.ProcessOptions{PageRange: &documentaipb.ProcessOptions_IndividualPageSelector_{
			IndividualPageSelector: &documentaipb.ProcessOptions_IndividualPageSelector{Pages: []int32{2}}}}},
	} {
		want := map[string]codes.Code{"native pdf parsing": codes.Unimplemented, "other version": codes.NotFound}[name]
		if want == 0 {
			want = codes.InvalidArgument
		}
		if _, err := d.ProcessDocument(ctx, req); code(err) != want {
			t.Errorf("%s: %v, want %v", name, err, want)
		}
	}
}

// TestDocumentAIPageLimit: a PDF over the online limit is refused with its
// page count, and page_range selects pages.
func TestDocumentAIPageLimit(t *testing.T) {
	data, err := os.ReadFile("../../embedding/testdata/seven-pages.pdf")
	if err != nil {
		t.Fatal(err)
	}
	none := func(int) ([]int, error) { return nil, nil }
	err = eachDocPage(data, "application/pdf", docAIPageLimit{pages: 6}, none, nil)
	if code(grpcError(err)) != codes.InvalidArgument || !strings.Contains(err.Error(), "Document pages exceed the limit: 6 got 7") {
		t.Errorf("over the limit: %v", err)
	}
	err = eachDocPage(data, "application/pdf", docAIPageLimit{pages: 2, imagelessPages: 5}, none, nil)
	if err == nil || !strings.Contains(err.Error(), "Try using imageless mode to increase the limit to 5") {
		t.Errorf("over the online limit: %v", err)
	}
	err = eachDocPage(data, "application/pdf", docAIPageLimit{pages: 5, imageless: true}, none, nil)
	if err == nil || !strings.Contains(err.Error(), "in imageless mode exceed the limit: 5 got 7") {
		t.Errorf("over the imageless limit: %v", err)
	}
	// Every page is read, in order, pages streamed to the readers.
	pages, err := readPages(context.Background(), data, "application/pdf", docAIPageLimit{pages: bigQueryMaxPages}, nil, false)
	if err != nil || len(pages) != 7 {
		t.Fatalf("readPages: %d pages, %v", len(pages), err)
	}
	for i, pg := range pages {
		if pg.n != i+1 || pg.w == 0 || pg.png != nil {
			t.Errorf("page %d: n %d, %dx%d, png %d bytes", i+1, pg.n, pg.w, pg.h, len(pg.png))
		}
	}
	if l := limitsOf("EXPENSE_PROCESSOR"); l.online != 10 || l.batch != 10 {
		t.Errorf("Expense Parser limits: %+v", l)
	}
	if l := limitsOf("US_DRIVER_LICENSE_PROCESSOR"); l.online != 2 {
		t.Errorf("US Driver License Parser limits: %+v", l)
	}
	for _, c := range []struct {
		opts *documentaipb.ProcessOptions
		want string
	}{
		{nil, "[1 2 3 4 5 6 7]"},
		{&documentaipb.ProcessOptions{PageRange: &documentaipb.ProcessOptions_FromStart{FromStart: 2}}, "[1 2]"},
		{&documentaipb.ProcessOptions{PageRange: &documentaipb.ProcessOptions_FromEnd{FromEnd: 3}}, "[5 6 7]"},
		{&documentaipb.ProcessOptions{PageRange: &documentaipb.ProcessOptions_IndividualPageSelector_{
			IndividualPageSelector: &documentaipb.ProcessOptions_IndividualPageSelector{Pages: []int32{6, 2, 6}}}}, "[2 6]"},
	} {
		got, err := selectPages(7, c.opts)
		if err != nil || fmt.Sprint(got) != c.want {
			t.Errorf("selectPages(%v) = %v %v, want %s", c.opts, got, err, c.want)
		}
	}
}
