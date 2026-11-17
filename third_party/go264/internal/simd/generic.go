package simd

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func sadGeneric(src []byte, srcStride int, ref []byte, refStride, w, h int) int {
	total := 0
	for y := 0; y < h; y++ {
		a := src[y*srcStride:]
		b := ref[y*refStride:]
		for x := 0; x < w; x++ {
			total += absInt(int(a[x]) - int(b[x]))
		}
	}
	return total
}

func SAD(src []byte, srcStride, srcOff int, ref []byte, refStride, refOff, w, h int) int {
	return sadDispatch(src[srcOff:], srcStride, ref[refOff:], refStride, w, h)
}

func SADGeneric(src []byte, srcStride, srcOff int, ref []byte, refStride, refOff, w, h int) int {
	return sadGeneric(src[srcOff:], srcStride, ref[refOff:], refStride, w, h)
}

func spanFits(b []byte, stride, w, h int) bool {
	if h == 0 {
		return true
	}
	return len(b) >= (h-1)*stride+w
}

func addResidualFits(plane []byte, stride, offset int) bool {
	if offset < 0 {
		return false
	}
	return len(plane)-offset >= 3*stride+4
}

func forward4x4Generic(b *[16]int32) {
	for i := 0; i < 16; i += 4 {
		s0, s1, s2, s3 := b[i], b[i+1], b[i+2], b[i+3]
		t0 := s0 + s3
		t1 := s1 + s2
		t2 := s1 - s2
		t3 := s0 - s3
		b[i] = t0 + t1
		b[i+1] = t3*2 + t2
		b[i+2] = t0 - t1
		b[i+3] = t3 - t2*2
	}
	for i := 0; i < 4; i++ {
		s0, s1, s2, s3 := b[i], b[i+4], b[i+8], b[i+12]
		t0 := s0 + s3
		t1 := s1 + s2
		t2 := s1 - s2
		t3 := s0 - s3
		b[i] = t0 + t1
		b[i+4] = t3*2 + t2
		b[i+8] = t0 - t1
		b[i+12] = t3 - t2*2
	}
}

func inverse4x4Generic(b *[16]int32) {
	for i := 0; i < 16; i += 4 {
		s0, s1, s2, s3 := b[i], b[i+1], b[i+2], b[i+3]
		t0 := s0 + s2
		t1 := s0 - s2
		t2 := s1>>1 - s3
		t3 := s1 + s3>>1
		b[i] = t0 + t3
		b[i+1] = t1 + t2
		b[i+2] = t1 - t2
		b[i+3] = t0 - t3
	}
	for i := 0; i < 4; i++ {
		s0, s1, s2, s3 := b[i], b[i+4], b[i+8], b[i+12]
		t0 := s0 + s2
		t1 := s0 - s2
		t2 := s1>>1 - s3
		t3 := s1 + s3>>1
		b[i] = (t0 + t3 + 32) >> 6
		b[i+4] = (t1 + t2 + 32) >> 6
		b[i+8] = (t1 - t2 + 32) >> 6
		b[i+12] = (t0 - t3 + 32) >> 6
	}
}

func quant4x4Generic(b *[16]int32, mf *[16]int32, f int32, qbits uint32) {
	qb := uint(qbits)
	for i := range b {
		v := b[i]
		if v >= 0 {
			b[i] = (v*mf[i] + f) >> qb
		} else {
			b[i] = -((-v*mf[i] + f) >> qb)
		}
	}
}

func dequant4x4Generic(b *[16]int32, scale *[16]int32, shift uint32) {
	if shift >= 4 {
		s := uint(shift - 4)
		for i := range b {
			b[i] = b[i] * scale[i] << s
		}
		return
	}
	s := uint(4 - shift)
	round := int32(1) << (s - 1)
	for i := range b {
		b[i] = (b[i]*scale[i] + round) >> s
	}
}

func clip1(v int32) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}

