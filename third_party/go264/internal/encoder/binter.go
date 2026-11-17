package encoder

import (
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/frame"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/mc"
)

func minPositive(a, b int8) int8 {
	if a >= 0 && b >= 0 {
		if a < b {
			return a
		}
		return b
	}
	if a > b {
		return a
	}
	return b
}

type DirectMode uint8

const (
	DirectSpatial DirectMode = iota
	DirectTemporal
)

type colBlock struct {
	mv     [2]int16
	refIdx int8
	refPOC int32
	intra  bool
}

func (s *mbEncoder) colocatedAt(bx4, by4 int) colBlock {
	col := s.e.colocated
	if col == nil || col.Motion == nil {
		return colBlock{intra: true}
	}
	if s.e.sps.Direct8x8Inference {
		if bx4 < 2 {
			bx4 = 0
		} else {
			bx4 = 3
		}
		if by4 < 2 {
			by4 = 0
		} else {
			by4 = 3
		}
	}
	mo := col.Motion
	x := s.mbx*4 + bx4
	y := s.mby*4 + by4
	if x >= mo.BlocksWide || y >= mo.BlocksHigh {
		return colBlock{intra: true}
	}
	i := mo.Index(x, y)
	for list := 0; list < 2; list++ {
		if mo.RefIdx[list][i] >= 0 {
			return colBlock{
				mv:     mo.Mv[list][i],
				refIdx: mo.RefIdx[list][i],
				refPOC: mo.RefPOC[list][i],
			}
		}
	}
	return colBlock{intra: true}
}

func (s *mbEncoder) colZero(bx4, by4 int) bool {
	if s.e.colocated == nil || s.e.colocated.LongTerm {
		return false
	}
	c := s.colocatedAt(bx4, by4)
	if c.intra || c.refIdx != 0 {
		return false
	}
	return c.mv[0] >= -1 && c.mv[0] <= 1 && c.mv[1] >= -1 && c.mv[1] <= 1
}

func (s *mbEncoder) spatialDirectBase() (ref [2]int8, mv [2][2]int16, zeroPred bool) {
	for list := 0; list < 2; list++ {
		a, b, c := s.neighboursFor(list, 0, 0, 16)
		ref[list] = minPositive(a.ref, minPositive(b.ref, c.ref))
	}
	if ref[0] < 0 && ref[1] < 0 {
		return [2]int8{0, 0}, [2][2]int16{}, true
	}
	for list := 0; list < 2; list++ {
		if ref[list] >= 0 {
			mv[list] = s.predictMV(list, 0, 0, 16, 16, ref[list], 0, mbTypeB16x16)
		}
	}
	return ref, mv, false
}

func (s *mbEncoder) spatialDirect(x, y, w, h int) {
	ref, mv, zeroPred := s.spatialDirectBase()
	for by := y; by < y+h; by += 4 {
		for bx := x; bx < x+w; bx += 4 {
			blockMv := mv
			if !zeroPred && s.colZero(bx>>2, by>>2) {
				for list := 0; list < 2; list++ {
					if ref[list] == 0 {
						blockMv[list] = [2]int16{}
					}
				}
			}
			for list := 0; list < 2; list++ {
				s.storeMotionIn(list, bx, by, 4, 4, blockMv[list], ref[list])
			}
		}
	}
}

func (s *mbEncoder) mapColToList0(refPOC int32) int8 {
	for i, p := range s.e.refL0 {
		if p != nil && int32(p.POC) == refPOC {
			return int8(i)
		}
	}
	return 0
}

func scaleMV(v int16, dist int) int16 {
	return int16((dist*int(v) + 128) >> 8)
}

func (s *mbEncoder) distScaleFactor(p0 *frame.Picture) (int, bool) {
	p1 := s.refPictureIn(1, 0)
	if p0 == nil || p1 == nil || p0.LongTerm {
		return 0, false
	}
	td := clip3(-128, 127, p1.POC-p0.POC)
	if td == 0 {
		return 0, false
	}
	tb := clip3(-128, 127, s.e.rec.POC-p0.POC)
	abs := td / 2
	if abs < 0 {
		abs = -abs
	}
	tx := (16384 + abs) / td
	return clip3(-1024, 1023, (tb*tx+32)>>6), true
}

