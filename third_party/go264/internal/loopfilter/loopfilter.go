package loopfilter

import (
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/deblock"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/frame"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/parallel"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/syntax"
)

type MB struct {
	Decoded        bool
	Intra          bool
	IPCM           bool
	QPY            int
	SliceID        int
	NzY            [16]uint8
	MvL0           [16][2]int16
	RefPicL0       [16]*frame.Picture
	MvL1           [16][2]int16
	RefPicL1       [16]*frame.Picture
	Bipredictive   bool
	Transform8x8   bool
	ChromaQPOffset [2]int
	DisableDeblock uint32
	AlphaOffset    int
	BetaOffset     int
}

var blkIdxGrid = [4][4]int{
	{0, 1, 4, 5},
	{2, 3, 6, 7},
	{8, 9, 12, 13},
	{10, 11, 14, 15},
}

func BlkIdxAt(x, y int) int { return blkIdxGrid[y>>2][x>>2] }

func (m *MB) filterQPY() int {
	if m.IPCM {
		return 0
	}
	return m.QPY
}

func lumaNonZero(m *MB, blk int) bool {
	if m.IPCM {
		return true
	}
	if m.Transform8x8 {
		base := blk &^ 3
		return m.NzY[base]|m.NzY[base+1]|m.NzY[base+2]|m.NzY[base+3] != 0
	}
	return m.NzY[blk] != 0
}

func mvApart(a, b [2]int16) bool {
	dx := int(a[0]) - int(b[0])
	dy := int(a[1]) - int(b[1])
	return dx <= -4 || dx >= 4 || dy <= -4 || dy >= 4
}

func motionDiffers(p, q *MB, pBlk, qBlk int) bool {
	v := p.RefPicL0[pBlk] != q.RefPicL0[qBlk]
	if !v && p.RefPicL0[pBlk] != nil {
		v = mvApart(p.MvL0[pBlk], q.MvL0[qBlk])
	}
	if !p.Bipredictive && !q.Bipredictive {
		return v
	}
	if !v {
		v = p.RefPicL1[pBlk] != q.RefPicL1[qBlk] || mvApart(p.MvL1[pBlk], q.MvL1[qBlk])
	}
	if !v {
		return false
	}
	if p.RefPicL0[pBlk] != q.RefPicL1[qBlk] || p.RefPicL1[pBlk] != q.RefPicL0[qBlk] {
		return true
	}
	return mvApart(p.MvL0[pBlk], q.MvL1[qBlk]) || mvApart(p.MvL1[pBlk], q.MvL0[qBlk])
}

func edgeStrength(p, q *MB, pBlk, qBlk int, mbEdge bool) uint8 {
	if p.Intra || q.Intra {
		if mbEdge {
			return 4
		}
		return 3
	}
	if lumaNonZero(p, pBlk) || lumaNonZero(q, qBlk) {
		return 2
	}
	if motionDiffers(p, q, pBlk, qBlk) {
		return 1
	}
	return 0
}

type edgeContext struct {
	at  func(mbx, mby int) *MB
	pic *frame.Picture
}

func Apply(pic *frame.Picture, widthMBs, heightMBs int, at func(mbx, mby int) *MB) {
	if pic == nil || at == nil {
		return
	}
	ctx := edgeContext{at: at, pic: pic}
	parallel.Wavefront(heightMBs, widthMBs, 2, ctx.filterMB)
}

func (c *edgeContext) neighbourFor(m *MB, mbx, mby, dx, dy int) *MB {
	n := c.at(mbx+dx, mby+dy)
	if n == nil || !n.Decoded {
		return nil
	}
	if m.DisableDeblock == 2 && n.SliceID != m.SliceID {
		return nil
	}
	return n
}

func (c *edgeContext) filterMB(mbx, mby int) {
	cur := c.at(mbx, mby)
	if cur == nil || !cur.Decoded || cur.DisableDeblock == 1 {
		return
	}
	left := c.neighbourFor(cur, mbx, mby, -1, 0)
	top := c.neighbourFor(cur, mbx, mby, 0, -1)

	step := 1
	if cur.Transform8x8 {
		step = 2
	}
	for e := 0; e < 4; e += step {
		c.verticalEdge(cur, left, mbx, mby, e)
	}
	for e := 0; e < 4; e += step {
		c.horizontalEdge(cur, top, mbx, mby, e)
	}
}

