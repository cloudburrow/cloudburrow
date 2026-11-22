package officedoc

import (
	"errors"
	"strconv"
	"strings"
)

// docxStyle is a paragraph style: what it makes a paragraph, its outline
// level (-1 for none) and the style it is based on.
type docxStyle struct {
	kind    string // "title", "subtitle", "heading-N" or ""
	outline int
	basedOn string
}

// docx is a DOCX package being read.
type docx struct {
	styles map[string]docxStyle
	// numFmt is each numbering's format, by numId and level.
	numFmt map[string]map[int]string
	page   int
}

func parseDOCX(p *pkg) (*Document, error) {
	const main = "word/document.xml"
	root, err := p.loadXML(main)
	if err != nil {
		return nil, err
	}
	if root == nil {
		return nil, errors.New("the document is not a valid DOCX file: it has no word/document.xml")
	}
	body := root.path("document", "body")
	if body == nil {
		return nil, errors.New("the document is not a valid DOCX file: word/document.xml has no body")
	}
	d := &docx{styles: map[string]docxStyle{}, numFmt: map[string]map[int]string{}, page: 1}
	if err := d.loadStyles(p); err != nil {
		return nil, err
	}
	if err := d.loadNumbering(p); err != nil {
		return nil, err
	}
	blocks := d.content(body)
	doc := &Document{Pages: d.page}
	// The page header and footer of the document's (first) section.
	sect := body.child("sectPr")
	if sect == nil {
		sect = body.find("sectPr")
	}
	rels, err := p.rels(main)
	if err != nil {
		return nil, err
	}
	var header, footer string
	if sect != nil {
		if header, err = d.headerFooter(p, sect, "headerReference", rels); err != nil {
			return nil, err
		}
		if footer, err = d.headerFooter(p, sect, "footerReference", rels); err != nil {
			return nil, err
		}
	}
	if header != "" {
		doc.Blocks = append(doc.Blocks, Block{Type: "header", Text: header, Page: 1})
	}
	doc.Blocks = append(doc.Blocks, blocks...)
	if footer != "" {
		doc.Blocks = append(doc.Blocks, Block{Type: "footer", Text: footer, Page: 1})
	}
	return doc, nil
}

// headerFooter is the text of the default (else the first) part a
// section's references of kind name.
func (d *docx) headerFooter(p *pkg, sect *node, name string, rels map[string]string) (string, error) {
	refs := sect.all(name)
	if len(refs) == 0 {
		return "", nil
	}
	ref := refs[0]
	for _, r := range refs {
		if r.attr("type") == "default" {
			ref = r
			break
		}
	}
	target, ok := rels[ref.relID()]
	if !ok {
		return "", nil
	}
	n, err := p.loadXML(target)
	if err != nil || n == nil {
		return "", err
	}
	var parts []string
	var walk func(*node)
	walk = func(n *node) {
		for _, k := range n.kids {
			if k.name == "p" {
				if t := collapse(d.paraText(k, nil)); t != "" {
					parts = append(parts, t)
				}
				continue
			}
			walk(k)
		}
	}
	walk(n)
	return strings.Join(parts, " "), nil
}

func (d *docx) loadStyles(p *pkg) error {
	root, err := p.loadXML("word/styles.xml")
	if err != nil || root == nil {
		return err
	}
	for _, s := range root.path("styles").all("style") {
		if t := s.attr("type"); t != "" && t != "paragraph" {
			continue
		}
		id := s.attr("styleId")
		name := strings.ToLower(strings.TrimSpace(s.child("name").attr("val")))
		st := docxStyle{outline: -1, basedOn: s.child("basedOn").attr("val")}
		switch {
		case name == "title" || strings.EqualFold(id, "Title"):
			st.kind = "title"
		case name == "subtitle" || strings.EqualFold(id, "Subtitle"):
			st.kind = "subtitle"
		default:
			for _, cand := range []string{strings.TrimPrefix(name, "heading "), strings.TrimPrefix(strings.ToLower(id), "heading")} {
				if n, err := strconv.Atoi(strings.TrimSpace(cand)); err == nil && n >= 1 && cand != name && cand != strings.ToLower(id) {
					st.kind = headingType(n)
					break
				}
			}
		}
		if ol := s.path("pPr", "outlineLvl"); ol != nil {
			if n, err := strconv.Atoi(ol.attr("val")); err == nil {
				st.outline = n
			}
		}
		d.styles[id] = st
	}
	return nil
}

func (d *docx) loadNumbering(p *pkg) error {
	root, err := p.loadXML("word/numbering.xml")
	if err != nil || root == nil {
		return err
	}
	num := root.path("numbering")
	abstract := map[string]map[int]string{}
	for _, a := range num.all("abstractNum") {
		lv := map[int]string{}
		for _, l := range a.all("lvl") {
			n, _ := strconv.Atoi(l.attr("ilvl"))
			lv[n] = l.child("numFmt").attr("val")
		}
		abstract[a.attr("abstractNumId")] = lv
	}
	for _, n := range num.all("num") {
		lv := map[int]string{}
		for k, v := range abstract[n.child("abstractNumId").attr("val")] {
			lv[k] = v
		}
		for _, o := range n.all("lvlOverride") {
			if l := o.child("lvl"); l != nil {
				i, _ := strconv.Atoi(o.attr("ilvl"))
				if f := l.child("numFmt"); f != nil {
					lv[i] = f.attr("val")
				}
			}
		}
		d.numFmt[n.attr("numId")] = lv
	}
	return nil
}

