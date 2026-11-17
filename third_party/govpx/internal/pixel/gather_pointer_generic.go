// CloudBurrow: upstream "//go:build !goexperiment.simd || (!arm64 && !amd64) || purego" removed: the GOEXPERIMENT=simd
// kernels are not vendored, so this portable Go fallback is always built.

package pixel

func Gather16PtrFast(_ *byte, _ss int, _unused *byte, _ps int, _out *int16) bool { return false }
func Gather8PtrFast(_ *byte, _ss int, _unused *byte, _ps int, _out *int16) bool  { return false }
