// CloudBurrow: upstream "//go:build !goexperiment.simd || !amd64 || purego" removed: the GOEXPERIMENT=simd
// kernels are not vendored, so this portable Go fallback is always built.

package pixel

import "unsafe"

func SSE8Ptr(src *byte, ss int, ref *byte, rs int) uint32 {
	return SSE8(unsafe.Slice(src, 7*ss+8), ss, unsafe.Slice(ref, 7*rs+8), rs, 8)
}
