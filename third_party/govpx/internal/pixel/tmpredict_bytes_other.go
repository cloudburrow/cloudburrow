// CloudBurrow: upstream "//go:build !goexperiment.simd || !arm64 || purego" removed: the GOEXPERIMENT=simd
// kernels are not vendored, so this portable Go fallback is always built.

package pixel

func TMPredictBytes(dst []byte, ds int, above, left []byte, top byte, w int) bool { return false }
