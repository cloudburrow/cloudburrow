package vp9hbd

import "github.com/cloudburrow/cloudburrow/third_party/govpx/internal/vp9/tables"

// reconstruct is spec 8.6.2 and the 2D inverse transform of 8.7.2, with
// libvpx's 1-D transforms (txfm1d.go).
func (d *Decoder) reconstruct(plane, x, y, txSz, txType int) {
	n := 4 << uint(txSz)
	coefs := d.coefs[:n*n]
	var tmp [1024]int32
	var in, out [32]int32
	maxV := int32(1)<<uint(d.h.bitDepth) - 1
	dst := d.cur.planes[plane]
	stride := d.cur.strides[plane]
	if d.h.lossless {
		// Walsh-Hadamard (8.7.1.10): rows shifted by 2, columns by 0.
		for i := range 4 {
			iwht(coefs[i*4:i*4+4], tmp[i*4:i*4+4], 2)
		}
		for j := range 4 {
			for i := range 4 {
				in[i] = tmp[i*4+j]
			}
			iwht(in[:4], out[:4], 0)
			for i := range 4 {
				p := (y+i)*stride + x + j
				dst[p] = uint16(min(maxV, max(0, int32(dst[p])+out[i])))
			}
		}
		return
	}
	var rowF, colF func(in, out []int32)
	rowF, colF = dct1d[txSz], dct1d[txSz]
	if txType == dctAdst || txType == adstAdst {
		rowF = adst1d[txSz]
	}
	if txType == adstDct || txType == adstAdst {
		colF = adst1d[txSz]
	}
	for i := range n {
		row := coefs[i*n : i*n+n]
		zero := true
		for _, v := range row {
			if v != 0 {
				zero = false
				break
			}
		}
		if zero {
			continue // tmp's row stays zero, as the transform of zeros is
		}
		rowF(row, tmp[i*n:i*n+n])
	}
	shift := uint(min(6, txSz+4))
	round := int32(1) << (shift - 1)
	for j := range n {
		for i := range n {
			in[i] = tmp[i*n+j]
		}
		colF(in[:n], out[:n])
		for i := range n {
			p := (y+i)*stride + x + j
			v := int32(dst[p]) + ((out[i] + round) >> shift)
			dst[p] = uint16(min(maxV, max(0, v)))
		}
	}
}

var (
	dct1d  = [4]func(in, out []int32){idct4, idct8, idct16, idct32}
	adst1d = [4]func(in, out []int32){iadst4, iadst8, iadst16, nil}
)

func iwht(in, out []int32, shift uint) {
	a := in[0] >> shift
	c := in[1] >> shift
	dd := in[2] >> shift
	b := in[3] >> shift
	a += c
	dd -= b
	e := (a - dd) >> 1
	b = e - b
	c = e - c
	a -= b
	dd += c
	out[0], out[1], out[2], out[3] = a, b, c, dd
}