// paraKind is what a paragraph's style makes it ("" for a paragraph).
func (d *docx) paraKind(pPr *node) string {
	if ol := pPr.child("outlineLvl"); ol != nil {
		if n, err := strconv.Atoi(ol.attr("val")); err == nil && n < 9 {
			return headingType(n + 1)
		}
	}
	id := pPr.child("pStyle").attr("val")
	for range 16 { // basedOn chains are short; a cycle stops here
		st, ok := d.styles[id]
		if !ok {
			return ""
		}
		if st.kind != "" {
			return st.kind
		}
		if st.outline >= 0 && st.outline < 9 {
			return headingType(st.outline + 1)
		}
		id = st.basedOn
	}
	return ""
}

// paraText is a paragraph's text; breaks, when not nil, is called for each
// page break with whether text came before it in the paragraph.
func (d *docx) paraText(p *node, breaks func(afterText bool)) string {
	var b strings.Builder
	var walk func(*node)
	walk = func(n *node) {
		for _, k := range n.kids {
			switch k.name {
			case "t":
				b.WriteString(k.chars())
			case "tab":
				b.WriteString(" ")
			case "br", "cr":
				if k.attr("type") == "page" {
					if breaks != nil {
						breaks(strings.TrimSpace(b.String()) != "")
					}
				} else {
					b.WriteString(" ")
				}
			case "lastRenderedPageBreak":
				if breaks != nil {
					breaks(strings.TrimSpace(b.String()) != "")
				}
			case "del", "delText", "instrText", "pPr", "rPr", "drawing", "pict", "object", "footnoteReference", "endnoteReference":
			default:
				walk(k)
			}
		}
	}
	walk(p)
	return b.String()
}

// content is the blocks of a body or a table cell.
func (d *docx) content(n *node) []Block {
	var out []Block
	lb := &listBuilder{out: &out}
	var walk func(n *node)
	walk = func(n *node) {
		for _, k := range n.kids {
			switch k.name {
			case "p":
				d.paragraph(k, &out, lb)
			case "tbl":
				lb.flush()
				out = append(out, d.table(k))
			case "sdt":
				walk(k.child("sdtContent"))
			case "customXml", "smartTag", "ins":
				walk(k)
			}
		}
	}
	walk(n)
	lb.flush()
	return out
}

func (d *docx) paragraph(p *node, out *[]Block, lb *listBuilder) {
	pPr := p.child("pPr")
	if pb := pPr.child("pageBreakBefore"); pb != nil && pb.attr("val") != "0" && pb.attr("val") != "false" {
		d.page++
	}
	page := d.page
	pending := 0
	text := collapse(d.paraText(p, func(afterText bool) {
		if afterText {
			pending++
		} else {
			d.page++
			page = d.page
		}
	}))
	defer func() {
		d.page += pending
		if s := pPr.child("sectPr"); s != nil && s.path("type").attr("val") != "continuous" {
			d.page++
		}
	}()
	if text == "" {
		return
	}
	if np := pPr.child("numPr"); np != nil && np.child("numId").attr("val") != "0" {
		id := np.child("numId").attr("val")
		lvl, _ := strconv.Atoi(np.child("ilvl").attr("val"))
		f := d.numFmt[id][lvl]
		lb.add(text, lvl, f != "" && f != "bullet" && f != "none", page)
		return
	}
	lb.flush()
	kind := d.paraKind(pPr)
	if kind == "" {
		kind = "paragraph"
	}
	*out = append(*out, Block{Type: kind, Text: text, Page: page})
}

func (d *docx) table(tbl *node) Block {
	page := d.page
	t := &Table{}
	// open is the cell a vertical merge started, by grid column.
	open := map[int]*Cell{}
	inHeader := true
	for _, tr := range tbl.all("tr") {
		trPr := tr.child("trPr")
		col := 0
		if gb := trPr.child("gridBefore"); gb != nil {
			col, _ = strconv.Atoi(gb.attr("val"))
		}
		var row []Cell
		for _, tc := range tr.all("tc") {
			tcPr := tc.child("tcPr")
			span := 1
			if gs := tcPr.child("gridSpan"); gs != nil {
				span, _ = strconv.Atoi(gs.attr("val"))
				span = spanOr1(span)
			}
			if vm := tcPr.child("vMerge"); vm != nil && vm.attr("val") != "restart" {
				if c := open[col]; c != nil {
					c.RowSpan++
				}
				col += span
				continue
			}
			row = append(row, Cell{Blocks: d.content(tc), RowSpan: 1, ColSpan: span})
			if vm := tcPr.child("vMerge"); vm != nil {
				// the cell is opened once its row is placed
				row[len(row)-1].RowSpan = -col - 1
			} else {
				delete(open, col)
			}
			col += span
		}
		header := inHeader && trPr.child("tblHeader") != nil && trPr.child("tblHeader").attr("val") != "0" && trPr.child("tblHeader").attr("val") != "false"
		if !header {
			inHeader = false
		}
		var dst *[][]Cell
		if header {
			dst = &t.Header
		} else {
			dst = &t.Body
		}
		*dst = append(*dst, row)
		placed := (*dst)[len(*dst)-1]
		for i := range placed {
			if placed[i].RowSpan < 0 {
				c := -placed[i].RowSpan - 1
				placed[i].RowSpan = 1
				open[c] = &placed[i]
			}
		}
	}
	return Block{Type: "table", Page: page, Table: t}
}
