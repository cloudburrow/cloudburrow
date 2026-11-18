package dsp

import "github.com/cloudburrow/cloudburrow/third_party/govpx/internal/pixel"

// Pure-Go fallback for the 16x16 variance block on architectures
// without a SIMD port. Mirrors libvpx v1.16.0 vpx_dsp/variance.c
// semantics with a width-specialized scalar loop.

func varianceBlock16x16(src []byte, srcStride int, ref []byte, refStride int) (int, int) {
	sum, sse := pixel.Stats(src, srcStride, ref, refStride, 16, 16)
	return sum, int(sse)
}

// VarianceBlock16x16PtrFast is the pointer-form fallback used by hot
// callers when bounds have already been validated. The fallback build
// has no SIMD kernel to bypass, so this uses the same width-specialized
// scalar loop while keeping the pointer-form hot-call contract.
func VarianceBlock16x16PtrFast(src *byte, srcStride int, ref *byte, refStride int) (int, int) {
	return variancePtr(src, srcStride, ref, refStride, 16, 16)
}

func sse16x16PtrFast(src *byte, srcStride int, ref *byte, refStride int) int {
	return SSE16xNPtrFast(src, srcStride, ref, refStride, 16)
}
