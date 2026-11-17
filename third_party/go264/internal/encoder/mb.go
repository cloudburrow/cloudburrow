package encoder

import (
	"fmt"

	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/bits"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/cabac"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/cavlc"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/loopfilter"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/pred"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/syntax"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/transform"
)

var golombToIntraCBP = [48]uint8{
	47, 31, 15, 0, 23, 27, 29, 30, 7, 11, 13, 14, 39, 43, 45, 46,
	16, 3, 5, 10, 12, 19, 21, 26, 28, 35, 37, 42, 44, 1, 2, 4,
	8, 17, 18, 20, 24, 6, 9, 22, 25, 32, 33, 34, 36, 40, 38, 41,
}

var golombToInterCBP = [48]uint8{
	0, 16, 1, 2, 4, 8, 32, 3, 5, 10, 12, 15, 47, 7, 11, 13,
	14, 6, 9, 31, 35, 37, 42, 44, 33, 34, 36, 40, 39, 43, 45, 46,
	17, 18, 20, 24, 19, 21, 26, 28, 23, 27, 29, 30, 22, 25, 38, 41,
}

var (
	intraCBPToGolomb [48]uint32
	interCBPToGolomb [48]uint32
)

func init() {
	for code, cbp := range golombToIntraCBP {
		intraCBPToGolomb[cbp] = uint32(code)
	}
	for code, cbp := range golombToInterCBP {
		interCBPToGolomb[cbp] = uint32(code)
	}
}

type mbEncoder struct {
	e   *Encoder
	w   *bits.Writer
	cur *mbInfo
	nb  neighbours

	mbx int
	mby int
	qpY int

	isP            bool
	isB            bool
	numRefs        int
	numRefsL1      int
	hints          *frameHints
	span           sliceRange
	sliceQP        int
	prevQP         int
	pendingSkipRun uint32

	weights    *syntax.PredWeightTable
	weightMode int

	cb                 *cabac.Encoder
	prevQPDeltaNonZero bool

	trellis bool
	tb      trellisBlock
	tw      *bits.Writer
	trialW  *bits.Writer

	refresh           refreshPlan
	refreshNoTopRight bool
	skipAll           bool

	scratch     [256]byte
	scratchB    [256]byte
	segs        []motionSegment
	predA       predBuffer
	predB       predBuffer
	parts       []partResult
	subs        []subResult
	bparts      []bPartResult
	bsubs       []bSubResult
	lumaScan    [16][16]int32
	lumaDCScan  [16]int32
	chromaDC    [2]transform.ChromaDC
	chromaScan  [2][4][16]int32
	luma8x8Scan [4][64]int32
	saved8x8    intraLumaState
}

func (s *mbEncoder) reset() {
	s.lumaScan = [16][16]int32{}
	s.lumaDCScan = [16]int32{}
	s.chromaDC = [2]transform.ChromaDC{}
	s.chromaScan = [2][4][16]int32{}
	s.luma8x8Scan = [4][64]int32{}
	s.cur.Transform8x8 = false
}

func (s *mbEncoder) configure(e *Encoder, w *bits.Writer, hdr *syntax.SliceHeader, p sliceJob, hints *frameHints) {
	s.e, s.w = e, w
	s.qpY, s.sliceQP, s.prevQP = p.qp, p.qp, p.qp
	s.isP, s.isB = hdr.SliceType.IsP(), hdr.SliceType.IsB()
	s.numRefs, s.numRefsL1 = p.active, p.activeL1
	s.hints = hints
	s.span = sliceRange{firstMB: p.firstMB, endMB: p.endMB}
	s.refresh = e.refresh
	s.skipAll = p.skipAll
	s.trellis = e.cfg.Trellis
	s.pendingSkipRun = 0
	s.cb = nil
	s.prevQPDeltaNonZero = false
	s.weights = nil
	s.weightMode = e.weightModeFor(hdr.SliceType)
	if s.weightMode == weightExplicit {
		s.weights = &hdr.PredWeight
	}
}

