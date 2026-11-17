// CloudBurrow: upstream "//go:build !goexperiment.simd || (!arm64 && !amd64) || purego" removed: the GOEXPERIMENT=simd
// kernels are not vendored, so this portable Go fallback is always built.

package pixel

func SSE16(src []byte, ss int, ref []byte, rs, h int) uint32 { return SSE16Scalar(src, ss, ref, rs, h) }
func SSE8(src []byte, ss int, ref []byte, rs, h int) uint32  { return SSE8Scalar(src, ss, ref, rs, h) }