func (s *mbEncoder) temporalDirect(x, y, w, h int) {
	for by := y; by < y+h; by += 4 {
		for bx := x; bx < x+w; bx += 4 {
			c := s.colocatedAt(bx>>2, by>>2)
			if c.intra {
				s.storeMotionIn(0, bx, by, 4, 4, [2]int16{}, 0)
				s.storeMotionIn(1, bx, by, 4, 4, [2]int16{}, 0)
				continue
			}
			ref0 := s.mapColToList0(c.refPOC)
			dist, scaled := s.distScaleFactor(s.refPictureIn(0, ref0))
			if !scaled {
				s.storeMotionIn(0, bx, by, 4, 4, c.mv, ref0)
				s.storeMotionIn(1, bx, by, 4, 4, [2]int16{}, 0)
				continue
			}
			mv0 := [2]int16{scaleMV(c.mv[0], dist), scaleMV(c.mv[1], dist)}
			mv1 := [2]int16{mv0[0] - c.mv[0], mv0[1] - c.mv[1]}
			s.storeMotionIn(0, bx, by, 4, 4, mv0, ref0)
			s.storeMotionIn(1, bx, by, 4, 4, mv1, 0)
		}
	}
}

func (s *mbEncoder) directMotion(x, y, w, h int) {
	if s.e.cfg.DirectMode == DirectTemporal {
		s.temporalDirect(x, y, w, h)
		return
	}
	s.spatialDirect(x, y, w, h)
}

func (s *mbEncoder) markDirect(x, y, w, h int) {
	for by := y; by < y+h; by += 4 {
		for bx := x; bx < x+w; bx += 4 {
			s.cur.directBlk[zscanOf[by>>2][bx>>2]] = true
		}
	}
}

type motionSegment struct {
	x, y, w, h int
	mv         [2][2]int16
	ref        [2]int8
}

func sameSegmentMotion(m *mbInfo, a, b int) bool {
	return m.MvL0[a] == m.MvL0[b] && m.refIdx[a] == m.refIdx[b] &&
		m.MvL1[a] == m.MvL1[b] && m.refIdxL1[a] == m.refIdxL1[b]
}

func motionSegments(dst []motionSegment, m *mbInfo) []motionSegment {
	segs := dst[:0]
	var used [16]bool
	for by := 0; by < 16; by += 4 {
		for bx := 0; bx < 16; bx += 4 {
			z := zscanOf[by>>2][bx>>2]
			if used[z] {
				continue
			}
			w := 4
			for bx+w < 16 {
				nz := zscanOf[by>>2][(bx+w)>>2]
				if used[nz] || !sameSegmentMotion(m, z, nz) {
					break
				}
				w += 4
			}
			h := 4
			for by+h < 16 {
				same := true
				for i := 0; i < w; i += 4 {
					nz := zscanOf[(by+h)>>2][(bx+i)>>2]
					if used[nz] || !sameSegmentMotion(m, z, nz) {
						same = false
						break
					}
				}
				if !same {
					break
				}
				h += 4
			}
			for yy := by; yy < by+h; yy += 4 {
				for xx := bx; xx < bx+w; xx += 4 {
					used[zscanOf[yy>>2][xx>>2]] = true
				}
			}
			segs = append(segs, motionSegment{
				x: bx, y: by, w: w, h: h,
				mv:  [2][2]int16{m.MvL0[z], m.MvL1[z]},
				ref: [2]int8{m.refIdx[z], m.refIdxL1[z]},
			})
		}
	}
	return segs
}

func clampComponent(v, lo, hi, fracBits int) int {
	frac := v & (1<<uint(fracBits) - 1)
	iv := v >> uint(fracBits)
	if iv < lo {
		iv = lo
	}
	if iv > hi {
		iv = hi
	}
	return iv<<uint(fracBits) | frac
}

const chromaTapAfter = 1

type predBuffer struct {
	y  [256]byte
	cb [64]byte
	cr [64]byte
}