func addResidual4x4Generic(plane []byte, stride, offset int, b *[16]int32) {
	for y := 0; y < 4; y++ {
		row := plane[offset+y*stride:]
		row[0] = clip1(int32(row[0]) + b[y*4])
		row[1] = clip1(int32(row[1]) + b[y*4+1])
		row[2] = clip1(int32(row[2]) + b[y*4+2])
		row[3] = clip1(int32(row[3]) + b[y*4+3])
	}
}

func spanFitsMargin(b []byte, stride, off, w, h, left, right, top, bottom int) bool {
	minOff := off - left - top*stride
	maxOff := off + w - 1 + right + (h-1+bottom)*stride
	return minOff >= 0 && maxOff < len(b)
}

func satd4x4Generic(src []byte, srcStride int, ref []byte, refStride int) int {
	var d [16]int
	for y := 0; y < 4; y++ {
		a := src[y*srcStride:]
		b := ref[y*refStride:]
		for x := 0; x < 4; x++ {
			d[y*4+x] = int(a[x]) - int(b[x])
		}
	}
	for i := 0; i < 16; i += 4 {
		s0, s1, s2, s3 := d[i], d[i+1], d[i+2], d[i+3]
		t0 := s0 + s1
		t1 := s0 - s1
		t2 := s2 + s3
		t3 := s2 - s3
		d[i] = t0 + t2
		d[i+1] = t1 + t3
		d[i+2] = t0 - t2
		d[i+3] = t1 - t3
	}
	total := 0
	for i := 0; i < 4; i++ {
		s0, s1, s2, s3 := d[i], d[i+4], d[i+8], d[i+12]
		t0 := s0 + s1
		t1 := s0 - s1
		t2 := s2 + s3
		t3 := s2 - s3
		total += absInt(t0+t2) + absInt(t1+t3) + absInt(t0-t2) + absInt(t1-t3)
	}
	return (total + 1) >> 1
}

func SATD4x4(src []byte, srcStride, srcOff int, ref []byte, refStride, refOff int) int {
	return satd4x4Dispatch(src[srcOff:], srcStride, ref[refOff:], refStride)
}

var satd8x8Offsets = [4][2]int{{0, 0}, {4, 0}, {0, 4}, {4, 4}}

func satd8x8Generic(src []byte, srcStride int, ref []byte, refStride int) int {
	total := 0
	for _, o := range satd8x8Offsets {
		total += satd4x4Generic(src[o[1]*srcStride+o[0]:], srcStride, ref[o[1]*refStride+o[0]:], refStride)
	}
	return total
}

func SATD8x8(src []byte, srcStride, srcOff int, ref []byte, refStride, refOff int) int {
	return satd8x8Dispatch(src[srcOff:], srcStride, ref[refOff:], refStride)
}

func hFilter1(src []byte, off int) int {
	return int(src[off-2]) -
		5*int(src[off-1]) +
		20*int(src[off]) +
		20*int(src[off+1]) -
		5*int(src[off+2]) +
		int(src[off+3])
}

func vFilter1(src []byte, stride, off int) int {
	return int(src[off-2*stride]) -
		5*int(src[off-stride]) +
		20*int(src[off]) +
		20*int(src[off+stride]) -
		5*int(src[off+2*stride]) +
		int(src[off+3*stride])
}

func mcClip1(v int) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}

func sixTapHorizGeneric(dst []byte, dstStride, dstOff int, src []byte, srcStride, srcOff, w, h int) {
	for y := 0; y < h; y++ {
		so := srcOff + y*srcStride
		do := dstOff + y*dstStride
		for x := 0; x < w; x++ {
			dst[do+x] = mcClip1((hFilter1(src, so+x) + 16) >> 5)
		}
	}
}

func sixTapVertGeneric(dst []byte, dstStride, dstOff int, src []byte, srcStride, srcOff, w, h int) {
	for y := 0; y < h; y++ {
		so := srcOff + y*srcStride
		do := dstOff + y*dstStride
		for x := 0; x < w; x++ {
			dst[do+x] = mcClip1((vFilter1(src, srcStride, so+x) + 16) >> 5)
		}
	}
}

