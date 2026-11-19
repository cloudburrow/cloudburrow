// Added by CloudBurrow (#1651): arithmetic-coded JPEG (SOF9 sequential and
// SOF10 progressive, with DAC conditioning) and 12-bit sample precision
// (SOF1, SOF2, SOF9 and SOF10), on top of Go's image/jpeg decoder. Written
// from ITU-T T.81 | ISO/IEC 10918-1: the arithmetic decoder and its
// probability estimation state machine (Annex D, Table D.3), the DC and AC
// statistical models and decoding procedures (F.1.4.4, F.2.4), and the
// progressive ones (G.1.3). See PROVENANCE.md.

package jpeg

import (
	"image"
	"image/color"
	"math"
)

// scanComp is one component of a scan: its index into d.comp and its DC
// and AC table selectors (Huffman tables, or arithmetic conditioning
// tables and statistics areas).
type scanComp struct {
	compIndex uint8
	td        uint8 // DC table selector.
	ta        uint8 // AC table selector.
}

// qe is one row of T.81 Table D.3: the LPS probability estimate and the
// next state after an LPS (with the MPS switch in bit 7) and an MPS.
type qe struct {
	qe         uint16
	nlps, nmps uint8
}

// qeTable is T.81 Table D.3, plus state 113: Qe X'5A1D' that never moves,
// for the fixed one-half probability of the AC sign (F.1.4.4.2) and of
// progressive refinement bits (G.1.3).
var qeTable = [114]qe{
	{0x5A1D, 0x80 | 1, 1}, {0x2586, 14, 2}, {0x1114, 16, 3}, {0x080B, 18, 4},
	{0x03D8, 20, 5}, {0x01DA, 23, 6}, {0x00E5, 25, 7}, {0x006F, 28, 8},
	{0x0036, 30, 9}, {0x001A, 33, 10}, {0x000D, 35, 11}, {0x0006, 9, 12},
	{0x0003, 10, 13}, {0x0001, 12, 13}, {0x5A7F, 0x80 | 15, 15}, {0x3F25, 36, 16},
	{0x2CF2, 38, 17}, {0x207C, 39, 18}, {0x17B9, 40, 19}, {0x1182, 42, 20},
	{0x0CEF, 43, 21}, {0x09A1, 45, 22}, {0x072F, 46, 23}, {0x055C, 48, 24},
	{0x0406, 49, 25}, {0x0303, 51, 26}, {0x0240, 52, 27}, {0x01B1, 54, 28},
	{0x0144, 56, 29}, {0x00F5, 57, 30}, {0x00B7, 59, 31}, {0x008A, 60, 32},
	{0x0068, 62, 33}, {0x004E, 63, 34}, {0x003B, 32, 35}, {0x002C, 33, 9},
	{0x5AE1, 0x80 | 37, 37}, {0x484C, 64, 38}, {0x3A0D, 65, 39}, {0x2EF1, 67, 40},
	{0x261F, 68, 41}, {0x1F33, 69, 42}, {0x19A8, 70, 43}, {0x1518, 72, 44},
	{0x1177, 73, 45}, {0x0E74, 74, 46}, {0x0BFB, 75, 47}, {0x09F8, 77, 48},
	{0x0861, 78, 49}, {0x0706, 79, 50}, {0x05CD, 48, 51}, {0x04DE, 50, 52},
	{0x040F, 50, 53}, {0x0363, 51, 54}, {0x02D4, 52, 55}, {0x025C, 53, 56},
	{0x01F8, 54, 57}, {0x01A4, 55, 58}, {0x0160, 56, 59}, {0x0125, 57, 60},
	{0x00F6, 58, 61}, {0x00CB, 59, 62}, {0x00AB, 61, 63}, {0x008F, 61, 32},
	{0x5B12, 0x80 | 65, 65}, {0x4D04, 80, 66}, {0x412C, 81, 67}, {0x37D8, 82, 68},
	{0x2FE8, 83, 69}, {0x293C, 84, 70}, {0x2379, 86, 71}, {0x1EDF, 87, 72},
	{0x1AA9, 87, 73}, {0x174E, 72, 74}, {0x1424, 72, 75}, {0x119C, 74, 76},
	{0x0F6B, 74, 77}, {0x0D51, 75, 78}, {0x0BB6, 77, 79}, {0x0A40, 77, 48},
	{0x5832, 0x80 | 80, 81}, {0x4D1C, 88, 82}, {0x438E, 89, 83}, {0x3BDD, 90, 84},
	{0x34EE, 91, 85}, {0x2EAE, 92, 86}, {0x299A, 93, 87}, {0x2516, 86, 71},
	{0x5570, 0x80 | 88, 89}, {0x4CA9, 95, 90}, {0x44D9, 96, 91}, {0x3E22, 97, 92},
	{0x3824, 99, 93}, {0x32B4, 99, 94}, {0x2E17, 93, 86}, {0x56A8, 0x80 | 95, 96},
	{0x4F46, 101, 97}, {0x47E5, 102, 98}, {0x41CF, 103, 99}, {0x3C3D, 104, 100},
	{0x375E, 99, 93}, {0x5231, 105, 102}, {0x4C0F, 106, 103}, {0x4639, 107, 104},
	{0x415E, 103, 99}, {0x5627, 0x80 | 105, 106}, {0x50E7, 108, 107}, {0x4B85, 109, 103},
	{0x5597, 110, 109}, {0x504F, 111, 107}, {0x5A10, 0x80 | 110, 111}, {0x5522, 112, 109},
	{0x59EB, 0x80 | 112, 111},
	{0x5A1D, 113, 113},
}

