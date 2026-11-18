package bits

import mathbits "math/bits"

type Writer struct {
	buf   []byte
	cache uint64
	n     uint
	total int
	err   error
}

func NewWriter() *Writer { return &Writer{} }

func NewWriterSize(capacity int) *Writer {
	return &Writer{buf: make([]byte, 0, capacity)}
}

func (w *Writer) Reset() {
	w.buf = w.buf[:0]
	w.cache = 0
	w.n = 0
	w.total = 0
	w.err = nil
}

func (w *Writer) Err() error { return w.err }

func (w *Writer) fail(err error) {
	if w.err == nil {
		w.err = err
	}
}

func (w *Writer) BitsWritten() int { return w.total }

func (w *Writer) ByteAligned() bool { return w.total&7 == 0 }

func (w *Writer) WriteBits(v uint32, n int) {
	if w.err != nil {
		return
	}
	if n < 0 || n > 32 {
		w.fail(ErrBitCount)
		return
	}
	if n == 0 {
		return
	}
	masked := uint64(v)
	if n < 32 {
		masked &= 1<<uint(n) - 1
	}
	w.cache = w.cache<<uint(n) | masked
	w.n += uint(n)
	w.total += n
	for w.n >= 8 {
		w.n -= 8
		w.buf = append(w.buf, byte(w.cache>>w.n))
	}
}

func (w *Writer) WriteBit(b uint32) { w.WriteBits(b&1, 1) }

func (w *Writer) WriteFlag(f bool) {
	if f {
		w.WriteBit(1)
		return
	}
	w.WriteBit(0)
}

func (w *Writer) WriteUE(v uint32) {
	c := uint64(v) + 1
	n := mathbits.Len64(c)
	w.WriteBits(0, n-1)
	if n > 32 {
		w.WriteBit(1)
		w.WriteBits(uint32(c), n-1)
		return
	}
	w.WriteBits(uint32(c), n)
}

func (w *Writer) WriteSE(v int32) {
	if v == -2147483648 {
		w.fail(ErrRange)
		return
	}
	var k uint32
	if v > 0 {
		k = uint32(v)*2 - 1
	} else {
		k = uint32(-v) * 2
	}
	w.WriteUE(k)
}

func (w *Writer) WriteTE(v, max uint32) {
	switch {
	case max == 0:
	case max == 1:
		w.WriteBit(1 - v&1)
	default:
		w.WriteUE(v)
	}
}

func (w *Writer) AlignZero() {
	for w.err == nil && !w.ByteAligned() {
		w.WriteBit(0)
	}
}

func (w *Writer) AlignOne() {
	for w.err == nil && !w.ByteAligned() {
		w.WriteBit(1)
	}
}

func (w *Writer) WriteRBSPTrailingBits() {
	w.WriteBit(1)
	w.AlignZero()
}

func (w *Writer) Bytes() []byte {
	if w.n == 0 {
		return w.buf
	}
	out := make([]byte, len(w.buf), len(w.buf)+1)
	copy(out, w.buf)
	return append(out, byte(w.cache<<(8-w.n)))
}

func (w *Writer) AppendBits(src *Writer, from int) {
	if src.err != nil {
		w.fail(src.err)
		return
	}
	full := len(src.buf) * 8
	i := max(from, 0)
	for ; i < src.total && i&7 != 0; i++ {
		w.WriteBit(uint32(src.bitAt(i)))
	}
	for ; i+32 <= full; i += 32 {
		b := src.buf[i>>3:]
		w.WriteBits(uint32(b[0])<<24|uint32(b[1])<<16|uint32(b[2])<<8|uint32(b[3]), 32)
	}
	for ; i+8 <= full; i += 8 {
		w.WriteBits(uint32(src.buf[i>>3]), 8)
	}
	for ; i < src.total; i++ {
		w.WriteBit(uint32(src.bitAt(i)))
	}
}

func (w *Writer) bitAt(i int) byte {
	full := len(w.buf) * 8
	if i < full {
		return w.buf[i>>3] >> (7 - uint(i&7)) & 1
	}
	j := uint(i - full)
	return byte(w.cache>>(w.n-1-j)) & 1
}
