//go:build amd64 && !purego

package simd

func sadDispatch(src []byte, srcStride int, ref []byte, refStride, w, h int) int {
	if !spanFits(src, srcStride, w, h) || !spanFits(ref, refStride, w, h) {
		return sadGeneric(src, srcStride, ref, refStride, w, h)
	}
	switch {
	case w == 16 && h == 16:
		return sad16x16(src, srcStride, ref, refStride)
	case w == 16 && h == 8:
		return sad16x8(src, srcStride, ref, refStride)
	case w == 8 && h == 16:
		return sad8x16(src, srcStride, ref, refStride)
	case w == 8 && h == 8:
		return sad8x8(src, srcStride, ref, refStride)
	case w == 8 && h == 4:
		return sad8x4(src, srcStride, ref, refStride)
	case w == 4 && h == 8:
		return sad4x8(src, srcStride, ref, refStride)
	case w == 4 && h == 4:
		return sad4x4(src, srcStride, ref, refStride)
	}
	return sadGeneric(src, srcStride, ref, refStride, w, h)
}

func Accelerated() bool { return hasSSE41 }

func Forward4x4(b *[16]int32) {
	forward4x4(b)
}

func Inverse4x4(b *[16]int32) {
	inverse4x4(b)
}

func Quant4x4(b *[16]int32, mf *[16]int32, f int32, qbits uint32) {
	if !hasSSE41 {
		quant4x4Generic(b, mf, f, qbits)
		return
	}
	quant4x4(b, mf, f, uint64(qbits))
}

func Dequant4x4(b *[16]int32, scale *[16]int32, shift uint32) {
	if !hasSSE41 {
		dequant4x4Generic(b, scale, shift)
		return
	}
	if shift >= 4 {
		dequantLeft4x4(b, scale, uint64(shift-4))
		return
	}
	s := uint64(4 - shift)
	round := int32(1) << (s - 1)
	dequantRight4x4(b, scale, s, round)
}

func AddResidual4x4(plane []byte, stride, offset int, b *[16]int32) {
	if !hasSSE41 || !addResidualFits(plane, stride, offset) {
		addResidual4x4Generic(plane, stride, offset, b)
		return
	}
	addResidual4x4(plane[offset:], stride, b)
}

func satd4x4Dispatch(src []byte, srcStride int, ref []byte, refStride int) int {
	if !hasSSE41 || !spanFits(src, srcStride, 4, 4) || !spanFits(ref, refStride, 4, 4) {
		return satd4x4Generic(src, srcStride, ref, refStride)
	}
	return satd4x4(src, srcStride, ref, refStride)
}

func satd8x8Dispatch(src []byte, srcStride int, ref []byte, refStride int) int {
	if !hasSSE41 || !spanFits(src, srcStride, 8, 8) || !spanFits(ref, refStride, 8, 8) {
		return satd8x8Generic(src, srcStride, ref, refStride)
	}
	if hasAVX2 {
		return satd8x8AVX2(src, srcStride, ref, refStride)
	}
	return satd8x8(src, srcStride, ref, refStride)
}

type sixTapFn func(dst []byte, dstStride int, src []byte, srcStride int)

func sixTapHorizDispatch(dst []byte, dstStride, dstOff int, src []byte, srcStride, srcOff, w, h int) {
	if hasSSE41 && spanFitsMargin(src, srcStride, srcOff, w, h, 2, 3, 0, 0) && spanFits(dst[dstOff:], dstStride, w, h) {
		d, s := dst[dstOff:], src[srcOff:]
		if hasAVX2 {
			switch {
			case w == 16 && h == 16:
				sixTapHoriz16x16AVX2(d, dstStride, s, srcStride)
				return
			case w == 16 && h == 8:
				sixTapHoriz16x8AVX2(d, dstStride, s, srcStride)
				return
			}
		}
		switch {
		case w == 16 && h == 16:
			sixTapHoriz16x16(d, dstStride, s, srcStride)
			return
		case w == 16 && h == 8:
			sixTapHoriz16x8(d, dstStride, s, srcStride)
			return
		case w == 8 && h == 16:
			sixTapHoriz8x16(d, dstStride, s, srcStride)
			return
		case w == 8 && h == 8:
			sixTapHoriz8x8(d, dstStride, s, srcStride)
			return
		case w == 8 && h == 4:
			sixTapHoriz8x4(d, dstStride, s, srcStride)
			return
		case w == 4 && h == 8:
			sixTapHoriz4x8(d, dstStride, s, srcStride)
			return
		case w == 4 && h == 4:
			sixTapHoriz4x4(d, dstStride, s, srcStride)
			return
		}
	}
	sixTapHorizGeneric(dst, dstStride, dstOff, src, srcStride, srcOff, w, h)
}

