package encoder

import "github.com/cloudburrow/cloudburrow/third_party/go264/internal/cabac"

func (s *mbEncoder) cabacQPDelta() {
	delta := s.qpY - s.prevQP
	s.cb.MBQPDelta(delta, s.prevQPDeltaNonZero)
	s.prevQPDeltaNonZero = delta != 0
	s.prevQP = s.qpY
	s.cur.QPY = s.qpY
}

func (s *mbEncoder) cabacHoldQP() {
	s.qpY = s.prevQP
	s.cur.QPY = s.prevQP
	s.prevQPDeltaNonZero = false
}

func (s *mbEncoder) writeResidualCABAC(i16 bool) {
	if i16 {
		a, b := s.lumaDCCBFContext()
		s.cur.cbfLumaDC = countNonZero(s.lumaDCScan[:]) != 0
		s.cb.ResidualBlock(s.lumaDCScan[:], cabac.CatIntra16x16DC, a, b, 1)
	}
	for i8 := 0; i8 < 4; i8++ {
		if s.cur.cbpLuma&(1<<uint(i8)) == 0 {
			continue
		}
		if s.cur.Transform8x8 {
			s.cb.ResidualBlock8x8(&s.luma8x8Scan[i8])
			continue
		}
		for i4 := 0; i4 < 4; i4++ {
			blk := i8*4 + i4
			a, b := s.lumaCBFContext(blk)
			cat := cabac.CatLuma4x4
			start := 0
			if i16 {
				cat = cabac.CatIntra16x16AC
				start = 1
			}
			s.cb.ResidualBlock(s.lumaScan[blk][start:], cat, a, b, 1)
		}
	}
	if s.cur.cbpChroma == 0 {
		return
	}
	for plane := 0; plane < 2; plane++ {
		a, b := s.chromaDCCBFContext(plane)
		s.cur.cbfChromaDC[plane] = countNonZero(s.chromaDC[plane][:]) != 0
		s.cb.ResidualBlock(s.chromaDC[plane][:], cabac.CatChromaDC, a, b, 1)
	}
	if s.cur.cbpChroma < 2 {
		return
	}
	for plane := 0; plane < 2; plane++ {
		for blk := 0; blk < 4; blk++ {
			a, b := s.chromaACCBFContext(plane, blk)
			s.cb.ResidualBlock(s.chromaScan[plane][blk][1:], cabac.CatChromaAC, a, b, 1)
		}
	}
}

func (s *mbEncoder) intraMBTypeValue() int {
	if s.cur.kind == mbTypeINxN {
		return 0
	}
	cbpLumaBit := 0
	if s.cur.cbpLuma != 0 {
		cbpLumaBit = 1
	}
	return 1 + int(s.cur.intra16Mode) + 4*s.cur.cbpChroma + 12*cbpLumaBit
}

func (s *mbEncoder) writeIntraModesCABAC() {
	if s.cur.Transform8x8 {
		for i8 := 0; i8 < 4; i8++ {
			s.cb.Intra4x4PredMode(s.predIntra4x4Mode(i8*4), int(s.cur.intra4Modes[i8*4]))
		}
		return
	}
	for blk := 0; blk < 16; blk++ {
		s.cb.Intra4x4PredMode(s.predIntra4x4Mode(blk), int(s.cur.intra4Modes[blk]))
	}
}

func (s *mbEncoder) writeIntraMBCABAC(inP bool) {
	mbType := s.intraMBTypeValue()
	if inP {
		s.cb.MBTypeP(mbType, true)
	} else {
		s.cb.IntraMBType(s.intraMBTypeInc(), mbType)
	}
	s.writeIntraMBBodyCABAC()
}

func (s *mbEncoder) writeIntraMBCABACInB() {
	s.cb.MBTypeB(s.bMBTypeInc(), s.intraMBTypeValue(), true)
	s.writeIntraMBBodyCABAC()
}

func (s *mbEncoder) writeIntraMBBodyCABAC() {
	if s.cur.kind == mbTypeINxN {
		if s.e.pps.Transform8x8Mode {
			s.cb.TransformSize8x8Flag(s.transform8x8Inc(), s.cur.Transform8x8)
		}
		s.writeIntraModesCABAC()
		s.cb.IntraChromaPredMode(s.chromaPredInc(), int(s.cur.chromaMode))
		left, top := s.neighbourCBP(s.nb.left), s.neighbourCBP(s.nb.top)
		s.cb.CodedBlockPatternLuma(left, top, s.cur.cbpLuma)
		s.cb.CodedBlockPatternChroma(left, top, s.cur.cbpChroma)
		if s.cur.cbpLuma != 0 || s.cur.cbpChroma != 0 {
			s.cabacQPDelta()
			s.writeResidualCABAC(false)
		} else {
			s.cabacHoldQP()
		}
		return
	}

	s.cb.IntraChromaPredMode(s.chromaPredInc(), int(s.cur.chromaMode))
	s.cabacQPDelta()
	s.writeResidualCABAC(true)
}

func (s *mbEncoder) interMBTypeValue() int {
	switch s.cur.kind {
	case mbTypeP16x8:
		return 1
	case mbTypeP8x16:
		return 2
	case mbTypeP8x8:
		return 3
	}
	return 0
}

func (s *mbEncoder) writeRefIdxCABAC(x, y int, ref int8) {
	if s.numRefs <= 1 {
		return
	}
	s.cb.RefIdx(s.refIdxInc(0, x, y), int(ref))
}

func (s *mbEncoder) writeMVDCABAC(x, y int, mvd [2]int16) {
	sumX, sumY := s.mvdNeighbourSum(0, x, y)
	s.cb.MVD(cabac.MVDHorizontal, sumX, int(mvd[0]))
	s.cb.MVD(cabac.MVDVertical, sumY, int(mvd[1]))
}

func (s *mbEncoder) writeInterMBCABAC() {
	s.cb.MBTypeP(s.interMBTypeValue(), false)

	if s.cur.kind == mbTypeP8x8 {
		for _, sub := range s.subs {
			s.cb.SubMBTypeP(sub.subType)
		}
		for i, sub := range s.subs {
			s.writeRefIdxCABAC(i%2*8, i/2*8, sub.ref)
		}
		for i, sub := range s.subs {
			ox, oy := i%2*8, i/2*8
			sp := subPartitionsOf(subMbShapes[sub.subType], ox, oy)
			for j := 0; j < sp.n; j++ {
				p := sp.items[j]
				s.writeMVDCABAC(p.x, p.y, sub.parts[j].mvd)
			}
		}
	} else {
		parts := partitionsFor(s.cur.kind)
		for i, p := range parts {
			s.writeRefIdxCABAC(p.x, p.y, s.parts[i].ref)
		}
		for i, p := range parts {
			s.writeMVDCABAC(p.x, p.y, s.parts[i].mvd)
		}
	}

	left, top := s.neighbourCBP(s.nb.left), s.neighbourCBP(s.nb.top)
	s.cb.CodedBlockPatternLuma(left, top, s.cur.cbpLuma)
	s.cb.CodedBlockPatternChroma(left, top, s.cur.cbpChroma)
	s.writeTransformSize8x8CABAC()
	if s.cur.cbpLuma != 0 || s.cur.cbpChroma != 0 {
		s.cabacQPDelta()
		s.writeResidualCABAC(false)
	} else {
		s.cabacHoldQP()
	}
}
