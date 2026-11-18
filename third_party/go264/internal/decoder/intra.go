package decoder

import (
	"fmt"

	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/cavlc"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/loopfilter"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/pred"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/syntax"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/transform"
)

type mbResidual struct {
	lumaDC   transform.Block
	luma     [16]transform.Block
	luma8x8  [4]transform.Block8x8
	chromaDC [2]transform.ChromaDC
	chromaAC [2][4]transform.Block
}

func (m *mbResidual) reset() { *m = mbResidual{} }

func (d *sliceDecoder) predIntra4x4Mode(blk int) int {
	x, y := blockX[blk], blockY[blk]
	mbA, mbB := d.nb.left, d.nb.top
	if x > 0 {
		mbA = d.cur
	}
	if y > 0 {
		mbB = d.cur
	}
	if mbA == nil || mbB == nil {
		return pred.I4x4DC
	}
	if d.constrained && (!mbA.Intra || !mbB.Intra) {
		return pred.I4x4DC
	}
	modeA := pred.I4x4DC
	if mbA.kind == mbTypeINxN {
		if x > 0 {
			modeA = int(mbA.intra4Modes[loopfilter.BlkIdxAt(x-4, y)])
		} else {
			modeA = int(mbA.intra4Modes[loopfilter.BlkIdxAt(12, y)])
		}
	}
	modeB := pred.I4x4DC
	if mbB.kind == mbTypeINxN {
		if y > 0 {
			modeB = int(mbB.intra4Modes[loopfilter.BlkIdxAt(x, y-4)])
		} else {
			modeB = int(mbB.intra4Modes[loopfilter.BlkIdxAt(x, 12)])
		}
	}
	if modeA < modeB {
		return modeA
	}
	return modeB
}

func (d *sliceDecoder) parseIntraPredModes() error {
	for blk := 0; blk < 16; blk++ {
		usePrev, err := d.r.ReadFlag()
		if err != nil {
			return err
		}
		mode := d.predIntra4x4Mode(blk)
		if !usePrev {
			rem, err := d.r.ReadBits(3)
			if err != nil {
				return err
			}
			if int(rem) < mode {
				mode = int(rem)
			} else {
				mode = int(rem) + 1
			}
		}
		d.cur.intra4Modes[blk] = int8(mode)
	}
	return nil
}

func (d *sliceDecoder) parseIntra8x8PredModes() error {
	for i8 := 0; i8 < 4; i8++ {
		blk := i8 * 4
		usePrev, err := d.r.ReadFlag()
		if err != nil {
			return err
		}
		mode := d.predIntra4x4Mode(blk)
		if !usePrev {
			rem, err := d.r.ReadBits(3)
			if err != nil {
				return err
			}
			if int(rem) < mode {
				mode = int(rem)
			} else {
				mode = int(rem) + 1
			}
		}
		d.setIntra8x8Mode(i8, mode)
	}
	return nil
}

func (d *sliceDecoder) setIntra8x8Mode(i8, mode int) {
	for k := 0; k < 4; k++ {
		d.cur.intra4Modes[i8*4+k] = int8(mode)
	}
}

func (d *sliceDecoder) parseChromaPredMode() error {
	v, err := d.r.ReadUE()
	if err != nil {
		return err
	}
	if v > 3 {
		return fmt.Errorf("%w: intra_chroma_pred_mode %d", ErrCorrupt, v)
	}
	d.cur.chromaMode = int8(v)
	return nil
}

func (d *sliceDecoder) parseCBP(intraNxN bool) error {
	v, err := d.r.ReadUE()
	if err != nil {
		return err
	}
	if v > 47 {
		return fmt.Errorf("%w: coded_block_pattern code %d", ErrCorrupt, v)
	}
	var cbp uint8
	if intraNxN {
		cbp = golombToIntraCBP[v]
	} else {
		cbp = golombToInterCBP[v]
	}
	d.cur.cbpLuma = int(cbp & 15)
	d.cur.cbpChroma = int(cbp >> 4)
	return nil
}