func (s *mbEncoder) predictOneB(list int, seg motionSegment, x, y int,
	dstY []byte, strideY, offY int, dstCb, dstCr []byte, strideC, offC int) bool {

	ref := s.refPictureIn(list, seg.ref[list])
	if ref == nil {
		return false
	}
	mv := seg.mv[list]
	mvx := clampComponent(int(mv[0]),
		lumaTapBefore-frame.LumaMargin-x,
		ref.Width+frame.LumaMargin-lumaTapAfter-x-seg.w, 2)
	mvy := clampComponent(int(mv[1]),
		lumaTapBefore-frame.LumaMargin-y,
		ref.Height+frame.LumaMargin-lumaTapAfter-y-seg.h, 2)
	s.predictLuma(dstY, strideY, offY, ref, ref.LumaOffset(x, y), seg.w, seg.h, mvx, mvy)

	cx, cy := x/2, y/2
	cw, ch := seg.w/2, seg.h/2
	cmvx := clampComponent(int(mv[0]),
		-frame.ChromaMargin-cx,
		ref.Width/2+frame.ChromaMargin-chromaTapAfter-cx-cw, 3)
	cmvy := clampComponent(int(mv[1]),
		-frame.ChromaMargin-cy,
		ref.Height/2+frame.ChromaMargin-chromaTapAfter-cy-ch, 3)
	mc.PredictChroma(dstCb, strideC, offC, ref.Cb, ref.StrideC, ref.ChromaOffset(cx, cy),
		cw, ch, cmvx, cmvy)
	mc.PredictChroma(dstCr, strideC, offC, ref.Cr, ref.StrideC, ref.ChromaOffset(cx, cy),
		cw, ch, cmvx, cmvy)
	return true
}

func (s *mbEncoder) compensateB() {
	rec := s.e.rec
	baseX, baseY := s.mbx*16, s.mby*16
	s.segs = motionSegments(s.segs, s.cur)
	for _, seg := range s.segs {
		x, y := baseX+seg.x, baseY+seg.y
		use0 := seg.ref[0] >= 0
		use1 := seg.ref[1] >= 0
		switch {
		case use0 && use1:
			a, b := &s.predA, &s.predB
			okA := s.predictOneB(0, seg, x, y, a.y[:], 16, 0, a.cb[:], a.cr[:], 8, 0)
			okB := s.predictOneB(1, seg, x, y, b.y[:], 16, 0, b.cb[:], b.cr[:], 8, 0)
			if !okA || !okB {
				continue
			}
			s.combineBi(seg, x, y)
		case use0, use1:
			list := 0
			if use1 {
				list = 1
			}
			if s.predictOneB(list, seg, x, y, rec.Y, rec.StrideY, rec.LumaOffset(x, y),
				rec.Cb, rec.Cr, rec.StrideC, rec.ChromaOffset(x/2, y/2)) {
				s.weightUniRegion(list, seg.ref[list], x, y, seg.w, seg.h)
			}
		}
	}
}

type bPartResult struct {
	mv   [2][2]int16
	mvd  [2][2]int16
	ref  [2]int8
	pred uint8
	cost int
}

type bCandidate struct {
	typeIdx int
	kind    int
	t8      bool
	parts   []bPartResult
	subs    []bSubResult
}

func (s *mbEncoder) numRefsFor(list int) int {
	if list == 0 {
		return s.numRefs
	}
	return s.numRefsL1
}

func (s *mbEncoder) biSATD(p partition, mv [2][2]int16, ref [2]int8) int {
	ref0 := s.refPictureIn(0, ref[0])
	ref1 := s.refPictureIn(1, ref[1])
	if ref0 == nil || ref1 == nil {
		return 1 << 30
	}
	x, y := s.mbx*16+p.x, s.mby*16+p.y
	s.predictLuma(s.scratch[:], 16, 0, ref0, ref0.LumaOffset(x, y),
		p.w, p.h, int(mv[0][0]), int(mv[0][1]))
	s.predictLuma(s.scratchB[:], 16, 0, ref1, ref1.LumaOffset(x, y),
		p.w, p.h, int(mv[1][0]), int(mv[1][1]))
	s.combineLumaBi(s.scratch[:], 16, 0, s.scratch[:], 16, 0, s.scratchB[:], 16, 0,
		p.w, p.h, ref)
	return satdBlock(s.e.src.Y, s.e.src.StrideY, s.e.src.LumaOffset(x, y), s.scratch[:], 16, 0, p.w, p.h)
}

