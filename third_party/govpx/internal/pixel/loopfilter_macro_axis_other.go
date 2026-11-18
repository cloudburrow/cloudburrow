// CloudBurrow: upstream "//go:build !goexperiment.simd || !arm64 || purego" removed: the GOEXPERIMENT=simd
// kernels are not vendored, so this portable Go fallback is always built.

package pixel

const NativeLFByAxis = false

func CanLoopFilterMacroHorizontal16(p []byte, pitch int, mblimit, blimit byte) bool                 { return false }
func LoopFilterMacroHorizontal16Unchecked(p []byte, pitch int, mblimit, blimit, limit, thresh byte) {}

func CanLoopFilterMacroVertical16(p []byte, pitch int, mblimit, blimit byte) bool                 { return false }
func LoopFilterMacroVertical16Unchecked(p []byte, pitch int, mblimit, blimit, limit, thresh byte) {}