func (s *mbEncoder) encodeAt(addr int, hdr *syntax.SliceHeader, sliceID int) error {
	e := s.e
	mbx, mby := addr%e.widthMBs, addr/e.widthMBs
	s.mbx = mbx
	s.mby = mby
	s.qpY = clampQP(s.sliceQP + s.hints.qpDelta(mbx, mby))
	s.cur = e.at(mbx, mby)
	*s.cur = mbInfo{MB: loopfilter.MB{
		QPY:            s.qpY,
		ChromaQPOffset: [2]int{int(e.pps.ChromaQPIndexOffset), int(e.pps.SecondChromaQPIndexOffset)},
		Bipredictive:   s.isB,
		SliceID:        sliceID,
		DisableDeblock: hdr.DisableDeblockingFilterIDC,
		AlphaOffset:    int(hdr.SliceAlphaC0OffsetDiv2) * 2,
		BetaOffset:     int(hdr.SliceBetaOffsetDiv2) * 2,
	}}
	for i := range s.cur.refIdx {
		s.cur.refIdx[i] = -1
		s.cur.refIdxL1[i] = -1
	}
	s.nb = e.around(mbx, mby, s.span)
	s.refreshNoTopRight = s.refresh.inBand(mbx) && mbx+1 >= s.refresh.end &&
		s.refresh.end < e.widthMBs && s.nb.topRight != nil
	s.reset()

	if err := s.encodeMB(); err != nil {
		return err
	}
	s.cur.Decoded = true
	return nil
}

func (e *Encoder) encodeSlice(w *bits.Writer, hdr *syntax.SliceHeader, p sliceJob, hints *frameHints) error {
	s := &mbEncoder{}
	s.configure(e, w, hdr, p, hints)
	if e.pps.CABAC {
		s.cb = &cabac.Encoder{}
		if err := s.cb.Init(w, p.qp, hdr.SliceType.IsI(), hdr.CABACInitIDC); err != nil {
			return err
		}
	}
	for addr := p.firstMB; addr < p.endMB; addr++ {
		if err := s.encodeAt(addr, hdr, p.id+1); err != nil {
			return err
		}
		if s.cb != nil {
			s.cb.EndOfSlice(addr == p.endMB-1)
		}
	}
	if s.cb != nil {
		s.cb.Finish()
		return w.Err()
	}
	if (s.isP || s.isB) && s.pendingSkipRun > 0 {
		w.WriteUE(s.pendingSkipRun)
	}
	return w.Err()
}

func (s *mbEncoder) encodeMB() error {
	switch {
	case s.isB:
		if s.cb != nil {
			return s.encodeBMBCABAC()
		}
		skipped, err := s.encodeBMB()
		if err != nil {
			return err
		}
		if skipped {
			s.pendingSkipRun++
		}
		return nil
	case s.isP:
		if s.cb != nil {
			return s.encodeInterMBCABAC()
		}
		skipped, err := s.encodeInterMB()
		if err != nil {
			return err
		}
		if skipped {
			s.pendingSkipRun++
		}
		return nil
	}
	if s.cb != nil {
		s.encodeIntraModes()
		s.writeIntraMBCABAC(false)
		return s.w.Err()
	}
	return s.encodeIntraMB(0)
}

func (s *mbEncoder) lumaOffset(blk int) int {
	return s.e.rec.LumaOffset(s.mbx*16+blockX[blk], s.mby*16+blockY[blk])
}

func (s *mbEncoder) chromaOffset(blk int) int {
	return s.e.rec.ChromaOffset(s.mbx*8+chromaBlockX[blk], s.mby*8+chromaBlockY[blk])
}

