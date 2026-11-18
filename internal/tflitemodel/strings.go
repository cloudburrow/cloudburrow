package tflitemodel

import (
	"encoding/binary"
	"fmt"
)

// STRING tensors (#1766): a constant's buffer is LiteRT's string layout
// (tensorflow/lite/string_util.h): an int32 count N, N + 1 int32 byte
// offsets from the buffer's start, then the strings' bytes. They are
// carried in Tensor.S, and the operators that move elements without
// arithmetic (RESHAPE, SQUEEZE, EXPAND_DIMS, TRANSPOSE, CONCATENATION,
// PACK, STRIDED_SLICE, GATHER) take them; arithmetic on one is an
// *UnsupportedError.
func stringConstant(t *Tensor, raw []byte) (*Tensor, error) {
	if len(raw) < 4 {
		return nil, fmt.Errorf("its STRING buffer holds %d bytes", len(raw))
	}
	le := binary.LittleEndian
	n := int(int32(le.Uint32(raw)))
	if n < 0 || 4*(n+2) > len(raw) {
		return nil, fmt.Errorf("its STRING buffer counts %d strings in %d bytes", n, len(raw))
	}
	if n != prod(t.Shape) {
		return nil, fmt.Errorf("its STRING buffer holds %d strings, not the %d its shape needs", n, prod(t.Shape))
	}
	t.S = make([]string, n)
	for i := range n {
		a, b := int(le.Uint32(raw[4+4*i:])), int(le.Uint32(raw[8+4*i:]))
		if a < 0 || b < a || b > len(raw) {
			return nil, fmt.Errorf("its STRING buffer's string %d runs out of it", i)
		}
		t.S[i] = string(raw[a:b])
	}
	return t, nil
}
