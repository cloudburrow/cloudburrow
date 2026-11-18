package ocr

import (
	"context"
	"image"
	"strconv"
	"strings"
)

// The layout of a recognised page (#1799): Tesseract's result iterator,
// read through the module's cb_get_layout, as blocks of paragraphs of
// words of symbols, each with its box and confidence. Cloud Vision's
// fullTextAnnotation has the same hierarchy, so its text detection can
// answer boxes.

// Quad is a box's four corners in the image as it was given to Read
// (before any turn upright), in the text's reading order: top-left,
// top-right, bottom-right, bottom-left. For a page Read turned, the corners
// are turned back with it, so the first is still the text's top-left.
type Quad [4]image.Point

// Symbol is one recognised character.
type Symbol struct {
	Text       string
	Box        Quad
	Confidence float64 // 0 to 1
	// LineEnd is true for the last symbol of a text line.
	LineEnd bool
}

// Word is a word's symbols.
type Word struct {
	Box        Quad
	Confidence float64
	Symbols    []Symbol
}

// Paragraph is a paragraph's words.
type Paragraph struct {
	Box        Quad
	Confidence float64
	Words      []Word
}

// Block is a block's paragraphs.
type Block struct {
	Box        Quad
	Confidence float64
	Paragraphs []Paragraph
}

// ReadLayout is Read, with the page's layout in Result.Blocks.
func ReadLayout(ctx context.Context, img image.Image, ppi int) (Result, error) {
	return read(ctx, img, ppi, true)
}

// layout reads engine h's last recognition; turned is the clockwise
// rotation Read undid, and w, h0 the original image's size.
func (e *engine) layout(ctx context.Context, h uint64, turned, w, h0 int) ([]Block, error) {
	p, err := e.call(ctx, "cb_get_layout", h)
	if err != nil || p == 0 {
		return nil, err
	}
	defer e.call(ctx, "cb_free", p)
	return parseLayout(e.readCString(uint32(p)), turned, w, h0), nil
}

// parseLayout reads cb_get_layout's lines.
func parseLayout(s string, turned, w, h int) []Block {
	var blocks []Block
	quad := func(f []string) (Quad, float64, bool) {
		if len(f) < 5 {
			return Quad{}, 0, false
		}
		var n [5]int
		for i := range n {
			v, err := strconv.Atoi(f[i])
			if err != nil {
				return Quad{}, 0, false
			}
			n[i] = v
		}
		l, t, r, b := n[0], n[1], n[2], n[3]
		q := Quad{{l, t}, {r, t}, {r, b}, {l, b}}
		for i := range q {
			q[i] = unturn(q[i], turned, w, h)
		}
		return q, float64(n[4]) / 10000, true
	}
	for _, line := range strings.Split(s, "\n") {
		f := strings.SplitN(line, " ", 7)
		if len(f) < 6 {
			continue
		}
		box, conf, ok := quad(f[1:6])
		if !ok {
			continue
		}
		switch f[0] {
		case "B":
			blocks = append(blocks, Block{Box: box, Confidence: conf})
		case "P":
			if len(blocks) > 0 {
				b := &blocks[len(blocks)-1]
				b.Paragraphs = append(b.Paragraphs, Paragraph{Box: box, Confidence: conf})
			}
		case "W":
			if p := lastParagraph(blocks); p != nil {
				p.Words = append(p.Words, Word{Box: box, Confidence: conf})
			}
		case "L":
		default:
			if !strings.HasPrefix(f[0], "S") || len(f) < 7 {
				continue
			}
			if p := lastParagraph(blocks); p != nil && len(p.Words) > 0 {
				wd := &p.Words[len(p.Words)-1]
				wd.Symbols = append(wd.Symbols, Symbol{Text: f[6], Box: box, Confidence: conf, LineEnd: strings.Contains(f[0], "E")})
			}
		}
	}
	return blocks
}

func lastParagraph(blocks []Block) *Paragraph {
	if len(blocks) == 0 || len(blocks[len(blocks)-1].Paragraphs) == 0 {
		return nil
	}
	ps := blocks[len(blocks)-1].Paragraphs
	return &ps[len(ps)-1]
}

// unturn maps a point of the page rotate(g, turned) made back to g, whose
// size is w×h.
func unturn(p image.Point, turned, w, h int) image.Point {
	switch turned {
	case 90:
		return image.Point{X: w - p.Y, Y: p.X}
	case 180:
		return image.Point{X: w - p.X, Y: h - p.Y}
	case 270:
		return image.Point{X: p.Y, Y: h - p.X}
	}
	return p
}