// arithState is the arithmetic decoder (T.81 D.2, in the software
// convention: C holds the code bytes read so far, compared against A
// shifted up by the count of bits not yet consumed) and the statistics
// areas of the scan in progress.
type arithState struct {
	c, a   int64
	ct     int
	marker bool // a marker (or the end of the data) was reached: feed zeros

	dcStats [maxTh + 1][64]uint8
	acStats [maxTh + 1][256]uint8
	dcCtx   [maxComponents]int
	fixed   uint8

	// DAC conditioning: each DC table's L and U, each AC table's Kx.
	dcL, dcU, acK [maxTh + 1]uint8
	condSet       [2][maxTh + 1]bool
}

// processDAC reads a DAC segment (T.81 B.2.4.3).
func (d *decoder) processDAC(n int) error {
	for n > 0 {
		if n < 2 {
			return FormatError("DAC has wrong length")
		}
		if err := d.readFull(d.tmp[:2]); err != nil {
			return err
		}
		n -= 2
		tc, tb, cs := d.tmp[0]>>4, d.tmp[0]&0x0f, d.tmp[1]
		if tc > 1 || tb > maxTh {
			return FormatError("bad DAC table")
		}
		if tc == 0 {
			l, u := cs&0x0f, cs>>4
			if l > u {
				return FormatError("bad DAC DC conditioning")
			}
			d.ar.dcL[tb], d.ar.dcU[tb] = l, u
		} else {
			if cs < 1 || cs > 63 {
				return FormatError("bad DAC AC conditioning")
			}
			d.ar.acK[tb] = cs
		}
		d.ar.condSet[tc][tb] = true
	}
	return nil
}

// arithStart resets the decoder and the statistics areas of the scan's
// tables, at the start of a scan and at each restart marker.
func (d *decoder) arithStart(nComp int, scan []scanComp, zigStart int32, ah uint32) {
	d.ar.c, d.ar.a, d.ar.ct, d.ar.marker = 0, 0, -16, false
	d.ar.fixed = 113
	for _, s := range scan[:nComp] {
		if !d.progressive || zigStart == 0 && ah == 0 {
			d.ar.dcStats[s.td] = [64]uint8{}
			d.ar.dcCtx[s.compIndex] = 0
		}
		if !d.progressive || zigStart != 0 {
			d.ar.acStats[s.ta] = [256]uint8{}
		}
	}
}

// arithByte is the next byte of entropy-coded data, unstuffed. At a
// marker it leaves the marker unread and returns zeros from then on, as
// T.81 D.2.6 asks.
func (d *decoder) arithByte() int64 {
	if d.ar.marker {
		return 0
	}
	x, err := d.readByte()
	if err != nil {
		d.ar.marker = true
		return 0
	}
	if x != 0xff {
		return int64(x)
	}
	for {
		for d.bytes.i == d.bytes.j {
			if err := d.fill(); err != nil {
				d.ar.marker = true
				return 0
			}
		}
		switch d.bytes.buf[d.bytes.i] {
		case 0x00: // a stuffed zero
			d.bytes.i++
			return 0xff
		case 0xff: // fill bytes before a marker
			d.bytes.i++
		default:
			d.bytes.i-- // leave 0xff and the marker for the marker reader
			d.ar.marker = true
			return 0
		}
	}
}

// arithDecode decodes one binary decision with statistics bin st (T.81
// D.2.4 and D.2.5: renormalisation, then the conditional exchanges and
// probability estimation).
func (d *decoder) arithDecode(st *uint8) int {
	e := &d.ar
	for e.a < 0x8000 {
		if e.ct--; e.ct < 0 {
			e.c = e.c<<8 | d.arithByte()
			if e.ct += 8; e.ct < 0 {
				// Still reading the first two bytes (D.2.7).
				if e.ct++; e.ct == 0 {
					e.a = 0x8000
				}
			}
		}
		e.a <<= 1
	}
	sv := *st
	q := &qeTable[sv&0x7f]
	qv := int64(q.qe)
	e.a -= qv
	if t := e.a << uint(e.ct); e.c >= t {
		e.c -= t
		if e.a < qv {
			*st = sv&0x80 ^ q.nmps
		} else {
			*st = sv&0x80 ^ q.nlps
			sv ^= 0x80
		}
		e.a = qv
	} else if e.a < 0x8000 {
		if e.a < qv {
			*st = sv&0x80 ^ q.nlps
			sv ^= 0x80
		} else {
			*st = sv&0x80 ^ q.nmps
		}
	}
	return int(sv >> 7)
}

