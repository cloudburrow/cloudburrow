package officedoc

import (
	"errors"
	"strconv"
	"strings"
)

func parsePPTX(p *pkg) (*Document, error) {
	const pres = "ppt/presentation.xml"
	root, err := p.loadXML(pres)
	if err != nil {
		return nil, err
	}
	if root == nil {
		return nil, errors.New("the document is not a valid PPTX file: it has no ppt/presentation.xml")
	}
	rels, err := p.rels(pres)
	if err != nil {
		return nil, err
	}
	doc := &Document{}
	for _, id := range root.path("presentation", "sldIdLst").all("sldId") {
		target, ok := rels[id.relID()]
		if !ok {
			continue
		}
		slide, err := p.loadXML(target)
		if err != nil {
			return nil, err
		}
		if slide == nil {
			continue
		}
		doc.Pages++
		page := doc.Pages
		tree := slide.path("sld", "cSld", "spTree")
		doc.Blocks = append(doc.Blocks, pptxShapes(tree, page)...)
	}
	return doc, nil
}

// pptxShapes is the blocks of a shape tree's shapes, in order.
func pptxShapes(tree *node, page int) []Block {
	var out []Block
	if tree == nil {
		return nil
	}
	for _, k := range tree.kids {
		switch k.name {
		case "sp":
			out = append(out, pptxShape(k, page)...)
		case "grpSp":
			out = append(out, pptxShapes(k, page)...)
		case "graphicFrame":
			if tbl := k.find("tbl"); tbl != nil {
				out = append(out, Block{Type: "table", Page: page, Table: pptxTable(tbl, page)})
			}
		}
	}
	return out
}

// pptxParaText is a DrawingML paragraph's text.
func pptxParaText(p *node) string {
	var b strings.Builder
	for _, k := range p.kids {
		switch k.name {
		case "r", "fld":
			if t := k.child("t"); t != nil {
				b.WriteString(t.chars())
			}
		case "br":
			b.WriteString(" ")
		}
	}
	return collapse(b.String())
}

func pptxShape(sp *node, page int) []Block {
	body := sp.child("txBody")
	if body == nil {
		return nil
	}
	ph := sp.path("nvSpPr", "nvPr", "ph")
	phType := ph.attr("type")
	if ph != nil && phType == "" {
		phType = "body"
	}
	var heading string
	switch phType {
	case "title":
		heading = "heading-1"
	case "ctrTitle":
		heading = "title"
	case "subTitle":
		heading = "subtitle"
	case "dt", "sldNum", "hdr":
		return nil
	case "ftr":
		heading = "footer"
	}
	if heading != "" {
		var parts []string
		for _, p := range body.all("p") {
			if t := pptxParaText(p); t != "" {
				parts = append(parts, t)
			}
		}
		if len(parts) == 0 {
			return nil
		}
		return []Block{{Type: heading, Text: strings.Join(parts, " "), Page: page}}
	}
	return pptxParagraphs(body, page, phType == "body")
}

// pptxParagraphs is a text body's paragraphs and lists; bullets says body
// paragraphs are bulleted unless they say otherwise (a body placeholder's
// master default).
func pptxParagraphs(body *node, page int, bullets bool) []Block {
	var out []Block
	lb := &listBuilder{out: &out}
	for _, p := range body.all("p") {
		text := pptxParaText(p)
		if text == "" {
			continue
		}
		pPr := p.child("pPr")
		lvl, _ := strconv.Atoi(pPr.attr("lvl"))
		switch {
		case pPr.child("buNone") != nil:
			lb.flush()
			out = append(out, Block{Type: "paragraph", Text: text, Page: page})
		case pPr.child("buAutoNum") != nil:
			lb.add(text, lvl, true, page)
		case pPr.child("buChar") != nil || pPr.child("buBlip") != nil || bullets:
			lb.add(text, lvl, false, page)
		default:
			lb.flush()
			out = append(out, Block{Type: "paragraph", Text: text, Page: page})
		}
	}
	lb.flush()
	return out
}

func pptxTable(tbl *node, page int) *Table {
	t := &Table{}
	first := tbl.child("tblPr").attr("firstRow")
	header := first == "1" || first == "true"
	for i, tr := range tbl.all("tr") {
		var row []Cell
		for _, tc := range tr.all("tc") {
			if tc.attr("hMerge") == "1" || tc.attr("hMerge") == "true" || tc.attr("vMerge") == "1" || tc.attr("vMerge") == "true" {
				continue
			}
			rs, _ := strconv.Atoi(tc.attr("rowSpan"))
			cs, _ := strconv.Atoi(tc.attr("gridSpan"))
			var blocks []Block
			if b := tc.child("txBody"); b != nil {
				blocks = pptxParagraphs(b, page, false)
			}
			row = append(row, Cell{Blocks: blocks, RowSpan: spanOr1(rs), ColSpan: spanOr1(cs)})
		}
		if i == 0 && header {
			t.Header = append(t.Header, row)
		} else {
			t.Body = append(t.Body, row)
		}
	}
	return t
}
