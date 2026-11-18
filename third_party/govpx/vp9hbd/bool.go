package vp9hbd

import "errors"

var errData = errors.New("vp9hbd: invalid VP9 data")

// bitReader reads the uncompressed header, f(n) and s(n) (spec 9.1).
type bitReader struct {
	data []byte
	pos  int // in bits
}

func (r *bitReader) bit() int {
	if r.pos>>3 >= len(r.data) {
		r.pos++
		return 0
	}
	b := int(r.data[r.pos>>3]>>(7-uint(r.pos&7))) & 1
	r.pos++
	return b
}

func (r *bitReader) f(n int) int {
	x := 0
	for range n {
		x = 2*x + r.bit()
	}
	return x
}

func (r *bitReader) s(n int) int {
	v := r.f(n)
	if r.bit() == 1 {
		return -v
	}
	return v
}

// boolDecoder is the spec's Boolean decoder (9.2), reading a window of up to
// 64 bits at a time; past the end of its data it reads zero bits.
type boolDecoder struct {
	data  []byte
	pos   int
	value uint64 // the next bits, MSB first
	count int    // valid bits in value
	rng   uint32
}

func (b *boolDecoder) init(data []byte) error {
	if len(data) < 1 {
		return errData
	}
	b.data, b.pos, b.value, b.count, b.rng = data, 0, 0, 0, 255
	b.fill()
	if b.read(128) != 0 {
		return errData
	}
	return nil
}

func (b *boolDecoder) fill() {
	for b.count <= 56 {
		var c byte
		if b.pos < len(b.data) {
			c = b.data[b.pos]
			b.pos++
		}
		b.value |= uint64(c) << uint(56-b.count)
		b.count += 8
	}
}

func (b *boolDecoder) read(p int) int {
	split := 1 + (((b.rng - 1) * uint32(p)) >> 8)
	if b.count < 8 {
		b.fill()
	}
	big := uint64(split) << 56
	bit := 0
	if b.value >= big {
		b.rng -= split
		b.value -= big
		bit = 1
	} else {
		b.rng = split
	}
	for b.rng < 128 {
		b.rng <<= 1
		b.value <<= 1
		b.count--
	}
	return bit
}

func (b *boolDecoder) literal(n int) int {
	x := 0
	for range n {
		x = 2*x + b.read(128)
	}
	return x
}

// tree decodes a tree-coded value (spec 9.3.3) with probs indexed by node.
func (b *boolDecoder) tree(t []int8, probs []uint8) int {
	n := 0
	for {
		n = int(t[n+b.read(int(probs[n>>1]))])
		if n <= 0 {
			return -n
		}
	}
}