func (s *mbEncoder) encodeIntraModes() {
	cost16, mode16 := s.searchIntra16x16()
	lambda := lambdaTable[s.qpY]

	cost8 := -1
	if s.e.pps.Transform8x8Mode && !s.refreshNoTopRight {
		cost8 = s.encodeIntra8x8()
		s.saveIntraLuma(&s.saved8x8)
	}
	cost4 := s.encodeIntra4x4()

	best, choice := cost4, choiceIntra4x4
	if c := cost16 + lambda*8; c < best {
		best, choice = c, choiceIntra16x16
	}
	if cost8 >= 0 && cost8 < best {
		choice = choiceIntra8x8
	}

	switch choice {
	case choiceIntra16x16:
		s.reset()
		s.cur.kind = mbTypeI16x16
		s.cur.intra16Mode = int8(mode16)
		s.encodeIntra16x16(mode16)
	case choiceIntra8x8:
		s.restoreIntraLuma(&s.saved8x8)
		s.cur.kind = mbTypeINxN
		s.cur.Transform8x8 = true
	default:
		s.cur.kind = mbTypeINxN
	}
	s.cur.Intra = true
	s.cur.skipped = false

	chromaMode := s.searchChroma()
	s.cur.chromaMode = int8(chromaMode)
	s.encodeChroma(chromaMode)
}

func (s *mbEncoder) encodeIntraMB(typeOffset uint32) error {
	s.encodeIntraModes()
	return s.writeIntraMB(typeOffset)
}

func (s *mbEncoder) searchIntra16x16() (int, int) {
	avail := mbAvailability(s.nb)
	off := s.e.rec.LumaOffset(s.mbx*16, s.mby*16)
	srcOff := s.e.src.LumaOffset(s.mbx*16, s.mby*16)
	best := -1
	bestMode := pred.I16x16DC
	for mode := 0; mode < 4; mode++ {
		if !pred.Intra16x16ModeAvailable(mode, avail) {
			continue
		}
		pred.Intra16x16(s.e.rec.Y, s.e.rec.StrideY, off, mode, avail)
		c := satdBlock(s.e.src.Y, s.e.src.StrideY, srcOff, s.e.rec.Y, s.e.rec.StrideY, off, 16, 16)
		if best < 0 || c < best {
			best = c
			bestMode = mode
		}
	}
	return best, bestMode
}

func (s *mbEncoder) encodeIntra4x4() int {
	avail := mbAvailability(s.nb)
	_ = avail
	lambda := lambdaTable[s.qpY]
	total := 0
	var block transform.Block
	for blk := 0; blk < 16; blk++ {
		a := lumaAvailability(blk, s.nb)
		off := s.lumaOffset(blk)
		srcOff := s.e.src.LumaOffset(s.mbx*16+blockX[blk], s.mby*16+blockY[blk])
		predMode := s.predIntra4x4Mode(blk)

		best := -1
		bestMode := pred.I4x4DC
		for mode := 0; mode < 9; mode++ {
			if !pred.Intra4x4ModeAvailable(mode, a) {
				continue
			}
			if s.refreshNoTopRight && blk == topRightSourceBlock && usesAboveRight4x4(mode) {
				continue
			}
			pred.Intra4x4(s.e.rec.Y, s.e.rec.StrideY, off, mode, a)
			c := satd4x4(s.e.src.Y, s.e.src.StrideY, srcOff, s.e.rec.Y, s.e.rec.StrideY, off)
			if mode != predMode {
				c += lambda * 3
			}
			if best < 0 || c < best {
				best = c
				bestMode = mode
			}
		}
		total += best
		s.cur.intra4Modes[blk] = int8(bestMode)

		pred.Intra4x4(s.e.rec.Y, s.e.rec.StrideY, off, bestMode, a)
		transform.Residual4x4(&block, s.e.src.Y, s.e.src.StrideY, srcOff, s.e.rec.Y, s.e.rec.StrideY, off)
		transform.Forward4x4(&block)
		orig := block
		quant4x4(&block, s.qpY, s.e.lumaQuant4x4(true), true)
		transform.BlockToScan(&s.lumaScan[blk], &block)
		if s.trellis {
			s.trellisLuma4x4(blk, &orig, 0, true, cabac.CatLuma4x4)
			transform.ScanToBlock(&block, &s.lumaScan[blk])
		}
		s.cur.NzY[blk] = uint8(countNonZero(s.lumaScan[blk][:]))
		dequant4x4(&block, s.qpY, s.e.lumaLevel4x4(true), false)
		transform.Inverse4x4(&block)
		transform.AddResidual4x4(s.e.rec.Y, s.e.rec.StrideY, off, &block)
	}
	s.cur.cbpLuma = 0
	for i8 := 0; i8 < 4; i8++ {
		for i4 := 0; i4 < 4; i4++ {
			if s.cur.NzY[i8*4+i4] != 0 {
				s.cur.cbpLuma |= 1 << uint(i8)
				break
			}
		}
	}
	return total
}

