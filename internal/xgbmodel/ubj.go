package xgbmodel

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// decodeUBJ reads one Universal Binary JSON value (https://ubjson.org,
// the format XGBoost writes a .ubj model in, and a model.bst since
// XGBoost 2.1) into the values encoding/json would read from the same
// document as JSON: map[string]any, []any, string, float64, bool and nil.
// Integers are float64 too, as JSON numbers are.
func decodeUBJ(data []byte) (any, error) {
	d := &ubjDecoder{b: data}
	v, err := d.value()
	if err != nil {
		return nil, err
	}
	for d.i < len(d.b) && d.b[d.i] == 'N' {
		d.i++
	}
	if d.i != len(d.b) {
		return nil, fmt.Errorf("UBJSON: %d bytes after the value", len(d.b)-d.i)
	}
	return v, nil
}

type ubjDecoder struct {
	b     []byte
	i     int
	depth int
}

var errUBJShort = errors.New("UBJSON: the data ends inside a value")

func (d *ubjDecoder) need(n int) ([]byte, error) {
	if n < 0 || d.i+n > len(d.b) {
		return nil, errUBJShort
	}
	p := d.b[d.i : d.i+n]
	d.i += n
	return p, nil
}

func (d *ubjDecoder) marker() (byte, error) {
	for {
		p, err := d.need(1)
		if err != nil {
			return 0, err
		}
		if p[0] != 'N' { // no-op
			return p[0], nil
		}
	}
}

func (d *ubjDecoder) value() (any, error) {
	m, err := d.marker()
	if err != nil {
		return nil, err
	}
	return d.typed(m)
}

// number reads a value of numeric marker m.
func (d *ubjDecoder) number(m byte) (float64, bool, error) {
	var size int
	switch m {
	case 'i', 'U':
		size = 1
	case 'I':
		size = 2
	case 'l', 'd':
		size = 4
	case 'L', 'D':
		size = 8
	default:
		return 0, false, nil
	}
	p, err := d.need(size)
	if err != nil {
		return 0, true, err
	}
	switch m {
	case 'i':
		return float64(int8(p[0])), true, nil
	case 'U':
		return float64(p[0]), true, nil
	case 'I':
		return float64(int16(binary.BigEndian.Uint16(p))), true, nil
	case 'l':
		return float64(int32(binary.BigEndian.Uint32(p))), true, nil
	case 'L':
		return float64(int64(binary.BigEndian.Uint64(p))), true, nil
	case 'd':
		return float64(math.Float32frombits(binary.BigEndian.Uint32(p))), true, nil
	default:
		return math.Float64frombits(binary.BigEndian.Uint64(p)), true, nil
	}
}

func (d *ubjDecoder) length() (int, error) {
	m, err := d.marker()
	if err != nil {
		return 0, err
	}
	n, ok, err := d.number(m)
	if err != nil {
		return 0, err
	}
	if !ok || m == 'd' || m == 'D' || n < 0 || n > float64(len(d.b)) {
		return 0, fmt.Errorf("UBJSON: bad length at byte %d", d.i)
	}
	return int(n), nil
}

func (d *ubjDecoder) str() (string, error) {
	n, err := d.length()
	if err != nil {
		return "", err
	}
	p, err := d.need(n)
	return string(p), err
}

func (d *ubjDecoder) typed(m byte) (any, error) {
	if n, ok, err := d.number(m); ok || err != nil {
		return n, err
	}
	switch m {
	case 'Z':
		return nil, nil
	case 'T':
		return true, nil
	case 'F':
		return false, nil
	case 'C':
		p, err := d.need(1)
		if err != nil {
			return nil, err
		}
		return string(p), nil
	case 'S', 'H':
		return d.str()
	case '[', '{':
		return d.container(m)
	}
	return nil, fmt.Errorf("UBJSON: unknown marker %q at byte %d", m, d.i-1)
}

// container reads an array or object, in its plain form or the
// optimized one ($ type and/or # count).
func (d *ubjDecoder) container(open byte) (any, error) {
	if d.depth++; d.depth > 256 {
		return nil, errors.New("UBJSON: nested too deep")
	}
	defer func() { d.depth-- }()
	var elem byte
	count := -1
	if d.i < len(d.b) && d.b[d.i] == '$' {
		d.i++
		p, err := d.need(1)
		if err != nil {
			return nil, err
		}
		elem = p[0]
		if d.i >= len(d.b) || d.b[d.i] != '#' {
			return nil, errors.New("UBJSON: a typed container without a count")
		}
	}
	if d.i < len(d.b) && d.b[d.i] == '#' {
		d.i++
		n, err := d.length()
		if err != nil {
			return nil, err
		}
		count = n
	}
	read := func() (any, error) {
		if elem != 0 {
			return d.typed(elem)
		}
		return d.value()
	}
	if open == '[' {
		var out []any
		if count >= 0 {
			out = make([]any, 0, min(count, len(d.b)))
		}
		for k := 0; count < 0 || k < count; k++ {
			if count < 0 {
				m, err := d.marker()
				if err != nil {
					return nil, err
				}
				if m == ']' {
					break
				}
				d.i--
			}
			v, err := read()
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		if out == nil {
			out = []any{}
		}
		return out, nil
	}
	out := map[string]any{}
	for k := 0; count < 0 || k < count; k++ {
		if count < 0 {
			if d.i < len(d.b) && d.b[d.i] == '}' {
				d.i++
				break
			}
		}
		key, err := d.str()
		if err != nil {
			return nil, err
		}
		v, err := read()
		if err != nil {
			return nil, err
		}
		out[key] = v
	}
	return out, nil
}