// predictIntra is spec 8.5.1.
func (d *Decoder) predictIntra(plane, x, y int, haveLeft, haveAbove, notOnRight bool, txSz, blockIdx int) {
	b := &d.b
	var mode int
	switch {
	case plane > 0:
		mode = b.uvMode
	case b.miSize >= block8x8:
		mode = b.yMode
	default:
		mode = b.subModes[blockIdx]
	}
	log2Size := uint(txSz + 2)
	size := 1 << log2Size
	maxX := d.miCols*8 - 1
	maxY := d.miRows*8 - 1
	if plane > 0 {
		maxX = ((d.miCols * 8) >> uint(d.h.ssX)) - 1
		maxY = ((d.miRows * 8) >> uint(d.h.ssY)) - 1
	}
	bd := uint(d.h.bitDepth)
	base := int32(1) << (bd - 1)
	pl := d.cur.planes[plane]
	stride := d.cur.strides[plane]
	var aboveBuf [65]int32
	above := aboveBuf[1:]
	var left [32]int32
	if !haveAbove {
		for i := range 2 * size {
			above[i] = base - 1
		}
	} else {
		row := (y - 1) * stride
		for i := range size {
			above[i] = int32(pl[row+min(maxX, x+i)])
		}
		if notOnRight && txSz == tx4x4 {
			for i := size; i < 2*size; i++ {
				above[i] = int32(pl[row+min(maxX, x+i)])
			}
		} else {
			for i := size; i < 2*size; i++ {
				above[i] = above[size-1]
			}
		}
	}
	switch {
	case haveAbove && haveLeft:
		aboveBuf[0] = int32(pl[(y-1)*stride+min(maxX, x-1)])
	case haveAbove:
		aboveBuf[0] = base + 1
	default:
		aboveBuf[0] = base - 1
	}
	if haveLeft {
		for i := range size {
			left[i] = int32(pl[min(maxY, y+i)*stride+x-1])
		}
	} else {
		for i := range size {
			left[i] = base + 1
		}
	}
	aboveM1 := aboveBuf[0]
	// aboveAt reads aboveRow[i] for i >= -1.
	aboveAt := func(i int) int32 { return aboveBuf[i+1] }
	var pred [32][32]int32
	r2 := func(v int32, n uint) int32 { return (v + (1 << n >> 1)) >> n }
	switch mode {
	case vPred:
		for i := range size {
			for j := range size {
				pred[i][j] = above[j]
			}
		}
	case hPred:
		for i := range size {
			for j := range size {
				pred[i][j] = left[i]
			}
		}
	case d207Pred:
		for j := range size {
			pred[size-1][j] = left[size-1]
		}
		for i := 0; i < size-1; i++ {
			pred[i][0] = r2(left[i]+left[i+1], 1)
		}
		for i := 0; i < size-2; i++ {
			pred[i][1] = r2(left[i]+2*left[i+1]+left[i+2], 2)
		}
		pred[size-2][1] = r2(left[size-2]+3*left[size-1], 2)
		for j := 2; j < size; j++ {
			for i := size - 2; i >= 0; i-- {
				pred[i][j] = pred[i+1][j-2]
			}
		}
	case d45Pred:
		for i := range size {
			for j := range size {
				if i+j+2 < size*2 {
					pred[i][j] = r2(above[i+j]+above[i+j+1]*2+above[i+j+2], 2)
				} else {
					pred[i][j] = above[2*size-1]
				}
			}
		}
	case d63Pred:
		for i := range size {
			for j := range size {
				i2 := i / 2
				if i&1 != 0 {
					pred[i][j] = r2(above[i2+j]+above[i2+j+1]*2+above[i2+j+2], 2)
				} else {
					pred[i][j] = r2(above[i2+j]+above[i2+j+1], 1)
				}
			}
		}
	case d117Pred:
		for j := range size {
			pred[0][j] = r2(aboveAt(j-1)+above[j], 1)
		}
		pred[1][0] = r2(left[0]+2*aboveM1+above[0], 2)
		for j := 1; j < size; j++ {
			pred[1][j] = r2(aboveAt(j-2)+2*aboveAt(j-1)+above[j], 2)
		}
		pred[2][0] = r2(aboveM1+2*left[0]+left[1], 2)
		for i := 3; i < size; i++ {
			pred[i][0] = r2(left[i-3]+2*left[i-2]+left[i-1], 2)
		}
		for i := 2; i < size; i++ {
			for j := 1; j < size; j++ {
				pred[i][j] = pred[i-2][j-1]
			}
		}
	case d135Pred:
		pred[0][0] = r2(left[0]+2*aboveM1+above[0], 2)
		for j := 1; j < size; j++ {
			pred[0][j] = r2(aboveAt(j-2)+2*aboveAt(j-1)+above[j], 2)
		}
		pred[1][0] = r2(aboveM1+2*left[0]+left[1], 2)
		for i := 2; i < size; i++ {
			pred[i][0] = r2(left[i-2]+2*left[i-1]+left[i], 2)
		}
		for i := 1; i < size; i++ {
			for j := 1; j < size; j++ {
				pred[i][j] = pred[i-1][j-1]
			}
		}
	case d153Pred:
		pred[0][0] = r2(left[0]+aboveM1, 1)
		for i := 1; i < size; i++ {
			pred[i][0] = r2(left[i-1]+left[i], 1)
		}
		pred[0][1] = r2(left[0]+2*aboveM1+above[0], 2)
		pred[1][1] = r2(aboveM1+2*left[0]+left[1], 2)
		for i := 2; i < size; i++ {
			pred[i][1] = r2(left[i-2]+2*left[i-1]+left[i], 2)
		}
		for j := 2; j < size; j++ {
			pred[0][j] = r2(aboveAt(j-3)+2*aboveAt(j-2)+aboveAt(j-1), 2)
		}
		for i := 1; i < size; i++ {
			for j := 2; j < size; j++ {
				pred[i][j] = pred[i-1][j-2]
			}
		}
	case tmPred:
		maxV := int32(1)<<bd - 1
		for i := range size {
			for j := range size {
				pred[i][j] = min(maxV, max(0, above[j]+left[i]-aboveM1))
			}
		}
	default: // DC_PRED
		var v int32
		switch {
		case haveLeft && haveAbove:
			var sum int32
			for k := range size {
				sum += left[k] + above[k]
			}
			v = (sum + int32(size)) >> (log2Size + 1)
		case haveLeft:
			var sum int32
			for k := range size {
				sum += left[k]
			}
			v = (sum + (1 << (log2Size - 1))) >> log2Size
		case haveAbove:
			var sum int32
			for k := range size {
				sum += above[k]
			}
			v = (sum + (1 << (log2Size - 1))) >> log2Size
		default:
			v = base
		}
		for i := range size {
			for j := range size {
				pred[i][j] = v
			}
		}
	}
	for i := range size {
		row := (y + i) * stride
		for j := range size {
			pl[row+x+j] = uint16(pred[i][j])
		}
	}
}