func (d *sliceDecoder) parseQPDelta() error {
	delta, err := d.r.ReadSE()
	if err != nil {
		return err
	}
	if delta < -26 || delta > 25 {
		return fmt.Errorf("%w: mb_qp_delta %d", ErrCorrupt, delta)
	}
	d.qpY = (d.qpY + int(delta) + 52) % 52
	return nil
}

func (d *sliceDecoder) readLumaDC(res *mbResidual) error {
	var scan [16]int32
	n, err := cavlc.ReadBlock(d.r, scan[:], d.lumaNC(0))
	if err != nil {
		return err
	}
	_ = n
	transform.ScanToBlock(&res.lumaDC, &scan)
	return nil
}

func (d *sliceDecoder) readLumaAC(res *mbResidual, blk int, startIdx int) error {
	var scan [16]int32
	n, err := cavlc.ReadBlock(d.r, scan[startIdx:], d.lumaNC(blk))
	if err != nil {
		return err
	}
	d.cur.NzY[blk] = uint8(n)
	transform.ScanToBlock(&res.luma[blk], &scan)
	return nil
}

func (d *sliceDecoder) readChromaDC(res *mbResidual, plane int) error {
	var dc [4]int32
	if _, err := cavlc.ReadBlock(d.r, dc[:], -1); err != nil {
		return err
	}
	copy(res.chromaDC[plane][:], dc[:])
	return nil
}

func (d *sliceDecoder) readChromaAC(res *mbResidual, plane, blk int) error {
	var scan [16]int32
	n, err := cavlc.ReadBlock(d.r, scan[1:], d.chromaNC(plane, blk))
	if err != nil {
		return err
	}
	if plane == 0 {
		d.cur.nzCb[blk] = uint8(n)
	} else {
		d.cur.nzCr[blk] = uint8(n)
	}
	transform.ScanToBlock(&res.chromaAC[plane][blk], &scan)
	return nil
}

func scanToBlock8x8(dst *transform.Block8x8, scan *[64]int32) {
	for i := 0; i < 64; i++ {
		dst[transform.ZigZagScan8x8[i]] = scan[i]
	}
}

func (d *sliceDecoder) readLuma8x8(res *mbResidual, i8 int) error {
	var scan [64]int32
	for i4 := 0; i4 < 4; i4++ {
		blk := i8*4 + i4
		var sub [16]int32
		n, err := cavlc.ReadBlock(d.r, sub[:], d.lumaNC(blk))
		if err != nil {
			return err
		}
		d.cur.NzY[blk] = uint8(n)
		for k := 0; k < 16; k++ {
			scan[4*k+i4] = sub[k]
		}
	}
	scanToBlock8x8(&res.luma8x8[i8], &scan)
	return nil
}

func (d *sliceDecoder) addLuma8x8(res *mbResidual, i8 int, off int, intra bool) {
	b := &res.luma8x8[i8]
	transform.Dequant8x8(b, d.qpY, d.scal.luma8x8(intra))
	transform.Inverse8x8(b)
	transform.AddResidual8x8(d.pic.Y, d.pic.StrideY, off, b)
}

