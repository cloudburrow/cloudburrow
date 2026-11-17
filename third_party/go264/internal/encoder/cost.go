package encoder

import (
	"math"

	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/simd"
)

var lambdaTable [52]int

func init() {
	for qp := 0; qp < 52; qp++ {
		v := int(math.Round(math.Pow(2, float64(qp-12)/6)))
		if v < 1 {
			v = 1
		}
		lambdaTable[qp] = v
	}
}

func sad(src []byte, srcStride, srcOff int, ref []byte, refStride, refOff, w, h int) int {
	return simd.SAD(src, srcStride, srcOff, ref, refStride, refOff, w, h)
}

func satd4x4(src []byte, srcStride, srcOff int, ref []byte, refStride, refOff int) int {
	return simd.SATD4x4(src, srcStride, srcOff, ref, refStride, refOff)
}

func satdBlock(src []byte, srcStride, srcOff int, ref []byte, refStride, refOff, w, h int) int {
	total := 0
	y := 0
	for ; y+8 <= h; y += 8 {
		x := 0
		for ; x+8 <= w; x += 8 {
			total += simd.SATD8x8(src, srcStride, srcOff+y*srcStride+x, ref, refStride, refOff+y*refStride+x)
		}
		for ; x+4 <= w; x += 4 {
			total += satd4x4(src, srcStride, srcOff+y*srcStride+x, ref, refStride, refOff+y*refStride+x)
			total += satd4x4(src, srcStride, srcOff+(y+4)*srcStride+x, ref, refStride, refOff+(y+4)*refStride+x)
		}
	}
	for ; y+4 <= h; y += 4 {
		for x := 0; x+4 <= w; x += 4 {
			total += satd4x4(src, srcStride, srcOff+y*srcStride+x, ref, refStride, refOff+y*refStride+x)
		}
	}
	return total
}
