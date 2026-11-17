// CloudBurrow: upstream "//go:build !goexperiment.simd || (!arm64 && !amd64) || purego" removed: the GOEXPERIMENT=simd
// kernels are not vendored, so this portable Go fallback is always built.

package pixel

// LoopFilterMacroInternal16 leaves unsupported targets to the separate edge paths.
func LoopFilterMacroInternal16(p []byte, step, outer, mblimit, blimit, limit, thresh int) bool {
	return false
}
