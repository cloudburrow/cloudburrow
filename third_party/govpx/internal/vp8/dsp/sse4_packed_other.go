// CloudBurrow: upstream "//go:build !goexperiment.simd || !arm64 || purego" removed: the GOEXPERIMENT=simd
// kernels are not vendored, so this portable Go fallback is always built.

package dsp

func sse4RowsGo(_ []byte, _ int, _ []byte, _, _ int) (int, bool) { return 0, false }