// predictInter is spec 8.5.2.
func (d *Decoder) predictInter(plane, x, y, w, h, blockIdx int) {
	b := &d.b
	isCompound := b.refFrame[1] > intraFrame
	preds := &d.predBuf
	nRefs := 1
	if isCompound {
		nRefs = 2
	}
	for refList := range nRefs {
		m := d.selectMv(plane, refList, blockIdx)
		cm := d.clampMv(plane, m)
		refIdx := d.h.refFrameIdx[b.refFrame[refList]-lastFrame]
		ref := d.refs[refIdx]
		startX, startY, stepX, stepY := d.scaleMotion(plane, ref, x, y, cm)
		d.blockInterPrediction(plane, ref, startX, startY, stepX, stepY, w, h, preds[refList][:])
	}
	pl := d.cur.planes[plane]
	stride := d.cur.strides[plane]
	for i := range h {
		row := (y + i) * stride
		for j := range w {
			v := preds[0][i*w+j]
			if isCompound {
				v = (v + preds[1][i*w+j] + 1) >> 1
			}
			pl[row+x+j] = uint16(v)
		}
	}
}

// selectMv is spec 8.5.2.1.
func (d *Decoder) selectMv(plane, refList, blockIdx int) mv {
	b := &d.b
	bm := &b.blockMvs[refList]
	if plane == 0 || b.miSize >= block8x8 {
		return bm[blockIdx]
	}
	ssX, ssY := d.h.ssX, d.h.ssY
	switch {
	case ssX == 0 && ssY == 0:
		return bm[blockIdx]
	case ssX == 0 && ssY == 1:
		return mv{q2(bm[blockIdx].row + bm[blockIdx+2].row), q2(bm[blockIdx].col + bm[blockIdx+2].col)}
	case ssX == 1 && ssY == 0:
		return mv{q2(bm[blockIdx].row + bm[blockIdx+1].row), q2(bm[blockIdx].col + bm[blockIdx+1].col)}
	}
	return mv{q4(bm[0].row + bm[1].row + bm[2].row + bm[3].row), q4(bm[0].col + bm[1].col + bm[2].col + bm[3].col)}
}

