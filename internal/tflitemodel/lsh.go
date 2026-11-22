package tflitemodel

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
)

// opLSHProjection is LSH_PROJECTION (lsh_projection.cc, #1775): the
// locality-sensitive hash of an input's rows. Its inputs are the seeds
// hash FLOAT32 [num_hash, num_bits], the input (any numeric type, its
// first dimension the rows) and an optional weight FLOAT32 [rows]. Each
// seed's bit is the sign of the rows' (weighted) sum of
// farmhash::Fingerprint64 of the seed's four bytes followed by the row's
// bytes. LSHProjectionOptions' type SPARSE (1) answers INT32 [num_hash],
// each hash's num_bits bits read as a number plus i << num_bits; DENSE (2)
// answers INT32 [num_hash * num_bits], the bits themselves.
const opLSHProjection = 15

func init() { opNames[opLSHProjection] = "LSH_PROJECTION" }

const (
	lshSparse = 1
	lshDense  = 2
)

func lshProjection(o op, hash, in, weight *Tensor) (*Tensor, error) {
	if hash.Type != Float32 || len(hash.Shape) != 2 {
		return nil, fmt.Errorf("operator LSH_PROJECTION: hash must be FLOAT32 [num_hash, num_bits], not %s %v", hash.Type, hash.Shape)
	}
	numHash, numBits := hash.Shape[0], hash.Shape[1]
	if numBits > 32 {
		return nil, fmt.Errorf("operator LSH_PROJECTION: num_bits %d is more than 32", numBits)
	}
	if len(in.Shape) < 1 || in.Shape[0] < 1 {
		return nil, fmt.Errorf("operator LSH_PROJECTION: input of shape %v has no rows", in.Shape)
	}
	rows := in.Shape[0]
	width, ok := lshItemBytes[in.Type]
	if !ok {
		return nil, &UnsupportedError{"operator LSH_PROJECTION on " + in.Type.String() + " tensors is not supported"}
	}
	if weight != nil && (weight.Type != Float32 || weight.Len() != rows) {
		return nil, fmt.Errorf("operator LSH_PROJECTION: weight must be FLOAT32 [%d], not %s %v", rows, weight.Type, weight.Shape)
	}
	per := in.Len() / rows
	raw := make([]byte, in.Len()*width)
	for i := range in.Len() {
		p := raw[i*width:]
		switch in.Type {
		case Float32:
			binary.LittleEndian.PutUint32(p, math.Float32bits(in.F[i]))
		case Bool:
			if in.B[i] {
				p[0] = 1
			}
		default:
			v := uint64(in.I[i])
			for b := range width {
				p[b] = byte(v >> (8 * b))
			}
		}
	}
	key := make([]byte, 4+per*width)
	bit := func(seed float32) int32 {
		score := 0.0
		binary.LittleEndian.PutUint32(key, math.Float32bits(seed))
		for r := range rows {
			copy(key[4:], raw[r*per*width:(r+1)*per*width])
			v := float64(int64(fingerprint64(key)))
			if weight == nil {
				score += v
			} else {
				score += float64(weight.F[r]) * v
			}
		}
		if score > 0 {
			return 1
		}
		return 0
	}
	r := &Tensor{Type: Int32}
	switch t := o.i8(4, 0); t {
	case lshSparse:
		r.Shape = []int{numHash}
		for i := range numHash {
			var sig int32
			for j := range numBits {
				sig = sig<<1 | bit(hash.F[i*numBits+j])
			}
			r.I = append(r.I, int64(sig+int32(i)*(1<<numBits)))
		}
	case lshDense:
		r.Shape = []int{numHash * numBits}
		for _, s := range hash.F[:numHash*numBits] {
			r.I = append(r.I, int64(bit(s)))
		}
	default:
		return nil, fmt.Errorf("operator LSH_PROJECTION: projection type %d is neither SPARSE nor DENSE", t)
	}
	return r, nil
}

// lshItemBytes is each type's element size, as LSH_PROJECTION hashes its
// bytes; FLOAT16 and FLOAT64 are held as float32 here, so their bytes
// are not the model's.
var lshItemBytes = map[Type]int{Float32: 4, Int32: 4, Uint32: 4, Int64: 8, Uint64: 8, Int16: 2, Uint16: 2, Int8: 1, Uint8: 1, Bool: 1}

