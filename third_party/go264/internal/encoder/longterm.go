package encoder

import (
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/frame"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/syntax"
)

const (
	longTermMatchSAD     = 16 * 16 * 3
	longTermStaleRatio   = 50
	longTermQuietRatio   = 98
	longTermRefreshGuard = 8
)

type longTermSlot struct {
	pic *frame.Picture
	age int
}

type refMarking struct {
	adaptive bool
	mmcos    []syntax.MMCO
	promote  *frame.Picture
	slot     int
	evict    []*frame.Picture
}

func (e *Encoder) refCap() int {
	n := e.cfg.RefFrames + e.cfg.LongTermReferences
	if n > 16 {
		n = 16
	}
	return n
}

func (e *Encoder) longTermCount() int {
	n := 0
	for _, s := range e.ltSlots {
		if s.pic != nil {
			n++
		}
	}
	return n
}

func (e *Encoder) appendLongTerm(dst []*frame.Picture) []*frame.Picture {
	for _, s := range e.ltSlots {
		if s.pic != nil {
			dst = append(dst, s.pic)
		}
	}
	return dst
}

func (e *Encoder) resetLongTerm() {
	for i := range e.ltSlots {
		if e.ltSlots[i].pic != nil {
			e.ltSlots[i].pic.LongTerm = false
			e.free = append(e.free, e.ltSlots[i].pic)
		}
		e.ltSlots[i] = longTermSlot{}
	}
	e.ltMaxSent = false
}

func (e *Encoder) picNumDiff(pic *frame.Picture) uint32 {
	num := int(pic.FrameNum)
	if num > int(e.frameNum) {
		num -= int(e.sps.MaxFrameNum())
	}
	return uint32(int(e.frameNum) - num)
}

func (e *Encoder) matchRatio(ref *frame.Picture) int {
	src := e.src
	total := e.widthMBs * e.heightMBs
	if total == 0 {
		return 100
	}
	hit := 0
	for mby := 0; mby < e.heightMBs; mby++ {
		for mbx := 0; mbx < e.widthMBs; mbx++ {
			x, y := mbx*16, mby*16
			if sad(src.Y, src.StrideY, src.LumaOffset(x, y),
				ref.Y, ref.StrideY, ref.LumaOffset(x, y), 16, 16) <= longTermMatchSAD {
				hit++
			}
		}
	}
	return hit * 100 / total
}

func (e *Encoder) quietPicture(h *frameHints, newest *frame.Picture) bool {
	if h != nil && h.changedMB != nil {
		changed := 0
		for _, c := range h.changedMB {
			if c {
				changed++
			}
		}
		return changed*100 <= (100-longTermQuietRatio)*len(h.changedMB)
	}
	return e.matchRatio(newest) >= longTermQuietRatio
}

func (e *Encoder) chooseLongTerm(h *frameHints) (int, *frame.Picture) {
	newest := e.refs[0]
	empty := -1
	for i := range e.ltSlots {
		if e.ltSlots[i].pic == nil {
			empty = i
			break
		}
	}
	if empty == 0 {
		return 0, newest
	}
	if empty > 0 && e.quietPicture(h, newest) {
		return empty, newest
	}
	worst, ratio := -1, longTermStaleRatio
	for i := range e.ltSlots {
		s := e.ltSlots[i]
		if s.pic == nil || s.age < longTermRefreshGuard {
			continue
		}
		if r := e.matchRatio(s.pic); r < ratio {
			worst, ratio = i, r
		}
	}
	if worst < 0 {
		return 0, nil
	}
	return worst, newest
}

func removePicture(list []*frame.Picture, pic *frame.Picture) []*frame.Picture {
	for i, p := range list {
		if p == pic {
			return append(list[:i], list[i+1:]...)
		}
	}
	return list
}

func (e *Encoder) planMarking(p picture, h *frameHints) refMarking {
	if len(e.ltSlots) == 0 || p.idr || !p.reference || len(e.refs) == 0 {
		return refMarking{}
	}
	slot, target := e.chooseLongTerm(h)
	if target == nil {
		return refMarking{}
	}
	m := refMarking{adaptive: true, promote: target, slot: slot}
	if !e.ltMaxSent {
		m.mmcos = append(m.mmcos, syntax.MMCO{
			Op:                       4,
			MaxLongTermFrameIdxPlus1: uint32(len(e.ltSlots)),
		})
	}
	long := e.longTermCount()
	if e.ltSlots[slot].pic != nil {
		m.mmcos = append(m.mmcos, syntax.MMCO{Op: 2, LongTermPicNum: uint32(slot)})
		long--
	}
	m.mmcos = append(m.mmcos, syntax.MMCO{
		Op:                        3,
		DifferenceOfPicNumsMinus1: e.picNumDiff(target) - 1,
		LongTermFrameIdx:          uint32(slot),
	})
	long++

	short := append([]*frame.Picture(nil), e.refs...)
	short = removePicture(short, target)
	for len(short)+long+1 > e.refCap() && len(short) != 0 {
		oldest := short[len(short)-1]
		short = short[:len(short)-1]
		m.mmcos = append(m.mmcos, syntax.MMCO{
			Op:                        1,
			DifferenceOfPicNumsMinus1: e.picNumDiff(oldest) - 1,
		})
		m.evict = append(m.evict, oldest)
	}
	return m
}

func (e *Encoder) applyMarking() {
	for i := range e.ltSlots {
		if e.ltSlots[i].pic != nil {
			e.ltSlots[i].age++
		}
	}
	m := e.mark
	if !m.adaptive {
		return
	}
	e.ltMaxSent = true
	if old := e.ltSlots[m.slot].pic; old != nil {
		old.LongTerm = false
		e.free = append(e.free, old)
	}
	e.refs = removePicture(e.refs, m.promote)
	m.promote.LongTerm = true
	e.ltSlots[m.slot] = longTermSlot{pic: m.promote}
	for _, pic := range m.evict {
		e.refs = removePicture(e.refs, pic)
		e.free = append(e.free, pic)
	}
}
