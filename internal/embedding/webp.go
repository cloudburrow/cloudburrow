package embedding

import (
	"image"
)

// Lossy WebP (VP8) pixels are limited-range BT.601 YUV with 4:2:0 chroma.
// golang.org/x/image/webp returns them as an image.YCbCr (image.NYCbCrA
// with alpha), whose At converts with JPEG's full-range formula and takes
// the nearest chroma sample. Every other WebP reader the model's users meet
// (libwebp's own dwebp, and so sharp, Pillow, Chrome and Transformers.js)
// converts with libwebp's integer limited-range formula after its "fancy"
// bilinear chroma upsampling, and the difference is large enough to move
// an embedding (cosine 0.965 against Transformers.js on a lossy 768 x 768
// shape, #1651). webpRGB reproduces libwebp 1.x's EmitFancyRGB with
// UpsampleRgbLinePair and VP8YuvToRgb (src/dec/io_dec.c,
// src/dsp/upsampling.c, src/dsp/yuv.h).

// webpRGB converts a lossy WebP's planes to NRGBA as libwebp does. It
// returns nil when img is not 4:2:0 YCbCr.
func webpRGB(img image.Image) *image.NRGBA {
	var yc *image.YCbCr
	var alpha []uint8
	var aStride int
	switch m := img.(type) {
	case *image.YCbCr:
		yc = m
	case *image.NYCbCrA:
		yc, alpha, aStride = &m.YCbCr, m.A, m.AStride
	default:
		return nil
	}
	if yc.SubsampleRatio != image.YCbCrSubsampleRatio420 {
		return nil
	}
	b := yc.Rect
	w, h := b.Dx(), b.Dy()
	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	yRow := func(y int) []uint8 { return yc.Y[y*yc.YStride : y*yc.YStride+w] }
	uvW := (w + 1) / 2
	uRow := func(r int) []uint8 { return yc.Cb[r*yc.CStride : r*yc.CStride+uvW] }
	vRow := func(r int) []uint8 { return yc.Cr[r*yc.CStride : r*yc.CStride+uvW] }
	dst := func(y int) []uint8 { return out.Pix[y*out.Stride : y*out.Stride+w*4] }

	// The first row mirrors its chroma row; each following pair of rows
	// (2k-1, 2k) blends chroma rows k-1 and k; an even height's last row
	// mirrors the last chroma row.
	webpUpsamplePair(yRow(0), nil, uRow(0), vRow(0), uRow(0), vRow(0), dst(0), nil, w)
	for y := 1; y+1 < h; y += 2 {
		k := (y + 1) / 2
		webpUpsamplePair(yRow(y), yRow(y+1), uRow(k-1), vRow(k-1), uRow(k), vRow(k), dst(y), dst(y+1), w)
	}
	if h > 1 && h%2 == 0 {
		k := h/2 - 1
		webpUpsamplePair(yRow(h-1), nil, uRow(k), vRow(k), uRow(k), vRow(k), dst(h-1), nil, w)
	}
	for y := 0; y < h; y++ {
		row := dst(y)
		for x := 0; x < w; x++ {
			a := uint8(255)
			if alpha != nil {
				a = alpha[y*aStride+x]
			}
			row[x*4+3] = a
		}
	}
	return out
}

// webpUpsamplePair is libwebp's UpsampleRgbLinePair: two rows of luma
// with the chroma rows above (top) and below (cur) them, each chroma value
// the 9-3-3-1 blend of its four neighbours, the two channels packed in one
// uint32 as libwebp packs them (its rounding included).
func webpUpsamplePair(topY, bottomY, topU, topV, curU, curV, topDst, bottomDst []uint8, n int) {
	load := func(u, v uint8) uint32 { return uint32(u) | uint32(v)<<16 }
	put := func(dst []uint8, x int, y uint8, uv uint32) {
		webpYUVToRGB(int(y), int(uv&0xff), int(uv>>16), dst[x*4:x*4+3])
	}
	last := (n - 1) >> 1
	tl, l := load(topU[0], topV[0]), load(curU[0], curV[0])
	put(topDst, 0, topY[0], (3*tl+l+0x00020002)>>2)
	if bottomY != nil {
		put(bottomDst, 0, bottomY[0], (3*l+tl+0x00020002)>>2)
	}
	for x := 1; x <= last; x++ {
		t, uv := load(topU[x], topV[x]), load(curU[x], curV[x])
		avg := tl + t + l + uv + 0x00080008
		diag12 := (avg + 2*(t+l)) >> 3
		diag03 := (avg + 2*(tl+uv)) >> 3
		put(topDst, 2*x-1, topY[2*x-1], (diag12+tl)>>1)
		put(topDst, 2*x, topY[2*x], (diag03+t)>>1)
		if bottomY != nil {
			put(bottomDst, 2*x-1, bottomY[2*x-1], (diag03+l)>>1)
			put(bottomDst, 2*x, bottomY[2*x], (diag12+uv)>>1)
		}
		tl, l = t, uv
	}
	if n&1 == 0 {
		put(topDst, n-1, topY[n-1], (3*tl+l+0x00020002)>>2)
		if bottomY != nil {
			put(bottomDst, n-1, bottomY[n-1], (3*l+tl+0x00020002)>>2)
		}
	}
}

// webpYUVToRGB is libwebp's VP8YuvToRgb: BT.601 limited range in 14-bit
// fixed point.
func webpYUVToRGB(y, u, v int, rgb []uint8) {
	mult := func(v, c int) int { return (v * c) >> 8 }
	clip := func(v int) uint8 {
		const mask = (256 << 6) - 1
		switch {
		case v&^mask == 0:
			return uint8(v >> 6)
		case v < 0:
			return 0
		}
		return 255
	}
	rgb[0] = clip(mult(y, 19077) + mult(v, 26149) - 14234)
	rgb[1] = clip(mult(y, 19077) - mult(u, 6419) - mult(v, 13320) + 8708)
	rgb[2] = clip(mult(y, 19077) + mult(u, 33050) - 17685)
}
