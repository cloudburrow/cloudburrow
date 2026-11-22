package vertexai

import (
	"context"
	"image"
	"strings"
	"testing"

	"cloud.google.com/go/documentai/apiv1/documentaipb"

	"github.com/cloudburrow/cloudburrow/internal/ocr"
	"github.com/cloudburrow/cloudburrow/internal/officedoc"
)

// TestLayoutTree (#1799): headings nest the blocks after them, a deeper
// heading inside a shallower one; lists and tables keep their structure;
// page headers and footers stay at the top; ids are in document order and
// a heading's page span reaches its last child's page.
func TestLayoutTree(t *testing.T) {
	cell := func(s string) officedoc.Cell {
		return officedoc.Cell{Blocks: []officedoc.Block{{Type: "paragraph", Text: s, Page: 2}}, RowSpan: 1, ColSpan: 1}
	}
	blocks := []officedoc.Block{
		{Type: "header", Text: "ACME confidential", Page: 1},
		{Type: "title", Text: "Annual report", Page: 1},
		{Type: "heading-1", Text: "Revenue", Page: 1},
		{Type: "paragraph", Text: "Revenue grew.", Page: 1},
		{Type: "heading-2", Text: "By region", Page: 1},
		{Type: "table", Page: 2, Table: &officedoc.Table{Header: [][]officedoc.Cell{{cell("Region"), cell("Sales")}},
			Body: [][]officedoc.Cell{{cell("EU"), cell("10")}, {cell("US"), cell("20")}}}},
		{Type: "heading-1", Text: "Outlook", Page: 2},
		{Type: "list", Page: 2, List: &officedoc.List{Ordered: true, Entries: [][]officedoc.Block{
			{{Type: "paragraph", Text: "Hire", Page: 2}}, {{Type: "paragraph", Text: "Grow", Page: 2}}}}},
		{Type: "footer", Text: "Page 2", Page: 2},
	}
	doc := layoutFromBlocks("text/html", blocks, layoutOptions{chunkWords: 500, ancestorHeadings: true})
	top := doc.GetDocumentLayout().GetBlocks()
	if len(top) != 3 || top[0].GetTextBlock().GetType() != "header" || top[1].GetTextBlock().GetType() != "heading-1" ||
		top[1].GetTextBlock().GetText() != "Annual report" || top[2].GetTextBlock().GetType() != "footer" {
		t.Fatalf("top blocks: %v", top)
	}
	title := top[1].GetTextBlock()
	if len(title.GetBlocks()) != 2 || title.GetBlocks()[0].GetTextBlock().GetText() != "Revenue" || title.GetBlocks()[1].GetTextBlock().GetText() != "Outlook" {
		t.Fatalf("title's children: %v", title.GetBlocks())
	}
	if ps := top[1].GetPageSpan(); ps.GetPageStart() != 1 || ps.GetPageEnd() != 2 {
		t.Errorf("title's page span: %v", ps)
	}
	rev := title.GetBlocks()[0].GetTextBlock()
	if len(rev.GetBlocks()) != 2 || rev.GetBlocks()[1].GetTextBlock().GetType() != "heading-2" {
		t.Fatalf("Revenue's children: %v", rev.GetBlocks())
	}
	tb := rev.GetBlocks()[1].GetTextBlock().GetBlocks()[0].GetTableBlock()
	if len(tb.GetHeaderRows()) != 1 || len(tb.GetBodyRows()) != 2 || tb.GetBodyRows()[1].GetCells()[1].GetBlocks()[0].GetTextBlock().GetText() != "20" ||
		tb.GetBodyRows()[0].GetCells()[0].GetColSpan() != 1 {
		t.Errorf("table: %v", tb)
	}
	lb := title.GetBlocks()[1].GetTextBlock().GetBlocks()[0].GetListBlock()
	if lb.GetType() != "ordered" || len(lb.GetListEntries()) != 2 || lb.GetListEntries()[1].GetBlocks()[0].GetTextBlock().GetText() != "Grow" {
		t.Errorf("list: %v", lb)
	}
	if top[0].GetBlockId() != "1" || top[1].GetBlockId() != "2" || title.GetBlocks()[0].GetBlockId() != "3" {
		t.Errorf("ids: %s %s %s", top[0].GetBlockId(), top[1].GetBlockId(), title.GetBlocks()[0].GetBlockId())
	}

	chunks := doc.GetChunkedDocument().GetChunks()
	// A chunk ends before each heading that follows its content.
	if len(chunks) != 3 {
		t.Fatalf("chunks: %v", chunks)
	}
	if c := chunks[0].GetContent(); c != "Annual report\nRevenue\nRevenue grew." {
		t.Errorf("first chunk: %q", c)
	}
	// With ancestor headings, a chunk starts with the headings above it.
	if c := chunks[1].GetContent(); c != "Annual report\nRevenue\nBy region\n| Region | Sales |\n| --- | --- |\n| EU | 10 |\n| US | 20 |" {
		t.Errorf("second chunk: %q", c)
	}
	if c := chunks[2].GetContent(); c != "Annual report\nOutlook\n1. Hire\n2. Grow" {
		t.Errorf("third chunk: %q", c)
	}
	if f := chunks[2].GetPageFooters(); len(f) != 1 || f[0].GetText() != "Page 2" || len(chunks[0].GetPageHeaders()) != 1 ||
		len(chunks[2].GetPageHeaders()) != 0 {
		t.Errorf("headers and footers: %v %v", chunks[0], chunks[2])
	}
	if ps := chunks[1].GetPageSpan(); ps.GetPageStart() != 1 || ps.GetPageEnd() != 2 {
		t.Errorf("second chunk's pages: %v", ps)
	}

	// Without ancestor headings, and small chunks: a table splits by rows
	// with its header repeated.
	doc = layoutFromBlocks("text/html", blocks, layoutOptions{chunkWords: 5})
	var contents []string
	for _, c := range doc.GetChunkedDocument().GetChunks() {
		contents = append(contents, c.GetContent())
		if n := wordCount(strings.Split(c.GetContent(), "\n")); n > 5 {
			t.Errorf("chunk of %d words: %q", n, c.GetContent())
		}
	}
	all := strings.Join(contents, "\n~\n")
	if !strings.Contains(all, "| Region | Sales |\n| --- | --- |\n| US | 20 |") || strings.Contains(all, "Annual report\nOutlook") {
		t.Errorf("small chunks: %q", all)
	}
}