// fingerprint64 is farmhash::Fingerprint64 (Google's FarmHash, MIT), the
// hash LSH_PROJECTION's kernel takes: farmhashna's Hash64.
func fingerprint64(s []byte) uint64 {
	n := len(s)
	switch {
	case n <= 16:
		return farmLen0to16(s)
	case n <= 32:
		return farmLen17to32(s)
	case n <= 64:
		return farmLen33to64(s)
	}
	x := uint64(81) // the seed
	y := x*farmK1 + 113
	z := farmShiftMix(y*farmK2+113) * farmK2
	var v1, v2, w1, w2 uint64
	x = x*farmK2 + f64(s, 0)
	end := ((n - 1) / 64) * 64
	last64 := end + ((n - 1) & 63) - 63
	for p := 0; p != end; p += 64 {
		x = bits.RotateLeft64(x+y+v1+f64(s, p+8), -37) * farmK1
		y = bits.RotateLeft64(y+v2+f64(s, p+48), -42) * farmK1
		x ^= w2
		y += v1 + f64(s, p+40)
		z = bits.RotateLeft64(z+w1, -33) * farmK1
		v1, v2 = farmWeak32(s, p, v2*farmK1, x+w1)
		w1, w2 = farmWeak32(s, p+32, z+w2, y+f64(s, p+16))
		z, x = x, z
	}
	mul := farmK1 + ((z & 0xff) << 1)
	p := last64
	w1 += uint64((n - 1) & 63)
	v1 += w1
	w1 += v1
	x = bits.RotateLeft64(x+y+v1+f64(s, p+8), -37) * mul
	y = bits.RotateLeft64(y+v2+f64(s, p+48), -42) * mul
	x ^= w2 * 9
	y += v1*9 + f64(s, p+40)
	z = bits.RotateLeft64(z+w1, -33) * mul
	v1, v2 = farmWeak32(s, p, v2*mul, x+w1)
	w1, w2 = farmWeak32(s, p+32, z+w2, y+f64(s, p+16))
	z, x = x, z
	return farmLen16(farmLen16(v1, w1, mul)+farmShiftMix(y)*farmK0+z, farmLen16(v2, w2, mul)+x, mul)
}

const (
	farmK0 = 0xc3a5c85c97cb3127
	farmK1 = 0xb492b66fbe98f273
	farmK2 = 0x9ae16a3b2f90404f
)

func f64(s []byte, p int) uint64 { return binary.LittleEndian.Uint64(s[p:]) }
func f32(s []byte, p int) uint64 { return uint64(binary.LittleEndian.Uint32(s[p:])) }

func farmShiftMix(v uint64) uint64 { return v ^ v>>47 }

func farmLen16(u, v, mul uint64) uint64 {
	a := (u ^ v) * mul
	a ^= a >> 47
	b := (v ^ a) * mul
	b ^= b >> 47
	return b * mul
}

func farmLen0to16(s []byte) uint64 {
	n := uint64(len(s))
	switch {
	case n >= 8:
		mul := farmK2 + n*2
		a := f64(s, 0) + farmK2
		b := f64(s, len(s)-8)
		c := bits.RotateLeft64(b, -37)*mul + a
		d := (bits.RotateLeft64(a, -25) + b) * mul
		return farmLen16(c, d, mul)
	case n >= 4:
		mul := farmK2 + n*2
		a := f32(s, 0)
		return farmLen16(n+(a<<3), f32(s, len(s)-4), mul)
	case n > 0:
		a, b, c := uint64(s[0]), uint64(s[n>>1]), uint64(s[n-1])
		y := uint32(a + b<<8)
		z := uint32(n + c<<2)
		return farmShiftMix(uint64(y)*farmK2^uint64(z)*farmK0) * farmK2
	}
	return farmK2
}

func farmLen17to32(s []byte) uint64 {
	n := uint64(len(s))
	mul := farmK2 + n*2
	a := f64(s, 0) * farmK1
	b := f64(s, 8)
	c := f64(s, len(s)-8) * mul
	d := f64(s, len(s)-16) * farmK2
	return farmLen16(bits.RotateLeft64(a+b, -43)+bits.RotateLeft64(c, -30)+d, a+bits.RotateLeft64(b+farmK2, -18)+c, mul)
}

func farmLen33to64(s []byte) uint64 {
	n := uint64(len(s))
	mul := farmK2 + n*2
	a := f64(s, 0) * farmK2
	b := f64(s, 8)
	c := f64(s, len(s)-8) * mul
	d := f64(s, len(s)-16) * farmK2
	y := bits.RotateLeft64(a+b, -43) + bits.RotateLeft64(c, -30) + d
	z := farmLen16(y, a+bits.RotateLeft64(b+farmK2, -18)+c, mul)
	e := f64(s, 16) * mul
	f := f64(s, 24)
	g := (y + f64(s, len(s)-32)) * mul
	h := (z + f64(s, len(s)-24)) * mul
	return farmLen16(bits.RotateLeft64(e+f, -43)+bits.RotateLeft64(g, -30)+h, e+bits.RotateLeft64(f+a, -18)+g, mul)
}

// farmWeak32 is WeakHashLen32WithSeeds of the 32 bytes at p.
func farmWeak32(s []byte, p int, a, b uint64) (uint64, uint64) {
	w, x, y, z := f64(s, p), f64(s, p+8), f64(s, p+16), f64(s, p+24)
	a += w
	b = bits.RotateLeft64(b+a+z, -21)
	c := a
	a += x
	a += y
	b += bits.RotateLeft64(a, -44)
	return a + z, b + c
}
