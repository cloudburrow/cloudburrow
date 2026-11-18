package transform

import "github.com/cloudburrow/cloudburrow/third_party/go264/internal/simd"

type Block [16]int32

type ChromaDC [4]int32

var normAdjust = [6][3]int32{
	{10, 16, 13},
	{11, 18, 14},
	{13, 20, 16},
	{14, 23, 18},
	{16, 25, 20},
	{18, 29, 23},
}

var quantCoef = [6][3]int32{
	{13107, 5243, 8066},
	{11916, 4660, 7490},
	{10082, 4194, 6554},
	{9362, 3647, 5825},
	{8192, 3355, 5243},
	{7282, 2893, 4559},
}

var posClass = [16]int{
	0, 2, 0, 2,
	2, 1, 2, 1,
	0, 2, 0, 2,
	2, 1, 2, 1,
}

var levelScale [6][16]int32

var levelQuant [6][16]int32

func init() {
	for m := 0; m < 6; m++ {
		for i := 0; i < 16; i++ {
			levelScale[m][i] = normAdjust[m][posClass[i]] * 16
			levelQuant[m][i] = quantCoef[m][posClass[i]]
		}
	}
}

func Forward4x4(b *Block) {
	simd.Forward4x4((*[16]int32)(b))
}

func Inverse4x4(b *Block) {
	simd.Inverse4x4((*[16]int32)(b))
}

func Hadamard4x4(b *Block) {
	for i := 0; i < 16; i += 4 {
		s0, s1, s2, s3 := b[i], b[i+1], b[i+2], b[i+3]
		t0 := s0 + s3
		t1 := s1 + s2
		t2 := s1 - s2
		t3 := s0 - s3
		b[i] = t0 + t1
		b[i+1] = t3 + t2
		b[i+2] = t0 - t1
		b[i+3] = t3 - t2
	}
	for i := 0; i < 4; i++ {
		s0, s1, s2, s3 := b[i], b[i+4], b[i+8], b[i+12]
		t0 := s0 + s3
		t1 := s1 + s2
		t2 := s1 - s2
		t3 := s0 - s3
		b[i] = t0 + t1
		b[i+4] = t3 + t2
		b[i+8] = t0 - t1
		b[i+12] = t3 - t2
	}
}

func HadamardForwardDC4x4(b *Block) {
	for i := 0; i < 16; i += 4 {
		s0, s1, s2, s3 := b[i], b[i+1], b[i+2], b[i+3]
		t0 := s0 + s3
		t1 := s1 + s2
		t2 := s1 - s2
		t3 := s0 - s3
		b[i] = t0 + t1
		b[i+1] = t3 + t2
		b[i+2] = t0 - t1
		b[i+3] = t3 - t2
	}
	for i := 0; i < 4; i++ {
		s0, s1, s2, s3 := b[i], b[i+4], b[i+8], b[i+12]
		t0 := s0 + s3
		t1 := s1 + s2
		t2 := s1 - s2
		t3 := s0 - s3
		b[i] = (t0 + t1 + 1) >> 1
		b[i+4] = (t3 + t2 + 1) >> 1
		b[i+8] = (t0 - t1 + 1) >> 1
		b[i+12] = (t3 - t2 + 1) >> 1
	}
}

func Hadamard2x2(b *ChromaDC) {
	a0, a1, a2, a3 := b[0], b[1], b[2], b[3]
	b[0] = a0 + a1 + a2 + a3
	b[1] = a0 - a1 + a2 - a3
	b[2] = a0 + a1 - a2 - a3
	b[3] = a0 - a1 - a2 + a3
}

func Dequant4x4(b *Block, qp int, skipDC bool) {
	m := qp % 6
	shift := qp / 6
	scale := &levelScale[m]

	var savedDC int32
	if skipDC {
		savedDC = b[0]
	}
	simd.Dequant4x4((*[16]int32)(b), scale, uint32(shift))
	if skipDC {
		b[0] = savedDC
	}
}

func DequantLumaDC(b *Block, qp int) {
	Hadamard4x4(b)
	scale := levelScale[qp%6][0]
	shift := qp / 6
	if shift >= 6 {
		s := uint(shift - 6)
		for i := range b {
			b[i] = b[i] * scale << s
		}
		return
	}
	s := uint(6 - shift)
	round := int32(1) << (s - 1)
	for i := range b {
		b[i] = (b[i]*scale + round) >> s
	}
}

func DequantChromaDC(b *ChromaDC, qp int) {
	Hadamard2x2(b)
	scale := levelScale[qp%6][0]
	shift := uint(qp / 6)
	for i := range b {
		b[i] = (b[i] * scale << shift) >> 5
	}
}

func Quant4x4(b *Block, qp int, intra bool) {
	m := qp % 6
	qbits := uint(15 + qp/6)
	var f int32
	if intra {
		f = int32(1) << qbits / 3
	} else {
		f = int32(1) << qbits / 6
	}
	mf := &levelQuant[m]
	simd.Quant4x4((*[16]int32)(b), mf, f, uint32(qbits))
}

func QuantLumaDC(b *Block, qp int, intra bool) {
	HadamardForwardDC4x4(b)
	mf := quantCoef[qp%6][0]
	qbits := uint(16 + qp/6)
	var f int32
	if intra {
		f = int32(1) << qbits / 3
	} else {
		f = int32(1) << qbits / 6
	}
	for i := range b {
		v := b[i]
		if v >= 0 {
			b[i] = (v*mf + f) >> qbits
		} else {
			b[i] = -((-v*mf + f) >> qbits)
		}
	}
}

func QuantChromaDC(b *ChromaDC, qp int, intra bool) {
	Hadamard2x2(b)
	mf := quantCoef[qp%6][0]
	qbits := uint(16 + qp/6)
	var f int32
	if intra {
		f = int32(1) << qbits / 3
	} else {
		f = int32(1) << qbits / 6
	}
	for i := range b {
		v := b[i]
		if v >= 0 {
			b[i] = (v*mf + f) >> qbits
		} else {
			b[i] = -((-v*mf + f) >> qbits)
		}
	}
}
