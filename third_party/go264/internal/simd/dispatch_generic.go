//go:build !amd64 || purego

package simd

func sadDispatch(src []byte, srcStride int, ref []byte, refStride, w, h int) int {
	return sadGeneric(src, srcStride, ref, refStride, w, h)
}

func Accelerated() bool { return false }

func Forward4x4(b *[16]int32) {
	forward4x4Generic(b)
}

func Inverse4x4(b *[16]int32) {
	inverse4x4Generic(b)
}

func Quant4x4(b *[16]int32, mf *[16]int32, f int32, qbits uint32) {
	quant4x4Generic(b, mf, f, qbits)
}

func Dequant4x4(b *[16]int32, scale *[16]int32, shift uint32) {
	dequant4x4Generic(b, scale, shift)
}

func AddResidual4x4(plane []byte, stride, offset int, b *[16]int32) {
	addResidual4x4Generic(plane, stride, offset, b)
}

func satd4x4Dispatch(src []byte, srcStride int, ref []byte, refStride int) int {
	return satd4x4Generic(src, srcStride, ref, refStride)
}

func satd8x8Dispatch(src []byte, srcStride int, ref []byte, refStride int) int {
	return satd8x8Generic(src, srcStride, ref, refStride)
}

func sixTapHorizDispatch(dst []byte, dstStride, dstOff int, src []byte, srcStride, srcOff, w, h int) {
	sixTapHorizGeneric(dst, dstStride, dstOff, src, srcStride, srcOff, w, h)
}

func sixTapVertDispatch(dst []byte, dstStride, dstOff int, src []byte, srcStride, srcOff, w, h int) {
	sixTapVertGeneric(dst, dstStride, dstOff, src, srcStride, srcOff, w, h)
}

func sixTapHVDispatch(dst []byte, dstStride, dstOff int, src []byte, srcStride, srcOff, w, h int) {
	sixTapHVGeneric(dst, dstStride, dstOff, src, srcStride, srcOff, w, h)
}

func bilinearChromaDispatch(dst []byte, dstStride, dstOff int, src []byte, srcStride, srcOff, w, h, xFrac, yFrac int) {
	bilinearChromaGeneric(dst, dstStride, dstOff, src, srcStride, srcOff, w, h, xFrac, yFrac)
}

func DeblockLumaNormal(plane []byte, offset, stride int, tc0, bs *[16]uint8, alpha, beta int) bool {
	return false
}

func DeblockLumaStrong(plane []byte, offset, stride, alpha, beta int) bool {
	return false
}

func DeblockLumaVerticalNormal(plane []byte, offset, stride int, tc0, bs *[16]uint8, alpha, beta int) bool {
	return false
}

func avgBytesDispatch(dst []byte, dstStride, dstOff int, a []byte, aStride, aOff int, b []byte, bStride, bOff, w, h int) {
	avgBytesGeneric(dst, dstStride, dstOff, a, aStride, aOff, b, bStride, bOff, w, h)
}

func copyBlockDispatch(dst []byte, dstStride, dstOff int, src []byte, srcStride, srcOff, w, h int) {
	copyBlockGeneric(dst, dstStride, dstOff, src, srcStride, srcOff, w, h)
}
