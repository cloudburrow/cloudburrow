package ac3

import "errors"

var errShort = errors.New("ac3: the syncframe ends early")

// bitReader reads a syncframe most significant bit first. Reading past
// the end sets err and returns zeros.
type bitReader struct {
	b   []byte
	pos int // in bits
	err error
}

func (r *bitReader) u(n int) int {
	if n == 0 {
		return 0
	}
	if r.pos+n > len(r.b)*8 {
		r.err = errShort
		r.pos += n
		return 0
	}
	v := 0
	for range n {
		v = v<<1 | int(r.b[r.pos>>3]>>(7-uint(r.pos&7))&1)
		r.pos++
	}
	return v
}

func (r *bitReader) flag() bool { return r.u(1) == 1 }

// s reads an n-bit two's complement value.
func (r *bitReader) s(n int) int {
	v := r.u(n)
	if n > 0 && v&(1<<(n-1)) != 0 {
		v -= 1 << n
	}
	return v
}

func (r *bitReader) skip(n int) {
	if r.pos+n > len(r.b)*8 {
		r.err = errShort
	}
	r.pos += n
}