func (s *mbEncoder) refIdxBits(list int, ref int8, lambda int) int {
	n := s.numRefsFor(list)
	if n <= 1 {
		return 0
	}
	return lambda * bitsForTE(uint32(ref), uint32(n-1))
}

func (s *mbEncoder) searchBPartition(p partition, partIdx, kind, lambda int) bPartResult {
	var uni [2]partResult
	for list := 0; list < 2; list++ {
		uni[list] = s.searchPartitionList(list, p, partIdx, kind, lambda)
	}

	best := bPartResult{pred: predL0, cost: uni[0].cost}
	best.mv[0], best.mvd[0], best.ref = uni[0].mv, uni[0].mvd, [2]int8{uni[0].ref, -1}
	if uni[1].cost < best.cost {
		best = bPartResult{pred: predL1, cost: uni[1].cost}
		best.mv[1], best.mvd[1], best.ref = uni[1].mv, uni[1].mvd, [2]int8{-1, uni[1].ref}
	}

	mv := [2][2]int16{uni[0].mv, uni[1].mv}
	ref := [2]int8{uni[0].ref, uni[1].ref}
	biCost := s.biSATD(p, mv, ref)
	for list := 0; list < 2; list++ {
		biCost += lambda * (bitsForSE(int(uni[list].mvd[0])) + bitsForSE(int(uni[list].mvd[1])))
		biCost += s.refIdxBits(list, ref[list], lambda)
	}
	if biCost < best.cost {
		best = bPartResult{pred: predBi, cost: biCost}
		best.mv = mv
		best.mvd = [2][2]int16{uni[0].mvd, uni[1].mvd}
		best.ref = ref
	}
	return best
}

func (s *mbEncoder) storeBPartition(p partition, r bPartResult) {
	for list := 0; list < 2; list++ {
		if r.pred&(1<<uint(list)) == 0 {
			s.storeMotionIn(list, p.x, p.y, p.w, p.h, [2]int16{}, -1)
			continue
		}
		s.storeMotionIn(list, p.x, p.y, p.w, p.h, r.mv[list], r.ref[list])
		s.storeMVDIn(list, p.x, p.y, p.w, p.h, r.mvd[list])
	}
}

func (s *mbEncoder) searchBShape(kind, lambda int) bCandidate {
	parts := bPartitionsFor(kind)
	results := make([]bPartResult, len(parts))
	s.clearMotion()
	for i, p := range parts {
		r := s.searchBPartition(p, i, kind, lambda)
		s.storeBPartition(p, r)
		results[i] = r
	}
	var pred [2]uint8
	for i := range results {
		pred[i] = results[i].pred
	}
	return bCandidate{typeIdx: bMBTypeValue(kind, pred), kind: kind, parts: results}
}

func (s *mbEncoder) applyBCandidate(c bCandidate) {
	s.reset()
	s.cur.kind = c.kind
	s.cur.bTypeIdx = c.typeIdx
	s.cur.Transform8x8 = c.t8
	s.cur.Intra = false
	s.cur.skipped = false
	s.bparts = c.parts
	s.bsubs = c.subs
	if c.kind == mbTypeB8x8 {
		s.applyB8x8(c)
		s.compensateB()
		s.quantiseInterLuma()
		s.quantiseInterChroma()
		s.reconstructInterLuma()
		s.reconstructInterChroma()
		return
	}
	s.clearMotion()
	parts := bPartitionsFor(c.kind)
	for list := 0; list < 2; list++ {
		for i, p := range parts {
			if c.parts[i].pred&(1<<uint(list)) == 0 {
				continue
			}
			s.storeRefIdxIn(list, p.x, p.y, p.w, p.h, c.parts[i].ref[list])
		}
	}
	for list := 0; list < 2; list++ {
		for i, p := range parts {
			if c.parts[i].pred&(1<<uint(list)) == 0 {
				s.storeMotionIn(list, p.x, p.y, p.w, p.h, [2]int16{}, -1)
				continue
			}
			s.storeMotionIn(list, p.x, p.y, p.w, p.h, c.parts[i].mv[list], c.parts[i].ref[list])
			s.storeMVDIn(list, p.x, p.y, p.w, p.h, c.parts[i].mvd[list])
		}
	}
	s.compensateB()
	s.quantiseInterLuma()
	s.quantiseInterChroma()
	s.reconstructInterLuma()
	s.reconstructInterChroma()
}