func sixTapVertDispatch(dst []byte, dstStride, dstOff int, src []byte, srcStride, srcOff, w, h int) {
	if hasSSE41 && spanFitsMargin(src, srcStride, srcOff, w, h, 0, 0, 2, 3) && spanFits(dst[dstOff:], dstStride, w, h) {
		d, s := dst[dstOff:], src[srcOff:]
		if hasAVX2 {
			switch {
			case w == 16 && h == 16:
				sixTapVert16x16AVX2(d, dstStride, s, srcStride)
				return
			case w == 16 && h == 8:
				sixTapVert16x8AVX2(d, dstStride, s, srcStride)
				return
			}
		}
		switch {
		case w == 16 && h == 16:
			sixTapVert16x16(d, dstStride, s, srcStride)
			return
		case w == 16 && h == 8:
			sixTapVert16x8(d, dstStride, s, srcStride)
			return
		case w == 8 && h == 16:
			sixTapVert8x16(d, dstStride, s, srcStride)
			return
		case w == 8 && h == 8:
			sixTapVert8x8(d, dstStride, s, srcStride)
			return
		case w == 8 && h == 4:
			sixTapVert8x4(d, dstStride, s, srcStride)
			return
		case w == 4 && h == 8:
			sixTapVert4x8(d, dstStride, s, srcStride)
			return
		case w == 4 && h == 4:
			sixTapVert4x4(d, dstStride, s, srcStride)
			return
		}
	}
	sixTapVertGeneric(dst, dstStride, dstOff, src, srcStride, srcOff, w, h)
}

func sixTapHVDispatch(dst []byte, dstStride, dstOff int, src []byte, srcStride, srcOff, w, h int) {
	if hasSSE41 && spanFitsMargin(src, srcStride, srcOff, w, h, 2, 3, 2, 3) && spanFits(dst[dstOff:], dstStride, w, h) {
		d, s := dst[dstOff:], src[srcOff:]
		if hasAVX2 {
			switch {
			case w == 16 && h == 16:
				sixTapHV16x16AVX2(d, dstStride, s, srcStride)
				return
			case w == 16 && h == 8:
				sixTapHV16x8AVX2(d, dstStride, s, srcStride)
				return
			}
		}
		switch {
		case w == 16 && h == 16:
			sixTapHV16x16(d, dstStride, s, srcStride)
			return
		case w == 16 && h == 8:
			sixTapHV16x8(d, dstStride, s, srcStride)
			return
		case w == 8 && h == 16:
			sixTapHV8x16(d, dstStride, s, srcStride)
			return
		case w == 8 && h == 8:
			sixTapHV8x8(d, dstStride, s, srcStride)
			return
		case w == 8 && h == 4:
			sixTapHV8x4(d, dstStride, s, srcStride)
			return
		case w == 4 && h == 8:
			sixTapHV4x8(d, dstStride, s, srcStride)
			return
		case w == 4 && h == 4:
			sixTapHV4x4(d, dstStride, s, srcStride)
			return
		}
	}
	sixTapHVGeneric(dst, dstStride, dstOff, src, srcStride, srcOff, w, h)
}

func bilinearChromaDispatch(dst []byte, dstStride, dstOff int, src []byte, srcStride, srcOff, w, h, xFrac, yFrac int) {
	if hasSSE41 && spanFitsMargin(src, srcStride, srcOff, w, h, 0, 1, 0, 1) && spanFits(dst[dstOff:], dstStride, w, h) {
		d, s := dst[dstOff:], src[srcOff:]
		xf, yf := int32(xFrac), int32(yFrac)
		switch {
		case w == 8 && h == 8:
			bilinearChroma8x8(d, dstStride, s, srcStride, xf, yf)
			return
		case w == 8 && h == 4:
			bilinearChroma8x4(d, dstStride, s, srcStride, xf, yf)
			return
		case w == 8 && h == 2:
			bilinearChroma8x2(d, dstStride, s, srcStride, xf, yf)
			return
		case w == 4 && h == 8:
			bilinearChroma4x8(d, dstStride, s, srcStride, xf, yf)
			return
		case w == 4 && h == 4:
			bilinearChroma4x4(d, dstStride, s, srcStride, xf, yf)
			return
		case w == 4 && h == 2:
			bilinearChroma4x2(d, dstStride, s, srcStride, xf, yf)
			return
		}
	}
	bilinearChromaGeneric(dst, dstStride, dstOff, src, srcStride, srcOff, w, h, xFrac, yFrac)
}

