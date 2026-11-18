package dsp

import (
	"github.com/cloudburrow/cloudburrow/third_party/govpx/internal/pixel"
	"unsafe"
)

// Ported from libvpx v1.16.0 vp8/common/dequantize.c.

func DequantizeBlock(qcoeff *[16]int16, dequant *[16]int16, dqcoeff *[16]int16) {
	// Exact in-place multiplication is safe; partial overlap retains scalar ordering.
	q, d, o := uintptr(unsafe.Pointer(qcoeff)), uintptr(unsafe.Pointer(dequant)), uintptr(unsafe.Pointer(dqcoeff))
	if pixel.Enabled && (q == o || q < o && o-q >= 32 || o < q && q-o >= 32) && (d == o || d < o && o-d >= 32 || o < d && d-o >= 32) {
		pixel.Mul16(qcoeff[:], dequant[:], dqcoeff[:], 16)
		return
	}

	for i := range 16 {
		dqcoeff[i] = qcoeff[i] * dequant[i]
	}
}

func DequantIDCT4x4Add(input *[16]int16, dequant *[16]int16, dst []byte, stride int) {
	// Sparse packets require only the DC multiply and add, not 15 zero products,
	// a full inverse transform, or clearing coefficients already known to be zero.
	if stride >= 4 && len(dst) >= 4 && stride <= (len(dst)-4)/3 {
		a, b := uintptr(unsafe.Pointer(input)), uintptr(unsafe.Pointer(&dst[0]))
		if a < b && b-a >= 32 || b < a && a-b >= uintptr(len(dst)) {
			ac := input[1] | input[2] | input[3] | input[4] | input[5] | input[6] | input[7] | input[8] | input[9] | input[10] | input[11] | input[12] | input[13] | input[14] | input[15]
			if ac == 0 {
				dc := input[0] * dequant[0]
				DCOnlyIDCT4x4AddInt32(int32(dc), dst, stride, dst, stride)
				input[0] = 0
				return
			}
		}
	}

	for i := range 16 {
		input[i] = dequant[i] * input[i]
	}

	IDCT4x4Add(input, dst, stride, dst, stride)

	for i := range 16 {
		input[i] = 0
	}
}
