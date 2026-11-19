package tflitemodel

import (
	"encoding/binary"
	"errors"
	"math"
)

// flexMap decodes a custom operator's custom_options, a FlexBuffer whose
// root is a map (FlexBuffers' format, flatbuffers/flexbuffers.h, Google's,
// Apache-2.0), to its scalar values: ints, uints, floats and bools, inline
// or indirect, each as a float64. Other values are left out.
func flexMap(b []byte) (map[string]float64, error) {
	bad := errors.New("custom_options is not a FlexBuffer map")
	if len(b) < 3 {
		return nil, bad
	}
	uintAt := func(at, w int) (uint64, bool) {
		if at < 0 || at+w > len(b) {
			return 0, false
		}
		switch w {
		case 1:
			return uint64(b[at]), true
		case 2:
			return uint64(binary.LittleEndian.Uint16(b[at:])), true
		case 4:
			return uint64(binary.LittleEndian.Uint32(b[at:])), true
		case 8:
			return binary.LittleEndian.Uint64(b[at:]), true
		}
		return 0, false
	}
	intAt := func(at, w int) (int64, bool) {
		u, ok := uintAt(at, w)
		switch w {
		case 1:
			return int64(int8(u)), ok
		case 2:
			return int64(int16(u)), ok
		case 4:
			return int64(int32(u)), ok
		}
		return int64(u), ok
	}
	floatAt := func(at, w int) (float64, bool) {
		u, ok := uintAt(at, w)
		if w == 4 {
			return float64(math.Float32frombits(uint32(u))), ok
		}
		return math.Float64frombits(u), ok && w == 8
	}
	rootW := int(b[len(b)-1])
	packed := b[len(b)-2]
	const fxMap, fxInt, fxUint, fxFloat, fxIndInt, fxIndUint, fxIndFloat, fxBool = 9, 1, 2, 3, 6, 7, 8, 26
	if packed>>2 != fxMap {
		return nil, bad
	}
	at := len(b) - 2 - rootW
	off, ok := uintAt(at, rootW)
	if !ok {
		return nil, bad
	}
	start := at - int(off)
	w := 1 << (packed & 3)
	n, ok := uintAt(start-w, w)
	if !ok || n > uint64(len(b)) {
		return nil, bad
	}
	koff, ok1 := uintAt(start-3*w, w)
	kw, ok2 := uintAt(start-2*w, w)
	if !ok1 || !ok2 {
		return nil, bad
	}
	keys := start - 3*w - int(koff)
	out := map[string]float64{}
	for i := range int(n) {
		ko, ok := uintAt(keys+i*int(kw), int(kw))
		if !ok {
			return nil, bad
		}
		ks := keys + i*int(kw) - int(ko)
		if ks < 0 || ks >= len(b) {
			return nil, bad
		}
		ke := ks
		for ke < len(b) && b[ke] != 0 {
			ke++
		}
		key := string(b[ks:ke])
		ti := start + int(n)*w + i
		if ti >= len(b) {
			return nil, bad
		}
		t := b[ti]
		pos := start + i*w
		vw := 1 << (t & 3)
		var v float64
		switch t >> 2 {
		case fxInt:
			x, ok := intAt(pos, w)
			if !ok {
				return nil, bad
			}
			v = float64(x)
		case fxUint, fxBool:
			x, ok := uintAt(pos, w)
			if !ok {
				return nil, bad
			}
			v = float64(x)
		case fxFloat:
			x, ok := floatAt(pos, w)
			if !ok {
				return nil, bad
			}
			v = x
		case fxIndInt, fxIndUint, fxIndFloat:
			o, ok := uintAt(pos, w)
			if !ok {
				return nil, bad
			}
			p := pos - int(o)
			switch t >> 2 {
			case fxIndInt:
				x, ok := intAt(p, vw)
				if !ok {
					return nil, bad
				}
				v = float64(x)
			case fxIndUint:
				x, ok := uintAt(p, vw)
				if !ok {
					return nil, bad
				}
				v = float64(x)
			default:
				x, ok := floatAt(p, vw)
				if !ok {
					return nil, bad
				}
				v = x
			}
		default:
			continue
		}
		out[key] = v
	}
	return out, nil
}
