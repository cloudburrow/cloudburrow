package decoder

import (
	"errors"

	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/frame"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/loopfilter"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/pred"
)

var (
	ErrUnsupported = errors.New("go264/decoder: unsupported bitstream feature")
	ErrCorrupt     = errors.New("go264/decoder: corrupt macroblock data")
	ErrOverLimit   = errors.New("go264/decoder: bitstream asks for more than the decoder allows")
)

type mbState struct {
	loopfilter.MB
	kind        int
	nzCb        [4]uint8
	nzCr        [4]uint8
	intra4Modes [16]int8
	intra16Mode int8
	chromaMode  int8
	cbpLuma     int
	cbpChroma   int
	cbfLumaDC   bool
	cbfChromaDC [2]bool

	refIdxL0 [16]int8
	refIdxL1 [16]int8
	mvdL0    [16][2]uint8
	mvdL1    [16][2]uint8

	directBlk [16]bool
}

func luma8x8Availability(i8 int, n neighbours, constrained bool) pred.Availability {
	var a pred.Availability
	if i8%2 == 1 || usableForIntra(n.left, constrained) {
		a |= pred.AvailLeft
	}
	if i8/2 == 1 || usableForIntra(n.top, constrained) {
		a |= pred.AvailTop
	}
	switch i8 {
	case 0:
		if usableForIntra(n.topLeft, constrained) {
			a |= pred.AvailTopLeft
		}
		if usableForIntra(n.top, constrained) {
			a |= pred.AvailTopRight
		}
	case 1:
		if usableForIntra(n.top, constrained) {
			a |= pred.AvailTopLeft
		}
		if usableForIntra(n.topRight, constrained) {
			a |= pred.AvailTopRight
		}
	case 2:
		if usableForIntra(n.left, constrained) {
			a |= pred.AvailTopLeft
		}
		a |= pred.AvailTopRight
	default:
		a |= pred.AvailTopLeft
	}
	return a
}

var topRightInsideMB = [16]bool{
	false, false, true, false,
	false, false, true, false,
	true, true, true, false,
	true, false, true, false,
}

type mbGrid struct {
	mbs       []mbState
	widthMBs  int
	heightMBs int
}

func newMBGrid(widthMBs, heightMBs int) *mbGrid {
	return &mbGrid{
		mbs:       make([]mbState, widthMBs*heightMBs),
		widthMBs:  widthMBs,
		heightMBs: heightMBs,
	}
}

func (g *mbGrid) at(mbx, mby int) *mbState {
	if mbx < 0 || mby < 0 || mbx >= g.widthMBs || mby >= g.heightMBs {
		return nil
	}
	return &g.mbs[mby*g.widthMBs+mbx]
}

func (g *mbGrid) neighbour(mbx, mby, dx, dy, sliceID int) *mbState {
	m := g.at(mbx+dx, mby+dy)
	if m == nil || !m.Decoded || m.SliceID != sliceID {
		return nil
	}
	return m
}

type neighbours struct {
	left     *mbState
	top      *mbState
	topLeft  *mbState
	topRight *mbState
}

func (g *mbGrid) around(mbx, mby, sliceID int) neighbours {
	return neighbours{
		left:     g.neighbour(mbx, mby, -1, 0, sliceID),
		top:      g.neighbour(mbx, mby, 0, -1, sliceID),
		topLeft:  g.neighbour(mbx, mby, -1, -1, sliceID),
		topRight: g.neighbour(mbx, mby, 1, -1, sliceID),
	}
}

func (g *mbGrid) motion() *frame.Motion {
	mo := frame.NewMotion(g.widthMBs, g.heightMBs)
	for mby := 0; mby < g.heightMBs; mby++ {
		for mbx := 0; mbx < g.widthMBs; mbx++ {
			m := g.at(mbx, mby)
			if m == nil || m.Intra || !m.Decoded {
				continue
			}
			for blk := 0; blk < 16; blk++ {
				bx := mbx*4 + blockX[blk]/4
				by := mby*4 + blockY[blk]/4
				i := mo.Index(bx, by)
				if r := m.refIdxL0[blk]; r >= 0 {
					mo.Mv[0][i] = m.MvL0[blk]
					mo.RefIdx[0][i] = r
					if p := m.RefPicL0[blk]; p != nil {
						mo.RefPOC[0][i] = int32(p.POC)
					}
				}
				if r := m.refIdxL1[blk]; r >= 0 {
					mo.Mv[1][i] = m.MvL1[blk]
					mo.RefIdx[1][i] = r
					if p := m.RefPicL1[blk]; p != nil {
						mo.RefPOC[1][i] = int32(p.POC)
					}
				}
			}
		}
	}
	return mo
}