func DeblockLumaNormal(plane []byte, offset, stride int, tc0, bs *[16]uint8, alpha, beta int) bool {
	if !hasAVX2 || !deblockFits(plane, offset, stride, 3, 2) {
		return false
	}
	deblockLumaNormalAVX2(plane, offset, stride, tc0, bs, int32(alpha), int32(beta))
	return true
}

func DeblockLumaStrong(plane []byte, offset, stride, alpha, beta int) bool {
	if !hasAVX2 || !deblockFits(plane, offset, stride, 4, 3) {
		return false
	}
	deblockLumaStrongAVX2(plane, offset, stride, int32(alpha), int32(beta))
	return true
}

func DeblockLumaVerticalNormal(plane []byte, offset, stride int, tc0, bs *[16]uint8, alpha, beta int) bool {
	if !hasAVX2 || offset < 4 || stride <= 0 || len(plane)-offset < 15*stride+4 {
		return false
	}
	deblockLumaVerticalNormalAVX2(plane, offset, stride, tc0, bs, int32(alpha), int32(beta))
	return true
}

func avgBytesDispatch(dst []byte, dstStride, dstOff int, a []byte, aStride, aOff int, b []byte, bStride, bOff, w, h int) {
	if hasSSE41 && spanFits(dst[dstOff:], dstStride, w, h) &&
		spanFits(a[aOff:], aStride, w, h) && spanFits(b[bOff:], bStride, w, h) {
		d, x, y := dst[dstOff:], a[aOff:], b[bOff:]
		switch {
		case w == 16 && h == 16:
			avgBytes16x16(d, dstStride, x, aStride, y, bStride)
			return
		case w == 16 && h == 8:
			avgBytes16x8(d, dstStride, x, aStride, y, bStride)
			return
		case w == 8 && h == 16:
			avgBytes8x16(d, dstStride, x, aStride, y, bStride)
			return
		case w == 8 && h == 8:
			avgBytes8x8(d, dstStride, x, aStride, y, bStride)
			return
		case w == 8 && h == 4:
			avgBytes8x4(d, dstStride, x, aStride, y, bStride)
			return
		case w == 4 && h == 8:
			avgBytes4x8(d, dstStride, x, aStride, y, bStride)
			return
		case w == 4 && h == 4:
			avgBytes4x4(d, dstStride, x, aStride, y, bStride)
			return
		}
	}
	avgBytesGeneric(dst, dstStride, dstOff, a, aStride, aOff, b, bStride, bOff, w, h)
}

func copyBlockDispatch(dst []byte, dstStride, dstOff int, src []byte, srcStride, srcOff, w, h int) {
	if hasSSE41 && spanFits(dst[dstOff:], dstStride, w, h) && spanFits(src[srcOff:], srcStride, w, h) {
		d, s := dst[dstOff:], src[srcOff:]
		switch {
		case w == 16 && h == 16:
			copyBlock16x16(d, dstStride, s, srcStride)
			return
		case w == 16 && h == 8:
			copyBlock16x8(d, dstStride, s, srcStride)
			return
		case w == 8 && h == 16:
			copyBlock8x16(d, dstStride, s, srcStride)
			return
		case w == 8 && h == 8:
			copyBlock8x8(d, dstStride, s, srcStride)
			return
		case w == 8 && h == 4:
			copyBlock8x4(d, dstStride, s, srcStride)
			return
		case w == 4 && h == 8:
			copyBlock4x8(d, dstStride, s, srcStride)
			return
		case w == 4 && h == 4:
			copyBlock4x4(d, dstStride, s, srcStride)
			return
		}
	}
	copyBlockGeneric(dst, dstStride, dstOff, src, srcStride, srcOff, w, h)
}