func (s *mbEncoder) encodeIntra16x16(mode int) {
	avail := mbAvailability(s.nb)
	off := s.e.rec.LumaOffset(s.mbx*16, s.mby*16)
	pred.Intra16x16(s.e.rec.Y, s.e.rec.StrideY, off, mode, avail)

	var blocks [16]transform.Block
	var dc transform.Block
	for blk := 0; blk < 16; blk++ {
		bOff := s.lumaOffset(blk)
		srcOff := s.e.src.LumaOffset(s.mbx*16+blockX[blk], s.mby*16+blockY[blk])
		transform.Residual4x4(&blocks[blk], s.e.src.Y, s.e.src.StrideY, srcOff,
			s.e.rec.Y, s.e.rec.StrideY, bOff)
		transform.Forward4x4(&blocks[blk])
		dcIdx := (blockY[blk]>>2)*4 + blockX[blk]>>2
		dc[dcIdx] = blocks[blk][0]
	}
	transform.HadamardForwardDC4x4(&dc)
	dcOrig := dc
	quantDCLevels(dc[:], s.qpY, s.e.lumaQuant4x4(true), true)
	transform.BlockToScan(&s.lumaDCScan, &dc)
	if s.trellis {
		s.trellisLumaDC(&dcOrig)
	}

	acNonZero := false
	for blk := 0; blk < 16; blk++ {
		orig := blocks[blk]
		blocks[blk][0] = 0
		quant4x4(&blocks[blk], s.qpY, s.e.lumaQuant4x4(true), true)
		blocks[blk][0] = 0
		transform.BlockToScan(&s.lumaScan[blk], &blocks[blk])
		if s.trellis {
			s.trellisLuma4x4(blk, &orig, 1, true, cabac.CatIntra16x16AC)
		}
		n := countNonZero(s.lumaScan[blk][1:])
		s.cur.NzY[blk] = uint8(n)
		if n != 0 {
			acNonZero = true
		}
	}
	if acNonZero {
		s.cur.cbpLuma = 15
	} else {
		s.cur.cbpLuma = 0
		for blk := 0; blk < 16; blk++ {
			s.cur.NzY[blk] = 0
			for i := 1; i < 16; i++ {
				s.lumaScan[blk][i] = 0
			}
		}
	}

	transform.ScanToBlock(&dc, &s.lumaDCScan)
	dequantLumaDC(&dc, s.qpY, s.e.lumaLevel4x4(true))
	for blk := 0; blk < 16; blk++ {
		var b transform.Block
		transform.ScanToBlock(&b, &s.lumaScan[blk])
		b[0] = dc[(blockY[blk]>>2)*4+blockX[blk]>>2]
		dequant4x4(&b, s.qpY, s.e.lumaLevel4x4(true), true)
		transform.Inverse4x4(&b)
		transform.AddResidual4x4(s.e.rec.Y, s.e.rec.StrideY, s.lumaOffset(blk), &b)
	}
}

func (s *mbEncoder) searchChroma() int {
	avail := mbAvailability(s.nb)
	off := s.e.rec.ChromaOffset(s.mbx*8, s.mby*8)
	srcOff := s.e.src.ChromaOffset(s.mbx*8, s.mby*8)
	best := -1
	bestMode := pred.ChromaDC
	for mode := 0; mode < 4; mode++ {
		if !pred.ChromaModeAvailable(mode, avail) {
			continue
		}
		pred.IntraChroma8x8(s.e.rec.Cb, s.e.rec.StrideC, off, mode, avail)
		pred.IntraChroma8x8(s.e.rec.Cr, s.e.rec.StrideC, off, mode, avail)
		c := satdBlock(s.e.src.Cb, s.e.src.StrideC, srcOff, s.e.rec.Cb, s.e.rec.StrideC, off, 8, 8)
		c += satdBlock(s.e.src.Cr, s.e.src.StrideC, srcOff, s.e.rec.Cr, s.e.rec.StrideC, off, 8, 8)
		if best < 0 || c < best {
			best = c
			bestMode = mode
		}
	}
	return bestMode
}

