package mc

import (
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/parallel"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/simd"
)

const planeTapBefore = 2

const planeTapAfter = 3

const planeBlocksPerTask = 128

type Planes struct {
	B      []byte
	H      []byte
	J      []byte
	Stride int
	rows   int
}

func (p *Planes) Ready() bool { return p != nil && p.B != nil }

func (p *Planes) Build(src []byte, stride, rows int) {
	if len(src) < stride*rows {
		return
	}
	n := stride * rows
	if p.Stride != stride || p.rows != rows || len(p.B) != n {
		p.B = make([]byte, n)
		p.H = make([]byte, n)
		p.J = make([]byte, n)
		p.Stride = stride
		p.rows = rows
	}
	w := stride - planeTapBefore - planeTapAfter
	h := rows - planeTapBefore - planeTapAfter
	bands := (h + 15) / 16
	chunk := max(1, planeBlocksPerTask/max(1, w/16))
	parallel.For(bands, chunk, func(lo, hi int) {
		for band := lo; band < hi; band++ {
			p.buildBand(src, stride, w, h, band*16)
		}
	})
}

func (p *Planes) buildBand(src []byte, stride, w, h, y int) {
	bh := 16
	if y+bh > h {
		bh = h - y
	}
	for x := 0; x < w; x += 16 {
		bw := 16
		if x+bw > w {
			bw = w - x
		}
		off := (planeTapBefore+y)*stride + planeTapBefore + x
		simd.SixTapHoriz(p.B, stride, off, src, stride, off, bw, bh)
		simd.SixTapVert(p.H, stride, off, src, stride, off, bw, bh)
		simd.SixTapHV(p.J, stride, off, src, stride, off, bw, bh)
	}
}

func PredictLumaPlanes(dst []byte, dstStride, dstOff int, src []byte, srcStride, srcOff int, p *Planes, w, h, mvx, mvy int) {
	srcOff += (mvy>>2)*srcStride + (mvx >> 2)
	xFrac := mvx & 3
	yFrac := mvy & 3
	stride := p.Stride

	switch {
	case xFrac == 0 && yFrac == 0:
		copyBlock(dst, dstStride, dstOff, src, srcStride, srcOff, w, h)
	case xFrac == 2 && yFrac == 0:
		copyBlock(dst, dstStride, dstOff, p.B, stride, srcOff, w, h)
	case xFrac == 0 && yFrac == 2:
		copyBlock(dst, dstStride, dstOff, p.H, stride, srcOff, w, h)
	case xFrac == 2 && yFrac == 2:
		copyBlock(dst, dstStride, dstOff, p.J, stride, srcOff, w, h)
	case yFrac == 0:
		gOff := srcOff
		if xFrac == 3 {
			gOff++
		}
		Average(dst, dstStride, dstOff, src, srcStride, gOff, p.B, stride, srcOff, w, h)
	case xFrac == 0:
		gOff := srcOff
		if yFrac == 3 {
			gOff += srcStride
		}
		Average(dst, dstStride, dstOff, src, srcStride, gOff, p.H, stride, srcOff, w, h)
	case xFrac == 2:
		if yFrac == 1 {
			Average(dst, dstStride, dstOff, p.B, stride, srcOff, p.J, stride, srcOff, w, h)
		} else {
			Average(dst, dstStride, dstOff, p.J, stride, srcOff, p.B, stride, srcOff+stride, w, h)
		}
	case yFrac == 2:
		if xFrac == 1 {
			Average(dst, dstStride, dstOff, p.H, stride, srcOff, p.J, stride, srcOff, w, h)
		} else {
			Average(dst, dstStride, dstOff, p.J, stride, srcOff, p.H, stride, srcOff+1, w, h)
		}
	default:
		switch {
		case xFrac == 1 && yFrac == 1:
			Average(dst, dstStride, dstOff, p.B, stride, srcOff, p.H, stride, srcOff, w, h)
		case xFrac == 3 && yFrac == 1:
			Average(dst, dstStride, dstOff, p.B, stride, srcOff, p.H, stride, srcOff+1, w, h)
		case xFrac == 1 && yFrac == 3:
			Average(dst, dstStride, dstOff, p.H, stride, srcOff, p.B, stride, srcOff+stride, w, h)
		default:
			Average(dst, dstStride, dstOff, p.H, stride, srcOff+1, p.B, stride, srcOff+stride, w, h)
		}
	}
}