func (s *mbEncoder) evaluateBCandidate(c bCandidate, lambda float64) (float64, error) {
	s.applyBCandidate(c)
	ssd := s.mbSSD()
	if s.cb != nil {
		n := s.trialBitsCABAC(func() {
			s.cb.MBSkipFlagB(s.skipFlagInc(), false)
			s.writeBMBCABAC()
		})
		return rdCost(ssd, n, lambda), nil
	}
	n, err := s.trialBits(s.writeBMB)
	if err != nil {
		return 0, err
	}
	return rdCost(ssd, n, lambda), nil
}

func (s *mbEncoder) applyBDirectMotion() {
	s.clearMotion()
	s.directMotion(0, 0, 16, 16)
	s.markDirect(0, 0, 16, 16)
}

func (s *mbEncoder) applyBSkip() {
	s.reset()
	s.cur.kind = mbTypeBSkip
	s.cur.bTypeIdx = 0
	s.cur.Intra = false
	s.cur.skipped = true
	s.cur.cbpLuma = 0
	s.cur.cbpChroma = 0
	s.cur.NzY = [16]uint8{}
	s.cur.nzCb = [4]uint8{}
	s.cur.nzCr = [4]uint8{}
	s.applyBDirectMotion()
	s.compensateB()
}

func (s *mbEncoder) applyBDirect(t8 bool) {
	s.reset()
	s.cur.kind = mbTypeBDirect
	s.cur.bTypeIdx = 0
	s.cur.Transform8x8 = t8
	s.cur.Intra = false
	s.cur.skipped = false
	s.applyBDirectMotion()
	s.compensateB()
	s.quantiseInterLuma()
	s.quantiseInterChroma()
	s.reconstructInterLuma()
	s.reconstructInterChroma()
}

func (s *mbEncoder) evaluateBSkip(lambda float64) float64 {
	s.applyBSkip()
	bits := 1
	if s.cb != nil {
		bits = s.trialBitsCABAC(func() { s.cb.MBSkipFlagB(s.skipFlagInc(), true) })
	}
	return rdCost(s.mbSSD(), bits, lambda)
}

func (s *mbEncoder) evaluateBDirect(t8 bool, lambda float64) (float64, error) {
	s.applyBDirect(t8)
	ssd := s.mbSSD()
	if s.cb != nil {
		n := s.trialBitsCABAC(func() {
			s.cb.MBSkipFlagB(s.skipFlagInc(), false)
			s.writeBMBCABAC()
		})
		return rdCost(ssd, n, lambda), nil
	}
	n, err := s.trialBits(s.writeBMB)
	if err != nil {
		return 0, err
	}
	return rdCost(ssd, n, lambda), nil
}

func (s *mbEncoder) evaluateBIntra(lambda float64) (float64, error) {
	s.encodeIntraModes()
	ssd := s.mbSSD()
	if s.cb != nil {
		n := s.trialBitsCABAC(func() {
			s.cb.MBSkipFlagB(s.skipFlagInc(), false)
			s.writeIntraMBCABACInB()
		})
		return rdCost(ssd, n, lambda), nil
	}
	n, err := s.trialBits(func() error { return s.writeIntraMB(23) })
	if err != nil {
		return 0, err
	}
	return rdCost(ssd, n, lambda), nil
}

func (s *mbEncoder) bShapes() []int {
	if s.e.cfg.MotionSearch == MotionSearchZero {
		return nil
	}
	return []int{mbTypeB16x16, mbTypeB16x8, mbTypeB8x16, mbTypeB8x8}
}

