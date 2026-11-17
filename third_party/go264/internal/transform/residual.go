package transform

import "github.com/cloudburrow/cloudburrow/third_party/go264/internal/simd"

func Clip1(v int32) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}

func Residual4x4(dst *Block, src []byte, srcStride, srcOff int, pred []byte, predStride, predOff int) {
	for y := 0; y < 4; y++ {
		s := src[srcOff+y*srcStride:]
		p := pred[predOff+y*predStride:]
		dst[y*4] = int32(s[0]) - int32(p[0])
		dst[y*4+1] = int32(s[1]) - int32(p[1])
		dst[y*4+2] = int32(s[2]) - int32(p[2])
		dst[y*4+3] = int32(s[3]) - int32(p[3])
	}
}

func AddResidual4x4(plane []byte, stride, offset int, b *Block) {
	simd.AddResidual4x4(plane, stride, offset, (*[16]int32)(b))
}

var zigzag4x4 = [16]int{
	0, 1, 4, 8,
	5, 2, 3, 6,
	9, 12, 13, 10,
	7, 11, 14, 15,
}

func ZigZagIndex(i int) int { return zigzag4x4[i] }

func ScanToBlock(dst *Block, scan *[16]int32) {
	for i := 0; i < 16; i++ {
		dst[zigzag4x4[i]] = scan[i]
	}
}

func BlockToScan(dst *[16]int32, b *Block) {
	for i := 0; i < 16; i++ {
		dst[i] = b[zigzag4x4[i]]
	}
}
