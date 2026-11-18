// CloudBurrow: upstream "//go:build !goexperiment.simd || (!arm64 && !amd64) || purego" removed: the GOEXPERIMENT=simd
// kernels are not vendored, so this portable Go fallback is always built.

package pixel

func AddConstantPair4(_ []byte, _ int, _ []byte, _ int, _, _ int16) bool { return false }
