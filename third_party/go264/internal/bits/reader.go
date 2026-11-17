package bits

import "errors"

var (
	ErrOverrun     = errors.New("go264/bits: read past end of buffer")
	ErrBitCount    = errors.New("go264/bits: bit count out of range")
	ErrInvalidCode = errors.New("go264/bits: malformed exponential-Golomb code")
	ErrRange       = errors.New("go264/bits: value out of representable range")
	ErrTrailing    = errors.New("go264/bits: missing rbsp_stop_one_bit")
	ErrAlignment   = errors.New("go264/bits: unexpected non-zero alignment bit")
)

type Reader struct {
	data    []byte
	pos     int
	end     int
	lastOne int
}

func NewReader(data []byte) *Reader {
	r := &Reader{}
	r.Reset(data)
	return r
}

func (r *Reader) Reset(data []byte) {
	r.data = data
	r.pos = 0
	r.end = len(data) * 8
	r.lastOne = -2
}

func (r *Reader) BitPos() int { return r.pos }

func (r *Reader) BitsLeft() int { return r.end - r.pos }

func (r *Reader) ByteAligned() bool { return r.pos&7 == 0 }

func (r *Reader) Seek(bitPos int) error {
	if bitPos < 0 || bitPos > r.end {
		return ErrOverrun
	}
	r.pos = bitPos
	return nil
}

func (r *Reader) Skip(n int) error {
	if n < 0 || r.pos+n > r.end {
		return ErrOverrun
	}
	r.pos += n
	return nil
}

func (r *Reader) ReadBits(n int) (uint32, error) {
	if n < 0 || n > 32 {
		return 0, ErrBitCount
	}
	if n == 0 {
		return 0, nil
	}
	if r.pos+n > r.end {
		return 0, ErrOverrun
	}
	byteIdx := r.pos >> 3
	bitOff := r.pos & 7
	span := (bitOff + n + 7) >> 3
	var acc uint64
	for i := 0; i < span; i++ {
		acc = acc<<8 | uint64(r.data[byteIdx+i])
	}
	acc >>= uint(span*8 - bitOff - n)
	r.pos += n
	if n == 32 {
		return uint32(acc), nil
	}
	return uint32(acc & (1<<uint(n) - 1)), nil
}

func (r *Reader) PeekBits(n int) (uint32, error) {
	save := r.pos
	v, err := r.ReadBits(n)
	r.pos = save
	return v, err
}

func (r *Reader) ReadBit() (uint32, error) {
	if r.pos >= r.end {
		return 0, ErrOverrun
	}
	b := r.data[r.pos>>3] >> uint(7-r.pos&7) & 1
	r.pos++
	return uint32(b), nil
}

func (r *Reader) ReadFlag() (bool, error) {
	b, err := r.ReadBit()
	return b == 1, err
}

func (r *Reader) ReadUE() (uint32, error) {
	v, err := r.readUE64()
	if err != nil {
		return 0, err
	}
	if v > 0xFFFFFFFF {
		return 0, ErrRange
	}
	return uint32(v), nil
}

func (r *Reader) readUE64() (uint64, error) {
	zeros := 0
	for {
		b, err := r.ReadBit()
		if err != nil {
			return 0, err
		}
		if b == 1 {
			break
		}
		zeros++
		if zeros > 32 {
			return 0, ErrInvalidCode
		}
	}
	if zeros == 0 {
		return 0, nil
	}
	rest, err := r.ReadBits(zeros)
	if err != nil {
		return 0, err
	}
	return uint64(1)<<uint(zeros) - 1 + uint64(rest), nil
}

func (r *Reader) ReadSE() (int32, error) {
	k, err := r.readUE64()
	if err != nil {
		return 0, err
	}
	if k == 0 {
		return 0, nil
	}
	var v int64
	if k&1 == 1 {
		v = int64(k+1) / 2
	} else {
		v = -int64(k / 2)
	}
	if v < -2147483647 || v > 2147483647 {
		return 0, ErrRange
	}
	return int32(v), nil
}

func (r *Reader) ReadTE(max uint32) (uint32, error) {
	if max == 0 {
		return 0, nil
	}
	if max == 1 {
		b, err := r.ReadBit()
		if err != nil {
			return 0, err
		}
		return 1 - b, nil
	}
	return r.ReadUE()
}

func (r *Reader) lastSetBit() int {
	if r.lastOne != -2 {
		return r.lastOne
	}
	r.lastOne = -1
	for i := len(r.data) - 1; i >= 0; i-- {
		b := r.data[i]
		if b == 0 {
			continue
		}
		for j := 0; j < 8; j++ {
			if b>>uint(j)&1 == 1 {
				r.lastOne = i*8 + (7 - j)
				break
			}
		}
		break
	}
	return r.lastOne
}

func (r *Reader) MoreRBSPData() bool {
	if r.pos >= r.end {
		return false
	}
	return r.pos < r.lastSetBit()
}

func (r *Reader) ReadRBSPTrailingBits() error {
	b, err := r.ReadBit()
	if err != nil {
		return err
	}
	if b != 1 {
		return ErrTrailing
	}
	for !r.ByteAligned() {
		z, err := r.ReadBit()
		if err != nil {
			return err
		}
		if z != 0 {
			return ErrAlignment
		}
	}
	return nil
}
