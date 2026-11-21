// Package officedoc reads the Office Open XML and HTML documents Document
// AI's Layout Parser takes (DOCX, PPTX, XLSX, XLSM and HTML, as
// https://docs.cloud.google.com/document-ai/docs/layout-parse-chunk lists
// them, read 2026-11-21) into layout blocks: titles, headings, paragraphs,
// lists, tables, page headers and footers, each on its page. It is pure Go
// (archive/zip, encoding/xml and golang.org/x/net/html): no converter runs.
package officedoc

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// The MIME types the Layout Parser documents for these formats.
const (
	MIMEHTML = "text/html"
	MIMEDOCX = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	MIMEPPTX = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	MIMEXLSX = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	MIMEXLSM = "application/vnd.ms-excel.sheet.macroenabled.12"
)

// MaxCells is the documented XLSX limit: "Files with up to 5 million cells
// can be processed."
const MaxCells = 5_000_000

// maxCells is MaxCells, lowered by tests.
var maxCells = MaxCells

// ErrTooManyCells is returned, wrapped with the count, for a spreadsheet of
// more than MaxCells cells.
var ErrTooManyCells = errors.New("the spreadsheet has more cells than the Layout Parser processes (5 million)")

// Zip limits: what is read from one part and from the whole archive.
const (
	maxPartBytes  = 100 << 20
	maxTotalBytes = 200 << 20
)

// Document is a document's layout blocks, in reading order.
type Document struct {
	Blocks []Block
	Pages  int // at least 1
}

// Block is one layout block.
type Block struct {
	// Type is "paragraph", "title", "subtitle", "heading-1" to "heading-5",
	// "header", "footer", "list" or "table".
	Type  string
	Text  string // a text block's text, whitespace collapsed
	Page  int    // 1-based
	List  *List  // Type "list"
	Table *Table // Type "table"
}

// List is a list's entries, each a list of blocks; a nested list is a
// "list" Block inside an entry.
type List struct {
	Ordered bool
	Entries [][]Block
}

// Table is a table's caption and its header and body rows.
type Table struct {
	Caption      string
	Header, Body [][]Cell
}

// Cell is a table cell: its blocks and spans (at least 1).
type Cell struct {
	Blocks           []Block
	RowSpan, ColSpan int
}

// baseType is mimeType in lower case without parameters.
func baseType(mimeType string) string {
	t, _, _ := strings.Cut(mimeType, ";")
	return strings.ToLower(strings.TrimSpace(t))
}

// Supported reports whether Parse reads mimeType.
func Supported(mimeType string) bool {
	switch baseType(mimeType) {
	case MIMEHTML, MIMEDOCX, MIMEPPTX, MIMEXLSX, MIMEXLSM:
		return true
	}
	return false
}

// Parse reads a document of one of the supported MIME types.
func Parse(content []byte, mimeType string) (*Document, error) {
	var doc *Document
	var err error
	switch baseType(mimeType) {
	case MIMEHTML:
		doc, err = parseHTML(content)
	case MIMEDOCX:
		doc, err = withZip(content, parseDOCX)
	case MIMEPPTX:
		doc, err = withZip(content, parsePPTX)
	case MIMEXLSX, MIMEXLSM:
		doc, err = withZip(content, parseXLSX)
	default:
		return nil, fmt.Errorf("unsupported document type %q", mimeType)
	}
	if err != nil {
		return nil, err
	}
	if doc.Pages < 1 {
		doc.Pages = 1
	}
	return doc, nil
}

// pkg is an opened OOXML package.
type pkg struct {
	files map[string]*zip.File
	read  int64
}

func withZip(content []byte, parse func(*pkg) (*Document, error)) (*Document, error) {
	zr, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
	if err != nil {
		return nil, fmt.Errorf("the document is not a valid Office file: %v", err)
	}
	p := &pkg{files: map[string]*zip.File{}}
	for _, f := range zr.File {
		p.files[strings.TrimPrefix(f.Name, "/")] = f
	}
	return parse(p)
}

// part is a part's bytes, nil when it is absent.
func (p *pkg) part(name string) ([]byte, error) {
	f, ok := p.files[strings.TrimPrefix(name, "/")]
	if !ok {
		return nil, nil
	}
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("reading %s: %v", name, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, maxPartBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %v", name, err)
	}
	if len(b) > maxPartBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes uncompressed", name, maxPartBytes)
	}
	p.read += int64(len(b))
	if p.read > maxTotalBytes {
		return nil, fmt.Errorf("the document is larger than %d bytes uncompressed", maxTotalBytes)
	}
	return b, nil
}

// rels are a part's relationships, by id: the target resolved against the
// part's directory.
func (p *pkg) rels(partName string) (map[string]string, error) {
	dir, file := path.Split(partName)
	b, err := p.part(dir + "_rels/" + file + ".rels")
	if err != nil || b == nil {
		return map[string]string{}, err
	}
	var r struct {
		Rel []struct {
			ID     string `xml:"Id,attr"`
			Target string `xml:"Target,attr"`
			Mode   string `xml:"TargetMode,attr"`
		} `xml:"Relationship"`
	}
	if err := xml.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("reading %s's relationships: %v", partName, err)
	}
	out := map[string]string{}
	for _, x := range r.Rel {
		if x.Mode == "External" {
			continue
		}
		t := x.Target
		if strings.HasPrefix(t, "/") {
			t = strings.TrimPrefix(t, "/")
		} else {
			t = path.Clean(dir + t)
		}
		out[x.ID] = t
	}
	return out, nil
}

