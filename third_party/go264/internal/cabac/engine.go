package cabac

import (
	"errors"

	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/bits"
)

var (
	ErrNotAligned = errors.New("go264/cabac: slice data does not start byte aligned")
	ErrAlignment  = errors.New("go264/cabac: cabac_alignment_one_bit is not one")
	ErrInitIDC    = errors.New("go264/cabac: cabac_init_idc out of range")
)

const NumContexts = 1024

type context struct {
	state uint8
	mps   uint8
}

type Decoder struct {
	r          *bits.Reader
	codIRange  uint32
	codIOffset uint32
	overrun    int
	overflow   bool
	ctx        [NumContexts]context
}

func clip3(lo, hi, v int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func (d *Decoder) initContexts(table *[1024][2]int8, sliceQPY int) {
	qp := clip3(0, 51, sliceQPY)
	for i := 0; i < NumContexts; i++ {
		m := int(table[i][0])
		n := int(table[i][1])
		pre := clip3(1, 126, ((m*qp)>>4)+n)
		if pre <= 63 {
			d.ctx[i].state = uint8(63 - pre)
			d.ctx[i].mps = 0
		} else {
			d.ctx[i].state = uint8(pre - 64)
			d.ctx[i].mps = 1
		}
	}
}

func (d *Decoder) Overrun() int { return d.overrun }

func (d *Decoder) LevelOverflow() bool { return d.overflow }

func (d *Decoder) Init(r *bits.Reader, sliceQPY int, intra bool, initIDC uint32) error {
	d.overrun = 0
	d.overflow = false
	if !r.ByteAligned() {
		if err := d.consumeAlignmentBits(r); err != nil {
			return err
		}
	}
	if intra {
		d.initContexts(&contextInitI, sliceQPY)
	} else {
		if initIDC > 2 {
			return ErrInitIDC
		}
		d.initContexts(contextInitPB[initIDC], sliceQPY)
	}
	return d.Restart(r)
}

func (d *Decoder) Restart(r *bits.Reader) error {
	d.r = r
	d.codIRange = 510
	v, err := r.ReadBits(9)
	if err != nil {
		return err
	}
	d.codIOffset = v
	return nil
}

func (d *Decoder) consumeAlignmentBits(r *bits.Reader) error {
	for !r.ByteAligned() {
		b, err := r.ReadBit()
		if err != nil {
			return err
		}
		if b != 1 {
			return ErrAlignment
		}
	}
	return nil
}

func (d *Decoder) readBit() uint32 {
	b, err := d.r.ReadBit()
	if err != nil {
		d.overrun++
		return 0
	}
	return b
}

func (d *Decoder) renormalize() {
	for d.codIRange < 256 {
		d.codIRange <<= 1
		d.codIOffset = d.codIOffset<<1 | d.readBit()
	}
}

func (d *Decoder) DecodeDecision(ctxIdx int) uint32 {
	c := &d.ctx[ctxIdx]
	q := d.codIRange >> 6 & 3
	lps := uint32(rangeTabLPS[c.state][q])
	d.codIRange -= lps

	var bin uint32
	if d.codIOffset >= d.codIRange {
		bin = uint32(1 - c.mps)
		d.codIOffset -= d.codIRange
		d.codIRange = lps
		if c.state == 0 {
			c.mps = 1 - c.mps
		}
		c.state = transIdxLPS[c.state]
	} else {
		bin = uint32(c.mps)
		c.state = transIdxMPS[c.state]
	}
	d.renormalize()
	return bin
}

func (d *Decoder) DecodeBypass() uint32 {
	d.codIOffset = d.codIOffset<<1 | d.readBit()
	if d.codIOffset >= d.codIRange {
		d.codIOffset -= d.codIRange
		return 1
	}
	return 0
}

func (d *Decoder) DecodeTerminate() uint32 {
	d.codIRange -= 2
	if d.codIOffset >= d.codIRange {
		return 1
	}
	d.renormalize()
	return 0
}

func (d *Decoder) State(ctxIdx int) (state, mps uint8) {
	return d.ctx[ctxIdx].state, d.ctx[ctxIdx].mps
}
