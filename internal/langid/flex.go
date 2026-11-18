package langid

import (
	"encoding/binary"
	"errors"
	"math"
)

// flexMap reads a FlexBuffer whose root is a map, as a custom operator's
// custom_options are (FlexBuffers' format, flatbuffers/flexbuffers.h,
// Google's, Apache-2.0): each key's value as an int, a uint, a bool or a
// vector of ints. Only what the language detector's operators read is
// decoded.
type flexValue struct {
	ints  []int64
	num   int64
	isNum bool
}

// FlexBuffer types (flexbuffers.h's Type).
const (
	fxNull       = 0
	fxInt        = 1
	fxUint       = 2
	fxFloat      = 3
	fxVectorInt  = 11
	fxVectorUint = 12
	fxBool       = 26
	fxMap        = 9
	fxVector     = 10
)

func flexUint(b []byte, at, width int) (uint64, error) {
	if at < 0 || at+width > len(b) {
		return 0, errors.New("custom_options: out of range")
	}
	switch width {
	case 1:
		return uint64(b[at]), nil
	case 2:
		return uint64(binary.LittleEndian.Uint16(b[at:])), nil
	case 4:
		return uint64(binary.LittleEndian.Uint32(b[at:])), nil
	case 8:
		return binary.LittleEndian.Uint64(b[at:]), nil
	}
	return 0, errors.New("custom_options: bad width")
}

func flexInt(b []byte, at, width int) (int64, error) {
	u, err := flexUint(b, at, width)
	if err != nil {
		return 0, err
	}
	switch width {
	case 1:
		return int64(int8(u)), nil
	case 2:
		return int64(int16(u)), nil
	case 4:
		return int64(int32(u)), nil
	}
	return int64(u), nil
}

// flexMap decodes a FlexBuffer map.
func flexMap(b []byte) (map[string]flexValue, error) {
	if len(b) < 3 {
		return nil, errors.New("custom_options: too short")
	}
	rootWidth := int(b[len(b)-1])
	packed := b[len(b)-2]
	at := len(b) - 2 - rootWidth
	if packed>>2 != fxMap {
		return nil, errors.New("custom_options: the root is not a map")
	}
	off, err := flexUint(b, at, rootWidth)
	if err != nil {
		return nil, err
	}
	vals := at - int(off)
	w := 1 << (packed & 3) // the map's element width
	n, err := flexUint(b, vals-w, w)
	if err != nil {
		return nil, err
	}
	keysOff, err := flexUint(b, vals-3*w, w)
	if err != nil {
		return nil, err
	}
	keysWidth, err := flexUint(b, vals-2*w, w)
	if err != nil {
		return nil, err
	}
	keys := vals - 3*w - int(keysOff)
	out := map[string]flexValue{}
	for i := 0; i < int(n); i++ {
		ko, err := flexUint(b, keys+i*int(keysWidth), int(keysWidth))
		if err != nil {
			return nil, err
		}
		ks := keys + i*int(keysWidth) - int(ko)
		end := ks
		for end < len(b) && b[end] != 0 {
			end++
		}
		if ks < 0 || end >= len(b) {
			return nil, errors.New("custom_options: bad key")
		}
		key := string(b[ks:end])
		tp := vals + int(n)*w + i
		if tp >= len(b) {
			return nil, errors.New("custom_options: out of range")
		}
		t, cw := b[tp]>>2, 1<<(b[tp]&3)
		slot := vals + i*w
		var v flexValue
		switch t {
		case fxNull:
		case fxInt, fxBool:
			v.num, err = flexInt(b, slot, w)
			v.isNum = true
		case fxUint:
			var u uint64
			u, err = flexUint(b, slot, w)
			v.num, v.isNum = int64(u), true
		case fxFloat:
			var u uint64
			u, err = flexUint(b, slot, w)
			if w == 4 {
				v.num = int64(math.Float32frombits(uint32(u)))
			} else {
				v.num = int64(math.Float64frombits(u))
			}
			v.isNum = true
		case fxVectorInt, fxVectorUint:
			var o uint64
			if o, err = flexUint(b, slot, w); err != nil {
				return nil, err
			}
			vs := slot - int(o)
			var cnt uint64
			if cnt, err = flexUint(b, vs-cw, cw); err != nil {
				return nil, err
			}
			for j := 0; j < int(cnt); j++ {
				var x int64
				if t == fxVectorInt {
					x, err = flexInt(b, vs+j*cw, cw)
				} else {
					var u uint64
					u, err = flexUint(b, vs+j*cw, cw)
					x = int64(u)
				}
				if err != nil {
					return nil, err
				}
				v.ints = append(v.ints, x)
			}
		default:
			continue // a type the operators do not read
		}
		if err != nil {
			return nil, err
		}
		out[key] = v
	}
	return out, nil
}
