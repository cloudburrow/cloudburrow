package vertexai

// The Layout Parser's documentLayout and chunkedDocument (#1799), in the
// shape https://docs.cloud.google.com/document-ai/docs/reference/rest/v1/Document
// gives them (read 2026-11-21): a tree of DocumentLayoutBlocks, each a
// textBlock (type paragraph, subtitle, heading-1 to heading-5, header or
// footer, with child blocks), a tableBlock (headerRows, bodyRows of cells
// with rowSpan and colSpan, caption) or a listBlock (ordered or unordered
// listEntries), every block with its blockId and pageSpan; and chunks with
// chunkId, content, pageSpan, pageHeaders and pageFooters
// ("sourceBlockIds: Unused", so it is left empty).
//
// https://docs.cloud.google.com/document-ai/docs/layout-parse-chunk (read
// 2026-11-21) lists what is detected per input: for PDFs (and images) the
// paragraphs, titles, headings, page headers and page footers come from
// Tesseract's layout here (figures and tables are not detected); for HTML,
// DOCX, PPTX, XLSX and XLSM the document's own structure is read by
// internal/officedoc in pure Go: paragraphs, lists, tables, titles,
// headings, page headers and footers. A heading's following blocks are its
// children, a deeper heading nested in a shallower one, so the tree
// "preserves the document's inherent hierarchy". Chunks are "augmented with
// contextual information, such as their ancestral headings": with
// include_ancestor_headings each chunk's content starts with the headings
// above its first block. A chunk never crosses a heading unless it holds
// only headings so far, and holds at most chunk_size words (a token here is
// a whitespace-separated word: UNVERIFIED how Google counts them); a block
// longer than that is split, a table by rows with its header rows repeated.
// Docs re-read 2026-11-21, still unanswered: ChunkingConfig.chunkSize gives
// no unit (https://cloud.google.com/document-ai/docs/reference/rest/v1/ProcessOptions,
// https://cloud.google.com/document-ai/docs/layout-parse-chunk).

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"cloud.google.com/go/documentai/apiv1/documentaipb"

	"github.com/cloudburrow/cloudburrow/internal/ocr"
	"github.com/cloudburrow/cloudburrow/internal/officedoc"
)

// layoutOptions are the chunking options a layout document is built with.
type layoutOptions struct {
	chunkWords       int
	ancestorHeadings bool
}

// layoutNode is a block of the tree being built.
type layoutNode struct {
	b        officedoc.Block
	level    int // a heading's depth (0 a title), maxLevel for others
	children []*layoutNode
}

const maxLevel = 99

// headingLevel is a text block type's depth: 0 for a title, 1-5 for
// heading-N, maxLevel for every other block.
func headingLevel(t string) int {
	switch {
	case t == "title":
		return 0
	case strings.HasPrefix(t, "heading-"):
		n, err := strconv.Atoi(strings.TrimPrefix(t, "heading-"))
		if err == nil && n >= 1 {
			return min(n, 5)
		}
	}
	return maxLevel
}

// layoutTree nests each heading's following blocks under it. Page headers
// and footers stay at the top level, in order.
func layoutTree(blocks []officedoc.Block) []*layoutNode {
	var roots, stack []*layoutNode
	for _, b := range blocks {
		n := &layoutNode{b: b, level: maxLevel}
		if b.Type == "header" || b.Type == "footer" {
			roots = append(roots, n)
			continue
		}
		if b.Type != "list" && b.Type != "table" {
			n.level = headingLevel(b.Type)
		}
		if n.level < maxLevel {
			for len(stack) > 0 && stack[len(stack)-1].level >= n.level {
				stack = stack[:len(stack)-1]
			}
		}
		if len(stack) == 0 {
			roots = append(roots, n)
		} else {
			top := stack[len(stack)-1]
			top.children = append(top.children, n)
		}
		if n.level < maxLevel {
			stack = append(stack, n)
		}
	}
	return roots
}

// textType is a block's documented LayoutTextBlock.type: a title is the
// top heading, heading-1 (the type has no title value).
func textType(t string) string {
	if t == "title" {
		return "heading-1"
	}
	if strings.HasPrefix(t, "heading-") {
		return "heading-" + strconv.Itoa(headingLevel(t))
	}
	return t
}

// layoutBuilder numbers blocks in document order.
type layoutBuilder struct{ next int }

func (lb *layoutBuilder) id() string {
	lb.next++
	return strconv.Itoa(lb.next)
}

