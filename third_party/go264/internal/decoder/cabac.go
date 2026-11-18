package decoder

import (
	"fmt"

	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/cabac"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/loopfilter"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/transform"
)

func (d *sliceDecoder) neighbourCBP(m *mbState) int {
	if m == nil {
		return cabac.CBPUnavailable
	}
	if m.IPCM {
		return cabac.CBPPCM
	}
	return m.cbpLuma | m.cbpChroma<<4
}

func (d *sliceDecoder) intraMBTypeInc() int {
	inc := 0
	if m := d.nb.left; m != nil && m.Intra && m.kind != mbTypeINxN {
		inc++
	}
	if m := d.nb.top; m != nil && m.Intra && m.kind != mbTypeINxN {
		inc++
	}
	return inc
}

func (d *sliceDecoder) chromaPredInc() int {
	inc := 0
	if m := d.nb.left; m != nil && m.Intra && m.chromaMode != 0 {
		inc++
	}
	if m := d.nb.top; m != nil && m.Intra && m.chromaMode != 0 {
		inc++
	}
	return inc
}

func (d *sliceDecoder) cbfCondTerm(m *mbState, present bool, flag bool) int {
	if m == nil {
		if d.cur.Intra {
			return 1
		}
		return 0
	}
	if m.IPCM {
		return 1
	}
	if !present {
		return 0
	}
	if flag {
		return 1
	}
	return 0
}

func (d *sliceDecoder) lumaCBFContext(blk int) (int, int) {
	x, y := blockX[blk], blockY[blk]
	var a, b int
	if x > 0 {
		a = d.cbfCondTerm(d.cur, true, d.cur.NzY[loopfilter.BlkIdxAt(x-4, y)] != 0)
	} else {
		m := d.nb.left
		a = d.cbfCondTerm(m, m != nil && m.hasLumaResidual(), m != nil && m.NzY[loopfilter.BlkIdxAt(12, y)] != 0)
	}
	if y > 0 {
		b = d.cbfCondTerm(d.cur, true, d.cur.NzY[loopfilter.BlkIdxAt(x, y-4)] != 0)
	} else {
		m := d.nb.top
		b = d.cbfCondTerm(m, m != nil && m.hasLumaResidual(), m != nil && m.NzY[loopfilter.BlkIdxAt(x, 12)] != 0)
	}
	return a, b
}

func (m *mbState) hasLumaResidual() bool {
	return m.kind != mbTypePSkip
}

func (d *sliceDecoder) lumaDCCBFContext() (int, int) {
	a := d.cbfCondTerm(d.nb.left, d.nb.left != nil && d.nb.left.kind == mbTypeI16x16,
		d.nb.left != nil && d.nb.left.cbfLumaDC)
	b := d.cbfCondTerm(d.nb.top, d.nb.top != nil && d.nb.top.kind == mbTypeI16x16,
		d.nb.top != nil && d.nb.top.cbfLumaDC)
	return a, b
}

func (d *sliceDecoder) chromaDCCBFContext(plane int) (int, int) {
	pick := func(m *mbState) (bool, bool) {
		if m == nil {
			return false, false
		}
		return m.cbpChroma != 0, m.cbfChromaDC[plane]
	}
	pa, fa := pick(d.nb.left)
	pb, fb := pick(d.nb.top)
	return d.cbfCondTerm(d.nb.left, pa, fa), d.cbfCondTerm(d.nb.top, pb, fb)
}

func (d *sliceDecoder) chromaACCBFContext(plane, blk int) (int, int) {
	nz := func(m *mbState, idx int) bool {
		if plane == 0 {
			return m.nzCb[idx] != 0
		}
		return m.nzCr[idx] != 0
	}
	x, y := chromaBlockX[blk], chromaBlockY[blk]
	var a, b int
	if x > 0 {
		a = d.cbfCondTerm(d.cur, true, nz(d.cur, blk-1))
	} else {
		m := d.nb.left
		a = d.cbfCondTerm(m, m != nil && m.cbpChroma == 2, m != nil && nz(m, blk+1))
	}
	if y > 0 {
		b = d.cbfCondTerm(d.cur, true, nz(d.cur, blk-2))
	} else {
		m := d.nb.top
		b = d.cbfCondTerm(m, m != nil && m.cbpChroma == 2, m != nil && nz(m, blk+2))
	}
	return a, b
}

