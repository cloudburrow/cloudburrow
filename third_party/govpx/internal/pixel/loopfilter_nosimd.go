// CloudBurrow: upstream "//go:build !goexperiment.simd || (!arm64 && !amd64) || purego" removed: the GOEXPERIMENT=simd
// kernels are not vendored, so this portable Go fallback is always built.

package pixel

func LoopFilter(_ []byte, _ int, _ int, _ int, _ int, _ int, _ int, _ int, _ int) bool { return false }