var errArithCode = FormatError("bad arithmetic-coded data")

// arithBlock decodes one block's share of a scan into b: the DC difference
// and AC coefficients of a sequential scan (F.2.4), or a progressive
// scan's first pass or refinement (G.1.3).
func (d *decoder) arithBlock(b *block, comp int, td, ta uint8, zigStart, zigEnd int32, ah, al uint32, dc *[maxComponents]int32) error {
	if ah != 0 {
		if zigStart == 0 {
			if d.arithDecode(&d.ar.fixed) != 0 {
				b[0] |= 1 << al
			}
			return nil
		}
		return d.arithRefineAC(b, ta, zigStart, zigEnd, al)
	}
	if zigStart == 0 {
		v, err := d.arithDC(comp, td)
		if err != nil {
			return err
		}
		dc[comp] += v
		b[0] = dc[comp] << al
	}
	k := max(zigStart, 1)
	if k > zigEnd {
		return nil
	}
	kx := int32(5)
	if d.ar.condSet[1][ta] {
		kx = int32(d.ar.acK[ta])
	}
	stats := &d.ar.acStats[ta]
	for ; k <= zigEnd; k++ {
		st := 3 * (k - 1)
		if d.arithDecode(&stats[st]) != 0 {
			break // EOB
		}
		for d.arithDecode(&stats[st+1]) == 0 {
			st += 3
			if k++; k > zigEnd {
				return errArithCode
			}
		}
		sign := d.arithDecode(&d.ar.fixed)
		st += 2
		m := int32(d.arithDecode(&stats[st]))
		if m != 0 && d.arithDecode(&stats[st]) != 0 {
			m <<= 1
			st = 189
			if k > kx {
				st = 217
			}
			for d.arithDecode(&stats[st]) != 0 {
				if m <<= 1; m == 0x8000 {
					return errArithCode
				}
				st++
			}
		}
		v := m
		st += 14
		for m >>= 1; m != 0; m >>= 1 {
			if d.arithDecode(&stats[st]) != 0 {
				v |= m
			}
		}
		v++
		if sign != 0 {
			v = -v
		}
		b[unzig[k]] = v << al
	}
	return nil
}

// arithDC decodes a DC difference (F.1.4.4.1, F.2.4.1), updating the
// component's conditioning context.
func (d *decoder) arithDC(comp int, td uint8) (int32, error) {
	stats := &d.ar.dcStats[td]
	ctx := d.ar.dcCtx[comp]
	if d.arithDecode(&stats[ctx]) == 0 {
		d.ar.dcCtx[comp] = 0
		return 0, nil
	}
	sign := d.arithDecode(&stats[ctx+1])
	st := ctx + 2 + sign
	m := int32(d.arithDecode(&stats[st]))
	if m != 0 {
		st = 20
		for d.arithDecode(&stats[st]) != 0 {
			if m <<= 1; m == 0x8000 {
				return 0, errArithCode
			}
			st++
		}
	}
	l, u := uint8(0), uint8(1)
	if d.ar.condSet[0][td] {
		l, u = d.ar.dcL[td], d.ar.dcU[td]
	}
	switch {
	case m < int32(1<<l>>1):
		d.ar.dcCtx[comp] = 0
	case m > int32(1<<u>>1):
		d.ar.dcCtx[comp] = 12 + 4*sign
	default:
		d.ar.dcCtx[comp] = 4 + 4*sign
	}
	v := m
	st += 14
	for m >>= 1; m != 0; m >>= 1 {
		if d.arithDecode(&stats[st]) != 0 {
			v |= m
		}
	}
	v++
	if sign != 0 {
		v = -v
	}
	return v, nil
}

// arithRefineAC decodes an AC successive approximation refinement (G.1.3.3).
func (d *decoder) arithRefineAC(b *block, ta uint8, zigStart, zigEnd int32, al uint32) error {
	p1, m1 := int32(1)<<al, int32(-1)<<al
	kex := zigEnd
	for ; kex > 0; kex-- {
		if b[unzig[kex]] != 0 {
			break
		}
	}
	stats := &d.ar.acStats[ta]
	for k := zigStart; k <= zigEnd; k++ {
		st := 3 * (k - 1)
		if k > kex && d.arithDecode(&stats[st]) != 0 {
			break // EOB
		}
		for {
			c := &b[unzig[k]]
			if *c != 0 {
				if d.arithDecode(&stats[st+2]) != 0 {
					if *c < 0 {
						*c += m1
					} else {
						*c += p1
					}
				}
				break
			}
			if d.arithDecode(&stats[st+1]) != 0 {
				if d.arithDecode(&d.ar.fixed) != 0 {
					*c = m1
				} else {
					*c = p1
				}
				break
			}
			st += 3
			if k++; k > zigEnd {
				return errArithCode
			}
		}
	}
	return nil
}

