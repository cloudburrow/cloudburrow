//go:build compat

package compat

import (
	"archive/zip"
	"bytes"
	"fmt"
	"strings"
	"testing"

	"cloud.google.com/go/documentai/apiv1/documentaipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Office documents the test writes: the parts Word, PowerPoint and Excel
// read, built here so no binary fixture is needed.
const (
	mimeDOCX = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	mimePPTX = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	mimeXLSX = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	relsNS   = "http://schemas.openxmlformats.org/package/2006/relationships"
	wordNS   = `xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"`
	slideNS  = `xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"`
	sheetNS  = `xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"`
)

func zipParts(t *testing.T, parts [][2]string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for _, p := range parts {
		f, err := w.Create(p[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + p[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func contentTypes(main string) [2]string {
	return [2]string{"[Content_Types].xml", `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
		`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
		`<Default Extension="xml" ContentType="application/xml"/><Override PartName="` + main + `" ContentType="application/xml"/></Types>`}
}

// sampleDOCX is a policy with a title, headings, a numbered list, a table
// and a page header and footer, over two pages.
func sampleDOCX(t *testing.T) []byte {
	para := func(style, text string) string {
		return `<w:p><w:pPr><w:pStyle w:val="` + style + `"/></w:pPr><w:r><w:t xml:space="preserve">` + text + `</w:t></w:r></w:p>`
	}
	item := func(text string) string {
		return `<w:p><w:pPr><w:numPr><w:ilvl w:val="0"/><w:numId w:val="1"/></w:numPr></w:pPr><w:r><w:t>` + text + `</w:t></w:r></w:p>`
	}
	cell := func(text string) string { return `<w:tc><w:p><w:r><w:t>` + text + `</w:t></w:r></w:p></w:tc>` }
	body := para("Title", "Staff handbook") +
		para("Heading1", "Annual leave") + para("Normal", "Every employee gets 25 days of paid leave a year.") +
		para("Heading2", "Booking leave") + item("Ask your manager two weeks ahead") + item("Record it in the HR system") +
		`<w:p><w:r><w:br w:type="page"/></w:r></w:p>` +
		para("Heading1", "Allowances") +
		`<w:tbl><w:tr><w:trPr><w:tblHeader/></w:trPr>` + cell("Grade") + cell("Days") + `</w:tr><w:tr>` + cell("Junior") + cell("25") + `</w:tr>` +
		`<w:tr>` + cell("Senior") + cell("30") + `</w:tr></w:tbl>` +
		`<w:sectPr><w:headerReference w:type="default" r:id="rId2"/><w:footerReference w:type="default" r:id="rId3"/></w:sectPr>`
	return zipParts(t, [][2]string{
		contentTypes("/word/document.xml"),
		{"_rels/.rels", `<Relationships xmlns="` + relsNS + `"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>`},
		{"word/document.xml", `<w:document ` + wordNS + `><w:body>` + body + `</w:body></w:document>`},
		{"word/_rels/document.xml.rels", `<Relationships xmlns="` + relsNS + `">` +
			`<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/header" Target="header1.xml"/>` +
			`<Relationship Id="rId3" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/footer" Target="footer1.xml"/>` +
			`<Relationship Id="rId4" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>` +
			`<Relationship Id="rId5" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/numbering" Target="numbering.xml"/></Relationships>`},
		{"word/header1.xml", `<w:hdr ` + wordNS + `><w:p><w:r><w:t>ACME Corp internal</w:t></w:r></w:p></w:hdr>`},
		{"word/footer1.xml", `<w:ftr ` + wordNS + `><w:p><w:r><w:t>HR-POL-7</w:t></w:r></w:p></w:ftr>`},
		{"word/styles.xml", `<w:styles ` + wordNS + `><w:style w:type="paragraph" w:styleId="Normal"><w:name w:val="Normal"/></w:style>` +
			`<w:style w:type="paragraph" w:styleId="Title"><w:name w:val="Title"/></w:style>` +
			`<w:style w:type="paragraph" w:styleId="Heading1"><w:name w:val="heading 1"/><w:pPr><w:outlineLvl w:val="0"/></w:pPr></w:style>` +
			`<w:style w:type="paragraph" w:styleId="Heading2"><w:name w:val="heading 2"/><w:pPr><w:outlineLvl w:val="1"/></w:pPr></w:style></w:styles>`},
		{"word/numbering.xml", `<w:numbering ` + wordNS + `><w:abstractNum w:abstractNumId="0"><w:lvl w:ilvl="0"><w:numFmt w:val="decimal"/></w:lvl></w:abstractNum>` +
			`<w:num w:numId="1"><w:abstractNumId w:val="0"/></w:num></w:numbering>`},
	})
}

// samplePPTX is two slides: a title slide, and an agenda with bullets and
// a table.
func samplePPTX(t *testing.T) []byte {
	shape := func(ph, paras string) string {
		return `<p:sp><p:nvSpPr><p:cNvPr id="2" name="s"/><p:cNvSpPr/><p:nvPr>` + ph + `</p:nvPr></p:nvSpPr><p:spPr/><p:txBody><a:bodyPr/>` + paras + `</p:txBody></p:sp>`
	}
	para := func(text string) string { return `<a:p><a:r><a:t>` + text + `</a:t></a:r></a:p>` }
	cell := func(text string) string { return `<a:tc><a:txBody><a:bodyPr/>` + para(text) + `</a:txBody></a:tc>` }
	slide := func(shapes string) string {
		return `<p:sld ` + slideNS + `><p:cSld><p:spTree><p:nvGrpSpPr><p:cNvPr id="1" name=""/><p:cNvGrpSpPr/><p:nvPr/></p:nvGrpSpPr><p:grpSpPr/>` +
			shapes + `</p:spTree></p:cSld></p:sld>`
	}
	return zipParts(t, [][2]string{
		contentTypes("/ppt/presentation.xml"),
		{"_rels/.rels", `<Relationships xmlns="` + relsNS + `"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="ppt/presentation.xml"/></Relationships>`},
		{"ppt/presentation.xml", `<p:presentation ` + slideNS + `><p:sldIdLst><p:sldId id="256" r:id="rId2"/><p:sldId id="257" r:id="rId3"/></p:sldIdLst></p:presentation>`},
		{"ppt/_rels/presentation.xml.rels", `<Relationships xmlns="` + relsNS + `">` +
			`<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slide" Target="slides/slide1.xml"/>` +
			`<Relationship Id="rId3" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/slide" Target="slides/slide2.xml"/></Relationships>`},
		{"ppt/slides/slide1.xml", slide(shape(`<p:ph type="ctrTitle"/>`, para("Quarterly review")) + shape(`<p:ph type="subTitle" idx="1"/>`, para("Q3 2026")))},
		{"ppt/slides/slide2.xml", slide(shape(`<p:ph type="title"/>`, para("Agenda")) +
			shape(`<p:ph idx="1"/>`, para("Revenue by region")+para("Hiring plan")) +
			`<p:graphicFrame><p:nvGraphicFramePr><p:cNvPr id="4" name="t"/><p:cNvGraphicFramePr/><p:nvPr/></p:nvGraphicFramePr><p:xfrm/>` +
			`<a:graphic><a:graphicData uri="http://schemas.openxmlformats.org/drawingml/2006/table"><a:tbl><a:tblPr firstRow="1"/><a:tblGrid/>` +
			`<a:tr h="0">` + cell("Region") + cell("Revenue") + `</a:tr><a:tr h="0">` + cell("EMEA") + cell("4.2M") + `</a:tr>` +
			`</a:tbl></a:graphicData></a:graphic></p:graphicFrame>`)},
	})
}

// sampleXLSX is a workbook of two sheets; cellsXLSX is one whose used
// range has cells cells.
func sampleXLSX(t *testing.T, sheet2 string) []byte {
	return zipParts(t, [][2]string{
		contentTypes("/xl/workbook.xml"),
		{"_rels/.rels", `<Relationships xmlns="` + relsNS + `"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>`},
		{"xl/workbook.xml", `<workbook ` + sheetNS + `><sheets><sheet name="Sales" sheetId="1" r:id="rId1"/><sheet name="Costs" sheetId="2" r:id="rId2"/></sheets></workbook>`},
		{"xl/_rels/workbook.xml.rels", `<Relationships xmlns="` + relsNS + `">` +
			`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>` +
			`<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet2.xml"/>` +
			`<Relationship Id="rId3" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/sharedStrings" Target="sharedStrings.xml"/></Relationships>`},
		{"xl/sharedStrings.xml", `<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><si><t>Region</t></si><si><t>Revenue</t></si><si><t>North</t></si><si><t>South</t></si></sst>`},
		{"xl/worksheets/sheet1.xml", `<worksheet ` + sheetNS + `><sheetData>` +
			`<row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c></row>` +
			`<row r="2"><c r="A2" t="s"><v>2</v></c><c r="B2"><v>1250.5</v></c></row>` +
			`<row r="3"><c r="A3" t="s"><v>3</v></c><c r="B3"><f>B2*2</f><v>2501</v></c></row></sheetData></worksheet>`},
		{"xl/worksheets/sheet2.xml", `<worksheet ` + sheetNS + `><sheetData>` + sheet2 + `</sheetData></worksheet>`},
	})
}

// styledPDF is a PDF of pages of lines, each line a font size and text at
// a height (points from the bottom of a 792-point page).
func styledPDF(pages ...[][3]string) []byte {
	var b bytes.Buffer
	var offs []int
	obj := func(body string) {
		offs = append(offs, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", len(offs), body)
	}
	b.WriteString("%PDF-1.4\n")
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	var kids []string
	for i := range pages {
		kids = append(kids, fmt.Sprintf("%d 0 R", 4+2*i))
	}
	obj(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(pages)))
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	for i, lines := range pages {
		obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>", 5+2*i))
		var s strings.Builder
		for _, l := range lines {
			fmt.Fprintf(&s, "BT /F1 %s Tf 72 %s Td (%s) Tj ET\n", l[0], l[1], l[2])
		}
		obj(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", s.Len(), s.String()))
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offs)+1)
	for _, o := range offs {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offs)+1, xref)
	return b.Bytes()
}

// layoutOutline is a documentLayout as one line a block: its type, page
// span and text, children indented.
func layoutOutline(blocks []*documentaipb.Document_DocumentLayout_DocumentLayoutBlock, indent string) string {
	var b strings.Builder
	for _, bl := range blocks {
		ps := bl.GetPageSpan()
		span := fmt.Sprintf("%d-%d", ps.GetPageStart(), ps.GetPageEnd())
		switch {
		case bl.GetTextBlock() != nil:
			fmt.Fprintf(&b, "%s%s %s %s\n", indent, bl.GetTextBlock().GetType(), span, bl.GetTextBlock().GetText())
			b.WriteString(layoutOutline(bl.GetTextBlock().GetBlocks(), indent+"  "))
		case bl.GetListBlock() != nil:
			fmt.Fprintf(&b, "%slist %s %s\n", indent, span, bl.GetListBlock().GetType())
			for _, e := range bl.GetListBlock().GetListEntries() {
				b.WriteString(layoutOutline(e.GetBlocks(), indent+"  * "))
			}
		case bl.GetTableBlock() != nil:
			tb := bl.GetTableBlock()
			fmt.Fprintf(&b, "%stable %s %q\n", indent, span, tb.GetCaption())
			for _, rows := range [][]*documentaipb.Document_DocumentLayout_DocumentLayoutBlock_LayoutTableRow{tb.GetHeaderRows(), tb.GetBodyRows()} {
				for _, r := range rows {
					var cells []string
					for _, c := range r.GetCells() {
						var texts []string
						for _, x := range c.GetBlocks() {
							texts = append(texts, x.GetTextBlock().GetText())
						}
						cells = append(cells, strings.Join(texts, "/"))
					}
					fmt.Fprintf(&b, "%s  | %s |\n", indent, strings.Join(cells, " | "))
				}
			}
		}
	}
	return b.String()
}

// TestDocumentAILayoutParserOffice (#1799): the Layout Parser through the
// official cloud.google.com/go/documentai clients (gRPC and REST) reads
// its documented inputs beyond PDFs and images
// (https://docs.cloud.google.com/document-ai/docs/layout-parse-chunk):
// DOCX, PPTX, XLSX and HTML, locally in pure Go, answering documentLayout
// (headings nesting their blocks, lists, tables, page headers and footers,
// page spans) and chunkedDocument (chunks of at most chunk_size words
// that start with their ancestor headings, with pageHeaders and
// pageFooters); a scanned PDF's larger lines are headings, its repeated
// top line a page header and its "Page N of M" lines footers. An XLSX over the
// documented 5 million cells, and a DOCX for the OCR processor, are
// INVALID_ARGUMENT.
//
// covers: google.cloud.documentai.v1.DocumentProcessorService/ProcessDocument
func TestDocumentAILayoutParserOffice(t *testing.T) {
	h := New(t)
	if h.Endpoint(EnvLocalAI) == "" {
		t.Skip("this instance serves no local AI endpoint")
	}
	g, r := documentAIClients(t, h)
	ctx := h.Context()
	project := h.Project()
	layout := createDocumentProcessor(t, h, g, project, "LAYOUT_PARSER_PROCESSOR")
	process := func(content []byte, mime string, size int32) (*documentaipb.Document, error) {
		c := g
		if mime == mimePPTX || mime == "text/html" {
			c = r
		}
		res, err := c.ProcessDocument(ctx, &documentaipb.ProcessRequest{Name: layout.GetName(),
			Source: &documentaipb.ProcessRequest_RawDocument{RawDocument: &documentaipb.RawDocument{Content: content, MimeType: mime}},
			ProcessOptions: &documentaipb.ProcessOptions{LayoutConfig: &documentaipb.ProcessOptions_LayoutConfig{
				ChunkingConfig: &documentaipb.ProcessOptions_LayoutConfig_ChunkingConfig{ChunkSize: size, IncludeAncestorHeadings: true}}}})
		return res.GetDocument(), err
	}

	for _, tc := range []struct {
		name, mime string
		content    []byte
		want       string
		chunks     []string // each a chunk's content prefix, in order
	}{
		{"DOCX", mimeDOCX, sampleDOCX(t), `header 1-1 ACME Corp internal
heading-1 1-2 Staff handbook
  heading-1 1-1 Annual leave
    paragraph 1-1 Every employee gets 25 days of paid leave a year.
    heading-2 1-1 Booking leave
      list 1-1 ordered
        * paragraph 1-1 Ask your manager two weeks ahead
        * paragraph 1-1 Record it in the HR system
  heading-1 2-2 Allowances
    table 2-2 ""
      | Grade | Days |
      | Junior | 25 |
      | Senior | 30 |
footer 1-1 HR-POL-7
`, []string{"Staff handbook\nAnnual leave\nEvery employee", "Staff handbook\nAnnual leave\nBooking leave\n1. Ask your manager",
			"Staff handbook\nAllowances\n| Grade | Days |\n| --- | --- |\n| Junior | 25 |"}},
		{"PPTX", mimePPTX, samplePPTX(t), `heading-1 1-2 Quarterly review
  subtitle 1-1 Q3 2026
  heading-1 2-2 Agenda
    list 2-2 unordered
      * paragraph 2-2 Revenue by region
      * paragraph 2-2 Hiring plan
    table 2-2 ""
      | Region | Revenue |
      | EMEA | 4.2M |
`, []string{"Quarterly review\nQ3 2026", "Quarterly review\nAgenda\n- Revenue by region\n- Hiring plan\n| Region | Revenue |"}},
		{"XLSX", mimeXLSX, sampleXLSX(t, `<row r="1"><c r="A1" t="inlineStr"><is><t>Item</t></is></c><c r="B1" t="inlineStr"><is><t>Cost</t></is></c></row>`+
			`<row r="2"><c r="A2" t="inlineStr"><is><t>Rent</t></is></c><c r="B2"><v>900</v></c></row>`), `table 1-1 "Sales"
  | Region | Revenue |
  | North | 1250.5 |
  | South | 2501 |
table 2-2 "Costs"
  | Item | Cost |
  | Rent | 900 |
`, []string{"Sales\n| Region | Revenue |\n| --- | --- |\n| North | 1250.5 |\n| South | 2501 |\nCosts"}},
		{"HTML", "text/html; charset=utf-8", []byte(`<!doctype html><html><head><title>Release notes</title><style>p{}</style></head><body>` +
			`<header>Product docs</header><h1>Version 2</h1><p>This release adds <b>exports</b>.</p><h2>Fixes</h2><ol><li>Login works</li><li>Faster search</li></ol>` +
			`<footer>Copyright 2026</footer></body></html>`), `heading-1 1-1 Release notes
  heading-1 1-1 Version 2
    paragraph 1-1 This release adds exports.
    heading-2 1-1 Fixes
      list 1-1 ordered
        * paragraph 1-1 Login works
        * paragraph 1-1 Faster search
header 1-1 Product docs
footer 1-1 Copyright 2026
`, nil},
	} {
		doc, err := process(tc.content, tc.mime, 100)
		if err != nil {
			t.Errorf("%s: ProcessDocument: %v", tc.name, err)
			continue
		}
		if got := layoutOutline(doc.GetDocumentLayout().GetBlocks(), ""); got != tc.want {
			t.Errorf("%s documentLayout:\n%s\nwant:\n%s", tc.name, got, tc.want)
		}
		chunks := doc.GetChunkedDocument().GetChunks()
		for i, c := range chunks {
			t.Logf("%s chunk (recorded) %s pages %d-%d: %q", tc.name, c.GetChunkId(), c.GetPageSpan().GetPageStart(), c.GetPageSpan().GetPageEnd(), c.GetContent())
			if c.GetChunkId() != fmt.Sprintf("c%d", i+1) {
				t.Errorf("%s chunk %d id %q", tc.name, i, c.GetChunkId())
			}
		}
		if tc.chunks != nil && len(chunks) != len(tc.chunks) {
			t.Errorf("%s: %d chunks, want %d", tc.name, len(chunks), len(tc.chunks))
		}
		for i, want := range tc.chunks {
			if i < len(chunks) && !strings.HasPrefix(chunks[i].GetContent(), want) {
				t.Errorf("%s chunk %d: %q, want it to start %q", tc.name, i, chunks[i].GetContent(), want)
			}
		}
		if tc.name == "DOCX" && len(chunks) > 0 {
			if hs := chunks[0].GetPageHeaders(); len(hs) != 1 || hs[0].GetText() != "ACME Corp internal" || len(chunks[0].GetPageFooters()) != 1 {
				t.Errorf("DOCX chunk headers and footers: %v", chunks[0])
			}
		}
	}

	// Small chunks: none over chunk_size words, a table split by rows.
	doc, err := process(sampleDOCX(t), mimeDOCX, 8)
	if err != nil {
		t.Fatalf("DOCX in chunks of 8: %v", err)
	}
	for _, c := range doc.GetChunkedDocument().GetChunks() {
		words := 0
		for _, w := range strings.Fields(c.GetContent()) {
			if strings.Trim(w, "|-") != "" {
				words++
			}
		}
		// The ancestor headings come on top of chunk_size: at most the
		// three of "Booking leave", 6 words.
		if words > 8+6 {
			t.Errorf("chunk of %d words: %q", words, c.GetContent())
		}
	}

	// A scanned PDF: a large title, a heading, body text, a header that
	// repeats and page numbers.
	pdf := styledPDF(
		[][3]string{{"10", "770", "ACME quarterly report"}, {"30", "680", "Results"}, {"18", "620", "Revenue"},
			{"11", "590", "Revenue grew in every region this quarter."}, {"10", "30", "Page 1 of 2"}},
		[][3]string{{"10", "770", "ACME quarterly report"}, {"18", "680", "Outlook"}, {"11", "650", "We expect more growth next year."}, {"10", "30", "Page 2 of 2"}})
	doc, err = process(pdf, "application/pdf", 100)
	if err != nil {
		t.Fatalf("scanned PDF: %v", err)
	}
	got := layoutOutline(doc.GetDocumentLayout().GetBlocks(), "")
	t.Logf("scanned PDF layout (recorded):\n%s", got)
	for _, want := range []string{"header 1-1 ACME quarterly report\n", "heading-1 1-2 Results\n", "  heading-2 1-1 Revenue\n",
		"    paragraph 1-1 Revenue grew in every region this quarter.\n", "  heading-2 2-2 Outlook\n", "footer 1-1 Page 1 of 2\n", "footer 2-2 Page 2 of 2\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("scanned PDF layout has no %q", want)
		}
	}
	if cs := doc.GetChunkedDocument().GetChunks(); len(cs) == 0 || len(cs[len(cs)-1].GetPageFooters()) == 0 ||
		!strings.HasPrefix(cs[len(cs)-1].GetContent(), "Results\nOutlook") {
		t.Errorf("scanned PDF chunks: %v", cs)
	}

	// Refusals.
	if _, err := process(sampleXLSX(t, `<row r="1"><c r="A1"><v>1</v></c></row><row r="200001"><c r="Z200001"><v>2</v></c></row>`), mimeXLSX, 0); status.Code(err) != codes.InvalidArgument ||
		!strings.Contains(err.Error(), "5 million") {
		t.Errorf("XLSX over 5 million cells: %v", err)
	}
	ocrP := createDocumentProcessor(t, h, g, project, "OCR_PROCESSOR")
	if _, err := g.ProcessDocument(ctx, &documentaipb.ProcessRequest{Name: ocrP.GetName(),
		Source: &documentaipb.ProcessRequest_RawDocument{RawDocument: &documentaipb.RawDocument{Content: sampleDOCX(t), MimeType: mimeDOCX}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("DOCX on the OCR processor: %v", err)
	}
}
