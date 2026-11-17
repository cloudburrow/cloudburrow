// CloudBurrow: upstream "//go:build !goexperiment.simd || !arm64 || purego" removed: the GOEXPERIMENT=simd
// kernels are not vendored, so this portable Go fallback is always built.

package pixel

const TMNative = false

// TMPredictNative preserves the original generic pixel semantics on other builds.
func TMPredictNative(dst []byte, ds int, above, left []byte, top byte, w int) {
	TMPredict(dst, ds, above, left, top, w)
}