func pageSpan(start, end int) *documentaipb.Document_DocumentLayout_DocumentLayoutBlock_LayoutPageSpan {
	return &documentaipb.Document_DocumentLayout_DocumentLayoutBlock_LayoutPageSpan{PageStart: int32(start), PageEnd: int32(end)}
}

// block is n as a DocumentLayoutBlock, and its last page.
func (lb *layoutBuilder) block(n *layoutNode) (*documentaipb.Document_DocumentLayout_DocumentLayoutBlock, int) {
	page := max(n.b.Page, 1)
	out := &documentaipb.Document_DocumentLayout_DocumentLayoutBlock{BlockId: lb.id()}
	end := page
	switch n.b.Type {
	case "list":
		l := &documentaipb.Document_DocumentLayout_DocumentLayoutBlock_LayoutListBlock{Type: "unordered"}
		if n.b.List != nil {
			if n.b.List.Ordered {
				l.Type = "ordered"
			}
			for _, e := range n.b.List.Entries {
				blocks, last := lb.blocks(e)
				end = max(end, last)
				l.ListEntries = append(l.ListEntries, &documentaipb.Document_DocumentLayout_DocumentLayoutBlock_LayoutListEntry{Blocks: blocks})
			}
		}
		out.Block = &documentaipb.Document_DocumentLayout_DocumentLayoutBlock_ListBlock{ListBlock: l}
	case "table":
		tb := &documentaipb.Document_DocumentLayout_DocumentLayoutBlock_LayoutTableBlock{}
		if t := n.b.Table; t != nil {
			tb.Caption = t.Caption
			rows := func(in [][]officedoc.Cell) []*documentaipb.Document_DocumentLayout_DocumentLayoutBlock_LayoutTableRow {
				var out []*documentaipb.Document_DocumentLayout_DocumentLayoutBlock_LayoutTableRow
				for _, r := range in {
					row := &documentaipb.Document_DocumentLayout_DocumentLayoutBlock_LayoutTableRow{}
					for _, c := range r {
						blocks, last := lb.blocks(c.Blocks)
						end = max(end, last)
						row.Cells = append(row.Cells, &documentaipb.Document_DocumentLayout_DocumentLayoutBlock_LayoutTableCell{Blocks: blocks,
							RowSpan: int32(max(c.RowSpan, 1)), ColSpan: int32(max(c.ColSpan, 1))})
					}
					out = append(out, row)
				}
				return out
			}
			tb.HeaderRows, tb.BodyRows = rows(t.Header), rows(t.Body)
		}
		out.Block = &documentaipb.Document_DocumentLayout_DocumentLayoutBlock_TableBlock{TableBlock: tb}
	default:
		t := &documentaipb.Document_DocumentLayout_DocumentLayoutBlock_LayoutTextBlock{Text: n.b.Text, Type: textType(n.b.Type)}
		for _, c := range n.children {
			cb, last := lb.block(c)
			end = max(end, last)
			t.Blocks = append(t.Blocks, cb)
		}
		out.Block = &documentaipb.Document_DocumentLayout_DocumentLayoutBlock_TextBlock{TextBlock: t}
	}
	out.PageSpan = pageSpan(page, end)
	return out, end
}

// blocks lays out a list entry's or a table cell's blocks, nesting their
// headings as the document's are.
func (lb *layoutBuilder) blocks(in []officedoc.Block) ([]*documentaipb.Document_DocumentLayout_DocumentLayoutBlock, int) {
	var out []*documentaipb.Document_DocumentLayout_DocumentLayoutBlock
	end := 0
	for _, n := range layoutTree(in) {
		b, last := lb.block(n)
		end = max(end, last)
		out = append(out, b)
	}
	return out, end
}

// layoutFromBlocks is the Layout Parser's Document of a document's blocks.
func layoutFromBlocks(mimeType string, blocks []officedoc.Block, o layoutOptions) *documentaipb.Document {
	lb := &layoutBuilder{}
	roots := layoutTree(blocks)
	layout := &documentaipb.Document_DocumentLayout{}
	for _, n := range roots {
		b, _ := lb.block(n)
		layout.Blocks = append(layout.Blocks, b)
	}
	return &documentaipb.Document{MimeType: mimeType, DocumentLayout: layout, ChunkedDocument: chunkTree(roots, o)}
}

// --- chunks ---------------------------------------------------------------

// chunkUnit is one block's text for chunking.
type chunkUnit struct {
	lines      []string // the block's text, one line each
	words      int
	heading    bool
	path       []string // the headings above it
	start, end int      // pages
	table      int      // header lines to repeat when a table is split
}

