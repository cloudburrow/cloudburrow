package mf

func I420ToNV12(dst []byte, dstStride int, src []byte, w, h int) {
	if w <= 0 || h <= 0 || w%2 != 0 || h%2 != 0 || dstStride < w {
		return
	}
	cw, ch := w/2, h/2
	ySize := w * h
	if len(src) < ySize+2*cw*ch || len(dst) < NV12Size(dstStride, h) {
		return
	}
	for y := 0; y < h; y++ {
		copy(dst[y*dstStride:y*dstStride+w], src[y*w:y*w+w])
	}
	cbOff := ySize
	crOff := ySize + cw*ch
	chromaBase := dstStride * h
	for y := 0; y < ch; y++ {
		row := chromaBase + y*dstStride
		srow := y * cw
		for x := 0; x < cw; x++ {
			dst[row+2*x] = src[cbOff+srow+x]
			dst[row+2*x+1] = src[crOff+srow+x]
		}
	}
}

func NV12ToI420(dst []byte, src []byte, srcStride int, w, h int) {
	NV12ToI420Offset(dst, src, srcStride, srcStride*h, w, h)
}

func NV12ToI420Offset(dst []byte, src []byte, srcStride, chromaOffset int, w, h int) {
	if w <= 0 || h <= 0 || w%2 != 0 || h%2 != 0 || srcStride < w {
		return
	}
	cw, ch := w/2, h/2
	if chromaOffset < srcStride*h {
		return
	}
	if len(dst) < I420Size(w, h) || len(src) < chromaOffset+srcStride*ch {
		return
	}
	for y := 0; y < h; y++ {
		copy(dst[y*w:y*w+w], src[y*srcStride:y*srcStride+w])
	}
	cbOff := w * h
	crOff := w*h + cw*ch
	for y := 0; y < ch; y++ {
		row := chromaOffset + y*srcStride
		drow := y * cw
		for x := 0; x < cw; x++ {
			dst[cbOff+drow+x] = src[row+2*x]
			dst[crOff+drow+x] = src[row+2*x+1]
		}
	}
}

func I420Size(w, h int) int {
	if w <= 0 || h <= 0 {
		return 0
	}
	return w*h + 2*(w/2)*(h/2)
}

func NV12Size(stride, h int) int {
	if stride <= 0 || h <= 0 {
		return 0
	}
	return stride*h + stride*(h/2)
}
