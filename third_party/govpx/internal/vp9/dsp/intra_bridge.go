package dsp

import (
	"github.com/cloudburrow/cloudburrow/third_party/govpx/internal/pixel"
	"unsafe"
)

// The SIMD bridge only accepts complete rows. Invalid windows retain the
// original predictor's partial-write/panic behavior.
func intraBridgeWindow(dst []byte, stride, bs int) bool {
	return pixel.Enabled && stride >= bs && len(dst) >= bs && stride <= (len(dst)-bs)/(bs-1)
}
func intraBridgeDisjoint(dst []byte, stride, bs int, edge []byte, count int) bool {
	if !intraBridgeWindow(dst, stride, bs) || len(edge) < count {
		return false
	}
	d, e := uintptr(unsafe.Pointer(unsafe.SliceData(dst))), uintptr(unsafe.Pointer(unsafe.SliceData(edge)))
	span := uintptr((bs-1)*stride + bs)
	return !((d <= e && e-d < span) || (e < d && d-e < uintptr(count)))
}
