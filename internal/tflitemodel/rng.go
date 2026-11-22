package tflitemodel

import (
	"fmt"
	"math/bits"
)

// opStablehloRngBitGenerator is STABLEHLO_RNG_BIT_GENERATOR
// (rng_bit_generator.cc, rng_util.cc, #1775): stablehlo.rng_bit_generator,
// the random bits of an output of static shape from a UINT64 state, and
// the state after them. StablehloRngBitGeneratorOptions' algorithm
// THREEFRY (2) is Threefry-2x32-20 of a u64[2] state (key, counter);
// PHILOX (1) and DEFAULT (0) are Philox-4x32-10 of a u64[2] or u64[3]
// state (key, counter, and the counter's high words, or the key again).
// The output (INT32, UINT32, INT64 or UINT64) is the generator's 32-bit
// words in order, two to a 64-bit element (the low word first), and only
// the counter's low 64 bits advance.
const opStablehloRngBitGenerator = 204

func init() { opNames[opStablehloRngBitGenerator] = "STABLEHLO_RNG_BIT_GENERATOR" }

const (
	rngDefault  = 0
	rngPhilox   = 1
	rngThreefry = 2
)

func rngBitGenerator(o op, a []*Tensor, n int) ([]*Tensor, error) {
	if len(a) != 1 || a[0] == nil || n != 2 || len(o.outDefs) != 2 {
		return nil, fmt.Errorf("operator STABLEHLO_RNG_BIT_GENERATOR takes one state and gives two outputs")
	}
	state := a[0]
	if state.Type != Uint64 || len(state.Shape) != 1 {
		return nil, fmt.Errorf("operator STABLEHLO_RNG_BIT_GENERATOR: the state must be UINT64 [n], not %s %v", state.Type, state.Shape)
	}
	out := o.outDefs[1]
	words := prod(out.shape)
	wide := false
	switch out.typ {
	case Int32, Uint32:
	case Int64, Uint64:
		words *= 2
		wide = true
	default:
		return nil, fmt.Errorf("operator STABLEHLO_RNG_BIT_GENERATOR: output type %s is not INT32, UINT32, INT64 or UINT64", out.typ)
	}
	for _, d := range out.shape {
		if d < 0 {
			return nil, fmt.Errorf("operator STABLEHLO_RNG_BIT_GENERATOR: output shape %v is not static", out.shape)
		}
	}
	// The state's words, as the kernel reads the u64s: little-endian.
	sw := make([]uint32, 0, 2*len(state.I))
	for _, v := range state.I {
		sw = append(sw, uint32(v), uint32(uint64(v)>>32))
	}
	var key0, key1 uint32
	var ctr []uint32
	var gen func() []uint32
	switch alg := o.i8(4, rngDefault); alg {
	case rngThreefry:
		if len(state.I) != 2 {
			return nil, fmt.Errorf("operator STABLEHLO_RNG_BIT_GENERATOR: THREEFRY takes a u64[2] state, not u64[%d]", len(state.I))
		}
		key0, key1, ctr = sw[0], sw[1], []uint32{sw[2], sw[3]}
		gen = func() []uint32 { return threefry2x32(key0, key1, [2]uint32{ctr[0], ctr[1]}) }
	case rngPhilox, rngDefault:
		if len(state.I) != 2 && len(state.I) != 3 {
			return nil, fmt.Errorf("operator STABLEHLO_RNG_BIT_GENERATOR: PHILOX takes a u64[2] or u64[3] state, not u64[%d]", len(state.I))
		}
		hi := [2]uint32{sw[0], sw[1]}
		if len(state.I) == 3 {
			hi = [2]uint32{sw[4], sw[5]}
		}
		key0, key1, ctr = sw[0], sw[1], []uint32{sw[2], sw[3], hi[0], hi[1]}
		gen = func() []uint32 { return philox4x32(key0, key1, [4]uint32{ctr[0], ctr[1], ctr[2], ctr[3]}) }
	default:
		return nil, fmt.Errorf("operator STABLEHLO_RNG_BIT_GENERATOR: unknown algorithm %d", alg)
	}
	buf := make([]uint32, 0, words)
	for len(buf) < words {
		v := gen()
		buf = append(buf, v[:min(len(v), words-len(buf))]...)
		if ctr[0]++; ctr[0] == 0 {
			ctr[1]++
		}
	}
	// The state after: the kernel writes key and counter over the first
	// two u64s and keeps the rest.
	sw[0], sw[1], sw[2], sw[3] = key0, key1, ctr[0], ctr[1]
	ns := &Tensor{Type: Uint64, Shape: append([]int{}, state.Shape...), I: make([]int64, len(state.I))}
	for i := range ns.I {
		ns.I[i] = int64(uint64(sw[2*i]) | uint64(sw[2*i+1])<<32)
	}
	r := &Tensor{Type: out.typ, Shape: append([]int{}, out.shape...)}
	if wide {
		for i := 0; i < words; i += 2 {
			r.I = append(r.I, int64(uint64(buf[i])|uint64(buf[i+1])<<32))
		}
	} else {
		for _, w := range buf {
			if out.typ == Int32 {
				r.I = append(r.I, int64(int32(w)))
			} else {
				r.I = append(r.I, int64(w))
			}
		}
	}
	return []*Tensor{ns, r}, nil
}

// threefry2x32 is Threefry-2x32 of 20 rounds, as rng_util.cc computes it.
func threefry2x32(k0, k1 uint32, c [2]uint32) []uint32 {
	rot := [2][4]int{{13, 15, 26, 6}, {17, 29, 16, 24}}
	k2 := k0 ^ k1 ^ 0x1BD11BDA
	c[0] += k0
	c[1] += k1
	round := func(r int, ks0, ks1, b uint32) {
		for _, n := range rot[r] {
			c[0] += c[1]
			c[1] = bits.RotateLeft32(c[1], n)
			c[1] ^= c[0]
		}
		c[0] += ks0
		c[1] += ks1 + b
	}
	round(0, k1, k2, 1)
	round(1, k2, k0, 2)
	round(0, k0, k1, 3)
	round(1, k1, k2, 4)
	round(0, k2, k0, 5)
	return c[:]
}

// philox4x32 is Philox-4x32 of 10 rounds, as rng_util.cc computes it.
func philox4x32(k0, k1 uint32, c [4]uint32) []uint32 {
	for range 10 {
		h0, l0 := bits.Mul32(0xD2511F53, c[0])
		h1, l1 := bits.Mul32(0xCD9E8D57, c[2])
		c = [4]uint32{h1 ^ c[1] ^ k0, l1, h0 ^ c[3] ^ k1, l0}
		k0 += 0x9E3779B9
		k1 += 0xBB67AE85
	}
	return c[:]
}
