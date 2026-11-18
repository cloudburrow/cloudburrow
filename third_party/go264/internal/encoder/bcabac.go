package encoder

import "github.com/cloudburrow/cloudburrow/third_party/go264/internal/cabac"

func (s *mbEncoder) bMBTypeInc() int {
	direct := func(m *mbInfo) bool {
		return m == nil || m.kind == mbTypeBSkip || m.kind == mbTypeBDirect
	}
	inc := 0
	if !direct(s.nb.left) {
		inc++
	}
	if !direct(s.nb.top) {
		inc++
	}
	return inc
}

func (s *mbEncoder) encodeBMBCABAC() error {
	choice, cand, err := s.decideBMB()
	if err != nil {
		return err
	}
	inc := s.skipFlagInc()
	switch choice {
	case choiceSkip:
		s.cabacHoldQP()
		s.applyBSkip()
		s.cur.QPY = s.prevQP
		s.cb.MBSkipFlagB(inc, true)
		return nil
	case choiceIntra:
		s.cb.MBSkipFlagB(inc, false)
		s.writeIntraMBCABACInB()
		return s.w.Err()
	}
	s.applyBChoice(choice, cand)
	s.cb.MBSkipFlagB(inc, false)
	s.writeBMBCABAC()
	return s.w.Err()
}

func (s *mbEncoder) writeRefIdxCABACIn(list, x, y int, ref int8) {
	if s.numRefsFor(list) <= 1 {
		return
	}
	s.cb.RefIdx(s.refIdxInc(list, x, y), int(ref))
}

func (s *mbEncoder) writeMVDCABACIn(list, x, y int, mvd [2]int16) {
	sumX, sumY := s.mvdNeighbourSum(list, x, y)
	s.cb.MVD(cabac.MVDHorizontal, sumX, int(mvd[0]))
	s.cb.MVD(cabac.MVDVertical, sumY, int(mvd[1]))
}

func (s *mbEncoder) writeBMBCABAC() {
	s.cb.MBTypeB(s.bMBTypeInc(), s.cur.bTypeIdx, false)
	if s.cur.kind == mbTypeB8x8 {
		s.writeB8x8CABAC()
	} else if parts := bPartitionsFor(s.cur.kind); parts != nil {
		for list := 0; list < 2; list++ {
			for i, p := range parts {
				if s.bparts[i].pred&(1<<uint(list)) != 0 {
					s.writeRefIdxCABACIn(list, p.x, p.y, s.bparts[i].ref[list])
				}
			}
		}
		for list := 0; list < 2; list++ {
			for i, p := range parts {
				if s.bparts[i].pred&(1<<uint(list)) != 0 {
					s.writeMVDCABACIn(list, p.x, p.y, s.bparts[i].mvd[list])
				}
			}
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
