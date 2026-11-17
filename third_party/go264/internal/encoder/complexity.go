package encoder

import (
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/frame"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/parallel"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/simd"
)

const (
	probeRange       = 32
	probeSettled     = 256
	probeRowsPerTask = 128
)

type complexityProbe struct {
	blocksX int
	blocksY int
	stride  int
	height  int
	cur     []byte
	prev    []byte
	mvx     []int8
	mvy     []int8
	rowCost []int
	primed  bool
}

func newComplexityProbe(widthMBs, heightMBs int) *complexityProbe {
	p := &complexityProbe{blocksX: widthMBs, blocksY: heightMBs,
		stride: widthMBs * 16, height: heightMBs * 16}
	p.cur = make([]byte, p.stride*p.height)
	p.prev = make([]byte, p.stride*p.height)
	p.mvx = make([]int8, widthMBs*heightMBs)
	p.mvy = make([]int8, widthMBs*heightMBs)
	p.rowCost = make([]int, heightMBs)
	return p
}

func (p *complexityProbe) take(src *frame.Picture) {
	parallel.For(p.height, probeRowsPerTask, func(lo, hi int) {
		for y := lo; y < hi; y++ {
			copy(p.cur[y*p.stride:(y+1)*p.stride], src.Y[src.LumaOffset(0, y):])
		}
	})
}

func (p *complexityProbe) match(bx, by, mvx, mvy int) int {
	return simd.SAD(p.cur, p.stride, by*16*p.stride+bx*16,
		p.prev, p.stride, (by*16+mvy)*p.stride+bx*16+mvx, 16, 16)
}

func (p *complexityProbe) inside(bx, by, mvx, mvy int) bool {
	x, y := bx*16+mvx, by*16+mvy
	return x >= 0 && y >= 0 && x+16 <= p.stride && y+16 <= p.height
}

func (p *complexityProbe) candidates(bx, by int) [3][2]int {
	here := by*p.blocksX + bx
	out := [3][2]int{{int(p.mvx[here]), int(p.mvy[here])}}
	if bx > 0 {
		out[1] = [2]int{int(p.mvx[here-1]), int(p.mvy[here-1])}
	}
	if by > 0 {
		out[2] = [2]int{int(p.mvx[here-p.blocksX]), int(p.mvy[here-p.blocksX])}
	}
	return out
}

func (p *complexityProbe) search(bx, by int) int {
	best := p.match(bx, by, 0, 0)
	if best <= probeSettled {
		p.mvx[by*p.blocksX+bx], p.mvy[by*p.blocksX+bx] = 0, 0
		return best
	}
	bestX, bestY := 0, 0
	for _, c := range p.candidates(bx, by) {
		if c[0] == 0 && c[1] == 0 || !p.inside(bx, by, c[0], c[1]) {
			continue
		}
		if d := p.match(bx, by, c[0], c[1]); d < best {
			best, bestX, bestY = d, c[0], c[1]
		}
	}
	for step := 16; step >= 1; step /= 2 {
		for {
			moved := false
			for _, d := range [4][2]int{{-step, 0}, {step, 0}, {0, -step}, {0, step}} {
				mvx, mvy := bestX+d[0], bestY+d[1]
				if mvx < -probeRange || mvx > probeRange || mvy < -probeRange || mvy > probeRange {
					continue
				}
				if !p.inside(bx, by, mvx, mvy) {
					continue
				}
				if c := p.match(bx, by, mvx, mvy); c < best {
					best, bestX, bestY, moved = c, mvx, mvy, true
				}
			}
			if !moved {
				break
			}
		}
	}
	p.mvx[by*p.blocksX+bx], p.mvy[by*p.blocksX+bx] = int8(bestX), int8(bestY)
	return best
}

func (p *complexityProbe) alone(bx, by int) int {
	off := by*16*p.stride + bx*16
	sum := 0
	for y := 0; y < 16; y++ {
		row := p.cur[off+y*p.stride:]
		for x := 0; x < 16; x++ {
			sum += int(row[x])
		}
	}
	mean := (sum + 128) / 256
	cost := 0
	for y := 0; y < 16; y++ {
		row := p.cur[off+y*p.stride:]
		for x := 0; x < 16; x++ {
			d := int(row[x]) - mean
			if d < 0 {
				d = -d
			}
			cost += d
		}
	}
	return cost
}

func (p *complexityProbe) measure(src *frame.Picture) (float64, bool) {
	p.take(src)
	if !p.primed {
		p.primed = true
		p.cur, p.prev = p.prev, p.cur
		return 0, false
	}
	for i := range p.rowCost {
		p.rowCost[i] = 0
	}
	parallel.Wavefront(p.blocksY, p.blocksX, 1, func(bx, by int) {
		cost := p.search(bx, by)
		if cost > probeSettled {
			if c := p.alone(bx, by); c < cost {
				cost = c
			}
		}
		p.rowCost[by] += cost
	})
	total := 0
	for _, c := range p.rowCost {
		total += c
	}
	p.cur, p.prev = p.prev, p.cur
	return float64(total), true
}