func (d *sliceDecoder) readResidual(res *mbResidual, i16 bool) error {
	if i16 {
		if err := d.readLumaDC(res); err != nil {
			return err
		}
	}
	for i8 := 0; i8 < 4; i8++ {
		if d.cur.cbpLuma&(1<<uint(i8)) == 0 {
			for i4 := 0; i4 < 4; i4++ {
				d.cur.NzY[i8*4+i4] = 0
			}
			continue
		}
		if d.cur.Transform8x8 {
			if err := d.readLuma8x8(res, i8); err != nil {
				return err
			}
			continue
		}
		for i4 := 0; i4 < 4; i4++ {
			blk := i8*4 + i4
			start := 0
			if i16 {
				start = 1
			}
			if err := d.readLumaAC(res, blk, start); err != nil {
				return err
			}
		}
	}
	if d.cur.cbpChroma == 0 {
		return nil
	}
	for plane := 0; plane < 2; plane++ {
		if err := d.readChromaDC(res, plane); err != nil {
			return err
		}
	}
	if d.cur.cbpChroma < 2 {
		return nil
	}
	for plane := 0; plane < 2; plane++ {
		for blk := 0; blk < 4; blk++ {
			if err := d.readChromaAC(res, plane, blk); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *sliceDecoder) reconstructIntra4x4(res *mbResidual) {
	baseX, baseY := d.mbx*16, d.mby*16
	for blk := 0; blk < 16; blk++ {
		off := d.pic.LumaOffset(baseX+blockX[blk], baseY+blockY[blk])
		avail := lumaAvailability(blk, d.nb, d.constrained)
		pred.Intra4x4(d.pic.Y, d.pic.StrideY, off, int(d.cur.intra4Modes[blk]), avail)
		if d.cur.NzY[blk] == 0 {
			continue
		}
		dequant4x4(&res.luma[blk], d.qpY, d.scal.luma4x4(true), false)
		transform.Inverse4x4(&res.luma[blk])
		transform.AddResidual4x4(d.pic.Y, d.pic.StrideY, off, &res.luma[blk])
	}
}

func (d *sliceDecoder) reconstructIntra8x8(res *mbResidual) {
	baseX, baseY := d.mbx*16, d.mby*16
	for i8 := 0; i8 < 4; i8++ {
		off := d.pic.LumaOffset(baseX+i8%2*8, baseY+i8/2*8)
		avail := luma8x8Availability(i8, d.nb, d.constrained)
		pred.Intra8x8(d.pic.Y, d.pic.StrideY, off, int(d.cur.intra4Modes[i8*4]), avail)
		if d.cur.cbpLuma&(1<<uint(i8)) == 0 {
			continue
		}
		d.addLuma8x8(res, i8, off, true)
	}
}

func (d *sliceDecoder) reconstructIntra16x16(res *mbResidual) {
	baseX, baseY := d.mbx*16, d.mby*16
	off := d.pic.LumaOffset(baseX, baseY)
	avail := mbAvailability(d.nb, d.constrained)
	pred.Intra16x16(d.pic.Y, d.pic.StrideY, off, int(d.cur.intra16Mode), avail)

	scale := d.scal.luma4x4(true)
	dequantLumaDC(&res.lumaDC, d.qpY, scale)
	for blk := 0; blk < 16; blk++ {
		dcIdx := (blockY[blk]>>2)*4 + blockX[blk]>>2
		res.luma[blk][0] = res.lumaDC[dcIdx]
		dequant4x4(&res.luma[blk], d.qpY, scale, true)
		transform.Inverse4x4(&res.luma[blk])
		transform.AddResidual4x4(d.pic.Y, d.pic.StrideY,
			d.pic.LumaOffset(baseX+blockX[blk], baseY+blockY[blk]), &res.luma[blk])
	}
}

func (d *sliceDecoder) reconstructChroma(res *mbResidual) {
	baseX, baseY := d.mbx*8, d.mby*8
	off := d.pic.ChromaOffset(baseX, baseY)
	avail := mbAvailability(d.nb, d.constrained)
	planes := [2][]byte{d.pic.Cb, d.pic.Cr}
	for plane := 0; plane < 2; plane++ {
		pred.IntraChroma8x8(planes[plane], d.pic.StrideC, off, int(d.cur.chromaMode), avail)
	}
	d.addChromaResidual(res)
}

func (d *sliceDecoder) addChromaResidual(res *mbResidual) {
	if d.cur.cbpChroma == 0 {
		return
	}
	baseX, baseY := d.mbx*8, d.mby*8
	planes := [2][]byte{d.pic.Cb, d.pic.Cr}
	offsets := [2]int32{d.pps.ChromaQPIndexOffset, d.pps.SecondChromaQPIndexOffset}
	for plane := 0; plane < 2; plane++ {
		qpc := syntax.ChromaQP(d.qpY, int(offsets[plane]))
		scale := d.scal.chroma4x4(d.cur.Intra, plane)
		dequantChromaDC(&res.chromaDC[plane], qpc, scale)
		for blk := 0; blk < 4; blk++ {
			b := &res.chromaAC[plane][blk]
			b[0] = res.chromaDC[plane][blk]
			dequant4x4(b, qpc, scale, true)
			transform.Inverse4x4(b)
			transform.AddResidual4x4(planes[plane], d.pic.StrideC,
				d.pic.ChromaOffset(baseX+chromaBlockX[blk], baseY+chromaBlockY[blk]), b)
		}
	}
}

func (d *sliceDecoder) decodeIPCM() error {
	if err := d.alignForPCM(); err != nil {
		return err
	}
	baseX, baseY := d.mbx*16, d.mby*16
	for y := 0; y < 16; y++ {
		row := d.pic.Y[d.pic.LumaOffset(baseX, baseY+y):]
		for x := 0; x < 16; x++ {
			v, err := d.r.ReadBits(8)
			if err != nil {
				return err
			}
			row[x] = byte(v)
		}
	}
	cx, cy := d.mbx*8, d.mby*8
	for _, plane := range [][]byte{d.pic.Cb, d.pic.Cr} {
		for y := 0; y < 8; y++ {
			row := plane[d.pic.ChromaOffset(cx, cy+y):]
			for x := 0; x < 8; x++ {
				v, err := d.r.ReadBits(8)
				if err != nil {
					return err
				}
				row[x] = byte(v)
			}
		}
	}
	d.cur.IPCM = true
	d.cur.QPY = 0
	for i := range d.cur.NzY {
		d.cur.NzY[i] = 16
	}
	for i := 0; i < 4; i++ {
		d.cur.nzCb[i] = 16
		d.cur.nzCr[i] = 16
	}
	return nil
}

func (d *sliceDecoder) alignForPCM() error {
	for !d.r.ByteAligned() {
		if _, err := d.r.ReadBit(); err != nil {
			return err
		}
	}
	return nil
}

func (d *sliceDecoder) decodeIntraMB(info mbTypeInfo, res *mbResidual) error {
	d.cur.Intra = true
	d.cur.kind = info.kind

	switch info.kind {
	case mbTypeIPCM:
		return d.decodeIPCM()

	case mbTypeINxN:
		if d.pps.Transform8x8Mode {
			flag, err := d.r.ReadFlag()
			if err != nil {
				return err
			}
			d.cur.Transform8x8 = flag
		}
		if d.cur.Transform8x8 {
			if err := d.parseIntra8x8PredModes(); err != nil {
				return err
			}
		} else if err := d.parseIntraPredModes(); err != nil {
			return err
		}
		if err := d.parseChromaPredMode(); err != nil {
			return err
		}
		if err := d.parseCBP(true); err != nil {
			return err
		}
		if d.cur.cbpLuma != 0 || d.cur.cbpChroma != 0 {
			if err := d.parseQPDelta(); err != nil {
				return err
			}
			if err := d.readResidual(res, false); err != nil {
				return err
			}
		}
		d.cur.QPY = d.qpY
		if d.cur.Transform8x8 {
			d.reconstructIntra8x8(res)
		} else {
			d.reconstructIntra4x4(res)
		}
		d.reconstructChroma(res)
		return nil

	case mbTypeI16x16:
		d.cur.intra16Mode = int8(info.intra16PredMode)
		d.cur.cbpLuma = info.cbpLuma
		d.cur.cbpChroma = info.cbpChroma
		if err := d.parseChromaPredMode(); err != nil {
			return err
		}
		if err := d.parseQPDelta(); err != nil {
			return err
		}
		if err := d.readResidual(res, true); err != nil {
			return err
		}
		d.cur.QPY = d.qpY
		d.reconstructIntra16x16(res)
		d.reconstructChroma(res)
		return nil
	}
	return fmt.Errorf("%w: macroblock kind %d", ErrCorrupt, info.kind)
}