func (s *mbEncoder) encodeChroma(mode int) {
	avail := mbAvailability(s.nb)
	off := s.e.rec.ChromaOffset(s.mbx*8, s.mby*8)
	planes := [2][]byte{s.e.rec.Cb, s.e.rec.Cr}
	srcPlanes := [2][]byte{s.e.src.Cb, s.e.src.Cr}
	offsets := [2]int32{s.e.pps.ChromaQPIndexOffset, s.e.pps.SecondChromaQPIndexOffset}

	anyDC := false
	anyAC := false
	var blocks [2][4]transform.Block
	for plane := 0; plane < 2; plane++ {
		pred.IntraChroma8x8(planes[plane], s.e.rec.StrideC, off, mode, avail)
		qpc := syntax.ChromaQP(s.qpY, int(offsets[plane]))
		var dc transform.ChromaDC
		for blk := 0; blk < 4; blk++ {
			bOff := s.chromaOffset(blk)
			srcOff := s.e.src.ChromaOffset(s.mbx*8+chromaBlockX[blk], s.mby*8+chromaBlockY[blk])
			transform.Residual4x4(&blocks[plane][blk], srcPlanes[plane], s.e.src.StrideC, srcOff,
				planes[plane], s.e.rec.StrideC, bOff)
			transform.Forward4x4(&blocks[plane][blk])
			dc[blk] = blocks[plane][blk][0]
		}
		transform.Hadamard2x2(&dc)
		dcOrig := dc
		quantDCLevels(dc[:], qpc, s.e.chromaQuant4x4(true, plane), true)
		s.chromaDC[plane] = dc
		if s.trellis {
			s.trellisChromaDC(plane, qpc, &dcOrig, true)
		}
		for i := 0; i < 4; i++ {
			if s.chromaDC[plane][i] != 0 {
				anyDC = true
			}
		}
		for blk := 0; blk < 4; blk++ {
			orig := blocks[plane][blk]
			blocks[plane][blk][0] = 0
			quant4x4(&blocks[plane][blk], qpc, s.e.chromaQuant4x4(true, plane), true)
			blocks[plane][blk][0] = 0
			transform.BlockToScan(&s.chromaScan[plane][blk], &blocks[plane][blk])
			if s.trellis {
				s.trellisChromaAC(plane, blk, qpc, &orig, true)
			}
			n := countNonZero(s.chromaScan[plane][blk][1:])
			if plane == 0 {
				s.cur.nzCb[blk] = uint8(n)
			} else {
				s.cur.nzCr[blk] = uint8(n)
			}
			if n != 0 {
				anyAC = true
			}
		}
	}

	switch {
	case anyAC:
		s.cur.cbpChroma = 2
	case anyDC:
		s.cur.cbpChroma = 1
	default:
		s.cur.cbpChroma = 0
	}
	if s.cur.cbpChroma < 2 {
		for plane := 0; plane < 2; plane++ {
			for blk := 0; blk < 4; blk++ {
				for i := 1; i < 16; i++ {
					s.chromaScan[plane][blk][i] = 0
				}
			}
			s.cur.nzCb = [4]uint8{}
			s.cur.nzCr = [4]uint8{}
		}
	}

	for plane := 0; plane < 2; plane++ {
		qpc := syntax.ChromaQP(s.qpY, int(offsets[plane]))
		dc := s.chromaDC[plane]
		if s.cur.cbpChroma == 0 {
			dc = transform.ChromaDC{}
		}
		dequantChromaDC(&dc, qpc, s.e.chromaLevel4x4(true, plane))
		for blk := 0; blk < 4; blk++ {
			var b transform.Block
			transform.ScanToBlock(&b, &s.chromaScan[plane][blk])
			b[0] = dc[blk]
			dequant4x4(&b, qpc, s.e.chromaLevel4x4(true, plane), true)
			transform.Inverse4x4(&b)
			transform.AddResidual4x4(planes[plane], s.e.rec.StrideC, s.chromaOffset(blk), &b)
		}
	}
}

