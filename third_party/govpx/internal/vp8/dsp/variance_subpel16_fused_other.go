package dsp

import (
	"github.com/cloudburrow/cloudburrow/third_party/govpx/internal/pixel"
	"github.com/cloudburrow/cloudburrow/third_party/govpx/internal/vp8/tables"
	"unsafe"
)

// Fallback stubs for the fused kernels ported from libvpx v1.16.0
// vpx_dsp/arm/subpel_variance_neon.c on arm64.

func subpelVariance16x16Horizontal(src []byte, ss, x int, ref []byte, rs int) (int, int, bool) {
	if !pixel.Enabled {
		return 0, 0, false
	}
	sum, sse := pixel.SubpelStats(src, ss, ref, rs, 16, 16, tables.BilinearFilters[x], tables.BilinearFilters[0])
	return sum, int(sse), true
}
func subpelVariance16x16Vertical(src []byte, ss, y int, ref []byte, rs int) (int, int, bool) {
	if !pixel.Enabled {
		return 0, 0, false
	}
	sum, sse := pixel.SubpelStats(src, ss, ref, rs, 16, 16, tables.BilinearFilters[0], tables.BilinearFilters[y])
	return sum, int(sse), true
}
func subpelVariance16x16Bilinear(src []byte, ss, x, y int, ref []byte, rs int) (int, int, bool) {
	if !pixel.Enabled {
		return 0, 0, false
	}
	sum, sse := pixel.SubpelStats(src, ss, ref, rs, 16, 16, tables.BilinearFilters[x], tables.BilinearFilters[y])
	return sum, int(sse), true
}
func subpelVariance16x16PtrFast(src *byte, ss, x, y int, ref *byte, rs int) (int, int) {
	if !pixel.Enabled {
		return subpelVariance16x16PtrFastFallback(src, ss, x, y, ref, rs)
	}
	sum, sse := pixel.SubpelStats(unsafe.Slice(src, 16*ss+17), ss, unsafe.Slice(ref, 15*rs+16), rs, 16, 16, tables.BilinearFilters[x], tables.BilinearFilters[y])
	return int(sse) - sum*sum/256, int(sse)
}