// wordCount counts the words of lines, not a table's | and --- marks.
func wordCount(lines []string) int {
	n := 0
	for _, l := range lines {
		n += lineWords(l)
	}
	return n
}

func lineWords(l string) int {
	n := 0
	for _, w := range strings.Fields(l) {
		if strings.IndexFunc(w, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) >= 0 {
			n++
		}
	}
	return n
}

// flatText is a list entry's or cell's blocks as one line.
func flatText(blocks []officedoc.Block) string {
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "list":
			if b.List != nil {
				for _, e := range b.List.Entries {
					parts = append(parts, flatText(e))
				}
			}
		case "table":
			if b.Table != nil {
				for _, r := range append(append([][]officedoc.Cell{}, b.Table.Header...), b.Table.Body...) {
					for _, c := range r {
						parts = append(parts, flatText(c.Blocks))
					}
				}
			}
		default:
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(strings.Fields(strings.Join(parts, " ")), " ")
}

// tableLines renders a table as Markdown-style rows, its header rows first
// and separated from the body.
func tableLines(t *officedoc.Table) ([]string, int) {
	if t == nil {
		return nil, 0
	}
	row := func(r []officedoc.Cell) string {
		cells := make([]string, 0, len(r))
		for _, c := range r {
			for range max(c.ColSpan, 1) {
				cells = append(cells, strings.ReplaceAll(flatText(c.Blocks), "|", "\\|"))
			}
		}
		return "| " + strings.Join(cells, " | ") + " |"
	}
	var lines []string
	if t.Caption != "" {
		lines = append(lines, t.Caption)
	}
	for _, r := range t.Header {
		lines = append(lines, row(r))
	}
	if len(t.Header) > 0 {
		n := 0
		for _, c := range t.Header[len(t.Header)-1] {
			n += max(c.ColSpan, 1)
		}
		lines = append(lines, "|"+strings.Repeat(" --- |", n))
	}
	head := len(lines)
	for _, r := range t.Body {
		lines = append(lines, row(r))
	}
	return lines, head
}

// units walks the tree in document order.
func units(nodes []*layoutNode, path []string, headers, footers *[]officedoc.Block, out *[]chunkUnit) {
	for _, n := range nodes {
		page := max(n.b.Page, 1)
		u := chunkUnit{path: path, start: page, end: page}
		switch n.b.Type {
		case "header":
			*headers = append(*headers, n.b)
			continue
		case "footer":
			*footers = append(*footers, n.b)
			continue
		case "list":
			if n.b.List != nil {
				for i, e := range n.b.List.Entries {
					mark := "-"
					if n.b.List.Ordered {
						mark = strconv.Itoa(i+1) + "."
					}
					u.lines = append(u.lines, mark+" "+flatText(e))
				}
			}
		case "table":
			u.lines, u.table = tableLines(n.b.Table)
		default:
			if strings.TrimSpace(n.b.Text) != "" {
				u.lines = []string{n.b.Text}
			}
			u.heading = n.level < maxLevel
		}
		u.words = wordCount(u.lines)
		if len(u.lines) > 0 {
			*out = append(*out, u)
		}
		if len(n.children) > 0 {
			sub := path
			if u.heading && len(u.lines) > 0 {
				sub = append(append([]string{}, path...), n.b.Text)
			}
			units(n.children, sub, headers, footers, out)
		}
	}
}

// splitUnit cuts a unit longer than size words into pieces of at most size
// words (a table by rows, its header lines repeated in each piece; a text
// by words).
func splitUnit(u chunkUnit, size int) []chunkUnit {
	if u.words <= size {
		return []chunkUnit{u}
	}
	var out []chunkUnit
	piece := func(lines []string) {
		v := u
		v.lines, v.words = lines, wordCount(lines)
		out = append(out, v)
	}
	if u.table > 0 || len(u.lines) > 1 {
		head := u.lines[:u.table]
		cur := append([]string{}, head...)
		body := false
		for _, l := range u.lines[u.table:] {
			if body && wordCount(cur)+lineWords(l) > size {
				piece(cur)
				cur, body = append([]string{}, head...), false
			}
			cur, body = append(cur, l), true
		}
		if body {
			piece(cur)
		}
		var final []chunkUnit
		for _, p := range out {
			if p.words > size && len(p.lines) == u.table+1 {
				// One row longer than size: split as text.
				p.table, p.lines = 0, []string{strings.Join(p.lines, " ")}
				final = append(final, splitUnit(p, size)...)
				continue
			}
			final = append(final, p)
		}
		return final
	}
	w := strings.Fields(u.lines[0])
	for i := 0; i < len(w); i += size {
		piece([]string{strings.Join(w[i:min(i+size, len(w))], " ")})
	}
	return out
}