func countNonZero(v []int32) int {
	n := 0
	for _, x := range v {
		if x != 0 {
			n++
		}
	}
	return n
}

func (s *mbEncoder) writeIntraMB(typeOffset uint32) error {
	if s.cur.kind == mbTypeINxN {
		s.w.WriteUE(typeOffset)
		if s.e.pps.Transform8x8Mode {
			s.w.WriteFlag(s.cur.Transform8x8)
		}
		if s.cur.Transform8x8 {
			s.writeIntra8x8Modes()
		} else {
			for blk := 0; blk < 16; blk++ {
				mode := int(s.cur.intra4Modes[blk])
				predMode := s.predIntra4x4Mode(blk)
				if mode == predMode {
					s.w.WriteFlag(true)
					continue
				}
				s.w.WriteFlag(false)
				rem := mode
				if mode > predMode {
					rem = mode - 1
				}
				s.w.WriteBits(uint32(rem), 3)
			}
		}
		s.w.WriteUE(uint32(s.cur.chromaMode))
		cbp := uint8(s.cur.cbpLuma | s.cur.cbpChroma<<4)
		s.w.WriteUE(intraCBPToGolomb[cbp])
		if cbp != 0 {
			s.writeQPDelta()
			if err := s.writeResidual(false); err != nil {
				return err
			}
		} else {
			s.holdQP()
		}
		return s.w.Err()
	}

	cbpLumaBit := 0
	if s.cur.cbpLuma != 0 {
		cbpLumaBit = 1
	}
	mbType := 1 + int(s.cur.intra16Mode) + 4*s.cur.cbpChroma + 12*cbpLumaBit
	s.w.WriteUE(uint32(mbType) + typeOffset)
	s.w.WriteUE(uint32(s.cur.chromaMode))
	s.writeQPDelta()
	if err := s.writeResidual(true); err != nil {
		return err
	}
	return s.w.Err()
}

func (s *mbEncoder) writeResidual(i16 bool) error {
	if i16 {
		if err := cavlc.WriteBlock(s.w, s.lumaDCScan[:], s.lumaNC(0)); err != nil {
			return fmt.Errorf("writing luma DC: %w", err)
		}
	}
	for i8 := 0; i8 < 4; i8++ {
		if s.cur.cbpLuma&(1<<uint(i8)) == 0 {
			continue
		}
		if s.cur.Transform8x8 {
			if err := s.writeLuma8x8(i8); err != nil {
				return fmt.Errorf("writing luma 8x8 block %d: %w", i8, err)
			}
			continue
		}
		for i4 := 0; i4 < 4; i4++ {
			blk := i8*4 + i4
			scan := s.lumaScan[blk][:]
			if i16 {
				scan = s.lumaScan[blk][1:]
			}
			if err := cavlc.WriteBlock(s.w, scan, s.lumaNC(blk)); err != nil {
				return fmt.Errorf("writing luma block %d: %w", blk, err)
			}
		}
	}
	if s.cur.cbpChroma == 0 {
		return nil
	}
	for plane := 0; plane < 2; plane++ {
		if err := cavlc.WriteBlock(s.w, s.chromaDC[plane][:], -1); err != nil {
			return fmt.Errorf("writing chroma DC: %w", err)
		}
	}
	if s.cur.cbpChroma < 2 {
		return nil
	}
	for plane := 0; plane < 2; plane++ {
		for blk := 0; blk < 4; blk++ {
			if err := cavlc.WriteBlock(s.w, s.chromaScan[plane][blk][1:], s.chromaNC(plane, blk)); err != nil {
				return fmt.Errorf("writing chroma block: %w", err)
			}
		}
	}
	return nil
}

func clampQP(qp int) int {
	if qp < 0 {
		return 0
	}
	if qp > 51 {
		return 51
	}
	return qp
}

func (s *mbEncoder) writeQPDelta() {
	s.w.WriteSE(int32(s.qpY - s.prevQP))
	s.prevQP = s.qpY
	s.cur.QPY = s.qpY
}

func (s *mbEncoder) holdQP() {
	s.qpY = s.prevQP
	s.cur.QPY = s.prevQP
}
