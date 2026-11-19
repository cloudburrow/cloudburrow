package tflitemodel

import (
	"fmt"

	flatbuffers "github.com/google/flatbuffers/go"
)

// Sparse constants and DENSIFY (#1775): a constant with SparsityParameters
// (schema.fbs: traversal_order, block_map, dim_metadata of DENSE and
// SPARSE_CSR levels) holds only its stored values, which are expanded to
// the dense tensor when the model is read, as LiteRT's
// sparsity/format_converter.cc SparseToDense does; DENSIFY, which
// TFLiteConverter's sparsity optimization puts after one, then passes it on.
const opDensify = 124

func init() { opNames[opDensify] = "DENSIFY" }

// indexVector is a SparseIndexVector union (Int32Vector, Uint16Vector,
// Uint8Vector) at the type and value slots.
func indexVector(t *flatbuffers.Table, typeSlot, valSlot int) ([]int, error) {
	kind := t.GetByteSlot(flatbuffers.VOffsetT(typeSlot), 0)
	v := sub(t, valSlot)
	if v == nil || kind == 0 {
		return nil, nil
	}
	p := field(v, 4)
	if p == 0 {
		return nil, nil
	}
	n := v.VectorLen(p)
	start := v.Vector(p)
	out := make([]int, n)
	for i := range n {
		switch kind {
		case 1:
			out[i] = int(v.GetInt32(start + flatbuffers.UOffsetT(4*i)))
		case 2:
			out[i] = int(v.GetUint16(start + flatbuffers.UOffsetT(2*i)))
		case 3:
			out[i] = int(v.GetUint8(start + flatbuffers.UOffsetT(i)))
		default:
			return nil, fmt.Errorf("a sparse index vector of type %d", kind)
		}
	}
	return out, nil
}

// sparseConstant is a sparse constant's dense tensor of shape.
func sparseConstant(typ Type, shape []int, raw []byte, sp *flatbuffers.Table) (*Tensor, error) {
	size := typeSize[typ]
	if size == 0 {
		return nil, &UnsupportedError{"sparse tensors of type " + typ.String() + " are not supported"}
	}
	vals, err := constant(typ, []int{len(raw) / size}, raw)
	if err != nil {
		return nil, err
	}
	order, blockMap := ints(sp, 4), ints(sp, 6)
	meta := tables(sp, 8)
	rank := len(shape)
	if len(order) != len(meta) || len(order) != rank+len(blockMap) {
		return nil, fmt.Errorf("its sparsity has %d traversal dimensions and %d metadata for rank %d", len(order), len(meta), rank)
	}
	dense := make([]bool, len(meta))
	sizes := make([]int, len(meta))
	segs := make([][]int, len(meta))
	idxs := make([][]int, len(meta))
	for l, md := range meta {
		dense[l] = md.GetInt8Slot(4, 0) == 0
		sizes[l] = int(md.GetInt32Slot(6, 0))
		if !dense[l] {
			if segs[l], err = indexVector(md, 8, 10); err != nil {
				return nil, err
			}
			if idxs[l], err = indexVector(md, 12, 14); err != nil {
				return nil, err
			}
		}
	}
	blockSize := make([]int, len(blockMap))
	for j := range blockMap {
		k := order[rank+j]
		if k < 0 || k >= len(meta) {
			return nil, fmt.Errorf("its sparsity's traversal order names dimension %d", k)
		}
		blockSize[j] = sizes[k]
	}
	out := alloc(vals, shape)
	if out.Len() == 0 && len(vals.F)+len(vals.I) > 0 {
		return nil, fmt.Errorf("its sparsity has values for an empty shape")
	}
	next := 0
	indices := make([]int, len(order))
	var walk func(level, prev int) error
	walk = func(level, prev int) error {
		if level == len(order) {
			orig := make([]int, rank)
			for i := range rank {
				if order[i] < 0 || order[i] >= rank {
					return fmt.Errorf("its sparsity's traversal order %v", order)
				}
				orig[order[i]] = indices[i]
			}
			for i := rank; i < len(order); i++ {
				b := order[i] - rank
				if b < 0 || b >= len(blockMap) || blockMap[b] < 0 || blockMap[b] >= rank {
					return fmt.Errorf("its sparsity's block map %v", blockMap)
				}
				d := blockMap[b]
				orig[d] = orig[d]*blockSize[b] + indices[i]
			}
			for d, c := range orig {
				if c >= shape[d] {
					return fmt.Errorf("its sparsity indexes %v outside shape %v", orig, shape)
				}
			}
			if next >= vals.Len() {
				return fmt.Errorf("its sparsity indexes more values than its %d", vals.Len())
			}
			at(out, ravel(orig, shape), vals, next)
			next++
			return nil
		}
		if dense[level] {
			for i := range sizes[level] {
				indices[level] = i
				if err := walk(level+1, prev*sizes[level]+i); err != nil {
					return err
				}
			}
			return nil
		}
		sg, ix := segs[level], idxs[level]
		if prev+1 >= len(sg) {
			return nil
		}
		for i := sg[prev]; i < sg[prev+1]; i++ {
			if i < 0 || i >= len(ix) {
				return fmt.Errorf("its sparsity's segments run past its indices")
			}
			indices[level] = ix[i]
			if err := walk(level+1, i); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(0, 0); err != nil {
		return nil, err
	}
	return out, nil
}
