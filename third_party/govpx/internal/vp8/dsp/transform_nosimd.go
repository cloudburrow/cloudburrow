// CloudBurrow: upstream "//go:build !goexperiment.simd || (!arm64 && !amd64) || purego" removed: the GOEXPERIMENT=simd
// kernels are not vendored, so this portable Go fallback is always built.

package dsp

// Arithmetic follows libvpx v1.16.0 vp8/common/idctllm.c.
func inverse4Go(_ *[16]int16, _ []byte, _ int, _ []byte, _ int, _ bool, _ []int16) bool { return false }