func usableForIntra(m *mbState, constrainedIntraPred bool) bool {
	if m == nil {
		return false
	}
	if constrainedIntraPred && !m.Intra {
		return false
	}
	return true
}

func lumaAvailability(blk int, n neighbours, constrained bool) pred.Availability {
	x, y := blockX[blk], blockY[blk]
	var a pred.Availability
	if x > 0 || usableForIntra(n.left, constrained) {
		a |= pred.AvailLeft
	}
	if y > 0 || usableForIntra(n.top, constrained) {
		a |= pred.AvailTop
	}
	switch {
	case x > 0 && y > 0:
		a |= pred.AvailTopLeft
	case x == 0 && y > 0:
		if usableForIntra(n.left, constrained) {
			a |= pred.AvailTopLeft
		}
	case x > 0 && y == 0:
		if usableForIntra(n.top, constrained) {
			a |= pred.AvailTopLeft
		}
	default:
		if usableForIntra(n.topLeft, constrained) {
			a |= pred.AvailTopLeft
		}
	}
	switch {
	case y > 0:
		if topRightInsideMB[blk] {
			a |= pred.AvailTopRight
		}
	case x < 12:
		if usableForIntra(n.top, constrained) {
			a |= pred.AvailTopRight
		}
	default:
		if usableForIntra(n.topRight, constrained) {
			a |= pred.AvailTopRight
		}
	}
	return a
}

func mbAvailability(n neighbours, constrained bool) pred.Availability {
	var a pred.Availability
	if usableForIntra(n.left, constrained) {
		a |= pred.AvailLeft
	}
	if usableForIntra(n.top, constrained) {
		a |= pred.AvailTop
	}
	if usableForIntra(n.topLeft, constrained) {
		a |= pred.AvailTopLeft
	}
	if usableForIntra(n.topRight, constrained) {
		a |= pred.AvailTopRight
	}
	return a
}

func nonZeroLuma(m *mbState, blk int) (int, bool) {
	if m == nil {
		return 0, false
	}
	if m.IPCM {
		return 16, true
	}
	return int(m.NzY[blk]), true
}

func nonZeroChroma(m *mbState, plane, blk int) (int, bool) {
	if m == nil {
		return 0, false
	}
	if m.IPCM {
		return 16, true
	}
	if plane == 0 {
		return int(m.nzCb[blk]), true
	}
	return int(m.nzCr[blk]), true
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

func (d *sliceDecoder) lumaNC(blk int) int {
	x, y := blockX[blk], blockY[blk]
	var nA, nB int
	var okA, okB bool
	if x > 0 {
		nA, okA = nonZeroLuma(d.cur, loopfilter.BlkIdxAt(x-4, y))
	} else {
		nA, okA = nonZeroLuma(d.nb.left, loopfilter.BlkIdxAt(12, y))
	}
	if y > 0 {
		nB, okB = nonZeroLuma(d.cur, loopfilter.BlkIdxAt(x, y-4))
	} else {
		nB, okB = nonZeroLuma(d.nb.top, loopfilter.BlkIdxAt(x, 12))
	}
	return combineNC(nA, okA, nB, okB)
}

func (d *sliceDecoder) chromaNC(plane, blk int) int {
	x, y := chromaBlockX[blk], chromaBlockY[blk]
	var nA, nB int
	var okA, okB bool
	if x > 0 {
		nA, okA = nonZeroChroma(d.cur, plane, blk-1)
	} else {
		nA, okA = nonZeroChroma(d.nb.left, plane, blk+1)
	}
	if y > 0 {
		nB, okB = nonZeroChroma(d.cur, plane, blk-2)
	} else {
		nB, okB = nonZeroChroma(d.nb.top, plane, blk+2)
	}
	return combineNC(nA, okA, nB, okB)
}

func (d *Decoder) freshGrid(widthMBs, heightMBs int) *mbGrid {
	if g := d.grid; g != nil && g.widthMBs == widthMBs && g.heightMBs == heightMBs {
		clear(g.mbs)
		return g
	}
	return newMBGrid(widthMBs, heightMBs)
}