// idctCos[x][u] is C(u)/2·cos((2x+1)uπ/16), for the 12-bit path's
// floating-point inverse DCT (A.3.3), whose result is the true IDCT's
// rounded: Go's 8-bit integer IDCT is not specified for 12-bit ranges.
var idctCos = func() (t [8][8]float64) {
	for x := range 8 {
		for u := range 8 {
			c := 1.0
			if u == 0 {
				c = 1 / math.Sqrt2
			}
			t[x][u] = c / 2 * math.Cos(float64(2*x+1)*float64(u)*math.Pi/16)
		}
	}
	return t
}()

// store12 inverse-transforms a dequantized block of 12-bit samples, level
// shifts it by 2048 and stores it, clamped to [0, 4095], in its 16-bit
// plane.
func (d *decoder) store12(b *block, bx, by, compIndex int) error {
	var tmp, out [64]float64
	for y := range 8 {
		for x := range 8 {
			s := 0.0
			for u := range 8 {
				s += idctCos[x][u] * float64(b[8*y+u])
			}
			tmp[8*y+x] = s
		}
	}
	for x := range 8 {
		for y := range 8 {
			s := 0.0
			for v := range 8 {
				s += idctCos[y][v] * tmp[8*v+x]
			}
			out[8*y+x] = s
		}
	}
	h, v := 1, 1
	if d.flex {
		h, v = d.comp[compIndex].expandH, d.comp[compIndex].expandV
		bx, by = bx*h, by*v
	}
	var stride int
	switch {
	case d.nComp == 1:
		stride = d.img1.Stride
	case compIndex == 0:
		stride = d.img3.YStride
	case compIndex <= 2:
		stride = d.img3.CStride
	default:
		return UnsupportedError("12-bit CMYK or YCbCrK")
	}
	dst := d.pix16[compIndex][8*(by*stride+bx):]
	for y := range 8 {
		for x := range 8 {
			val := uint16(min(4095, max(0, math.Round(out[8*y+x])+2048)))
			for yy := range v {
				for xx := range h {
					dst[(y*v+yy)*stride+x*h+xx] = val
				}
			}
		}
	}
	return nil
}

// widen12 scales a 12-bit sample to 16 bits.
func widen12(v int) uint16 { return uint16(v<<4 | v>>8) }

// output12 is a 12-bit image: Gray16, or RGBA64 from YCbCr (JFIF's
// conversion, A.3 of T.871, at 12 bits) or RGB, chroma upsampled by
// replication as Go's YCbCr image is.
func (d *decoder) output12() (image.Image, error) {
	if d.img1 != nil {
		r := d.img1.Rect
		img := image.NewGray16(r)
		for y := r.Min.Y; y < r.Max.Y; y++ {
			for x := r.Min.X; x < r.Max.X; x++ {
				img.SetGray16(x, y, color.Gray16{Y: widen12(int(d.pix16[0][y*d.img1.Stride+x]))})
			}
		}
		return img, nil
	}
	if d.nComp != 3 {
		return nil, UnsupportedError("12-bit CMYK or YCbCrK")
	}
	m := d.img3
	r := m.Rect
	img := image.NewRGBA64(r)
	rgb := d.isRGB()
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			yi, ci := m.YOffset(x, y), m.COffset(x, y)
			yy, cb, cr := float64(d.pix16[0][yi]), float64(d.pix16[1][ci]), float64(d.pix16[2][ci])
			var rr, gg, bb float64
			if rgb {
				rr, gg, bb = yy, cb, cr
			} else {
				rr = yy + 1.402*(cr-2048)
				gg = yy - 0.344136286*(cb-2048) - 0.714136286*(cr-2048)
				bb = yy + 1.772*(cb-2048)
			}
			c := func(v float64) uint16 { return widen12(int(min(4095, max(0, math.Round(v))))) }
			img.SetRGBA64(x, y, color.RGBA64{R: c(rr), G: c(gg), B: c(bb), A: 0xffff})
		}
	}
	return img, nil
}

// drawYCbCr converts src into dst (image/internal/imageutil.DrawYCbCr's
// job, which this copy cannot import).
func drawYCbCr(dst *image.RGBA, src *image.YCbCr) {
	r := src.Rect
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			dst.Set(x, y, src.YCbCrAt(x, y))
		}
	}
}