// chunkTree chunks the tree's text.
func chunkTree(roots []*layoutNode, o layoutOptions) *documentaipb.Document_ChunkedDocument {
	size := o.chunkWords
	if size <= 0 {
		size = docAIDefaultChunkWords
	}
	var us []chunkUnit
	var headers, footers []officedoc.Block
	units(roots, nil, &headers, &footers, &us)
	out := &documentaipb.Document_ChunkedDocument{}
	var cur []chunkUnit
	words, content := 0, false
	flush := func() {
		if len(cur) == 0 {
			return
		}
		var lines []string
		if o.ancestorHeadings {
			lines = append(lines, cur[0].path...)
		}
		start, end := cur[0].start, cur[0].end
		for _, u := range cur {
			lines = append(lines, u.lines...)
			start, end = min(start, u.start), max(end, u.end)
		}
		c := &documentaipb.Document_ChunkedDocument_Chunk{ChunkId: "c" + strconv.Itoa(len(out.Chunks)+1), Content: strings.Join(lines, "\n"),
			PageSpan: &documentaipb.Document_ChunkedDocument_Chunk_ChunkPageSpan{PageStart: int32(start), PageEnd: int32(end)}}
		for _, h := range headers {
			if h.Page >= start && h.Page <= end {
				c.PageHeaders = append(c.PageHeaders, &documentaipb.Document_ChunkedDocument_Chunk_ChunkPageHeader{Text: h.Text,
					PageSpan: &documentaipb.Document_ChunkedDocument_Chunk_ChunkPageSpan{PageStart: int32(h.Page), PageEnd: int32(h.Page)}})
			}
		}
		for _, f := range footers {
			if f.Page >= start && f.Page <= end {
				c.PageFooters = append(c.PageFooters, &documentaipb.Document_ChunkedDocument_Chunk_ChunkPageFooter{Text: f.Text,
					PageSpan: &documentaipb.Document_ChunkedDocument_Chunk_ChunkPageSpan{PageStart: int32(f.Page), PageEnd: int32(f.Page)}})
			}
		}
		out.Chunks = append(out.Chunks, c)
		cur, words, content = nil, 0, false
	}
	for _, u := range us {
		if u.heading && content {
			flush()
		}
		for _, p := range splitUnit(u, size) {
			if len(cur) > 0 && words+p.words > size {
				flush()
			}
			cur = append(cur, p)
			words += p.words
			content = content || !p.heading
		}
	}
	flush()
	return out
}

// --- PDFs and images: Tesseract's layout ------------------------------------

// ocrPara is a paragraph Tesseract found, with what classifies it.
type ocrPara struct {
	page         int
	lines        []string
	height       float64 // median word height, pixels
	top, bottom  float64 // fraction of the page height
	words        int
	header, foot bool
	headingLevel int
	text         string
	key          string // text with digits removed, for repeated headers
}

var (
	bulletRE  = regexp.MustCompile(`^(?:[•◦▪▫‣●○■□➢►\-–—*·])\s+`)
	orderedRE = regexp.MustCompile(`^(?:\(?(?:\d{1,3}|[a-zA-Z]|[ivxIVX]{1,4})[.)])\s+`)
	pageNoRE  = regexp.MustCompile(`(?i)^(?:page\s*)?\d{1,4}(?:\s*(?:of|/)\s*\d{1,4})?$|^-\s*\d{1,4}\s*-$`)
	digitsRE  = regexp.MustCompile(`\d+`)
)

func quadHeight(q ocr.Quad) float64 {
	top := min(q[0].Y, q[1].Y)
	bottom := max(q[2].Y, q[3].Y)
	return float64(bottom - top)
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64{}, v...)
	sort.Float64s(s)
	return s[len(s)/2]
}