func (c *edgeContext) verticalEdge(cur, left *MB, mbx, mby, e int) {
	p := cur
	mbEdge := e == 0
	if mbEdge {
		if left == nil {
			return
		}
		p = left
	}
	x := e * 4
	var bs [4]uint8
	any := false
	for g := 0; g < 4; g++ {
		y := g * 4
		qBlk := BlkIdxAt(x, y)
		var pBlk int
		if mbEdge {
			pBlk = BlkIdxAt(12, y)
		} else {
			pBlk = BlkIdxAt(x-4, y)
		}
		bs[g] = edgeStrength(p, cur, pBlk, qBlk, mbEdge)
		if bs[g] != 0 {
			any = true
		}
	}
	if !any {
		return
	}
	qpAv := deblock.AverageQP(p.filterQPY(), cur.filterQPY())
	ia := deblock.IndexA(qpAv, cur.AlphaOffset)
	ib := deblock.IndexB(qpAv, cur.BetaOffset)
	deblock.FilterLumaEdgeVertical(c.pic.Y, c.pic.StrideY,
		c.pic.LumaOffset(mbx*16+x, mby*16), bs, ia, ib)

	if e != 0 && e != 2 {
		return
	}
	cx := mbx*8 + e*2
	offsets := cur.ChromaQPOffset
	planes := [2][]byte{c.pic.Cb, c.pic.Cr}
	for plane := 0; plane < 2; plane++ {
		qpc := deblock.AverageQP(
			syntax.ChromaQP(p.filterQPY(), offsets[plane]),
			syntax.ChromaQP(cur.filterQPY(), offsets[plane]))
		deblock.FilterChromaEdgeVertical(planes[plane], c.pic.StrideC,
			c.pic.ChromaOffset(cx, mby*8), bs,
			deblock.IndexA(qpc, cur.AlphaOffset), deblock.IndexB(qpc, cur.BetaOffset))
	}
}

func (c *edgeContext) horizontalEdge(cur, top *MB, mbx, mby, e int) {
	p := cur
	mbEdge := e == 0
	if mbEdge {
		if top == nil {
			return
		}
		p = top
	}
	y := e * 4
	var bs [4]uint8
	any := false
	for g := 0; g < 4; g++ {
		x := g * 4
		qBlk := BlkIdxAt(x, y)
		var pBlk int
		if mbEdge {
			pBlk = BlkIdxAt(x, 12)
		} else {
			pBlk = BlkIdxAt(x, y-4)
		}
		bs[g] = edgeStrength(p, cur, pBlk, qBlk, mbEdge)
		if bs[g] != 0 {
			any = true
		}
	}
	if !any {
		return
	}
	qpAv := deblock.AverageQP(p.filterQPY(), cur.filterQPY())
	deblock.FilterLumaEdgeHorizontal(c.pic.Y, c.pic.StrideY,
		c.pic.LumaOffset(mbx*16, mby*16+y), bs,
		deblock.IndexA(qpAv, cur.AlphaOffset), deblock.IndexB(qpAv, cur.BetaOffset))

	if e != 0 && e != 2 {
		return
	}
	cy := mby*8 + e*2
	offsets := cur.ChromaQPOffset
	planes := [2][]byte{c.pic.Cb, c.pic.Cr}
	for plane := 0; plane < 2; plane++ {
		qpc := deblock.AverageQP(
			syntax.ChromaQP(p.filterQPY(), offsets[plane]),
			syntax.ChromaQP(cur.filterQPY(), offsets[plane]))
		deblock.FilterChromaEdgeHorizontal(planes[plane], c.pic.StrideC,
			c.pic.ChromaOffset(mbx*8, cy), bs,
			deblock.IndexA(qpc, cur.AlphaOffset), deblock.IndexB(qpc, cur.BetaOffset))
	}
}
