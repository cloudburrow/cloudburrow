package officedoc

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

func parseHTML(content []byte) (*Document, error) {
	root, err := html.Parse(bytes.NewReader(content))
	if err != nil {
		return nil, fmt.Errorf("the document is not valid HTML: %v", err)
	}
	doc := &Document{Pages: 1}
	if t := findElem(root, atom.Title); t != nil {
		if s := collapse(textOf(t)); s != "" {
			doc.Blocks = append(doc.Blocks, Block{Type: "title", Text: s, Page: 1})
		}
	}
	body := findElem(root, atom.Body)
	if body == nil {
		body = root
	}
	doc.Blocks = append(doc.Blocks, htmlBlocks(body)...)
	return doc, nil
}

// findElem is the first element a in n's subtree.
func findElem(n *html.Node, a atom.Atom) *html.Node {
	if n.Type == html.ElementNode && n.DataAtom == a {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if f := findElem(c, a); f != nil {
			return f
		}
	}
	return nil
}

// skipped are the elements whose content is not document text.
var skipped = map[atom.Atom]bool{atom.Script: true, atom.Style: true, atom.Noscript: true, atom.Template: true, atom.Head: true,
	atom.Title: true, atom.Iframe: true, atom.Object: true, atom.Svg: true, atom.Math: true, atom.Select: true, atom.Button: true}

// blockElems are the elements that end the text before them.
var blockElems = map[atom.Atom]bool{atom.Address: true, atom.Article: true, atom.Aside: true, atom.Blockquote: true, atom.Body: true,
	atom.Center: true, atom.Dd: true, atom.Details: true, atom.Dialog: true, atom.Div: true, atom.Dl: true, atom.Dt: true,
	atom.Fieldset: true, atom.Figcaption: true, atom.Figure: true, atom.Form: true, atom.Hr: true, atom.Html: true, atom.Li: true,
	atom.Main: true, atom.Nav: true, atom.P: true, atom.Pre: true, atom.Section: true, atom.Summary: true, atom.Legend: true}

// textOf is the text in n's subtree, line breaks as spaces.
func textOf(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		switch n.Type {
		case html.TextNode:
			b.WriteString(n.Data)
		case html.ElementNode:
			if n.DataAtom != atom.Title && skipped[n.DataAtom] {
				return
			}
			if n.DataAtom == atom.Br || blockElems[n.DataAtom] {
				b.WriteString(" ")
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode && (blockElems[n.DataAtom] || n.DataAtom == atom.Td || n.DataAtom == atom.Th) {
			b.WriteString(" ")
		}
	}
	walk(n)
	return b.String()
}

// htmlBlocks is the blocks of n's children.
func htmlBlocks(n *html.Node) []Block {
	var out []Block
	var inline strings.Builder
	flush := func() {
		if s := collapse(inline.String()); s != "" {
			out = append(out, Block{Type: "paragraph", Text: s, Page: 1})
		}
		inline.Reset()
	}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			switch c.Type {
			case html.TextNode:
				inline.WriteString(c.Data)
				continue
			case html.ElementNode:
			default:
				continue
			}
			a := c.DataAtom
			switch {
			case skipped[a]:
			case a == atom.H1 || a == atom.H2 || a == atom.H3 || a == atom.H4 || a == atom.H5 || a == atom.H6:
				flush()
				lvl, _ := strconv.Atoi(c.Data[1:])
				if s := collapse(textOf(c)); s != "" {
					out = append(out, Block{Type: headingType(lvl), Text: s, Page: 1})
				}
			case a == atom.Header || a == atom.Footer:
				flush()
				if s := collapse(textOf(c)); s != "" {
					out = append(out, Block{Type: c.Data, Text: s, Page: 1})
				}
			case a == atom.Ul || a == atom.Ol || a == atom.Menu:
				flush()
				if l := htmlList(c); len(l.Entries) > 0 {
					out = append(out, Block{Type: "list", Page: 1, List: l})
				}
			case a == atom.Table:
				flush()
				out = append(out, Block{Type: "table", Page: 1, Table: htmlTable(c)})
			case a == atom.Br:
				inline.WriteString(" ")
			case blockElems[a]:
				flush()
				walk(c)
				flush()
			default:
				walk(c)
			}
		}
	}
	walk(n)
	flush()
	return out
}

func htmlList(n *html.Node) *List {
	l := &List{Ordered: n.DataAtom == atom.Ol}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode {
			continue
		}
		if c.DataAtom == atom.Li {
			if blocks := htmlBlocks(c); len(blocks) > 0 {
				l.Entries = append(l.Entries, blocks)
			}
			continue
		}
		if c.DataAtom == atom.Ul || c.DataAtom == atom.Ol {
			// A list directly in a list belongs to the entry before it.
			if sub := htmlList(c); len(sub.Entries) > 0 {
				b := Block{Type: "list", Page: 1, List: sub}
				if len(l.Entries) == 0 {
					l.Entries = append(l.Entries, nil)
				}
				l.Entries[len(l.Entries)-1] = append(l.Entries[len(l.Entries)-1], b)
			}
		}
	}
	return l
}

func htmlTable(n *html.Node) *Table {
	t := &Table{}
	var rows []*html.Node
	inHead := map[*html.Node]bool{}
	var collect func(*html.Node, bool)
	collect = func(n *html.Node, head bool) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type != html.ElementNode {
				continue
			}
			switch c.DataAtom {
			case atom.Caption:
				if t.Caption == "" {
					t.Caption = collapse(textOf(c))
				}
			case atom.Thead:
				collect(c, true)
			case atom.Tbody, atom.Tfoot:
				collect(c, false)
			case atom.Tr:
				rows = append(rows, c)
				inHead[c] = head
			}
		}
	}
	collect(n, false)
	for i, tr := range rows {
		var row []Cell
		allTH := true
		for c := tr.FirstChild; c != nil; c = c.NextSibling {
			if c.Type != html.ElementNode || c.DataAtom != atom.Td && c.DataAtom != atom.Th {
				continue
			}
			if c.DataAtom == atom.Td {
				allTH = false
			}
			rs, _ := strconv.Atoi(htmlAttr(c, "rowspan"))
			cs, _ := strconv.Atoi(htmlAttr(c, "colspan"))
			row = append(row, Cell{Blocks: htmlBlocks(c), RowSpan: spanOr1(rs), ColSpan: spanOr1(cs)})
		}
		if len(row) == 0 {
			continue
		}
		if inHead[tr] || i == 0 && allTH && len(t.Header) == 0 && len(t.Body) == 0 {
			t.Header = append(t.Header, row)
		} else {
			t.Body = append(t.Body, row)
		}
	}
	return t
}

func htmlAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}