// ocrBlocks classifies Tesseract's paragraphs as the Layout Parser does a
// PDF's: page headers and footers (short text in the top or bottom 8% of a
// page that repeats, digits aside, on another page, or is a page number),
// headings (short lines set at least 1.3 times the body's median word
// height, the tallest heading-1), list entries (lines that start with a
// bullet or a number), and paragraphs.
func ocrBlocks(pages []docPage) []officedoc.Block {
	var paras []*ocrPara
	var all []float64
	for _, pg := range pages {
		for _, blk := range pg.read.Blocks {
			for _, p := range blk.Paragraphs {
				var lines []string
				var line []string
				var hs []float64
				for _, w := range p.Words {
					s, end := wordText(w)
					if s == "" {
						continue
					}
					line = append(line, s)
					hs = append(hs, quadHeight(w.Box))
					if end {
						lines, line = append(lines, strings.Join(line, " ")), nil
					}
				}
				if len(line) > 0 {
					lines = append(lines, strings.Join(line, " "))
				}
				if len(lines) == 0 {
					continue
				}
				all = append(all, hs...)
				op := &ocrPara{page: pg.n, lines: lines, height: median(hs), words: len(hs)}
				if pg.h > 0 {
					op.top = float64(min(p.Box[0].Y, p.Box[1].Y)) / float64(pg.h)
					op.bottom = float64(max(p.Box[2].Y, p.Box[3].Y)) / float64(pg.h)
				}
				op.text = strings.Join(lines, " ")
				op.key = strings.ToLower(digitsRE.ReplaceAllString(op.text, "#"))
				paras = append(paras, op)
			}
		}
	}
	body := median(all)
	// Page headers and footers.
	bandKeys := map[string]map[int]bool{}
	for _, p := range paras {
		if p.words <= 12 && (p.top < 0.08 || p.bottom > 0.92) {
			if bandKeys[p.key] == nil {
				bandKeys[p.key] = map[int]bool{}
			}
			bandKeys[p.key][p.page] = true
		}
	}
	for _, p := range paras {
		if p.words > 12 || (p.top >= 0.08 && p.bottom <= 0.92) {
			continue
		}
		if len(bandKeys[p.key]) > 1 || pageNoRE.MatchString(p.text) {
			p.header, p.foot = p.top < 0.08, p.top >= 0.08
		}
	}
	// Headings, by height.
	var sizes []float64
	for _, p := range paras {
		if p.header || p.foot || body <= 0 || p.words > 15 || len(p.lines) > 2 || strings.HasSuffix(p.text, ".") {
			continue
		}
		if r := p.height / body; r >= 1.3 {
			p.headingLevel = -1
			sizes = append(sizes, r)
		}
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(sizes)))
	var levels []float64 // distinct sizes, 10% apart
	for _, r := range sizes {
		if len(levels) == 0 || levels[len(levels)-1]/r > 1.1 {
			levels = append(levels, r)
		}
	}
	for _, p := range paras {
		if p.headingLevel != -1 {
			continue
		}
		r := p.height / body
		lvl := len(levels)
		for i, l := range levels {
			if l/r <= 1.1 {
				lvl = i + 1
				break
			}
		}
		p.headingLevel = min(lvl, 5)
	}
	var out []officedoc.Block
	var list *officedoc.Block
	endList := func() {
		if list != nil {
			out = append(out, *list)
			list = nil
		}
	}
	for _, p := range paras {
		switch {
		case p.header || p.foot:
			t := "header"
			if p.foot {
				t = "footer"
			}
			endList()
			out = append(out, officedoc.Block{Type: t, Text: p.text, Page: p.page})
			continue
		case p.headingLevel > 0:
			endList()
			out = append(out, officedoc.Block{Type: "heading-" + strconv.Itoa(p.headingLevel), Text: p.text, Page: p.page})
			continue
		}
		entries, ordered, ok := listLines(p.lines)
		if !ok {
			endList()
			out = append(out, officedoc.Block{Type: "paragraph", Text: p.text, Page: p.page})
			continue
		}
		if list != nil && list.List.Ordered != ordered {
			endList()
		}
		if list == nil {
			list = &officedoc.Block{Type: "list", Page: p.page, List: &officedoc.List{Ordered: ordered}}
		}
		for _, e := range entries {
			list.List.Entries = append(list.List.Entries, []officedoc.Block{{Type: "paragraph", Text: e, Page: p.page}})
		}
	}
	endList()
	return out
}

// listLines splits a paragraph whose first line starts with a bullet or a
// number into its entries, a line without a marker continuing the entry
// before it.
func listLines(lines []string) ([]string, bool, bool) {
	marker := func(l string) (string, bool, bool) {
		if m := bulletRE.FindString(l); m != "" {
			return l[len(m):], false, true
		}
		if m := orderedRE.FindString(l); m != "" && utf8.RuneCountInString(l) > len(m) {
			return l[len(m):], true, true
		}
		return l, false, false
	}
	first, ordered, ok := marker(lines[0])
	if !ok {
		return nil, false, false
	}
	entries := []string{first}
	for _, l := range lines[1:] {
		if rest, _, ok := marker(l); ok {
			entries = append(entries, rest)
			continue
		}
		entries[len(entries)-1] += " " + l
	}
	return entries, ordered, true
}