func (d *sliceDecoder) cabacIntraPredModes() {
	for blk := 0; blk < 16; blk++ {
		pred := d.predIntra4x4Mode(blk)
		d.cur.intra4Modes[blk] = int8(d.cb.Intra4x4PredMode(pred))
	}
}

func (d *sliceDecoder) cabacIntra8x8PredModes() {
	for i8 := 0; i8 < 4; i8++ {
		pred := d.predIntra4x4Mode(i8 * 4)
		d.setIntra8x8Mode(i8, d.cb.Intra4x4PredMode(pred))
	}
}

func (d *sliceDecoder) cabacTransformSize8x8() {
	if !d.mayUse8x8Transform() {
		return
	}
	d.cur.Transform8x8 = d.cb.TransformSize8x8Flag(d.transform8x8Inc())
}

func (d *sliceDecoder) cabacLuma8x8(res *mbResidual, i8 int) {
	var scan [64]int32
	n := d.cb.ResidualBlock8x8(&scan)
	for i4 := 0; i4 < 4; i4++ {
		d.cur.NzY[i8*4+i4] = uint8(n)
	}
	scanToBlock8x8(&res.luma8x8[i8], &scan)
}

func (d *sliceDecoder) cabacResidual(res *mbResidual, i16 bool) {
	if i16 {
		var scan [16]int32
		a, b := d.lumaDCCBFContext()
		n := d.cb.ResidualBlock(scan[:], cabac.CatIntra16x16DC, a, b, 1)
		d.cur.cbfLumaDC = n != 0
		transform.ScanToBlock(&res.lumaDC, &scan)
	}
	for i8 := 0; i8 < 4; i8++ {
		if d.cur.cbpLuma&(1<<uint(i8)) == 0 {
			for i4 := 0; i4 < 4; i4++ {
				d.cur.NzY[i8*4+i4] = 0
			}
			continue
		}
		if d.cur.Transform8x8 {
			d.cabacLuma8x8(res, i8)
			continue
		}
		for i4 := 0; i4 < 4; i4++ {
			blk := i8*4 + i4
			var scan [16]int32
			a, b := d.lumaCBFContext(blk)
			cat := cabac.CatLuma4x4
			start := 0
			if i16 {
				cat = cabac.CatIntra16x16AC
				start = 1
			}
			n := d.cb.ResidualBlock(scan[start:], cat, a, b, 1)
			d.cur.NzY[blk] = uint8(n)
			transform.ScanToBlock(&res.luma[blk], &scan)
		}
	}
	if d.cur.cbpChroma == 0 {
		return
	}
	for plane := 0; plane < 2; plane++ {
		var dc [4]int32
		a, b := d.chromaDCCBFContext(plane)
		n := d.cb.ResidualBlock(dc[:], cabac.CatChromaDC, a, b, 1)
		d.cur.cbfChromaDC[plane] = n != 0
		copy(res.chromaDC[plane][:], dc[:])
	}
	if d.cur.cbpChroma < 2 {
		return
	}
	for plane := 0; plane < 2; plane++ {
		for blk := 0; blk < 4; blk++ {
			var scan [16]int32
			a, b := d.chromaACCBFContext(plane, blk)
			n := d.cb.ResidualBlock(scan[1:], cabac.CatChromaAC, a, b, 1)
			if plane == 0 {
				d.cur.nzCb[blk] = uint8(n)
			} else {
				d.cur.nzCr[blk] = uint8(n)
			}
			transform.ScanToBlock(&res.chromaAC[plane][blk], &scan)
		}
	}
}

