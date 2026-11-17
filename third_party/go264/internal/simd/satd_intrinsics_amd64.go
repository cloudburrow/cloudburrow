//go:build amd64 && !purego && goexperiment.simd && simdintrinsics

package simd

import (
	"encoding/binary"

	"simd/archsimd"
)

var le = binary.LittleEndian

var (
	satdEvenWordBits = [16]int16{-1, 0, -1, 0, -1, 0, -1, 0, -1, 0, -1, 0, -1, 0, -1, 0}
	satdEvenDWBits   = [16]int16{-1, -1, 0, 0, -1, -1, 0, 0, -1, -1, 0, 0, -1, -1, 0, 0}
	satdEvenQWBits   = [16]int16{-1, -1, -1, -1, 0, 0, 0, 0, -1, -1, -1, -1, 0, 0, 0, 0}

	satdEvenWords = archsimd.LoadInt16x16Array(&satdEvenWordBits).ToMask()
	satdEvenDW    = archsimd.LoadInt16x16Array(&satdEvenDWBits).ToMask()
	satdEvenQW    = archsimd.LoadInt16x16Array(&satdEvenQWBits).ToMask()
	satdOnes      = archsimd.BroadcastInt16x16(1)
)

func satdHadamard(sv, rv archsimd.Int16x16) int {
	d := sv.Sub(rv)

	t1 := d.PermuteScalarsLoGrouped(1, 0, 3, 2).PermuteScalarsHiGrouped(1, 0, 3, 2)
	s1 := d.Add(t1).IfElse(satdEvenWords, d.Sub(t1))

	t2 := s1.AsInt32x8().PermuteScalarsGrouped(1, 0, 3, 2).AsInt16x16()
	s2 := s1.Add(t2).IfElse(satdEvenDW, s1.Sub(t2))

	t3 := s2.AsInt32x8().PermuteScalarsGrouped(2, 3, 0, 1).AsInt16x16()
	s3 := s2.Add(t3).IfElse(satdEvenQW, s2.Sub(t3))

	t4 := s3.ConcatPermute128Scalars(1, 0, s3)
	tot := s3.Add(t4).Abs().Add(s3.Sub(t4).Abs())

	wide := tot.DotProductPairs(satdOnes)
	half := wide.GetLo().Add(wide.GetHi())
	total := int(half.GetElem(0)) + int(half.GetElem(1)) +
		int(half.GetElem(2)) + int(half.GetElem(3))
	return ((total >> 1) + 1) >> 1
}

func satdGather4(dst *[16]byte, src []byte, stride int) {
	copy(dst[0:4], src[0:4])
	copy(dst[4:8], src[stride:stride+4])
	copy(dst[8:12], src[2*stride:2*stride+4])
	copy(dst[12:16], src[3*stride:3*stride+4])
}

func satd4x4Intrinsics(src []byte, srcStride int, ref []byte, refStride int) int {
	var sb, rb [16]byte
	satdGather4(&sb, src, srcStride)
	satdGather4(&rb, ref, refStride)
	sv := archsimd.LoadUint8x16Array(&sb).ExtendToUint16().AsInt16x16()
	rv := archsimd.LoadUint8x16Array(&rb).ExtendToUint16().AsInt16x16()
	return satdHadamard(sv, rv)
}

func satdGather4Fast(dst *[16]byte, src []byte, stride int) {
	_ = src[3*stride+3]
	le.PutUint32(dst[0:4], le.Uint32(src[0:4]))
	le.PutUint32(dst[4:8], le.Uint32(src[stride:stride+4]))
	le.PutUint32(dst[8:12], le.Uint32(src[2*stride:2*stride+4]))
	le.PutUint32(dst[12:16], le.Uint32(src[3*stride:3*stride+4]))
}

func satd4x4IntrinsicsFast(src []byte, srcStride int, ref []byte, refStride int) int {
	var sb, rb [16]byte
	satdGather4Fast(&sb, src, srcStride)
	satdGather4Fast(&rb, ref, refStride)
	sv := archsimd.LoadUint8x16Array(&sb).ExtendToUint16().AsInt16x16()
	rv := archsimd.LoadUint8x16Array(&rb).ExtendToUint16().AsInt16x16()
	return satdHadamard(sv, rv)
}

func satdPackRows(src []byte, stride int) archsimd.Int16x16 {
	r0 := archsimd.LoadUint8x16(src[0:]).AsUint32x4()
	r1 := archsimd.LoadUint8x16(src[stride:]).AsUint32x4()
	r2 := archsimd.LoadUint8x16(src[2*stride:]).AsUint32x4()
	r3 := archsimd.LoadUint8x16(src[3*stride:]).AsUint32x4()
	lo := r0.InterleaveLo(r1).AsUint64x2()
	hi := r2.InterleaveLo(r3).AsUint64x2()
	return lo.InterleaveLo(hi).AsUint8x16().ExtendToUint16().AsInt16x16()
}

func satd4x4IntrinsicsWide(src []byte, srcStride int, ref []byte, refStride int) int {
	return satdHadamard(satdPackRows(src, srcStride), satdPackRows(ref, refStride))
}
