package syntaxnet

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/golang/snappy"
	"google.golang.org/protobuf/encoding/protowire"
)

// A TensorFlow V1 checkpoint (the format tf.train.Saver wrote in 2016,
// which SyntaxNet's parser-params and tagger-params are): a LevelDB-format
// table (tensorflow/core/lib/io/table_builder.cc) whose entry "" is a
// SavedTensorSlices holding the SavedTensorSliceMeta, and whose other
// entries are SavedTensorSlices holding one SavedSlice of a tensor each
// (tensorflow/core/util/saved_tensor_slice.proto). Only float tensors
// are kept, each assembled from its slices as a dense row-major array.

// tensor is a dense float tensor.
type tensor struct {
	shape []int
	data  []float32
}

const tableMagic = 0xdb4775248b80fb57

// readCheckpoint reads every float tensor of a V1 checkpoint.
func readCheckpoint(b []byte) (map[string]*tensor, error) {
	if len(b) < 48 || binary.LittleEndian.Uint64(b[len(b)-8:]) != tableMagic {
		return nil, errors.New("not a TensorFlow V1 checkpoint table")
	}
	footer := b[len(b)-48:]
	_, n1 := binary.Uvarint(footer)
	_, n2 := binary.Uvarint(footer[n1:])
	if n1 <= 0 || n2 <= 0 {
		return nil, errors.New("bad table footer")
	}
	idxOff, n3 := binary.Uvarint(footer[n1+n2:])
	idxSize, n4 := binary.Uvarint(footer[n1+n2+n3:])
	if n3 <= 0 || n4 <= 0 {
		return nil, errors.New("bad table footer")
	}
	index, err := tableBlock(b, idxOff, idxSize)
	if err != nil {
		return nil, err
	}
	out := map[string]*tensor{}
	shapes := map[string][]int{}
	var dataErr error
	err = blockEntries(index, func(_, v []byte) {
		off, a := binary.Uvarint(v)
		size, c := binary.Uvarint(v[a:])
		if a <= 0 || c <= 0 {
			dataErr = errors.New("bad block handle")
			return
		}
		blk, err := tableBlock(b, off, size)
		if err != nil {
			dataErr = err
			return
		}
		if err := blockEntries(blk, func(k, v []byte) {
			if dataErr != nil {
				return
			}
			if len(k) == 0 {
				dataErr = readMeta(v, shapes)
				return
			}
			dataErr = readSlice(v, shapes, out)
		}); err != nil {
			dataErr = err
		}
	})
	if err != nil {
		return nil, err
	}
	if dataErr != nil {
		return nil, dataErr
	}
	return out, nil
}

// tableBlock is the block at off, uncompressed.
func tableBlock(b []byte, off, size uint64) ([]byte, error) {
	if off+size+5 > uint64(len(b)) {
		return nil, errors.New("block out of range")
	}
	blk := b[off : off+size]
	switch b[off+size] {
	case 0:
		return blk, nil
	case 1:
		return snappy.Decode(nil, blk)
	}
	return nil, fmt.Errorf("unknown block compression %d", b[off+size])
}

// blockEntries calls f with each key and value of a block.
func blockEntries(blk []byte, f func(k, v []byte)) error {
	if len(blk) < 4 {
		return errors.New("short block")
	}
	nr := binary.LittleEndian.Uint32(blk[len(blk)-4:])
	end := len(blk) - 4 - 4*int(nr)
	if end < 0 {
		return errors.New("bad block restarts")
	}
	var key []byte
	for p := 0; p < end; {
		shared, a := binary.Uvarint(blk[p:])
		nonShared, c := binary.Uvarint(blk[p+a:])
		vlen, d := binary.Uvarint(blk[p+a+c:])
		if a <= 0 || c <= 0 || d <= 0 {
			return errors.New("bad block entry")
		}
		p += a + c + d
		if int(shared) > len(key) || p+int(nonShared)+int(vlen) > end {
			return errors.New("bad block entry")
		}
		key = append(key[:shared], blk[p:p+int(nonShared)]...)
		p += int(nonShared)
		f(key, blk[p:p+int(vlen)])
		p += int(vlen)
	}
	return nil
}

// fields calls f with each field of a protobuf message.
func fields(b []byte, f func(num protowire.Number, typ protowire.Type, v []byte, x uint64)) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]
		switch typ {
		case protowire.BytesType:
			v, m := protowire.ConsumeBytes(b)
			if m < 0 {
				return protowire.ParseError(m)
			}
			f(num, typ, v, 0)
			b = b[m:]
		case protowire.VarintType:
			x, m := protowire.ConsumeVarint(b)
			if m < 0 {
				return protowire.ParseError(m)
			}
			f(num, typ, nil, x)
			b = b[m:]
		default:
			m := protowire.ConsumeFieldValue(num, typ, b)
			if m < 0 {
				return protowire.ParseError(m)
			}
			b = b[m:]
		}
	}
	return nil
}

// readMeta reads SavedTensorSlices.meta: each float tensor's shape.
func readMeta(v []byte, shapes map[string][]int) error {
	return fields(v, func(num protowire.Number, _ protowire.Type, meta []byte, _ uint64) {
		if num != 1 {
			return
		}
		_ = fields(meta, func(num protowire.Number, _ protowire.Type, t []byte, _ uint64) {
			if num != 1 { // SavedTensorSliceMeta.tensor
				return
			}
			var name string
			var shape []int
			var dtype uint64
			_ = fields(t, func(num protowire.Number, _ protowire.Type, v []byte, x uint64) {
				switch num {
				case 1:
					name = string(v)
				case 2: // TensorShapeProto.dim
					_ = fields(v, func(num protowire.Number, _ protowire.Type, d []byte, _ uint64) {
						if num != 2 {
							return
						}
						_ = fields(d, func(num protowire.Number, _ protowire.Type, _ []byte, x uint64) {
							if num == 1 {
								shape = append(shape, int(x))
							}
						})
					})
				case 3:
					dtype = x
				}
			})
			if dtype == 1 { // DT_FLOAT
				if shape == nil {
					shape = []int{}
				}
				shapes[name] = shape
			}
		})
	})
}

// readSlice reads SavedTensorSlices.data into its tensor. SyntaxNet's
// variables are saved whole, one slice each.
func readSlice(v []byte, shapes map[string][]int, out map[string]*tensor) error {
	var err error
	ferr := fields(v, func(num protowire.Number, _ protowire.Type, s []byte, _ uint64) {
		if num != 2 {
			return
		}
		var name string
		var vals []float32
		_ = fields(s, func(num protowire.Number, typ protowire.Type, b []byte, _ uint64) {
			switch num {
			case 1:
				name = string(b)
			case 3: // TensorProto
				_ = fields(b, func(num protowire.Number, typ protowire.Type, p []byte, _ uint64) {
					if num == 5 && typ == protowire.BytesType { // float_val, packed
						for i := 0; i+4 <= len(p); i += 4 {
							vals = append(vals, math.Float32frombits(binary.LittleEndian.Uint32(p[i:])))
						}
					}
				})
			}
		})
		shape, ok := shapes[name]
		if !ok {
			return
		}
		n := 1
		for _, d := range shape {
			n *= d
		}
		if len(vals) != n {
			err = fmt.Errorf("tensor %s: %d values for shape %v (sliced tensors are not read)", name, len(vals), shape)
			return
		}
		out[name] = &tensor{shape: shape, data: vals}
	})
	if ferr != nil {
		return ferr
	}
	return err
}
