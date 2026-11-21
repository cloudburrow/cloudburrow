package officedoc

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// zipOf is a zip archive of the named parts.
func zipOf(t *testing.T, parts map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for name, body := range parts {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// outline is a document's blocks as one line each: "page type text", with
// lists and tables nested by indentation.
func outline(blocks []Block, indent string) string {
	var b strings.Builder
	for _, bl := range blocks {
		switch bl.Type {
		case "list":
			fmt.Fprintf(&b, "%s%d list ordered=%v\n", indent, bl.Page, bl.List.Ordered)
			for _, e := range bl.List.Entries {
				fmt.Fprintf(&b, "%s- entry\n", indent)
				b.WriteString(outline(e, indent+"  "))
			}
		case "table":
			fmt.Fprintf(&b, "%s%d table %q\n", indent, bl.Page, bl.Table.Caption)
			for _, part := range []struct {
				name string
				rows [][]Cell
			}{{"header", bl.Table.Header}, {"body", bl.Table.Body}} {
				for _, r := range part.rows {
					var cells []string
					for _, c := range r {
						var texts []string
						for _, x := range c.Blocks {
							texts = append(texts, x.Text)
						}
						cells = append(cells, fmt.Sprintf("%s(%dx%d)", strings.Join(texts, "/"), c.RowSpan, c.ColSpan))
					}
					fmt.Fprintf(&b, "%s  %s: %s\n", indent, part.name, strings.Join(cells, " | "))
				}
			}
		default:
			fmt.Fprintf(&b, "%s%d %s %s\n", indent, bl.Page, bl.Type, bl.Text)
		}
	}
	return b.String()
}

const wNS = `xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"`

func docxParts(body string) map[string]string {
	return map[string]string{
		"word/document.xml": `<?xml version="1.0"?><w:document ` + wNS + `><w:body>` + body +
			`<w:sectPr><w:headerReference w:type="first" r:id="rId8"/><w:headerReference w:type="default" r:id="rId7"/><w:footerReference w:type="default" r:id="rId9"/></w:sectPr></w:body></w:document>`,
		"word/_rels/document.xml.rels": `<?xml version="1.0"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
			`<Relationship Id="rId7" Type="header" Target="header1.xml"/><Relationship Id="rId8" Type="header" Target="header2.xml"/>` +
			`<Relationship Id="rId9" Type="footer" Target="/word/footer1.xml"/><Relationship Id="rId10" Type="hyperlink" Target="http://x" TargetMode="External"/></Relationships>`,
		"word/header1.xml": `<w:hdr ` + wNS + `><w:p><w:r><w:t>ACME Corp</w:t></w:r></w:p><w:p><w:r><w:t>Confidential</w:t></w:r></w:p></w:hdr>`,
		"word/header2.xml": `<w:hdr ` + wNS + `><w:p><w:r><w:t>First page header</w:t></w:r></w:p></w:hdr>`,
		"word/footer1.xml": `<w:ftr ` + wNS + `><w:p><w:r><w:t>Page footer</w:t></w:r></w:p></w:ftr>`,
		"word/styles.xml": `<w:styles ` + wNS + `>` +
			`<w:style w:type="paragraph" w:styleId="Title"><w:name w:val="Title"/></w:style>` +
			`<w:style w:type="paragraph" w:styleId="Subtitle"><w:name w:val="Subtitle"/></w:style>` +
			`<w:style w:type="paragraph" w:styleId="Heading1"><w:name w:val="heading 1"/><w:pPr><w:outlineLvl w:val="0"/></w:pPr></w:style>` +
			`<w:style w:type="paragraph" w:styleId="Kop2"><w:name w:val="heading 2"/></w:style>` +
			`<w:style w:type="paragraph" w:styleId="MyHeading"><w:name w:val="My heading"/><w:basedOn w:val="Kop2"/></w:style>` +
			`<w:style w:type="paragraph" w:styleId="Deep"><w:name w:val="Deep"/><w:pPr><w:outlineLvl w:val="7"/></w:pPr></w:style>` +
			`</w:styles>`,
		"word/numbering.xml": `<w:numbering ` + wNS + `>` +
			`<w:abstractNum w:abstractNumId="0"><w:lvl w:ilvl="0"><w:numFmt w:val="bullet"/></w:lvl><w:lvl w:ilvl="1"><w:numFmt w:val="decimal"/></w:lvl></w:abstractNum>` +
			`<w:abstractNum w:abstractNumId="1"><w:lvl w:ilvl="0"><w:numFmt w:val="decimal"/></w:lvl></w:abstractNum>` +
			`<w:num w:numId="1"><w:abstractNumId w:val="0"/></w:num><w:num w:numId="2"><w:abstractNumId w:val="1"/></w:num></w:numbering>`,
	}
}

func p(style, text string) string {
	s := `<w:p>`
	if style != "" {
		s += `<w:pPr><w:pStyle w:val="` + style + `"/></w:pPr>`
	}
	return s + `<w:r><w:t xml:space="preserve">` + text + `</w:t></w:r></w:p>`
}

func li(num, lvl, text string) string {
	return `<w:p><w:pPr><w:numPr><w:ilvl w:val="` + lvl + `"/><w:numId w:val="` + num + `"/></w:numPr></w:pPr><w:r><w:t>` + text + `</w:t></w:r></w:p>`
}

func TestDOCX(t *testing.T) {
	body := p("Title", "Annual  report") + p("Subtitle", "2026") + p("Heading1", "Overview") +
		p("", "First paragraph,") + `<w:p><w:r><w:t>with a</w:t><w:tab/><w:t>tab</w:t><w:br/><w:t>and break.</w:t></w:r>` +
		`<w:del><w:r><w:delText>gone</w:delText></w:r></w:del><w:r><w:instrText>PAGE</w:instrText></w:r></w:p>` +
		li("1", "0", "bullet one") + li("1", "1", "nested a") + li("1", "1", "nested b") + li("1", "0", "bullet two") +
		p("", "between") + li("2", "0", "step one") + li("2", "0", "step two") +
		`<w:p><w:r><w:br w:type="page"/><w:t>On page two</w:t></w:r></w:p>` +
		p("MyHeading", "Based-on heading") + p("Deep", "Deep heading") +
		`<w:p><w:pPr><w:outlineLvl w:val="2"/></w:pPr><w:r><w:t>Direct level</w:t></w:r></w:p>` +
		`<w:tbl><w:tr><w:trPr><w:tblHeader/></w:trPr><w:tc><w:p><w:r><w:t>Name</w:t></w:r></w:p></w:tc><w:tc><w:tcPr><w:gridSpan w:val="2"/></w:tcPr><w:p><w:r><w:t>Values</w:t></w:r></w:p></w:tc></w:tr>` +
		`<w:tr><w:tc><w:tcPr><w:vMerge w:val="restart"/></w:tcPr><w:p><w:r><w:t>A</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>1</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>2</w:t></w:r></w:p></w:tc></w:tr>` +
		`<w:tr><w:tc><w:tcPr><w:vMerge/></w:tcPr><w:p/></w:tc><w:tc><w:p><w:r><w:t>3</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>4</w:t></w:r></w:p></w:tc></w:tr></w:tbl>` +
		`<w:p><w:pPr><w:pageBreakBefore/></w:pPr><w:r><w:t>Page three</w:t></w:r></w:p>` +
		`<w:p><w:r><w:t>Ends page three</w:t><w:lastRenderedPageBreak/><w:t>.</w:t></w:r></w:p>` + p("", "Page four") + p("", "   ")
	doc, err := Parse(zipOf(t, docxParts(body)), MIMEDOCX)
	if err != nil {
		t.Fatal(err)
	}
	want := `1 header ACME Corp Confidential
1 title Annual report
1 subtitle 2026
1 heading-1 Overview
1 paragraph First paragraph,
1 paragraph with a tab and break.
1 list ordered=false
- entry
  1 paragraph bullet one
  1 list ordered=true
  - entry
    1 paragraph nested a
  - entry
    1 paragraph nested b
- entry
  1 paragraph bullet two
1 paragraph between
1 list ordered=true
- entry
  1 paragraph step one
- entry
  1 paragraph step two
2 paragraph On page two
2 heading-2 Based-on heading
2 heading-5 Deep heading
2 heading-3 Direct level
2 table ""
  header: Name(1x1) | Values(1x2)
  body: A(2x1) | 1(1x1) | 2(1x1)
  body: 3(1x1) | 4(1x1)
3 paragraph Page three
3 paragraph Ends page three.
4 paragraph Page four
1 footer Page footer
`
	if got := outline(doc.Blocks, ""); got != want {
		t.Errorf("DOCX blocks:\n%s\nwant:\n%s", got, want)
	}
	if doc.Pages != 4 {
		t.Errorf("Pages = %d, want 4", doc.Pages)
	}
}

const aNS = `xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"`

func sp(ph, paras string) string {
	nv := `<p:nvSpPr><p:cNvPr id="1" name="x"/><p:cNvSpPr/><p:nvPr/></p:nvSpPr>`
	if ph != "" {
		nv = `<p:nvSpPr><p:cNvPr id="1" name="x"/><p:cNvSpPr/><p:nvPr>` + ph + `</p:nvPr></p:nvSpPr>`
	}
	return `<p:sp>` + nv + `<p:txBody><a:bodyPr/>` + paras + `</p:txBody></p:sp>`
}

func ap(pPr, text string) string {
	return `<a:p>` + pPr + `<a:r><a:t>` + text + `</a:t></a:r></a:p>`
}

func slide(shapes string) string {
	return `<p:sld ` + aNS + `><p:cSld><p:spTree><p:nvGrpSpPr/><p:grpSpPr/>` + shapes + `</p:spTree></p:cSld></p:sld>`
}

func TestPPTX(t *testing.T) {
	parts := map[string]string{
		"ppt/presentation.xml": `<p:presentation ` + aNS + `><p:sldIdLst><p:sldId id="257" r:id="rId3"/><p:sldId id="256" r:id="rId2"/></p:sldIdLst></p:presentation>`,
		"ppt/_rels/presentation.xml.rels": `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
			`<Relationship Id="rId2" Target="slides/slide2.xml"/><Relationship Id="rId3" Target="slides/slide1.xml"/></Relationships>`,
		"ppt/slides/slide1.xml": slide(sp(`<p:ph type="ctrTitle"/>`, ap("", "Quarterly")+ap("", "Review")) +
			sp(`<p:ph type="subTitle" idx="1"/>`, ap("", "Q3 2026")) + sp(`<p:ph type="sldNum" idx="12"/>`, ap("", "1"))),
		"ppt/slides/slide2.xml": slide(sp(`<p:ph type="title"/>`, ap("", "Agenda")) +
			sp(`<p:ph idx="1"/>`, ap("", "Revenue")+ap(`<a:pPr lvl="1"/>`, "By region")+ap("", "Costs")+ap(`<a:pPr><a:buNone/></a:pPr>`, "Plain note")) +
			`<p:grpSp><p:nvGrpSpPr/><p:grpSpPr/>` + sp("", ap(`<a:pPr><a:buAutoNum type="arabicPeriod"/></a:pPr>`, "First")+ap(`<a:pPr><a:buAutoNum type="arabicPeriod"/></a:pPr>`, "Second")+ap("", "Free text")) + `</p:grpSp>` +
			`<p:graphicFrame><a:graphic><a:graphicData><a:tbl><a:tblPr firstRow="1"/>` +
			`<a:tr><a:tc gridSpan="2"><a:txBody>` + ap("", "Region") + `</a:txBody></a:tc><a:tc hMerge="1"><a:txBody>` + ap("", "") + `</a:txBody></a:tc></a:tr>` +
			`<a:tr><a:tc rowSpan="2"><a:txBody>` + ap("", "EU") + `</a:txBody></a:tc><a:tc><a:txBody>` + ap("", "10") + `</a:txBody></a:tc></a:tr>` +
			`<a:tr><a:tc vMerge="1"><a:txBody>` + ap("", "") + `</a:txBody></a:tc><a:tc><a:txBody>` + ap("", "20") + `</a:txBody></a:tc></a:tr>` +
			`</a:tbl></a:graphicData></a:graphic></p:graphicFrame>` +
			sp(`<p:ph type="ftr"/>`, ap("", "Company confidential"))),
		"ppt/notesSlides/notesSlide1.xml": slide(sp("", ap("", "speaker notes"))),
	}
	doc, err := Parse(zipOf(t, parts), MIMEPPTX)
	if err != nil {
		t.Fatal(err)
	}
	want := `1 title Quarterly Review
1 subtitle Q3 2026
2 heading-1 Agenda
2 list ordered=false
- entry
  2 paragraph Revenue
  2 list ordered=false
  - entry
    2 paragraph By region
- entry
  2 paragraph Costs
2 paragraph Plain note
2 list ordered=true
- entry
  2 paragraph First
- entry
  2 paragraph Second
2 paragraph Free text
2 table ""
  header: Region(1x2)
  body: EU(2x1) | 10(1x1)
  body: 20(1x1)
2 footer Company confidential
`
	if got := outline(doc.Blocks, ""); got != want {
		t.Errorf("PPTX blocks:\n%s\nwant:\n%s", got, want)
	}
	if doc.Pages != 2 {
		t.Errorf("Pages = %d, want 2", doc.Pages)
	}
}

func xlsxParts(sheet1 string) map[string]string {
	return map[string]string{
		"xl/workbook.xml": `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">` +
			`<sheets><sheet name="Sales" sheetId="1" r:id="rId1"/><sheet name="Empty" sheetId="2" r:id="rId2"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
			`<Relationship Id="rId1" Target="worksheets/sheet1.xml"/><Relationship Id="rId2" Target="worksheets/sheet2.xml"/></Relationships>`,
		"xl/sharedStrings.xml":     `<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><si><t>Region</t></si><si><r><t>Total </t></r><r><t>sales</t></r><rPh><t>x</t></rPh></si><si><t>North</t></si></sst>`,
		"xl/worksheets/sheet1.xml": sheet1,
		"xl/worksheets/sheet2.xml": `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData/></worksheet>`,
	}
}

const sheet1 = `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>` +
	`<row r="2"><c r="B2" t="s"><v>0</v></c><c r="C2" t="s"><v>1</v></c></row>` +
	`<row r="3"><c r="B3" t="s"><v>2</v></c><c r="C3"><f>1+1</f><v>1250.5</v></c><c r="D3" t="b"><v>1</v></c></row>` +
	`<row r="4"><c r="B4" t="inlineStr"><is><t>South</t></is></c><c r="C4" t="str"><v>n/a</v></c></row>` +
	`<row r="5"><c r="B5"/></row>` +
	`</sheetData><mergeCells count="1"><mergeCell ref="C2:D2"/></mergeCells></worksheet>`

func TestXLSX(t *testing.T) {
	doc, err := Parse(zipOf(t, xlsxParts(sheet1)), MIMEXLSM+"; charset=binary")
	if err != nil {
		t.Fatal(err)
	}
	want := `1 table "Sales"
  header: Region(1x1) | Total sales(1x2)
  body: North(1x1) | 1250.5(1x1) | TRUE(1x1)
  body: South(1x1) | n/a(1x1) | (1x1)
2 table "Empty"
`
	if got := outline(doc.Blocks, ""); got != want {
		t.Errorf("XLSX blocks:\n%s\nwant:\n%s", got, want)
	}
	if doc.Pages != 2 {
		t.Errorf("Pages = %d, want 2", doc.Pages)
	}
	if c := doc.Blocks[0].Table.Body[0][0].Blocks[0]; c.Page != 1 || c.Type != "paragraph" {
		t.Errorf("cell block %+v", c)
	}
}

func TestXLSXCellLimit(t *testing.T) {
	defer func(n int) { maxCells = n }(maxCells)
	maxCells = 8
	_, err := Parse(zipOf(t, xlsxParts(sheet1)), MIMEXLSX)
	if !errors.Is(err, ErrTooManyCells) {
		t.Fatalf("err = %v, want ErrTooManyCells", err)
	}
	// A sparse sheet whose used range is over the limit.
	sparse := `<worksheet><sheetData><row r="1"><c r="A1"><v>1</v></c></row><row r="100"><c r="Z100"><v>2</v></c></row></sheetData></worksheet>`
	maxCells = 100
	if _, err := Parse(zipOf(t, xlsxParts(sparse)), MIMEXLSX); !errors.Is(err, ErrTooManyCells) {
		t.Fatalf("sparse: err = %v, want ErrTooManyCells", err)
	}
	maxCells = 12
	if _, err := Parse(zipOf(t, xlsxParts(sheet1)), MIMEXLSX); err != nil {
		t.Fatalf("within the limit: %v", err)
	}
}

func TestHTML(t *testing.T) {
	src := `<!doctype html><html><head><title>Guide  page</title><style>p{}</style><script>var x="no";</script></head><body>
<header><nav>Home | Docs</nav></header>
<h1>Install</h1><p>Run the <b>installer</b>,<br>then restart.</p>
<div>Loose <i>inline</i> text</div>
<div><p>Inner one</p>tail text<p>Inner two</p></div>
<h6>Tiny</h6>
<ol><li>Download</li><li>Unpack<ul><li>on Linux</li><li><p>on macOS</p></li></ul></li></ol>
<table><caption>Sizes</caption><thead><tr><th colspan="2">Size</th></tr></thead><tbody><tr><td rowspan="2">A</td><td>1</td></tr><tr><td>2</td></tr></tbody></table>
<table><tr><th>K</th><th>V</th></tr><tr><td>k</td><td>v</td></tr></table>
<noscript>enable js</noscript>
<footer>© 2026 Example</footer></body></html>`
	doc, err := Parse([]byte(src), "TEXT/HTML; charset=utf-8")
	if err != nil {
		t.Fatal(err)
	}
	want := `1 title Guide page
1 header Home | Docs
1 heading-1 Install
1 paragraph Run the installer, then restart.
1 paragraph Loose inline text
1 paragraph Inner one
1 paragraph tail text
1 paragraph Inner two
1 heading-5 Tiny
1 list ordered=true
- entry
  1 paragraph Download
- entry
  1 paragraph Unpack
  1 list ordered=false
  - entry
    1 paragraph on Linux
  - entry
    1 paragraph on macOS
1 table "Sizes"
  header: Size(1x2)
  body: A(2x1) | 1(1x1)
  body: 2(1x1)
1 table ""
  header: K(1x1) | V(1x1)
  body: k(1x1) | v(1x1)
1 footer © 2026 Example
`
	if got := outline(doc.Blocks, ""); got != want {
		t.Errorf("HTML blocks:\n%s\nwant:\n%s", got, want)
	}
	if doc.Pages != 1 {
		t.Errorf("Pages = %d", doc.Pages)
	}
}

func TestSupported(t *testing.T) {
	for _, m := range []string{MIMEHTML, MIMEDOCX, MIMEPPTX, MIMEXLSX, MIMEXLSM, "Application/VND.ms-excel.sheet.macroEnabled.12", "text/html;charset=utf-8"} {
		if !Supported(m) {
			t.Errorf("Supported(%q) = false", m)
		}
	}
	for _, m := range []string{"application/pdf", "application/msword", "text/plain", ""} {
		if Supported(m) {
			t.Errorf("Supported(%q) = true", m)
		}
	}
}

func TestMalformed(t *testing.T) {
	cases := []struct {
		name    string
		content []byte
		mime    string
	}{
		{"not a zip", []byte("plain text"), MIMEDOCX},
		{"docx without document.xml", zipOf(t, map[string]string{"word/styles.xml": "<w:styles/>"}), MIMEDOCX},
		{"docx with bad xml", zipOf(t, map[string]string{"word/document.xml": "<w:document><w:body><w:p></w:document"}), MIMEDOCX},
		{"pptx without presentation.xml", zipOf(t, map[string]string{"ppt/slides/slide1.xml": slide("")}), MIMEPPTX},
		{"xlsx without workbook.xml", zipOf(t, map[string]string{"xl/worksheets/sheet1.xml": sheet1}), MIMEXLSX},
		{"xlsx with a bad sheet", zipOf(t, xlsxParts("<worksheet><sheetData><row>")), MIMEXLSX},
		{"unsupported type", []byte("%PDF-1.4"), "application/pdf"},
	}
	for _, c := range cases {
		if _, err := Parse(c.content, c.mime); err == nil {
			t.Errorf("%s: no error", c.name)
		}
	}
}

func TestCellRef(t *testing.T) {
	for ref, want := range map[string][2]int{"A1": {1, 1}, "Z9": {26, 9}, "AA10": {27, 10}, "c5": {3, 5}} {
		c, r, ok := cellRef(ref)
		if !ok || c != want[0] || r != want[1] {
			t.Errorf("cellRef(%q) = %d, %d, %v", ref, c, r, ok)
		}
	}
	for _, ref := range []string{"", "12", "A", "A0"} {
		if _, _, ok := cellRef(ref); ok {
			t.Errorf("cellRef(%q) ok", ref)
		}
	}
}
