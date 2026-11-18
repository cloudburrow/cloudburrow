package encoder

import "github.com/cloudburrow/cloudburrow/third_party/go264/internal/cabac"

func (s *mbEncoder) neighbourCBP(m *mbInfo) int {
	if m == nil {
		return cabac.CBPUnavailable
	}
	return m.cbpLuma | m.cbpChroma<<4
}

func (s *mbEncoder) intraMBTypeInc() int {
	inc := 0
	if m := s.nb.left; m != nil && m.Intra && m.kind != mbTypeINxN {
		inc++
	}
	if m := s.nb.top; m != nil && m.Intra && m.kind != mbTypeINxN {
		inc++
	}
	return inc
}

func (s *mbEncoder) chromaPredInc() int {
	inc := 0
	if m := s.nb.left; m != nil && m.Intra && m.chromaMode != 0 {
		inc++
	}
	if m := s.nb.top; m != nil && m.Intra && m.chromaMode != 0 {
		inc++
	}
	return inc
}

func (s *mbEncoder) skipFlagInc() int {
	inc := 0
	if m := s.nb.left; m != nil && !m.skipped {
		inc++
	}
	if m := s.nb.top; m != nil && !m.skipped {
		inc++
	}
	return inc
}

func (s *mbEncoder) cbfCondTerm(m *mbInfo, present, flag bool) int {
	if m == nil {
		if s.cur.Intra {
			return 1
		}
		return 0
	}
	if !present || !flag {
		return 0
	}
	return 1
}

func (m *mbInfo) hasLumaResidual() bool { return !m.skipped }

func (s *mbEncoder) lumaCBFContext(blk int) (int, int) {
	x, y := blockX[blk], blockY[blk]
	var a, b int
	if x > 0 {
		a = s.cbfCondTerm(s.cur, true, s.cur.NzY[blkIdxAt(x-4, y)] != 0)
	} else {
		m := s.nb.left
		a = s.cbfCondTerm(m, m != nil && m.hasLumaResidual(), m != nil && m.NzY[blkIdxAt(12, y)] != 0)
	}
	if y > 0 {
		b = s.cbfCondTerm(s.cur, true, s.cur.NzY[blkIdxAt(x, y-4)] != 0)
	} else {
		m := s.nb.top
		b = s.cbfCondTerm(m, m != nil && m.hasLumaResidual(), m != nil && m.NzY[blkIdxAt(x, 12)] != 0)
	}
	return a, b
}

func (s *mbEncoder) lumaDCCBFContext() (int, int) {
	a := s.cbfCondTerm(s.nb.left, s.nb.left != nil && s.nb.left.kind == mbTypeI16x16,
		s.nb.left != nil && s.nb.left.cbfLumaDC)
	b := s.cbfCondTerm(s.nb.top, s.nb.top != nil && s.nb.top.kind == mbTypeI16x16,
		s.nb.top != nil && s.nb.top.cbfLumaDC)
	return a, b
}

func (s *mbEncoder) chromaDCCBFContext(plane int) (int, int) {
	pick := func(m *mbInfo) (bool, bool) {
		if m == nil {
			return false, false
		}
		return m.cbpChroma != 0, m.cbfChromaDC[plane]
	}
	pa, fa := pick(s.nb.left)
	pb, fb := pick(s.nb.top)
	return s.cbfCondTerm(s.nb.left, pa, fa), s.cbfCondTerm(s.nb.top, pb, fb)
}

func (s *mbEncoder) chromaACCBFContext(plane, blk int) (int, int) {
	nz := func(m *mbInfo, idx int) bool {
		if plane == 0 {
			return m.nzCb[idx] != 0
		}
		return m.nzCr[idx] != 0
	}
	x, y := chromaBlockX[blk], chromaBlockY[blk]
	var a, b int
	if x > 0 {
		a = s.cbfCondTerm(s.cur, true, nz(s.cur, blk-1))
	} else {
		m := s.nb.left
		a = s.cbfCondTerm(m, m != nil && m.cbpChroma == 2, m != nil && nz(m, blk+1))
	}
	if y > 0 {
		b = s.cbfCondTerm(s.cur, true, nz(s.cur, blk-2))
	} else {
		m := s.nb.top
		b = s.cbfCondTerm(m, m != nil && m.cbpChroma == 2, m != nil && nz(m, blk+2))
	}
	return a, b
}

func (s *mbEncoder) neighbourBlock(x, y, curZ int) (*mbInfo, int) {
	switch {
	case x < 0 && y < 0:
		return s.nb.topLeft, blkIdxAt(12, 12)
	case x < 0:
		if y >= 16 {
			return nil, 0
		}
		return s.nb.left, blkIdxAt(12, y&^3)
	case y < 0:
		if x < 16 {
			return s.nb.top, blkIdxAt(x&^3, 12)
		}
		return s.nb.topRight, blkIdxAt(0, 12)
	case x >= 16 || y >= 16:
		return nil, 0
	}
	z := zscanOf[y>>2][x>>2]
	if z >= curZ {
		return nil, 0
	}
	return s.cur, z
}

func (s *mbEncoder) refIdxCondTerm(list, x, y, curZ int) bool {
	m, blk := s.neighbourBlock(x, y, curZ)
	if m == nil || m.refIdxOf(list, blk) <= 0 {
		return false
	}
	return !m.directBlk[blk]
}

func (s *mbEncoder) refIdxInc(list, x, y int) int {
	curZ := zscanOf[y>>2][x>>2]
	inc := 0
	if s.refIdxCondTerm(list, x-1, y, curZ) {
		inc++
	}
	if s.refIdxCondTerm(list, x, y-1, curZ) {
		inc += 2
	}
	return inc
}

func (s *mbEncoder) mvdNeighbourSum(list, x, y int) (int, int) {
	curZ := zscanOf[y>>2][x>>2]
	var sumX, sumY int
	for _, d := range [2][2]int{{x - 1, y}, {x, y - 1}} {
		m, blk := s.neighbourBlock(d[0], d[1], curZ)
		if m == nil || m.Intra {
			continue
		}
		mvd := m.mvdOf(list, blk)
		sumX += int(mvd[0])
		sumY += int(mvd[1])
	}
	return sumX, sumY
}

func absClip70(v int16) uint8 {
	if v < 0 {
		v = -v
	}
	if v > 70 {
		return 70
	}
	return uint8(v)
}

func (s *mbEncoder) storeMVDIn(list, x, y, w, h int, mvd [2]int16) {
	packed := [2]uint8{absClip70(mvd[0]), absClip70(mvd[1])}
	for by := y; by < y+h; by += 4 {
		for bx := x; bx < x+w; bx += 4 {
			z := zscanOf[by>>2][bx>>2]
			if list == 0 {
				s.cur.mvdL0[z] = packed
			} else {
				s.cur.mvdL1[z] = packed
			}
		}
	}
}

func (s *mbEncoder) storeMVD(x, y, w, h int, mvd [2]int16) {
	s.storeMVDIn(0, x, y, w, h, mvd)
}
