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

// opMultinomial is MULTINOMIAL (random_ops.cc): num_samples class indices
// per row of logits, from TensorFlow's Philox seeded as RANDOM_UNIFORM's
// is, each sample a double from two consecutive 32-bit draws scaled to the
// row's unnormalized CDF (non-finite logits never drawn).
const opMultinomial = 149

func init() { opNames[opMultinomial] = "MULTINOMIAL" }

func multinomial(o op, x, ns *Tensor) (*Tensor, error) {
	if len(x.Shape) != 2 || !x.Type.IsFloat() || ns == nil || ns.Len() != 1 {
		return nil, fmt.Errorf("operator MULTINOMIAL takes float logits [batch, classes] and one num_samples")
	}
	n := int(ns.f(0))
	if n < 0 {
		return nil, fmt.Errorf("operator MULTINOMIAL: num_samples %d", n)
	}
	to := o.outType
	if to != Int32 && to != Int64 {
		to = Int64
	}
	p := seeded(o)
	var buf []uint32
	next := func() uint32 {
		if len(buf) == 0 {
			s := p.next()
			buf = s[:]
		}
		v := buf[0]
		buf = buf[1:]
		return v
	}
	batch, classes := x.Shape[0], x.Shape[1]
	r := &Tensor{Type: to, Shape: []int{batch, n}, I: make([]int64, batch*n)}
	cdf := make([]float64, classes)
	for b := range batch {
		row := x.F[b*classes : (b+1)*classes]
		mx := float32(-math.MaxFloat32)
		for _, v := range row {
			if !math.IsInf(float64(v), 0) && !math.IsNaN(float64(v)) {
				mx = max(mx, v)
			}
		}
		total := 0.0
		for j, v := range row {
			if !math.IsInf(float64(v), 0) && !math.IsNaN(float64(v)) {
				total += math.Exp(float64(v) - float64(mx))
			}
			cdf[j] = total
		}
		for j := range n {
			x0, x1 := next(), next()
			u := math.Float64frombits(1023<<52|uint64(x0&0xfffff)<<32|uint64(x1)) - 1
			find := u * total
			k := 0
			for k < classes && cdf[k] <= find {
				k++
			}
			r.I[b*n+j] = int64(k)
		}
	}
	return r, nil
}

// opSkipGram is SKIP_GRAM (skip_gram.cc): the skip-grams of a one-string
// input's space-separated words (SkipGramOptions ngram_size,
// max_skip_size, include_all_ngrams), in the kernel's enumeration order.
const opSkipGram = 30

func init() { opNames[opSkipGram] = "SKIP_GRAM" }

func isSpace(c byte) bool { return c == ' ' || c >= '\t' && c <= '\r' }

func skipGram(o op, x *Tensor) (*Tensor, error) {
	if x.S == nil || len(x.S) != 1 {
		return nil, fmt.Errorf("operator SKIP_GRAM takes one STRING")
	}
	n, skip, all := int(o.i32(4, 0)), int(o.i32(6, 0)), o.bool(8)
	if n < 1 {
		return nil, fmt.Errorf("operator SKIP_GRAM: ngram_size %d", n)
	}
	s := x.S[0]
	var words []string
	prev := 0
	for i := 1; i < len(s); i++ {
		if isSpace(s[i]) {
			if i > prev && !isSpace(s[prev]) {
				words = append(words, s[prev:i])
			}
			prev = i + 1
		}
	}
	if len(s) > prev {
		words = append(words, s[prev:])
	}
	r := &Tensor{Type: String, S: []string{}}
	if len(words) < n {
		r.Shape = []int{0}
		return r, nil
	}
	stack := make([]int, n)
	idx := 1
	step := func() bool {
		if idx < n && stack[idx]+1 < len(words) {
			return idx == 0 || stack[idx]-stack[idx-1] <= skip
		}
		return false
	}
	for idx >= 0 {
		if step() {
			stack[idx]++
			idx++
			if idx < n {
				stack[idx] = stack[idx-1]
			}
			continue
		}
		if idx > 0 && (idx == n || all && idx <= n) {
			g := words[stack[0]]
			for k := 1; k < idx; k++ {
				g += " " + words[stack[k]]
			}
			r.S = append(r.S, g)
		}
		idx--
	}
	r.Shape = []int{len(r.S)}
	return r, nil
}
