package mc

import "github.com/cloudburrow/cloudburrow/third_party/go264/internal/simd"

func clip1(v int) int {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return v
}

func copyBlock(dst []byte, dstStride, dstOff int, src []byte, srcStride, srcOff, w, h int) {
	simd.CopyBlock(dst, dstStride, dstOff, src, srcStride, srcOff, w, h)
}

func averageSrcAndBlock(dst []byte, dstStride, dstOff int, src []byte, srcStride, srcOff int, b []byte, w, h int) {
	for y := 0; y < h; y++ {
		do := dstOff + y*dstStride
		so := srcOff + y*srcStride
		bo := y * w
		for x := 0; x < w; x++ {
			dst[do+x] = byte((int(src[so+x]) + int(b[bo+x]) + 1) >> 1)
		}
	}
}

func averageBlocks(dst []byte, dstStride, dstOff int, a []byte, b []byte, w, h int) {
	for y := 0; y < h; y++ {
		do := dstOff + y*dstStride
		o := y * w
		for x := 0; x < w; x++ {
			dst[do+x] = byte((int(a[o+x]) + int(b[o+x]) + 1) >> 1)
		}
	}
}

func PredictLuma(dst []byte, dstStride, dstOff int, src []byte, srcStride, srcOff int, w, h, mvx, mvy int) {
	srcOff += (mvy>>2)*srcStride + (mvx >> 2)
	xFrac := mvx & 3
	yFrac := mvy & 3

	switch {
	case xFrac == 0 && yFrac == 0:
		copyBlock(dst, dstStride, dstOff, src, srcStride, srcOff, w, h)
	case xFrac == 2 && yFrac == 0:
		simd.SixTapHoriz(dst, dstStride, dstOff, src, srcStride, srcOff, w, h)
	case xFrac == 0 && yFrac == 2:
		simd.SixTapVert(dst, dstStride, dstOff, src, srcStride, srcOff, w, h)
	case xFrac == 2 && yFrac == 2:
		simd.SixTapHV(dst, dstStride, dstOff, src, srcStride, srcOff, w, h)
	case yFrac == 0:
		var buf [256]byte
		b := buf[:w*h]
		simd.SixTapHoriz(b, w, 0, src, srcStride, srcOff, w, h)
		gOff := srcOff
		if xFrac == 3 {
			gOff++
		}
		averageSrcAndBlock(dst, dstStride, dstOff, src, srcStride, gOff, b, w, h)
	case xFrac == 0:
		var buf [256]byte
		b := buf[:w*h]
		simd.SixTapVert(b, w, 0, src, srcStride, srcOff, w, h)
		gOff := srcOff
		if yFrac == 3 {
			gOff += srcStride
		}
		averageSrcAndBlock(dst, dstStride, dstOff, src, srcStride, gOff, b, w, h)
	case xFrac == 2:
		var bBuf, jBuf [256]byte
		bArr := bBuf[:w*h]
		jArr := jBuf[:w*h]
		simd.SixTapHoriz(bArr, w, 0, src, srcStride, srcOff, w, h)
		simd.SixTapHV(jArr, w, 0, src, srcStride, srcOff, w, h)
		if yFrac == 1 {
			averageBlocks(dst, dstStride, dstOff, bArr, jArr, w, h)
		} else {
			var sBuf [256]byte
			sArr := sBuf[:w*h]
			simd.SixTapHoriz(sArr, w, 0, src, srcStride, srcOff+srcStride, w, h)
			averageBlocks(dst, dstStride, dstOff, jArr, sArr, w, h)
		}
	case yFrac == 2:
		var hBuf, jBuf [256]byte
		hArr := hBuf[:w*h]
		jArr := jBuf[:w*h]
		simd.SixTapVert(hArr, w, 0, src, srcStride, srcOff, w, h)
		simd.SixTapHV(jArr, w, 0, src, srcStride, srcOff, w, h)
		if xFrac == 1 {
			averageBlocks(dst, dstStride, dstOff, hArr, jArr, w, h)
		} else {
			var mBuf [256]byte
			mArr := mBuf[:w*h]
			simd.SixTapVert(mArr, w, 0, src, srcStride, srcOff+1, w, h)
			averageBlocks(dst, dstStride, dstOff, jArr, mArr, w, h)
		}
	default:
		var bBuf, hBuf, mBuf, sBuf [256]byte
		bArr := bBuf[:w*h]
		hArr := hBuf[:w*h]
		mArr := mBuf[:w*h]
		sArr := sBuf[:w*h]
		simd.SixTapHoriz(bArr, w, 0, src, srcStride, srcOff, w, h)
		simd.SixTapVert(hArr, w, 0, src, srcStride, srcOff, w, h)
		simd.SixTapVert(mArr, w, 0, src, srcStride, srcOff+1, w, h)
		simd.SixTapHoriz(sArr, w, 0, src, srcStride, srcOff+srcStride, w, h)
		switch {
		case xFrac == 1 && yFrac == 1:
			averageBlocks(dst, dstStride, dstOff, bArr, hArr, w, h)
		case xFrac == 3 && yFrac == 1:
			averageBlocks(dst, dstStride, dstOff, bArr, mArr, w, h)
		case xFrac == 1 && yFrac == 3:
			averageBlocks(dst, dstStride, dstOff, hArr, sArr, w, h)
		default:
			averageBlocks(dst, dstStride, dstOff, mArr, sArr, w, h)
		}
	}
}

func PredictChroma(dst []byte, dstStride, dstOff int, src []byte, srcStride, srcOff int, w, h, mvx, mvy int) {
	srcOff += (mvy>>3)*srcStride + (mvx >> 3)
	xFrac := mvx & 7
	yFrac := mvy & 7
	if xFrac == 0 && yFrac == 0 {
		copyBlock(dst, dstStride, dstOff, src, srcStride, srcOff, w, h)
		return
	}
	simd.BilinearChroma(dst, dstStride, dstOff, src, srcStride, srcOff, w, h, xFrac, yFrac)
}

func Average(dst []byte, dstStride, dstOff int, a []byte, aStride, aOff int, b []byte, bStride, bOff int, w, h int) {
	simd.AvgBytes(dst, dstStride, dstOff, a, aStride, aOff, b, bStride, bOff, w, h)
}

func WeightUni(dst []byte, stride, off, w, h, weight, offset, logWD int) {
	round := 0
	if logWD >= 1 {
		round = 1 << uint(logWD-1)
	}
	for y := 0; y < h; y++ {
		row := off + y*stride
		for x := 0; x < w; x++ {
			v := int(dst[row+x]) * weight
			if logWD >= 1 {
				v = (v + round) >> uint(logWD)
			}
			dst[row+x] = byte(clip1(v + offset))
		}
	}
}

func WeightBi(dst []byte, dstStride, dstOff int, a []byte, aStride, aOff int, b []byte, bStride, bOff int, w, h, w0, w1, o0, o1, logWD int) {
	round := 1 << uint(logWD)
	shift := uint(logWD + 1)
	offset := (o0 + o1 + 1) >> 1
	for y := 0; y < h; y++ {
		da := dstOff + y*dstStride
		aa := aOff + y*aStride
		bb := bOff + y*bStride
		for x := 0; x < w; x++ {
			v := int(a[aa+x])*w0 + int(b[bb+x])*w1 + round
			dst[da+x] = byte(clip1(v>>shift + offset))
		}
	}
}
