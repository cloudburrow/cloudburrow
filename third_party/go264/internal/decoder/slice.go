package decoder

import (
	"fmt"

	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/bits"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/cabac"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/frame"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/loopfilter"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/syntax"
)

type sliceDecoder struct {
	r    *bits.Reader
	sps  *syntax.SPS
	pps  *syntax.PPS
	hdr  *syntax.SliceHeader
	pic  *frame.Picture
	grid *mbGrid

	refList         []*frame.Picture
	refListL1       []*frame.Picture
	numRefIdxActive int
	numRefIdxL1     int
	directSpatial   bool
	colocated       *frame.Picture
	colShortTerm    bool
	direct8x8       bool
	weights         *syntax.PredWeightTable
	weightMode      int

	predA predBuffer
	predB predBuffer

	cur *mbState
	nb  neighbours

	mbx int
	mby int

	qpY         int
	sliceID     int
	constrained bool
	scal        *scalingTables

	cb                 *cabac.Decoder
	prevQPDeltaNonZero bool

	subPartsAtLeast8x8 bool
	mvdOverflow        bool

	segments []motionSegment
}

func (d *sliceDecoder) transform8x8Inc() int {
	inc := 0
	if m := d.nb.left; m != nil && m.Transform8x8 {
		inc++
	}
	if m := d.nb.top; m != nil && m.Transform8x8 {
		inc++
	}
	return inc
}

func (d *sliceDecoder) mayUse8x8Transform() bool {
	return d.pps.Transform8x8Mode && d.cur.cbpLuma != 0 && d.subPartsAtLeast8x8
}

func (d *sliceDecoder) totalMBs() int { return d.grid.widthMBs * d.grid.heightMBs }

func (d *sliceDecoder) startMB(mbAddr int) {
	d.mbx = mbAddr % d.grid.widthMBs
	d.mby = mbAddr / d.grid.widthMBs
	d.cur = d.grid.at(d.mbx, d.mby)
	*d.cur = mbState{
		MB: loopfilter.MB{
			SliceID:        d.sliceID,
			QPY:            d.qpY,
			ChromaQPOffset: [2]int{int(d.pps.ChromaQPIndexOffset), int(d.pps.SecondChromaQPIndexOffset)},
			DisableDeblock: d.hdr.DisableDeblockingFilterIDC,
			AlphaOffset:    int(d.hdr.SliceAlphaC0OffsetDiv2) * 2,
			BetaOffset:     int(d.hdr.SliceBetaOffsetDiv2) * 2,
		},
	}
	for i := range d.cur.refIdxL0 {
		d.cur.refIdxL0[i] = -1
		d.cur.refIdxL1[i] = -1
	}
	d.cur.Bipredictive = d.hdr.SliceType.IsB()
	d.subPartsAtLeast8x8 = true
	d.nb = d.grid.around(d.mbx, d.mby, d.sliceID)
}

func (d *sliceDecoder) run() error {
	d.qpY = d.hdr.QPY(d.pps)
	d.constrained = d.pps.ConstrainedIntraPred
	if d.pps.CABAC {
		d.cb = &cabac.Decoder{}
		return d.runCABAC()
	}
	var res mbResidual

	mbAddr := int(d.hdr.FirstMBInSlice)
	if mbAddr >= d.totalMBs() {
		return fmt.Errorf("%w: first_mb_in_slice %d beyond the picture", ErrCorrupt, mbAddr)
	}
	inter := !d.hdr.SliceType.IsI() && !d.hdr.SliceType.IsSI()

	for {
		if inter {
			run, err := d.r.ReadUE()
			if err != nil {
				return err
			}
			if int(run) > d.totalMBs()-mbAddr {
				return fmt.Errorf("%w: mb_skip_run %d overruns the picture", ErrCorrupt, run)
			}
			for i := uint32(0); i < run; i++ {
				d.startMB(mbAddr)
				if err := d.decodeSkip(); err != nil {
					return err
				}
				d.cur.Decoded = true
				mbAddr++
			}
			if run > 0 && !d.r.MoreRBSPData() {
				return nil
			}
		}
		if mbAddr >= d.totalMBs() {
			return fmt.Errorf("%w: slice extends past the last macroblock", ErrCorrupt)
		}
		d.startMB(mbAddr)
		res.reset()
		if err := d.decodeMacroblock(&res); err != nil {
			return err
		}
		d.cur.Decoded = true

		mbAddr++
		if !d.r.MoreRBSPData() {
			return nil
		}
		if mbAddr >= d.totalMBs() {
			return fmt.Errorf("%w: slice extends past the last macroblock", ErrCorrupt)
		}
	}
}

func (d *sliceDecoder) decodeSkip() error {
	if d.hdr.SliceType.IsB() {
		return d.decodeBSkip()
	}
	return d.decodePSkip()
}

func (d *sliceDecoder) decodeMacroblock(res *mbResidual) error {
	v, err := d.r.ReadUE()
	if err != nil {
		return err
	}
	if d.hdr.SliceType.IsI() {
		info, ok := intraMBType(v)
		if !ok {
			return fmt.Errorf("%w: mb_type %d in an I slice", ErrCorrupt, v)
		}
		return d.decodeIntraMB(info, res)
	}
	if d.hdr.SliceType.IsB() {
		if v < 23 {
			return d.decodeInterMBB(bMBTypes[v], res)
		}
		info, ok := intraMBType(v - 23)
		if !ok {
			return fmt.Errorf("%w: intra mb_type %d in a B slice", ErrCorrupt, v)
		}
		return d.decodeIntraMB(info, res)
	}
	if !d.hdr.SliceType.IsP() && !d.hdr.SliceType.IsSP() {
		return fmt.Errorf("%w: %s slices", ErrUnsupported, d.hdr.SliceType)
	}
	if v < 5 {
		info, ok := interMBType(v)
		if !ok {
			return fmt.Errorf("%w: mb_type %d in a P slice", ErrCorrupt, v)
		}
		return d.decodeInterMB(info, res)
	}
	info, ok := intraMBType(v - 5)
	if !ok {
		return fmt.Errorf("%w: intra mb_type %d in a P slice", ErrCorrupt, v)
	}
	return d.decodeIntraMB(info, res)
}