// ocrLine is a Tesseract paragraph of one line of words, each h pixels
// high, at y.
func ocrLine(text string, y, h int) ocr.Paragraph {
	var p ocr.Paragraph
	x := 50
	words := strings.Fields(text)
	for i, w := range words {
		var syms []ocr.Symbol
		for _, r := range w {
			syms = append(syms, ocr.Symbol{Text: string(r)})
		}
		syms[len(syms)-1].LineEnd = i == len(words)-1
		box := ocr.Quad{image.Pt(x, y), image.Pt(x+10*len(w), y), image.Pt(x+10*len(w), y+h), image.Pt(x, y+h)}
		p.Words = append(p.Words, ocr.Word{Box: box, Symbols: syms, Confidence: 0.9})
		x += 10*len(w) + 10
	}
	p.Box = ocr.Quad{image.Pt(50, y), image.Pt(x, y), image.Pt(x, y+h), image.Pt(50, y+h)}
	return p
}

// TestOCRBlocks (#1799): a scanned page's taller short lines are headings
// (the tallest heading-1), its repeated top line a page header, a page
// number at the bottom a footer, bulleted and numbered lines lists.
func TestOCRBlocks(t *testing.T) {
	page := func(n int, paras ...ocr.Paragraph) docPage {
		return docPage{n: n, w: 1000, h: 1000, read: ocr.Result{Blocks: []ocr.Block{{Paragraphs: paras}}}}
	}
	pages := []docPage{
		page(1, ocrLine("ACME Corp report", 20, 20), ocrLine("Quarterly Report", 100, 60), ocrLine("Sales", 200, 40),
			ocrLine("Sales grew in every region this quarter and we hired staff", 260, 20),
			ocrLine("• Europe", 300, 20), ocrLine("• Americas", 330, 20), ocrLine("1", 960, 20)),
		page(2, ocrLine("ACME Corp report", 20, 20), ocrLine("1. Hire more", 300, 20), ocrLine("2. Grow faster", 330, 20),
			ocrLine("Plain closing text for the body of the page here", 400, 20), ocrLine("2", 960, 20)),
	}
	var got []string
	for _, b := range ocrBlocks(pages) {
		s := b.Type + ":" + b.Text
		if b.List != nil {
			s = b.Type + ":"
			if b.List.Ordered {
				s += "ordered"
			}
			for _, e := range b.List.Entries {
				s += "/" + e[0].Text
			}
		}
		got = append(got, s)
	}
	want := []string{"header:ACME Corp report", "heading-1:Quarterly Report", "heading-2:Sales",
		"paragraph:Sales grew in every region this quarter and we hired staff", "list:/Europe/Americas", "footer:1",
		"header:ACME Corp report", "list:ordered/Hire more/Grow faster", "paragraph:Plain closing text for the body of the page here", "footer:2"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("blocks:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestLayoutParserHTML (#1799): ProcessDocument of an HTML document on
// the Layout Parser answers its headings, list and table; the OCR
// processor refuses it.
func TestLayoutParserHTML(t *testing.T) {
	ctx := context.Background()
	d := newDocAI(t, t.TempDir()+"/processors.json")
	layout := createProcessor(t, d, "LAYOUT_PARSER_PROCESSOR", "layout")
	html := `<html><head><title>Handbook</title></head><body><h1>Leave</h1><p>Staff get 25 days.</p><h2>Sick leave</h2>` +
		`<ul><li>Tell your manager</li><li>Rest</li></ul><table><tr><th>Grade</th><th>Days</th></tr><tr><td>A</td><td>25</td></tr></table></body></html>`
	res, err := d.ProcessDocument(ctx, &documentaipb.ProcessRequest{Name: layout.GetName(),
		Source: &documentaipb.ProcessRequest_RawDocument{RawDocument: &documentaipb.RawDocument{Content: []byte(html), MimeType: "text/html; charset=utf-8"}},
		ProcessOptions: &documentaipb.ProcessOptions{LayoutConfig: &documentaipb.ProcessOptions_LayoutConfig{
			ChunkingConfig: &documentaipb.ProcessOptions_LayoutConfig_ChunkingConfig{ChunkSize: 100, IncludeAncestorHeadings: true}}}})
	if err != nil {
		t.Fatal(err)
	}
	top := res.GetDocument().GetDocumentLayout().GetBlocks()
	if len(top) != 1 || top[0].GetTextBlock().GetText() != "Handbook" {
		t.Fatalf("blocks: %v", top)
	}
	leave := top[0].GetTextBlock().GetBlocks()[0].GetTextBlock()
	if leave.GetText() != "Leave" || leave.GetType() != "heading-1" || len(leave.GetBlocks()) != 2 {
		t.Fatalf("Leave: %v", leave)
	}
	sick := leave.GetBlocks()[1].GetTextBlock()
	if sick.GetType() != "heading-2" || len(sick.GetBlocks()) != 2 || sick.GetBlocks()[0].GetListBlock().GetType() != "unordered" ||
		len(sick.GetBlocks()[1].GetTableBlock().GetHeaderRows()) != 1 {
		t.Errorf("Sick leave: %v", sick)
	}
	chunks := res.GetDocument().GetChunkedDocument().GetChunks()
	if len(chunks) != 2 || !strings.HasPrefix(chunks[1].GetContent(), "Handbook\nLeave\nSick leave\n- Tell your manager") {
		t.Errorf("chunks: %v", chunks)
	}
}
