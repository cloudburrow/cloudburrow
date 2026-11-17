// CloudBurrow: upstream "//go:build !goexperiment.simd || !arm64 || purego" removed: the GOEXPERIMENT=simd
// kernels are not vendored, so this portable Go fallback is always built.

package pixel

const IntraDCFillNative = false

func IntraDCFill(dst []byte, ds int, above, left []byte, n int, up, lf bool) bool { return false }
