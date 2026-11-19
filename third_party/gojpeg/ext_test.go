// Added by CloudBurrow (#1651): tests of ext.go. The fixtures in testdata/
// were written by libjpeg-turbo 3.2.1's cjpeg and jpegtran (see
// PROVENANCE.md); each arithmetic-coded one is jpegtran's lossless
// transcode of a Huffman-coded twin, so its coefficients, and so its
// pixels, must be the twin's exactly.

package jpeg

import (
	"bufio"
	"bytes"
	"fmt"
	"image"
	stdjpeg "image/jpeg"
	"os"
	"testing"
)

func decodeFile(t *testing.T, name string) image.Image {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	m, err := Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return m
}

// TestArithmeticMatchesHuffmanTwin: arithmetic-coded sequential (SOF9) and
// progressive (SOF10) JPEGs, 4:2:0, 4:2:2 and 4:4:4 and grayscale, with
// and without restart intervals, decode to the pixels Go's image/jpeg
// decodes from their Huffman-coded twins.
func TestArithmeticMatchesHuffmanTwin(t *testing.T) {
	for arith, huff := range map[string]string{
		"a420.jpg":   "h420.jpg",  // SOF9, 4:2:0
		"a422r.jpg":  "h422r.jpg", // SOF9 written by cjpeg, 4:2:2, restart every 2 MCUs
		"ap444r.jpg": "h444.jpg",  // SOF10, 4:4:4, restart every MCU
		"apgray.jpg": "hgray.jpg", // SOF10, grayscale
	} {
		got := decodeFile(t, arith)
		b, err := os.ReadFile("testdata/" + huff)
		if err != nil {
			t.Fatal(err)
		}
		want, err := stdjpeg.Decode(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		if got.Bounds() != want.Bounds() {
			t.Fatalf("%s: bounds %v, want %v", arith, got.Bounds(), want.Bounds())
		}
		r, diff := want.Bounds(), 0
		for y := r.Min.Y; y < r.Max.Y; y++ {
			for x := r.Min.X; x < r.Max.X; x++ {
				r1, g1, b1, _ := got.At(x, y).RGBA()
				r2, g2, b2, _ := want.At(x, y).RGBA()
				if r1 != r2 || g1 != g2 || b1 != b2 {
					diff++
				}
			}
		}
		if diff != 0 {
			t.Errorf("%s: %d pixels differ from %s's", arith, diff, huff)
		}
	}
}

// readPNM12 reads a binary PPM or PGM of maxval 4095, as djpeg writes a
// 12-bit JPEG.
func readPNM12(t *testing.T, name string) (w, h, ch int, pix []int) {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(bytes.NewReader(b))
	var magic string
	var maxv int
	if _, err := fmt.Fscan(r, &magic, &w, &h, &maxv); err != nil || maxv != 4095 {
		t.Fatalf("%s: %v, maxval %d", name, err, maxv)
	}
	r.ReadByte()
	ch = 3
	if magic == "P5" {
		ch = 1
	}
	pix = make([]int, w*h*ch)
	for i := range pix {
		hi, _ := r.ReadByte()
		lo, _ := r.ReadByte()
		pix[i] = int(hi)<<8 | int(lo)
	}
	return
}

// Test12Bit: 12-bit JPEGs (extended sequential SOF1, progressive SOF2, and
// arithmetic SOF9 and SOF10, all transcodes of one image's coefficients)
// decode alike, within 1 (grayscale) and 2 (colour) twelve-bit steps of
// djpeg -dct float -nosmooth: the float IDCT's rounding, and libjpeg's
// fixed-point colour conversion.
func Test12Bit(t *testing.T) {
	for _, c := range []struct {
		files []string
		ref   string
		tol   int
	}{
		{[]string{"12-sof1.jpg", "12-sof2.jpg", "12-sof9.jpg", "12-sof10.jpg"}, "12.djpeg-float.ppm", 2},
		{[]string{"12g-sof1.jpg", "12g-sof10.jpg"}, "12g.djpeg-float.pgm", 1},
	} {
		w, h, ch, ref := readPNM12(t, c.ref)
		for _, f := range c.files {
			m := decodeFile(t, f)
			if m.Bounds() != image.Rect(0, 0, w, h) {
				t.Fatalf("%s: bounds %v, want %dx%d", f, m.Bounds(), w, h)
			}
			worst := 0
			for y := range h {
				for x := range w {
					var got [3]int
					switch m := m.(type) {
					case *image.Gray16:
						got[0] = int(m.Gray16At(x, y).Y) >> 4
					case *image.RGBA64:
						p := m.RGBA64At(x, y)
						got = [3]int{int(p.R) >> 4, int(p.G) >> 4, int(p.B) >> 4}
					default:
						t.Fatalf("%s: %T", f, m)
					}
					for k := range ch {
						worst = max(worst, abs(got[k]-ref[(y*w+x)*ch+k]))
					}
				}
			}
			if worst > c.tol {
				t.Errorf("%s: %d twelve-bit steps from djpeg's decode, want at most %d", f, worst, c.tol)
			}
		}
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// TestStillUnsupported: lossless (SOF3, SOF11) and hierarchical frames,
// and 12-bit baseline frames, stay UnsupportedError.
func TestStillUnsupported(t *testing.T) {
	b, err := os.ReadFile("testdata/a420.jpg")
	if err != nil {
		t.Fatal(err)
	}
	sof := bytes.Index(b, []byte{0xff, 0xc9})
	for _, m := range []byte{0xc3, 0xcb, 0xc5} {
		c := append([]byte(nil), b...)
		c[sof+1] = m
		if _, err := Decode(bytes.NewReader(c)); err == nil {
			t.Errorf("SOF %#x: decoded", m)
		} else if _, ok := err.(UnsupportedError); !ok {
			t.Errorf("SOF %#x: %v, want an UnsupportedError", m, err)
		}
	}
	b, err = os.ReadFile("testdata/12-sof1.jpg")
	if err != nil {
		t.Fatal(err)
	}
	sof = bytes.Index(b, []byte{0xff, 0xc1})
	b[sof+1] = 0xc0
	if _, err := Decode(bytes.NewReader(b)); err == nil {
		t.Error("12-bit baseline: decoded")
	} else if _, ok := err.(UnsupportedError); !ok {
		t.Errorf("12-bit baseline: %v, want an UnsupportedError", err)
	}
}
