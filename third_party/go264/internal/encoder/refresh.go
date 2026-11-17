package encoder

import (
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/frame"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/pred"
)

type refreshPlan struct {
	active bool
	start  int
	end    int
	sweep  bool
}

func (p refreshPlan) inBand(mbx int) bool {
	return p.active && mbx >= p.start && mbx < p.end
}

func (p refreshPlan) constrained(mbx int) bool {
	return p.active && mbx < p.start
}

func (e *Encoder) planRefresh(idr bool) refreshPlan {
	if e.cfg.IntraRefresh <= 0 {
		return refreshPlan{}
	}
	if idr {
		e.refreshPos = 0
		return refreshPlan{}
	}
	n := e.cfg.IntraRefresh
	e.refreshPos++
	if e.refreshPos > n {
		e.refreshPos = 1
	}
	k := e.refreshPos
	w := e.widthMBs
	start := w * (k - 1) / n
	end := w*k/n + 1
	if end > w {
		end = w
	}
	return refreshPlan{active: true, start: start, end: end, sweep: k == 1}
}

func (e *Encoder) recordRefreshEnd(pic *frame.Picture, p refreshPlan) {
	if e.cfg.IntraRefresh <= 0 || pic == nil {
		return
	}
	if p.active {
		e.refreshEnd[pic] = p.end
	} else {
		e.refreshEnd[pic] = e.widthMBs
	}
	e.refreshSeq[pic] = e.refreshNextSeq
	e.refreshNextSeq++
}

func (e *Encoder) openSweep() {
	e.refreshBarrier = e.refreshNextSeq - 1
	for _, r := range e.refs {
		e.refreshEnd[r] = 0
	}
	for _, s := range e.ltSlots {
		if s.pic != nil {
			e.refreshEnd[s.pic] = 0
		}
	}
}

func (e *Encoder) refreshSettled(ref *frame.Picture) bool {
	if e.refreshSeq == nil || ref == nil {
		return true
	}
	seq, ok := e.refreshSeq[ref]
	return ok && seq >= e.refreshBarrier
}

func (e *Encoder) refreshLimits(ref *frame.Picture) (limitLuma, limitChroma int, on bool) {
	if e.cfg.IntraRefresh <= 0 || ref == nil || e.refreshEnd == nil {
		return 0, 0, false
	}
	end, ok := e.refreshEnd[ref]
	if !ok || end >= e.widthMBs {
		return 0, 0, false
	}
	return end*16 - 3, end*8 - 1, true
}

func refreshAllows(x, w, mvx, mvy, limitLuma, limitChroma int) bool {
	right := x + (mvx >> 2) + w - 1
	if mvx&3 != 0 {
		right += lumaTapAfter
	}
	if right >= limitLuma {
		return false
	}
	chroma := x/2 + (mvx >> 3) + w/2 - 1
	if mvx&7 != 0 || mvy&7 != 0 {
		chroma++
	}
	return chroma < limitChroma
}

func refreshHiX(x, w, limitLuma, limitChroma int) int {
	hi := limitLuma - x - w
	if q := 2*(limitChroma-1-(x+w)/2) + 1; q < hi {
		hi = q
	}
	return hi
}

const topRightSourceBlock = 5

func usesAboveRight4x4(mode int) bool {
	return mode == pred.I4x4DiagonalDownLeft || mode == pred.I4x4VerticalLeft
}

func (s *mbEncoder) refreshBoundFor(ref *frame.Picture) (limitLuma, limitChroma int, on bool) {
	if !s.refresh.constrained(s.mbx) {
		return 0, 0, false
	}
	return s.e.refreshLimits(ref)
}

func (s *mbEncoder) refreshUsableRef(ref *frame.Picture) bool {
	if !s.refresh.active {
		return true
	}
	if !s.refresh.constrained(s.mbx) {
		return s.e.refreshSettled(ref)
	}
	limitLuma, limitChroma, on := s.e.refreshLimits(ref)
	if !on {
		return true
	}
	return refreshAllows(s.mbx*16, 16, 0, 0, limitLuma, limitChroma)
}

func (s *mbEncoder) refreshAllowsMB(mv [2]int16) bool {
	limitLuma, limitChroma, on := s.refreshBoundFor(s.refPicture(0))
	if !on {
		return true
	}
	return refreshAllows(s.mbx*16, 16, int(mv[0]), int(mv[1]), limitLuma, limitChroma)
}

func (s *mbEncoder) forcedIntra() bool {
	if !s.refresh.active {
		return false
	}
	return s.refresh.inBand(s.mbx) || !s.refreshUsableRef(s.refPicture(0))
}

func (s *mbEncoder) encodeForcedIntra() error {
	s.clearMotion()
	s.encodeIntraModes()
	s.w.WriteUE(s.pendingSkipRun)
	s.pendingSkipRun = 0
	return s.writeIntraMB(5)
}

func (s *mbEncoder) encodeForcedIntraCABAC() error {
	inc := s.skipFlagInc()
	s.clearMotion()
	s.encodeIntraModes()
	s.cb.MBSkipFlagP(inc, false)
	s.writeIntraMBCABAC(true)
	return s.w.Err()
}
