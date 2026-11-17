// CloudBurrow: upstream "//go:build !goexperiment.simd || (!arm64 && !amd64) || purego" removed: the GOEXPERIMENT=simd
// kernels are not vendored, so this portable Go fallback is always built.

package pixel

func diffMetricsSIMD(src []byte, ss int, ref []byte, rs, w, h int) (int, int, uint32) {
	panic("SIMD backend unavailable")
}
