package officedoc

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

func parseXLSX(p *pkg) (*Document, error) {
	const wb = "xl/workbook.xml"
	root, err := p.loadXML(wb)
	if err != nil {
		return nil, err
	}
	if root == nil {
		return nil, errors.New("the document is not a valid spreadsheet: it has no xl/workbook.xml")
	}
	rels, err := p.rels(wb)
	if err != nil {
		return nil, err
	}
	shared, err := sharedStrings(p)
	if err != nil {
		return nil, err
	}
	doc := &Document{}
	cells := 0
	for _, sh := range root.path("workbook", "sheets").all("sheet") {
		target, ok := rels[sh.relID()]
		if !ok {
			continue
		}
		b, err := p.part(target)
		if err != nil {
			return nil, err
		}
		if b == nil {
			continue
		}
		doc.Pages++
		t, n, err := readSheet(b, shared, maxCells-cells, doc.Pages)
		if err != nil {
			return nil, fmt.Errorf("sheet %q: %w", sh.attr("name"), err)
		}
		cells += n
		t.Caption = sh.attr("name")
		doc.Blocks = append(doc.Blocks, Block{Type: "table", Page: doc.Pages, Table: t})
	}
	return doc, nil
}

// sharedStrings is the workbook's shared string table.
func sharedStrings(p *pkg) ([]string, error) {
	root, err := p.loadXML("xl/sharedStrings.xml")
	if err != nil || root == nil {
		return nil, err
	}
	var out []string
	for _, si := range root.path("sst").all("si") {
		out = append(out, richText(si))
	}
	return out, nil
}

// richText is a string item's text: its t, or its runs' t, without
// phonetic runs.
func richText(si *node) string {
	if t := si.child("t"); t != nil {
		return t.chars()
	}
	var b strings.Builder
	for _, r := range si.all("r") {
		b.WriteString(r.child("t").chars())
	}
	return b.String()
}

// cellRef is a cell reference's 1-based column and row ("C5" is 3, 5).
func cellRef(ref string) (col, row int, ok bool) {
	i := 0
	for i < len(ref) && (ref[i] >= 'A' && ref[i] <= 'Z' || ref[i] >= 'a' && ref[i] <= 'z') {
		c := ref[i]
		if c >= 'a' {
			c -= 'a' - 'A'
		}
		col = col*26 + int(c-'A'+1)
		i++
		if col > 1<<20 {
			return 0, 0, false
		}
	}
	row, err := strconv.Atoi(strings.TrimLeft(ref[i:], "$"))
	return col, row, err == nil && i > 0 && row > 0
}

type sheetCell struct {
	col, row int
	text     string
}

// readSheet reads a worksheet into a table: its used range, the first row
// the header; n is its cell count, at most limit. Its cells' blocks are
// on page.
func readSheet(b []byte, shared []string, limit, page int) (t *Table, n int, err error) {
	dec := xml.NewDecoder(bytes.NewReader(b))
	dec.Strict = false
	var cells []sheetCell
	var merges []string
	seen := 0
	row, col := 0, 0
	var cur *sheetCell
	var curType string
	var inV, inIS, inT, inRPh bool
	var text strings.Builder
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, 0, err
		}
		switch x := tok.(type) {
		case xml.StartElement:
			switch x.Name.Local {
			case "row":
				if r, err := strconv.Atoi(attrOf(x, "r")); err == nil && r > 0 {
					row = r
				} else {
					row++
				}
				col = 0
			case "c":
				seen++
				if seen > limit {
					return nil, 0, fmt.Errorf("%w: more than %d", ErrTooManyCells, maxCells)
				}
				if c, r, ok := cellRef(attrOf(x, "r")); ok {
					col, row = c, r
				} else {
					col++
				}
				cur, curType = &sheetCell{col: col, row: row}, attrOf(x, "t")
				text.Reset()
			case "v":
				inV = true
			case "is":
				inIS = true
			case "t":
				inT = true
			case "rPh":
				inRPh = true
			case "mergeCell":
				merges = append(merges, attrOf(x, "ref"))
			}
		case xml.EndElement:
			switch x.Name.Local {
			case "v":
				inV = false
			case "is":
				inIS = false
			case "t":
				inT = false
			case "rPh":
				inRPh = false
			case "c":
				if cur != nil {
					cur.text = cellText(curType, text.String(), shared)
					if strings.TrimSpace(cur.text) != "" {
						cells = append(cells, *cur)
					}
				}
				cur = nil
			}
		case xml.CharData:
			if cur != nil && (inV || inIS && inT && !inRPh) {
				text.Write(x)
			}
		}
	}
	t = &Table{}
	if len(cells) == 0 {
		return t, seen, nil
	}
	minR, minC, maxR, maxC := cells[0].row, cells[0].col, cells[0].row, cells[0].col
	for _, c := range cells {
		minR, minC, maxR, maxC = min(minR, c.row), min(minC, c.col), max(maxR, c.row), max(maxC, c.col)
	}
	rows, cols := maxR-minR+1, maxC-minC+1
	if rows*cols > limit || rows*cols < 0 {
		return nil, 0, fmt.Errorf("%w: its used range has %d", ErrTooManyCells, rows*cols)
	}
	grid := make([][]Cell, rows)
	covered := make([][]bool, rows)
	for i := range grid {
		grid[i] = make([]Cell, cols)
		covered[i] = make([]bool, cols)
		for j := range grid[i] {
			grid[i][j] = Cell{RowSpan: 1, ColSpan: 1}
		}
	}
	for _, c := range cells {
		grid[c.row-minR][c.col-minC].Blocks = []Block{{Type: "paragraph", Text: collapse(c.text), Page: page}}
	}
	for _, m := range merges {
		a, z, ok := strings.Cut(m, ":")
		c1, r1, ok1 := cellRef(a)
		c2, r2, ok2 := cellRef(z)
		if !ok || !ok1 || !ok2 {
			continue
		}
		r1, r2, c1, c2 = max(r1, minR)-minR, min(r2, maxR)-minR, max(c1, minC)-minC, min(c2, maxC)-minC
		if r1 > r2 || c1 > c2 || covered[r1][c1] {
			continue
		}
		grid[r1][c1].RowSpan, grid[r1][c1].ColSpan = r2-r1+1, c2-c1+1
		for i := r1; i <= r2; i++ {
			for j := c1; j <= c2; j++ {
				if i != r1 || j != c1 {
					covered[i][j] = true
				}
			}
		}
	}
	for i := range grid {
		var row []Cell
		for j, c := range grid[i] {
			if !covered[i][j] {
				row = append(row, c)
			}
		}
		if i == 0 {
			t.Header = append(t.Header, row)
		} else {
			t.Body = append(t.Body, row)
		}
	}
	return t, max(seen, rows*cols), nil
}

// cellText is a cell's value as text, by its type.
func cellText(typ, v string, shared []string) string {
	switch typ {
	case "s":
		i, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || i < 0 || i >= len(shared) {
			return ""
		}
		return shared[i]
	case "b":
		if strings.TrimSpace(v) == "1" {
			return "TRUE"
		}
		return "FALSE"
	}
	return v
}

func attrOf(se xml.StartElement, local string) string {
	for _, a := range se.Attr {
		if a.Name.Local == local {
			return a.Value
		}
	}
	return ""
}
