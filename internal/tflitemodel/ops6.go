package tflitemodel

import (
	"encoding/binary"
	"fmt"
	"math"
)

// BITCAST and EMBEDDING_LOOKUP_SPARSE (#1775), as LiteRT's kernels
// (bitcast.cc, embedding_lookup_sparse.cc) compute them.
const (
	opEmbeddingLookupSparse = 33
	opBitcast               = 159
)

func init() {
	opNames[opEmbeddingLookupSparse] = "EMBEDDING_LOOKUP_SPARSE"
	opNames[opBitcast] = "BITCAST"
}

var typeSize = map[Type]int{Float32: 4, Float16: 2, Float64: 8, Int8: 1, Uint8: 1, Int16: 2, Uint16: 2, Int32: 4, Uint32: 4,
	Int64: 8, Uint64: 8, Bool: 1}

// bytesOf is a tensor's elements as their little-endian bytes.
func bytesOf(t *Tensor) ([]byte, error) {
	size := typeSize[t.Type]
	if size == 0 {
		return nil, &UnsupportedError{"BITCAST of " + t.Type.String() + " tensors is not supported"}
	}
	n := t.Len()
	b := make([]byte, n*size)
	le := binary.LittleEndian
	for i := range n {
		p := b[i*size:]
		switch t.Type {
		case Float32:
			le.PutUint32(p, math.Float32bits(t.F[i]))
		case Float64:
			le.PutUint64(p, math.Float64bits(float64(t.F[i])))
		case Float16:
			le.PutUint16(p, toHalf(t.F[i]))
		case Bool:
			if t.B[i] {
				p[0] = 1
			}
		default:
			v := uint64(t.I[i])
			for k := range size {
				p[k] = byte(v >> (8 * k))
			}
		}
	}
	return b, nil
}

// toHalf is v as IEEE 754 binary16, rounded to nearest even (a FLOAT16
// tensor's values are binary16 values, so this is exact for them).
func toHalf(v float32) uint16 {
	b := math.Float32bits(v)
	sign := uint16(b>>16) & 0x8000
	exp := int(b>>23&0xff) - 127 + 15
	frac := b & 0x7fffff
	switch {
	case b&0x7fffffff == 0:
		return sign
	case int(b>>23&0xff) == 0xff:
		if frac != 0 {
			return sign | 0x7e00
		}
		return sign | 0x7c00
	case exp >= 31:
		return sign | 0x7c00
	case exp <= 0:
		if exp < -10 {
			return sign
		}
		frac |= 0x800000
		shift := uint(14 - exp)
		h := frac >> shift
		if rem := frac & (1<<shift - 1); rem > 1<<(shift-1) || rem == 1<<(shift-1) && h&1 == 1 {
			h++
		}
		return sign | uint16(h)
	}
	h := uint32(exp)<<10 | frac>>13
	if rem := frac & 0x1fff; rem > 0x1000 || rem == 0x1000 && h&1 == 1 {
		h++
	}
	return sign | uint16(h)
}

// bitcast is BITCAST: x's bytes as the output tensor's type; a smaller type
// adds an innermost dimension, a larger one takes the innermost away.
func bitcast(x *Tensor, to Type) (*Tensor, error) {
	in, out := typeSize[x.Type], typeSize[to]
	if out == 0 {
		return nil, &UnsupportedError{"BITCAST to " + to.String() + " is not supported"}
	}
	raw, err := bytesOf(x)
	if err != nil {
		return nil, err
	}
	shape := append([]int{}, x.Shape...)
	switch {
	case in > out:
		shape = append(shape, in/out)
	case in < out:
		if len(shape) == 0 || shape[len(shape)-1] != out/in {
			return nil, fmt.Errorf("operator BITCAST: the innermost dimension of %v must be %d", x.Shape, out/in)
		}
		shape = shape[:len(shape)-1]
	}
	return constant(to, shape, raw)
}

// embeddingLookupSparse is EMBEDDING_LOOKUP_SPARSE: ids [n], indices [n, k],
// dense_shape [k], weights [n] and the embeddings value [rows, ...],
// combined per bucket (EmbeddingLookupSparseOptions' combiner: SUM, MEAN,
// SQRTN).
func embeddingLookupSparse(o op, a []*Tensor) (*Tensor, error) {
	for i := range 5 {
		if i >= len(a) || a[i] == nil {
			return nil, fmt.Errorf("operator %s takes 5 inputs", o.name)
		}
	}
	ids, idx, dense, w, val := axes(a[0]), a[1], axes(a[2]), floatsOf(a[3]), a[4]
	if len(idx.Shape) != 2 || idx.Shape[0] != len(ids) || idx.Shape[1] != len(dense) || len(w) != len(ids) || len(val.Shape) < 1 || !val.Type.IsFloat() {
		return nil, fmt.Errorf("operator %s: ids %d, indices %v, dense_shape %v, weights %d, value %v", o.name, len(ids), idx.Shape, dense, len(w), val.Shape)
	}
	k := len(dense)
	rows := val.Shape[0]
	emb := val.Len() / max(rows, 1)
	shape := append(append([]int{}, dense[:k-1]...), val.Shape[1:]...)
	r := &Tensor{Type: Float32, Shape: shape, F: make([]float32, prod(shape))}
	comb := o.i8(4, 0)
	ix := axes(idx)
	cur, num := 0, 0
	var total, squares float32
	finalize := func() {
		if comb == 0 || num == 0 {
			return
		}
		mult := total
		if comb == 2 {
			mult = float32(math.Sqrt(float64(squares)))
		}
		for j := range emb {
			r.F[cur+j] /= mult
		}
	}
	for i, id := range ids {
		if id < 0 || id >= rows {
			return nil, fmt.Errorf("Embedding Lookup Sparse: index out of bounds. Got %d, and bounds are [0, %d]", id, rows-1)
		}
		bucket, stride := 0, 1
		for d := k - 2; d >= 0; d-- {
			bucket += ix[i*k+d] * stride
			stride *= dense[d]
		}
		at := bucket * emb
		if at < 0 || at+emb > len(r.F) {
			return nil, fmt.Errorf("operator %s: index %v is outside dense_shape %v", o.name, ix[i*k:i*k+k], dense)
		}
		if at != cur {
			finalize()
			num, total, squares, cur = 0, 0, 0, at
		}
		num++
		total += w[i]
		squares += w[i] * w[i]
		for j := range emb {
			r.F[cur+j] += val.f(id*emb+j) * w[i]
		}
	}
	finalize()
	return r, nil
}