func (d *sliceDecoder) cabacQPDelta() error {
	delta := d.cb.MBQPDelta(d.prevQPDeltaNonZero)
	d.prevQPDeltaNonZero = delta != 0
	if delta < -26 || delta > 25 {
		return fmt.Errorf("%w: mb_qp_delta %d", ErrCorrupt, delta)
	}
	d.qpY = (d.qpY + delta + 52) % 52
	return nil
}

func (d *sliceDecoder) decodeIntraMBCABAC(info mbTypeInfo, res *mbResidual) error {
	d.cur.Intra = true
	d.cur.kind = info.kind

	switch info.kind {
	case mbTypeIPCM:
		if err := d.decodeIPCM(); err != nil {
			return err
		}
		d.prevQPDeltaNonZero = false
		return d.cb.Restart(d.r)

	case mbTypeINxN:
		if d.pps.Transform8x8Mode {
			d.cur.Transform8x8 = d.cb.TransformSize8x8Flag(d.transform8x8Inc())
		}
		if d.cur.Transform8x8 {
			d.cabacIntra8x8PredModes()
		} else {
			d.cabacIntraPredModes()
		}
		d.cur.chromaMode = int8(d.cb.IntraChromaPredMode(d.chromaPredInc()))
		cbp := d.cb.CodedBlockPatternLuma(d.neighbourCBP(d.nb.left), d.neighbourCBP(d.nb.top))
		d.cur.cbpLuma = cbp
		d.cur.cbpChroma = d.cb.CodedBlockPatternChroma(d.neighbourCBP(d.nb.left), d.neighbourCBP(d.nb.top))
		if d.cur.cbpLuma != 0 || d.cur.cbpChroma != 0 {
			if err := d.cabacQPDelta(); err != nil {
				return err
			}
			d.cabacResidual(res, false)
		} else {
			d.prevQPDeltaNonZero = false
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
		d.cur.chromaMode = int8(d.cb.IntraChromaPredMode(d.chromaPredInc()))
		if err := d.cabacQPDelta(); err != nil {
			return err
		}
		d.cabacResidual(res, true)
		d.cur.QPY = d.qpY
		d.reconstructIntra16x16(res)
		d.reconstructChroma(res)
		return nil
	}
	return fmt.Errorf("%w: macroblock kind %d", ErrCorrupt, info.kind)
}

func isSkipped(m *mbState) bool {
	return m.kind == mbTypePSkip || m.kind == mbTypeBSkip
}

func (d *sliceDecoder) skipFlagInc() int {
	inc := 0
	if m := d.nb.left; m != nil && !isSkipped(m) {
		inc++
	}
	if m := d.nb.top; m != nil && !isSkipped(m) {
		inc++
	}
	return inc
}

func (d *sliceDecoder) bMBTypeInc() int {
	direct := func(m *mbState) bool {
		return m == nil || m.kind == mbTypeBSkip || m.kind == mbTypeBDirect
	}
	inc := 0
	if !direct(d.nb.left) {
		inc++
	}
	if !direct(d.nb.top) {
		inc++
	}
	return inc
}

func (d *sliceDecoder) refIdxCondTerm(list, x, y, curZ int) bool {
	m, blk := d.neighbourBlock(x, y, curZ)
	if m == nil || m.refIdx(list, blk) <= 0 {
		return false
	}
	return !m.directBlk[blk]
}

func (d *sliceDecoder) cabacRefIdx(list, x, y, w, h int, maxRef int) (int8, error) {
	if maxRef == 0 {
		d.storeRefIdx(list, x, y, w, h, 0)
		return 0, nil
	}
	curZ := zscanOf[y>>2][x>>2]
	inc := 0
	if d.refIdxCondTerm(list, x-1, y, curZ) {
		inc++
	}
	if d.refIdxCondTerm(list, x, y-1, curZ) {
		inc += 2
	}
	ref := d.cb.RefIdx(inc)
	if ref > maxRef {
		return 0, fmt.Errorf("%w: ref_idx_l%d %d", ErrCorrupt, list, ref)
	}
	d.storeRefIdx(list, x, y, w, h, int8(ref))
	return int8(ref), nil
}

func (d *sliceDecoder) cabacMVDComponent(base, absSum int) int16 {
	v := d.cb.MVD(base, absSum)
	if v < -32768 || v > 32767 {
		d.mvdOverflow = true
		return 0
	}
	return int16(v)
}

func (d *sliceDecoder) cabacMVD(list, x, y, w, h int) [2]int16 {
	curZ := zscanOf[y>>2][x>>2]
	a := d.neighbourMVD(list, x-1, y, curZ)
	b := d.neighbourMVD(list, x, y-1, curZ)
	var mvd [2]int16
	mvd[0] = d.cabacMVDComponent(cabac.MVDHorizontal, int(a[0])+int(b[0]))
	mvd[1] = d.cabacMVDComponent(cabac.MVDVertical, int(a[1])+int(b[1]))
	d.storeMVD(list, x, y, w, h, mvd)
	return mvd
}

func (d *sliceDecoder) decodeP8x8CABAC(maxRef int) error {
	var sub [4]subMbInfo
	for i := 0; i < 4; i++ {
		sub[i] = subMbTable[d.cb.SubMBTypeP()]
		if sub[i].numParts != 1 {
			d.subPartsAtLeast8x8 = false
		}
	}
	var refs [4]int8
	for i := 0; i < 4; i++ {
		r, err := d.cabacRefIdx(0, i%2*8, i/2*8, 8, 8, maxRef)
		if err != nil {
			return err
		}
		refs[i] = r
	}
	for i := 0; i < 4; i++ {
		ox, oy := i%2*8, i/2*8
		s := sub[i]
		cols := 8 / s.w
		for p := 0; p < s.numParts; p++ {
			px := ox + p%cols*s.w
			py := oy + p/cols*s.h
			mvd := d.cabacMVD(0, px, py, s.w, s.h)
			mvp := d.predictMV(0, px, py, s.w, s.h, refs[i], p, mbTypeP8x8)
			d.storeMotion(0, px, py, s.w, s.h, [2]int16{mvp[0] + mvd[0], mvp[1] + mvd[1]}, refs[i])
		}
	}
	return nil
}

func (d *sliceDecoder) decodeInterMBCABAC(kind int, res *mbResidual) error {
	d.cur.kind = kind
	d.cur.Intra = false
	maxRef := d.numRefIdxActive - 1

	if kind == mbTypeP8x8 {
		if err := d.decodeP8x8CABAC(maxRef); err != nil {
			return err
		}
	} else {
		parts := partitionsOf(kind)
		var refs [2]int8
		for i, p := range parts {
			r, err := d.cabacRefIdx(0, p.x, p.y, p.w, p.h, maxRef)
			if err != nil {
				return err
			}
			refs[i] = r
		}
		for i, p := range parts {
			mvd := d.cabacMVD(0, p.x, p.y, p.w, p.h)
			mvp := d.predictMV(0, p.x, p.y, p.w, p.h, refs[i], i, kind)
			d.storeMotion(0, p.x, p.y, p.w, p.h, [2]int16{mvp[0] + mvd[0], mvp[1] + mvd[1]}, refs[i])
		}
	}

	left, top := d.neighbourCBP(d.nb.left), d.neighbourCBP(d.nb.top)
	d.cur.cbpLuma = d.cb.CodedBlockPatternLuma(left, top)
	d.cur.cbpChroma = d.cb.CodedBlockPatternChroma(left, top)
	d.cabacTransformSize8x8()
	if d.cur.cbpLuma != 0 || d.cur.cbpChroma != 0 {
		if err := d.cabacQPDelta(); err != nil {
			return err
		}
		d.cabacResidual(res, false)
	} else {
		d.prevQPDeltaNonZero = false
	}
	d.cur.QPY = d.qpY
	d.motionCompensate()
	d.addInterResidual(res)
	return nil
}

func (d *sliceDecoder) decodeB8x8CABAC(res *mbResidual) error {
	var sub [4]bSubTypeInfo
	anyDirect := false
	for i := 0; i < 4; i++ {
		sub[i] = bSubTypes[d.cb.SubMBTypeB()]
		anyDirect = anyDirect || sub[i].direct
	}
	d.noteBSubPartSizes(sub)
	d.applySubDirect(sub, anyDirect)

	var refs [2][4]int8
	for list := 0; list < 2; list++ {
		maxRef := int(d.maxRefFor(list))
		for i := 0; i < 4; i++ {
			refs[list][i] = -1
			if sub[i].direct {
				continue
			}
			if sub[i].pred&(1<<uint(list)) == 0 {
				d.storeRefIdx(list, i%2*8, i/2*8, 8, 8, -1)
				continue
			}
			r, err := d.cabacRefIdx(list, i%2*8, i/2*8, 8, 8, maxRef)
			if err != nil {
				return err
			}
			refs[list][i] = r
		}
	}
	for list := 0; list < 2; list++ {
		for i := 0; i < 4; i++ {
			if sub[i].direct {
				continue
			}
			if sub[i].pred&(1<<uint(list)) == 0 {
				d.storeMotion(list, i%2*8, i/2*8, 8, 8, [2]int16{}, -1)
				continue
			}
			s := sub[i]
			ox, oy := i%2*8, i/2*8
			cols := 8 / s.w
			for p := 0; p < s.numParts; p++ {
				px := ox + p%cols*s.w
				py := oy + p/cols*s.h
				mvd := d.cabacMVD(list, px, py, s.w, s.h)
				mvp := d.predictMV(list, px, py, s.w, s.h, refs[list][i], p, mbTypeB8x8)
				d.storeMotion(list, px, py, s.w, s.h,
					[2]int16{mvp[0] + mvd[0], mvp[1] + mvd[1]}, refs[list][i])
			}
		}
	}
	return nil
}

func (d *sliceDecoder) decodeInterMBBCABAC(info bMBTypeInfo, res *mbResidual) error {
	d.cur.kind = info.kind
	d.cur.Intra = false

	switch {
	case info.direct:
		d.subPartsAtLeast8x8 = d.direct8x8
		d.directMotion(0, 0, 16, 16)
		d.markDirect(0, 0, 16, 16)

	case info.kind == mbTypeB8x8:
		if err := d.decodeB8x8CABAC(res); err != nil {
			return err
		}

	default:
		parts := bPartitions(info.kind)
		var refs [2][2]int8
		for list := 0; list < 2; list++ {
			maxRef := int(d.maxRefFor(list))
			for i := range parts {
				refs[list][i] = -1
				if info.pred[i]&(1<<uint(list)) == 0 {
					continue
				}
				r, err := d.cabacRefIdx(list, parts[i].x, parts[i].y, parts[i].w, parts[i].h, maxRef)
				if err != nil {
					return err
				}
				refs[list][i] = r
			}
		}
		for list := 0; list < 2; list++ {
			for i, p := range parts {
				if refs[list][i] < 0 {
					d.storeMotion(list, p.x, p.y, p.w, p.h, [2]int16{}, -1)
					continue
				}
				mvd := d.cabacMVD(list, p.x, p.y, p.w, p.h)
				mvp := d.predictMV(list, p.x, p.y, p.w, p.h, refs[list][i], i, info.kind)
				d.storeMotion(list, p.x, p.y, p.w, p.h,
					[2]int16{mvp[0] + mvd[0], mvp[1] + mvd[1]}, refs[list][i])
			}
		}
	}

	left, top := d.neighbourCBP(d.nb.left), d.neighbourCBP(d.nb.top)
	d.cur.cbpLuma = d.cb.CodedBlockPatternLuma(left, top)
	d.cur.cbpChroma = d.cb.CodedBlockPatternChroma(left, top)
	d.cabacTransformSize8x8()
	if d.cur.cbpLuma != 0 || d.cur.cbpChroma != 0 {
		if err := d.cabacQPDelta(); err != nil {
			return err
		}
		d.cabacResidual(res, false)
	} else {
		d.prevQPDeltaNonZero = false
	}
	d.cur.QPY = d.qpY
	d.motionCompensate()
	d.addInterResidual(res)
	return nil
}

func (d *sliceDecoder) decodeMacroblockCABAC(res *mbResidual) error {
	if d.hdr.SliceType.IsI() {
		v := d.cb.IntraMBType(d.intraMBTypeInc())
		info, ok := intraMBType(uint32(v))
		if !ok {
			return fmt.Errorf("%w: mb_type %d in a CABAC I slice", ErrCorrupt, v)
		}
		return d.decodeIntraMBCABAC(info, res)
	}
	if d.hdr.SliceType.IsB() {
		v, intra := d.cb.MBTypeB(d.bMBTypeInc())
		if intra {
			info, ok := intraMBType(uint32(v))
			if !ok {
				return fmt.Errorf("%w: intra mb_type %d in a CABAC B slice", ErrCorrupt, v)
			}
			return d.decodeIntraMBCABAC(info, res)
		}
		if v >= len(bMBTypes) {
			return fmt.Errorf("%w: mb_type %d in a CABAC B slice", ErrCorrupt, v)
		}
		return d.decodeInterMBBCABAC(bMBTypes[v], res)
	}
	v, intra := d.cb.MBTypeP()
	if intra {
		info, ok := intraMBType(uint32(v))
		if !ok {
			return fmt.Errorf("%w: intra mb_type %d in a CABAC P slice", ErrCorrupt, v)
		}
		return d.decodeIntraMBCABAC(info, res)
	}
	info, ok := interMBType(uint32(v))
	if !ok {
		return fmt.Errorf("%w: mb_type %d in a CABAC P slice", ErrCorrupt, v)
	}
	return d.decodeInterMBCABAC(info.kind, res)
}

func (d *sliceDecoder) cabacSkipFlag(isB bool) bool {
	inc := d.skipFlagInc()
	if isB {
		return d.cb.MBSkipFlagB(inc)
	}
	return d.cb.MBSkipFlagP(inc)
}

func (d *sliceDecoder) runCABAC() error {
	if err := d.cb.Init(d.r, d.qpY, d.hdr.SliceType.IsI(), d.hdr.CABACInitIDC); err != nil {
		return err
	}
	if !d.hdr.SliceType.IsI() && !d.hdr.SliceType.IsP() && !d.hdr.SliceType.IsB() {
		return fmt.Errorf("%w: CABAC %s slices", ErrUnsupported, d.hdr.SliceType)
	}
	inter := !d.hdr.SliceType.IsI()
	isB := d.hdr.SliceType.IsB()
	var res mbResidual
	mbAddr := int(d.hdr.FirstMBInSlice)
	if mbAddr >= d.totalMBs() {
		return fmt.Errorf("%w: first_mb_in_slice %d beyond the picture", ErrCorrupt, mbAddr)
	}
	for {
		d.startMB(mbAddr)
		res.reset()
		if inter && d.cabacSkipFlag(isB) {
			if err := d.decodeSkip(); err != nil {
				return err
			}
			d.prevQPDeltaNonZero = false
		} else if err := d.decodeMacroblockCABAC(&res); err != nil {
			return err
		}
		d.cur.Decoded = true
		if d.mvdOverflow {
			return fmt.Errorf("%w: motion vector difference out of range", ErrCorrupt)
		}
		if d.cb.Overrun() != 0 {
			return fmt.Errorf("%w: CABAC decoding ran past the end of the slice data", ErrCorrupt)
		}
		if d.cb.LevelOverflow() {
			return fmt.Errorf("%w: coefficient level out of range", ErrCorrupt)
		}
		if d.cb.EndOfSlice() {
			return nil
		}
		mbAddr++
		if mbAddr >= d.totalMBs() {
			return fmt.Errorf("%w: slice extends past the last macroblock", ErrCorrupt)
		}
	}
}
