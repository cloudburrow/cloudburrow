package encoder

import (
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/loopfilter"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/pred"
)

const (
	mbTypeINxN = iota
	mbTypeI16x16
	mbTypePSkip
	mbTypeP16x16
	mbTypeP16x8
	mbTypeP8x16
	mbTypeP8x8
)

type mbInfo struct {
	loopfilter.MB
	kind        int
	nzCb        [4]uint8
	nzCr        [4]uint8
	intra4Modes [16]int8
	intra16Mode int8
	chromaMode  int8
	cbpLuma     int
	cbpChroma   int
	refIdx      [16]int8
	refIdxL1    [16]int8

	mvdL0       [16][2]uint8
	mvdL1       [16][2]uint8
	directBlk   [16]bool
	bTypeIdx    int
	cbfLumaDC   bool
	cbfChromaDC [2]bool
	skipped     bool
}

func (m *mbInfo) refIdxOf(list, blk int) int8 {
	if list == 0 {
		return m.refIdx[blk]
	}
	return m.refIdxL1[blk]
}

func (m *mbInfo) mvOf(list, blk int) [2]int16 {
	if list == 0 {
		return m.MvL0[blk]
	}
	return m.MvL1[blk]
}

func (m *mbInfo) mvdOf(list, blk int) [2]uint8 {
	if list == 0 {
		return m.mvdL0[blk]
	}
	return m.mvdL1[blk]
}

var blockX = [16]int{0, 4, 0, 4, 8, 12, 8, 12, 0, 4, 0, 4, 8, 12, 8, 12}

var blockY = [16]int{0, 0, 4, 4, 0, 0, 4, 4, 8, 8, 12, 12, 8, 8, 12, 12}

var chromaBlockX = [4]int{0, 4, 0, 4}

var chromaBlockY = [4]int{0, 0, 4, 4}

func blkIdxAt(x, y int) int { return loopfilter.BlkIdxAt(x, y) }

var topRightInsideMB = [16]bool{
	false, false, true, false,
	false, false, true, false,
	true, true, true, false,
	true, false, true, false,
}

type neighbours struct {
	left     *mbInfo
	top      *mbInfo
	topLeft  *mbInfo
	topRight *mbInfo
}

func (e *Encoder) at(mbx, mby int) *mbInfo {
	if mbx < 0 || mby < 0 || mbx >= e.widthMBs || mby >= e.heightMBs {
		return nil
	}
	return &e.grid[mby*e.widthMBs+mbx]
}

type sliceRange struct {
	firstMB int
	endMB   int
}

func (e *Encoder) coded(mbx, mby int, r sliceRange) *mbInfo {
	if mbx < 0 || mby < 0 || mbx >= e.widthMBs || mby >= e.heightMBs {
		return nil
	}
	addr := mby*e.widthMBs + mbx
	if addr < r.firstMB || addr >= r.endMB {
		return nil
	}
	m := &e.grid[addr]
	if !m.Decoded {
		return nil
	}
	return m
}

func (e *Encoder) around(mbx, mby int, r sliceRange) neighbours {
	return neighbours{
		left:     e.coded(mbx-1, mby, r),
		top:      e.coded(mbx, mby-1, r),
		topLeft:  e.coded(mbx-1, mby-1, r),
		topRight: e.coded(mbx+1, mby-1, r),
	}
}

func lumaAvailability(blk int, n neighbours) pred.Availability {
	x, y := blockX[blk], blockY[blk]
	var a pred.Availability
	if x > 0 || n.left != nil {
		a |= pred.AvailLeft
	}
	if y > 0 || n.top != nil {
		a |= pred.AvailTop
	}
	switch {
	case x > 0 && y > 0:
		a |= pred.AvailTopLeft
	case x == 0 && y > 0:
		if n.left != nil {
			a |= pred.AvailTopLeft
		}
	case x > 0 && y == 0:
		if n.top != nil {
			a |= pred.AvailTopLeft
		}
	default:
		if n.topLeft != nil {
			a |= pred.AvailTopLeft
		}
	}
	switch {
	case y > 0:
		if topRightInsideMB[blk] {
			a |= pred.AvailTopRight
		}
	case x < 12:
		if n.top != nil {
			a |= pred.AvailTopRight
		}
	default:
		if n.topRight != nil {
			a |= pred.AvailTopRight
		}
	}
	return a
}

func mbAvailability(n neighbours) pred.Availability {
	var a pred.Availability
	if n.left != nil {
		a |= pred.AvailLeft
	}
	if n.top != nil {
		a |= pred.AvailTop
	}
	if n.topLeft != nil {
		a |= pred.AvailTopLeft
	}
	if n.topRight != nil {
		a |= pred.AvailTopRight
	}
	return a
}

func combineNC(nA int, okA bool, nB int, okB bool) int {
	switch {
	case okA && okB:
		return (nA + nB + 1) >> 1
	case okA:
		return nA
	case okB:
		return nB
	}
	return 0
}

func (s *mbEncoder) lumaNC(blk int) int {
	x, y := blockX[blk], blockY[blk]
	var nA, nB int
	var okA, okB bool
	if x > 0 {
		nA, okA = int(s.cur.NzY[blkIdxAt(x-4, y)]), true
	} else if s.nb.left != nil {
		nA, okA = int(s.nb.left.NzY[blkIdxAt(12, y)]), true
	}
	if y > 0 {
		nB, okB = int(s.cur.NzY[blkIdxAt(x, y-4)]), true
	} else if s.nb.top != nil {
		nB, okB = int(s.nb.top.NzY[blkIdxAt(x, 12)]), true
	}
	return combineNC(nA, okA, nB, okB)
}

func (s *mbEncoder) chromaNC(plane, blk int) int {
	x, y := chromaBlockX[blk], chromaBlockY[blk]
	var nA, nB int
	var okA, okB bool
	pick := func(m *mbInfo, idx int) int {
		if plane == 0 {
			return int(m.nzCb[idx])
		}
		return int(m.nzCr[idx])
	}
	if x > 0 {
		nA, okA = pick(s.cur, blk-1), true
	} else if s.nb.left != nil {
		nA, okA = pick(s.nb.left, blk+1), true
	}
	if y > 0 {
		nB, okB = pick(s.cur, blk-2), true
	} else if s.nb.top != nil {
		nB, okB = pick(s.nb.top, blk+2), true
	}
	return combineNC(nA, okA, nB, okB)
}

func (s *mbEncoder) predIntra4x4Mode(blk int) int {
	x, y := blockX[blk], blockY[blk]
	mbA, mbB := s.nb.left, s.nb.top
	if x > 0 {
		mbA = s.cur
	}
	if y > 0 {
		mbB = s.cur
	}
	if mbA == nil || mbB == nil {
		return pred.I4x4DC
	}
	modeA := pred.I4x4DC
	if mbA.kind == mbTypeINxN {
		if x > 0 {
			modeA = int(mbA.intra4Modes[blkIdxAt(x-4, y)])
		} else {
			modeA = int(mbA.intra4Modes[blkIdxAt(12, y)])
		}
	}
	modeB := pred.I4x4DC
	if mbB.kind == mbTypeINxN {
		if y > 0 {
			modeB = int(mbB.intra4Modes[blkIdxAt(x, y-4)])
		} else {
			modeB = int(mbB.intra4Modes[blkIdxAt(x, 12)])
		}
	}
	if modeA < modeB {
		return modeA
	}
	return modeB
}