// collapse is s with runs of white space as one space, trimmed.
func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// headingType is "heading-N" for level n (1-based), clamped to 1..5.
func headingType(n int) string {
	return fmt.Sprintf("heading-%d", min(max(n, 1), 5))
}

// listBuilder groups consecutive list entries, by level, into nested lists.
type listBuilder struct {
	out   *[]Block
	stack []*List // stack[i] is the open list at level i
	page  int
}

// add appends an entry of text at level (0-based) to the open list.
func (lb *listBuilder) add(text string, level int, ordered bool, page int) {
	if len(lb.stack) == 0 {
		lb.page = page
	}
	level = max(level, 0)
	if level > len(lb.stack) {
		level = len(lb.stack) // no skipped levels
	}
	lb.stack = lb.stack[:min(len(lb.stack), level+1)]
	if level == len(lb.stack) {
		l := &List{Ordered: ordered}
		if level == 0 {
			lb.stack = append(lb.stack, l)
		} else {
			parent := lb.stack[level-1]
			if len(parent.Entries) == 0 {
				parent.Entries = append(parent.Entries, nil)
			}
			last := len(parent.Entries) - 1
			parent.Entries[last] = append(parent.Entries[last], Block{Type: "list", Page: page, List: l})
			lb.stack = append(lb.stack, l)
		}
	}
	l := lb.stack[level]
	l.Entries = append(l.Entries, []Block{{Type: "paragraph", Text: text, Page: page}})
}

// flush closes the open list, appending it to out.
func (lb *listBuilder) flush() {
	if len(lb.stack) > 0 {
		*lb.out = append(*lb.out, Block{Type: "list", Page: lb.page, List: lb.stack[0]})
	}
	lb.stack = nil
}

// spanOr1 is a span of at least 1.
func spanOr1(n int) int { return max(n, 1) }

// node is an XML element: its local name, attributes, children and, for
// character data, its text (name "").
type node struct {
	name  string
	attrs []xml.Attr
	kids  []*node
	text  string
}

// parseXML reads b into a tree of nodes, the root returned.
func parseXML(b []byte) (*node, error) {
	dec := xml.NewDecoder(bytes.NewReader(b))
	dec.Strict = false
	root := &node{}
	stack := []*node{root}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		top := stack[len(stack)-1]
		switch t := tok.(type) {
		case xml.StartElement:
			n := &node{name: t.Name.Local, attrs: append([]xml.Attr(nil), t.Attr...)}
			top.kids = append(top.kids, n)
			stack = append(stack, n)
		case xml.EndElement:
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
		case xml.CharData:
			if len(top.kids) > 0 && top.kids[len(top.kids)-1].name == "" {
				top.kids[len(top.kids)-1].text += string(t)
			} else {
				top.kids = append(top.kids, &node{text: string(t)})
			}
		}
	}
	return root, nil
}

// attr is the value of n's attribute with local name local.
func (n *node) attr(local string) string {
	if n == nil {
		return ""
	}
	for _, a := range n.attrs {
		if a.Name.Local == local {
			return a.Value
		}
	}
	return ""
}

// relID is n's r:id, the attribute "id" in the relationships namespace.
func (n *node) relID() string {
	if n == nil {
		return ""
	}
	for _, a := range n.attrs {
		if a.Name.Local == "id" && strings.Contains(a.Name.Space, "relationships") {
			return a.Value
		}
	}
	return ""
}

// child is n's first child element named name, or nil.
func (n *node) child(name string) *node {
	if n == nil {
		return nil
	}
	for _, k := range n.kids {
		if k.name == name {
			return k
		}
	}
	return nil
}

// path is the element reached by following names from n, or nil.
func (n *node) path(names ...string) *node {
	for _, name := range names {
		n = n.child(name)
	}
	return n
}

// all is n's child elements named name.
func (n *node) all(name string) []*node {
	if n == nil {
		return nil
	}
	var out []*node
	for _, k := range n.kids {
		if k.name == name {
			out = append(out, k)
		}
	}
	return out
}

// find is the first element named name in n's subtree (n included).
func (n *node) find(name string) *node {
	if n == nil {
		return nil
	}
	if n.name == name {
		return n
	}
	for _, k := range n.kids {
		if f := k.find(name); f != nil {
			return f
		}
	}
	return nil
}

// chars is the character data directly in n.
func (n *node) chars() string {
	var b strings.Builder
	for _, k := range n.kids {
		if k.name == "" {
			b.WriteString(k.text)
		}
	}
	return b.String()
}

// loadXML is a part parsed, nil when absent.
func (p *pkg) loadXML(name string) (*node, error) {
	b, err := p.part(name)
	if err != nil || b == nil {
		return nil, err
	}
	n, err := parseXML(b)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %v", name, err)
	}
	return n, nil
}