func sixTapHVGeneric(dst []byte, dstStride, dstOff int, src []byte, srcStride, srcOff, w, h int) {
	for y := 0; y < h; y++ {
		so := srcOff + y*srcStride
		do := dstOff + y*dstStride
		for x := 0; x < w; x++ {
			xo := so + x
			cc := hFilter1(src, xo-2*srcStride)
			dd := hFilter1(src, xo-srcStride)
			h1 := hFilter1(src, xo)
			m1 := hFilter1(src, xo+srcStride)
			ee := hFilter1(src, xo+2*srcStride)
			ff := hFilter1(src, xo+3*srcStride)
			j1 := cc - 5*dd + 20*h1 + 20*m1 - 5*ee + ff
			dst[do+x] = mcClip1((j1 + 512) >> 10)
		}
	}
}

func bilinearChromaGeneric(dst []byte, dstStride, dstOff int, src []byte, srcStride, srcOff, w, h, xFrac, yFrac int) {
	for y := 0; y < h; y++ {
		so := srcOff + y*srcStride
		do := dstOff + y*dstStride
		for x := 0; x < w; x++ {
			p := so + x
			a := int(src[p])
			b := int(src[p+1])
			c := int(src[p+srcStride])
			d := int(src[p+srcStride+1])
			val := ((8-xFrac)*(8-yFrac)*a + xFrac*(8-yFrac)*b + (8-xFrac)*yFrac*c + xFrac*yFrac*d + 32) >> 6
			dst[do+x] = byte(val)
		}
	}
}

func SixTapHoriz(dst []byte, dstStride, dstOff int, src []byte, srcStride, srcOff, w, h int) {
	sixTapHorizDispatch(dst, dstStride, dstOff, src, srcStride, srcOff, w, h)
}

func SixTapVert(dst []byte, dstStride, dstOff int, src []byte, srcStride, srcOff, w, h int) {
	sixTapVertDispatch(dst, dstStride, dstOff, src, srcStride, srcOff, w, h)
}

func SixTapHV(dst []byte, dstStride, dstOff int, src []byte, srcStride, srcOff, w, h int) {
	sixTapHVDispatch(dst, dstStride, dstOff, src, srcStride, srcOff, w, h)
}

func BilinearChroma(dst []byte, dstStride, dstOff int, src []byte, srcStride, srcOff, w, h, xFrac, yFrac int) {
	bilinearChromaDispatch(dst, dstStride, dstOff, src, srcStride, srcOff, w, h, xFrac, yFrac)
}

func deblockFits(plane []byte, offset, stride, before, after int) bool {
	if stride <= 0 || offset < before*stride {
		return false
	}
	return len(plane)-offset >= after*stride+16
}

func avgBytesGeneric(dst []byte, dstStride, dstOff int, a []byte, aStride, aOff int, b []byte, bStride, bOff, w, h int) {
	for y := 0; y < h; y++ {
		do := dstOff + y*dstStride
		ao := aOff + y*aStride
		bo := bOff + y*bStride
		for x := 0; x < w; x++ {
			dst[do+x] = byte((int(a[ao+x]) + int(b[bo+x]) + 1) >> 1)
		}
	}
}

func AvgBytes(dst []byte, dstStride, dstOff int, a []byte, aStride, aOff int, b []byte, bStride, bOff, w, h int) {
	avgBytesDispatch(dst, dstStride, dstOff, a, aStride, aOff, b, bStride, bOff, w, h)
}

func copyBlockGeneric(dst []byte, dstStride, dstOff int, src []byte, srcStride, srcOff, w, h int) {
	for y := 0; y < h; y++ {
		do := dstOff + y*dstStride
		so := srcOff + y*srcStride
		copy(dst[do:do+w], src[so:so+w])
	}
}

func CopyBlock(dst []byte, dstStride, dstOff int, src []byte, srcStride, srcOff, w, h int) {
	copyBlockDispatch(dst, dstStride, dstOff, src, srcStride, srcOff, w, h)
}
