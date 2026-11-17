// CloudBurrow: upstream "//go:build !goexperiment.simd || !arm64 || purego" removed: the GOEXPERIMENT=simd
// kernels are not vendored, so this portable Go fallback is always built.

package pixel

func Intra4x4DCFill(dst []byte, ds int, above, left []byte) bool { return false }
