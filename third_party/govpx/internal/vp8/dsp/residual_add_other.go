// CloudBurrow: upstream "//go:build !goexperiment.simd || (!arm64 && !amd64) || purego" removed: the GOEXPERIMENT=simd
// kernels are not vendored, so this portable Go fallback is always built.

package dsp

func addResidual4Go(_ []byte, _ int, _ *[16]int16) bool { return false }