func (s *mbEncoder) decideBMB() (int, bCandidate, error) {
	rdLambda := lambdaRDTable[s.qpY]
	satdLambda := lambdaTable[s.qpY]

	bestCost, err := s.evaluateBDirect(false, rdLambda)
	if err != nil {
		return 0, bCandidate{}, err
	}
	bestChoice := choiceDirect
	var bestCand bCandidate

	if s.allowsB8x8(mbTypeBDirect, nil) {
		j, err := s.evaluateBDirect(true, rdLambda)
		if err != nil {
			return 0, bCandidate{}, err
		}
		if j < bestCost {
			bestCost, bestChoice, bestCand = j, choiceDirect, bCandidate{kind: mbTypeBDirect, t8: true}
		}
	}

	for _, kind := range s.bShapes() {
		var c bCandidate
		if kind == mbTypeB8x8 {
			c = s.searchB8x8(satdLambda)
		} else {
			c = s.searchBShape(kind, satdLambda)
		}
		j, err := s.evaluateBCandidate(c, rdLambda)
		if err != nil {
			return 0, bCandidate{}, err
		}
		if j < bestCost {
			bestCost, bestChoice, bestCand = j, choiceInter, c
		}
		if !s.allowsB8x8(kind, c.subs) {
			continue
		}
		c8 := c
		c8.t8 = true
		j8, err := s.evaluateBCandidate(c8, rdLambda)
		if err != nil {
			return 0, bCandidate{}, err
		}
		if j8 < bestCost {
			bestCost, bestChoice, bestCand = j8, choiceInter, c8
		}
	}

	if j := s.evaluateBSkip(rdLambda); j < bestCost {
		bestCost, bestChoice = j, choiceSkip
	}

	s.clearMotion()
	j, err := s.evaluateBIntra(rdLambda)
	if err != nil {
		return 0, bCandidate{}, err
	}
	if j < bestCost {
		bestChoice = choiceIntra
	}
	return bestChoice, bestCand, nil
}

func (s *mbEncoder) applyBChoice(choice int, cand bCandidate) {
	switch choice {
	case choiceSkip:
		s.applyBSkip()
	case choiceDirect:
		s.applyBDirect(cand.t8)
	default:
		s.applyBCandidate(cand)
	}
}

func (s *mbEncoder) encodeBMB() (bool, error) {
	choice, cand, err := s.decideBMB()
	if err != nil {
		return false, err
	}
	switch choice {
	case choiceSkip:
		s.holdQP()
		s.applyBSkip()
		s.cur.QPY = s.prevQP
		return true, nil
	case choiceIntra:
		s.w.WriteUE(s.pendingSkipRun)
		s.pendingSkipRun = 0
		return false, s.writeIntraMB(23)
	}
	s.applyBChoice(choice, cand)
	s.w.WriteUE(s.pendingSkipRun)
	s.pendingSkipRun = 0
	return false, s.writeBMB()
}

func (s *mbEncoder) writeRefIdxIn(list int, ref int8) {
	if s.numRefsFor(list) <= 1 {
		return
	}
	s.w.WriteTE(uint32(ref), uint32(s.numRefsFor(list)-1))
}

func (s *mbEncoder) writeBMB() error {
	s.w.WriteUE(uint32(s.cur.bTypeIdx))
	if s.cur.kind == mbTypeB8x8 {
		s.writeB8x8()
	} else if parts := bPartitionsFor(s.cur.kind); parts != nil {
		for list := 0; list < 2; list++ {
			for i := range parts {
				if s.bparts[i].pred&(1<<uint(list)) != 0 {
					s.writeRefIdxIn(list, s.bparts[i].ref[list])
				}
			}
		}
		for list := 0; list < 2; list++ {
			for i := range parts {
				if s.bparts[i].pred&(1<<uint(list)) != 0 {
					s.w.WriteSE(int32(s.bparts[i].mvd[list][0]))
					s.w.WriteSE(int32(s.bparts[i].mvd[list][1]))
				}
			}
		}
	}
	cbp := uint8(s.cur.cbpLuma | s.cur.cbpChroma<<4)
	s.w.WriteUE(interCBPToGolomb[cbp])
	s.writeTransformSize8x8()
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