func q2(v int32) int32 {
	if v < 0 {
		return (v - 1) / 2
	}
	return (v + 1) / 2
}

func q4(v int32) int32 {
	if v < 0 {
		return (v - 2) / 4
	}
	return (v + 2) / 4
}

// clampMv is spec 8.5.2.2.
func (d *Decoder) clampMv(plane int, m mv) mv {
	b := &d.b
	sx, sy := 0, 0
	if plane > 0 {
		sx, sy = d.h.ssX, d.h.ssY
	}
	bh := num8x8H[b.miSize]
	bw := num8x8W[b.miSize]
	toTop := -((b.miRow * 8) * 16) >> uint(sy)
	toBottom := (((d.miRows - bh - b.miRow) * 8) * 16) >> uint(sy)
	toLeft := -((b.miCol * 8) * 16) >> uint(sx)
	toRight := (((d.miCols - bw - b.miCol) * 8) * 16) >> uint(sx)
	spelLeft := (interpExtend + ((bw * 8) >> uint(sx))) << 4
	spelRight := spelLeft - 16
	spelTop := (interpExtend + ((bh * 8) >> uint(sy))) << 4
	spelBottom := spelTop - 16
	row := min(max((2*int(m.row))>>uint(sy), toTop-spelTop), toBottom+spelBottom)
	col := min(max((2*int(m.col))>>uint(sx), toLeft-spelLeft), toRight+spelRight)
	return mv{int32(row), int32(col)}
}

// scaleMotion is spec 8.5.2.3.
func (d *Decoder) scaleMotion(plane int, ref *frameBuf, x, y int, cm mv) (startX, startY, stepX, stepY int) {
	xScale := (ref.width << 14) / d.h.width
	yScale := (ref.height << 14) / d.h.height
	baseX := (x * xScale) >> 14
	baseY := (y * yScale) >> 14
	lumaX, lumaY := x, y
	if plane > 0 {
		lumaX <<= uint(d.h.ssX)
		lumaY <<= uint(d.h.ssY)
	}
	fracX := ((16 * lumaX * xScale) >> 14) & 15
	fracY := ((16 * lumaY * yScale) >> 14) & 15
	dX := ((int(cm.col) * xScale) >> 14) + fracX
	dY := ((int(cm.row) * yScale) >> 14) + fracY
	stepX = (16 * xScale) >> 14
	stepY = (16 * yScale) >> 14
	return (baseX << 4) + dX, (baseY << 4) + dY, stepX, stepY
}

// blockInterPrediction is spec 8.5.2.4.
func (d *Decoder) blockInterPrediction(plane int, ref *frameBuf, x, y, stepX, stepY, w, h int, pred []int32) {
	subX, subY := 0, 0
	if plane > 0 {
		subX, subY = ref.ssX, ref.ssY
	}
	lastX := ((ref.width + subX) >> uint(subX)) - 1
	lastY := ((ref.height + subY) >> uint(subY)) - 1
	refPl := ref.planes[plane]
	stride := ref.strides[plane]
	interH := (((h-1)*stepY + 15) >> 4) + 8
	filt := &tables.FilterKernels[d.b.interpFilter]
	maxV := int32(1)<<uint(d.h.bitDepth) - 1
	inter := d.interBuf[:]
	for r := range interH {
		row := min(max((y>>4)+r-3, 0), lastY) * stride
		for c := range w {
			p := x + stepX*c
			f := &(*filt)[p&15]
			var s int32
			px := (p >> 4) - 3
			for t := range 8 {
				s += int32(f[t]) * int32(refPl[row+min(max(px+t, 0), lastX)])
			}
			inter[r*w+c] = min(maxV, max(0, (s+64)>>7))
		}
	}
	for r := range h {
		for c := range w {
			p := (y & 15) + stepY*r
			f := &(*filt)[p&15]
			var s int32
			base := (p >> 4) * w
			for t := range 8 {
				s += int32(f[t]) * inter[base+t*w+c]
			}
			pred[r*w+c] = min(maxV, max(0, (s+64)>>7))
		}
	}
}
