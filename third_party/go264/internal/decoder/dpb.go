package decoder

import (
	"sort"

	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/frame"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/syntax"
)

type refFrame struct {
	pic          *frame.Picture
	frameNum     uint32
	frameNumWrap int
	picNum       int
	longTerm     bool
	longTermIdx  int
	// nonExisting marks a frame inferred for a gap in frame_num (section
	// 8.2.5.2). It holds a reference slot and a frame number but has no
	// picture of its own: pic is borrowed from the nearest real reference so
	// that a stream which wrongly refers to it still decodes.
	nonExisting bool
}

type dpb struct {
	refs        []*refFrame
	maxNumRefs  int
	maxFrameNum uint32

	prevPicOrderCntMsb int
	prevPicOrderCntLsb int
	prevFrameNumOffset int
	prevFrameNum       uint32

	// prevRefFrameNum is PrevRefFrameNum of section 7.4.3: the frame_num of the
	// previous reference picture, 0 after one with operation 5. It exists once
	// a reference picture has been stored.
	prevRefFrameNum uint32
	haveRef         bool

	// curTop and curBottom are TopFieldOrderCnt and BottomFieldOrderCnt of the
	// picture whose order count was derived last.
	curTop, curBottom int
}

func newDPB(sps *syntax.SPS) *dpb {
	n := int(sps.MaxNumRefFrames)
	if n < 1 {
		n = 1
	}
	return &dpb{maxNumRefs: n, maxFrameNum: sps.MaxFrameNum()}
}

func (b *dpb) clear() {
	b.refs = b.refs[:0]
	b.prevPicOrderCntMsb = 0
	b.prevPicOrderCntLsb = 0
	b.prevFrameNumOffset = 0
	b.prevFrameNum = 0
}

// computePOC returns PicOrderCnt of the frame, Min(TopFieldOrderCnt,
// BottomFieldOrderCnt) (8-1), and keeps both counts for the picture so that
// operation 5 can rebase them (section 8.2.1, tempPicOrderCnt).
func (b *dpb) computePOC(sps *syntax.SPS, hdr *syntax.SliceHeader) int {
	top := b.topPOC(sps, hdr)
	bottom := top
	switch sps.PicOrderCntType {
	case 0: // (8-5) for a frame
		bottom = top + int(hdr.DeltaPicOrderCntBottom)
	case 1: // (8-10)
		bottom = top + int(sps.OffsetForTopToBottomField) + int(hdr.DeltaPicOrderCnt[1])
	}
	b.curTop, b.curBottom = top, bottom
	return min(top, bottom)
}

// topPOC derives TopFieldOrderCnt (8.2.1.1 to 8.2.1.3) and advances the
// previous-picture state.
func (b *dpb) topPOC(sps *syntax.SPS, hdr *syntax.SliceHeader) int {
	switch sps.PicOrderCntType {
	case 0:
		maxLsb := int(sps.MaxPicOrderCntLsb())
		lsb := int(hdr.PicOrderCntLsb)
		msb := 0
		if hdr.IDR {
			b.prevPicOrderCntMsb = 0
			b.prevPicOrderCntLsb = 0
		} else {
			switch {
			case lsb < b.prevPicOrderCntLsb && b.prevPicOrderCntLsb-lsb >= maxLsb/2:
				msb = b.prevPicOrderCntMsb + maxLsb
			case lsb > b.prevPicOrderCntLsb && lsb-b.prevPicOrderCntLsb > maxLsb/2:
				msb = b.prevPicOrderCntMsb - maxLsb
			default:
				msb = b.prevPicOrderCntMsb
			}
		}
		poc := msb + lsb
		if hdr.NalRefIDC != 0 {
			b.prevPicOrderCntMsb = msb
			b.prevPicOrderCntLsb = lsb
		}
		return poc

	case 2:
		offset := 0
		if !hdr.IDR {
			offset = b.prevFrameNumOffset
			if hdr.FrameNum < b.prevFrameNum {
				offset += int(b.maxFrameNum)
			}
		}
		b.prevFrameNumOffset = offset
		b.prevFrameNum = hdr.FrameNum
		poc := 2 * (offset + int(hdr.FrameNum))
		if hdr.NalRefIDC == 0 {
			poc--
		}
		return poc
	}
	return b.pocType1(sps, hdr)
}

func (b *dpb) pocType1(sps *syntax.SPS, hdr *syntax.SliceHeader) int {
	offset := 0
	if !hdr.IDR {
		offset = b.prevFrameNumOffset
		if hdr.FrameNum < b.prevFrameNum {
			offset += int(b.maxFrameNum)
		}
	}
	b.prevFrameNumOffset = offset
	b.prevFrameNum = hdr.FrameNum

	cycle := len(sps.OffsetForRefFrame)
	absFrameNum := 0
	if cycle != 0 {
		absFrameNum = offset + int(hdr.FrameNum)
	}
	if hdr.NalRefIDC == 0 && absFrameNum > 0 {
		absFrameNum--
	}
	expected := 0
	if absFrameNum > 0 {
		cycleCnt := (absFrameNum - 1) / cycle
		inCycle := (absFrameNum - 1) % cycle
		var sum int
		for _, v := range sps.OffsetForRefFrame {
			sum += int(v)
		}
		expected = cycleCnt * sum
		for i := 0; i <= inCycle; i++ {
			expected += int(sps.OffsetForRefFrame[i])
		}
	}
	if hdr.NalRefIDC == 0 {
		expected += int(sps.OffsetForNonRefPic)
	}
	return expected + int(hdr.DeltaPicOrderCnt[0])
}

func (b *dpb) updatePicNums(currFrameNum uint32) {
	for _, r := range b.refs {
		if r.longTerm {
			continue
		}
		if r.frameNum > currFrameNum {
			r.frameNumWrap = int(r.frameNum) - int(b.maxFrameNum)
		} else {
			r.frameNumWrap = int(r.frameNum)
		}
		r.picNum = r.frameNumWrap
	}
}

func (b *dpb) shortTermByPicNum(picNum int) *refFrame {
	for _, r := range b.refs {
		if !r.longTerm && r.picNum == picNum {
			return r
		}
	}
	return nil
}

func (b *dpb) longTermByPicNum(num int) *refFrame {
	for _, r := range b.refs {
		if r.longTerm && r.longTermIdx == num {
			return r
		}
	}
	return nil
}

func (b *dpb) remove(target *refFrame) {
	for i, r := range b.refs {
		if r == target {
			b.refs = append(b.refs[:i], b.refs[i+1:]...)
			return
		}
	}
}

func (b *dpb) buildListP(hdr *syntax.SliceHeader, active int) []*frame.Picture {
	shortTerm := make([]*refFrame, 0, len(b.refs))
	longTerm := make([]*refFrame, 0, len(b.refs))
	for _, r := range b.refs {
		if r.longTerm {
			longTerm = append(longTerm, r)
		} else {
			shortTerm = append(shortTerm, r)
		}
	}
	sort.SliceStable(shortTerm, func(i, j int) bool {
		return shortTerm[i].picNum > shortTerm[j].picNum
	})
	sort.SliceStable(longTerm, func(i, j int) bool {
		return longTerm[i].longTermIdx < longTerm[j].longTermIdx
	})
	list := append(shortTerm, longTerm...)

	if hdr.ModificationL0Present {
		list = b.applyModifications(list, hdr.RefPicListModificationL0, hdr.FrameNum, active)
	}
	return truncate(list, active)
}

func (b *dpb) splitByOrder(currPOC int) (before, after, long []*refFrame) {
	for _, r := range b.refs {
		switch {
		case r.longTerm:
			long = append(long, r)
		case r.pic.POC < currPOC:
			before = append(before, r)
		default:
			after = append(after, r)
		}
	}
	sort.SliceStable(before, func(i, j int) bool { return before[i].pic.POC > before[j].pic.POC })
	sort.SliceStable(after, func(i, j int) bool { return after[i].pic.POC < after[j].pic.POC })
	sort.SliceStable(long, func(i, j int) bool { return long[i].longTermIdx < long[j].longTermIdx })
	return before, after, long
}

func sameFrames(a, b []*refFrame) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (b *dpb) buildListsB(hdr *syntax.SliceHeader, currPOC, activeL0, activeL1 int) ([]*frame.Picture, []*frame.Picture) {
	before, after, long := b.splitByOrder(currPOC)

	list0 := make([]*refFrame, 0, len(b.refs))
	list0 = append(list0, before...)
	list0 = append(list0, after...)
	list0 = append(list0, long...)

	list1 := make([]*refFrame, 0, len(b.refs))
	list1 = append(list1, after...)
	list1 = append(list1, before...)
	list1 = append(list1, long...)

	if len(list1) > 1 && sameFrames(list0, list1) {
		list1[0], list1[1] = list1[1], list1[0]
	}

	if hdr.ModificationL0Present {
		list0 = b.applyModifications(list0, hdr.RefPicListModificationL0, hdr.FrameNum, activeL0)
	}
	if hdr.ModificationL1Present {
		list1 = b.applyModifications(list1, hdr.RefPicListModificationL1, hdr.FrameNum, activeL1)
	}
	return truncate(list0, activeL0), truncate(list1, activeL1)
}

func truncate(list []*refFrame, active int) []*frame.Picture {
	out := make([]*frame.Picture, 0, active)
	for i := 0; i < active && i < len(list); i++ {
		out = append(out, list[i].pic)
	}
	return out
}

func (b *dpb) applyModifications(list []*refFrame, mods []syntax.RefPicListModification, frameNum uint32, active int) []*refFrame {
	predPicNum := int(frameNum)
	refIdx := 0
	for _, m := range mods {
		var target *refFrame
		switch m.IDC {
		case 0, 1:
			diff := int(m.Value) + 1
			if m.IDC == 0 {
				predPicNum -= diff
				if predPicNum < 0 {
					predPicNum += int(b.maxFrameNum)
				}
			} else {
				predPicNum += diff
				if predPicNum >= int(b.maxFrameNum) {
					predPicNum -= int(b.maxFrameNum)
				}
			}
			pn := predPicNum
			if pn > int(frameNum) {
				pn -= int(b.maxFrameNum)
			}
			target = b.shortTermByPicNum(pn)
		case 2:
			target = b.longTermByPicNum(int(m.Value))
		}
		if target == nil {
			continue
		}
		list = moveToIndex(list, target, refIdx)
		refIdx++
		if refIdx >= active {
			break
		}
	}
	return list
}

func moveToIndex(list []*refFrame, target *refFrame, idx int) []*refFrame {
	if idx > len(list) {
		idx = len(list)
	}
	out := make([]*refFrame, 0, len(list)+1)
	out = append(out, list[:idx]...)
	out = append(out, target)
	for _, r := range list[idx:] {
		if r != target {
			out = append(out, r)
		}
	}
	return out
}

// store marks the decoded picture as a reference. It reports whether the
// picture carried memory_management_control_operation 5, after which the
// caller must treat everything decoded before it as outputtable and give the
// picture the picture order count 0 (H.264 section 8.2.1, "tempPicOrderCnt").
func (b *dpb) store(pic *frame.Picture, hdr *syntax.SliceHeader) (mmco5 bool) {
	if hdr.NalRefIDC == 0 {
		return false
	}
	b.prevRefFrameNum, b.haveRef = hdr.FrameNum, true
	if hdr.IDR {
		b.refs = b.refs[:0]
		entry := &refFrame{pic: pic, frameNum: hdr.FrameNum, longTerm: hdr.LongTermReference}
		pic.LongTerm = hdr.LongTermReference
		b.refs = append(b.refs, entry)
		b.updatePicNums(hdr.FrameNum)
		return false
	}
	if hdr.AdaptiveRefPicMarking {
		entry := &refFrame{pic: pic, frameNum: hdr.FrameNum}
		b.applyMMCO(hdr, entry)
		if hasMMCO5(hdr) {
			// Section 7.4.3: after operation 5 the picture is inferred to have
			// had frame_num 0, and section 8.2.1 restarts the order count
			// from it, so the next picture's FrameNumWrap, FrameNumOffset and
			// PicOrderCntMsb are all taken against 0.
			entry.frameNum = 0
			pic.FrameNum = 0
			b.prevRefFrameNum = 0
			// 8.2.1: tempPicOrderCnt = Min(top, bottom) is taken off both
			// counts, and 8.2.1.1 then starts the next picture from
			// PicOrderCntMsb 0 and the rebased TopFieldOrderCnt.
			b.prevPicOrderCntMsb = 0
			b.prevPicOrderCntLsb = b.curTop - min(b.curTop, b.curBottom)
			b.prevFrameNumOffset = 0
			b.prevFrameNum = 0
			mmco5 = true
		}
		b.refs = append(b.refs, entry)
		b.updatePicNums(entry.frameNum)
		b.enforceCapacity()
		return mmco5
	}
	b.refs = append(b.refs, &refFrame{pic: pic, frameNum: hdr.FrameNum})
	b.updatePicNums(hdr.FrameNum)
	b.slidingWindow()
	b.enforceCapacity()
	return false
}

// fillGap runs the decoding process for gaps in frame_num (section 8.2.5.2).
// When the slice's frame_num is neither PrevRefFrameNum nor the value after it,
// frames were lost or, with gaps_in_frame_num_value_allowed_flag set, left out
// on purpose (temporal scalability, sub-sequences). Each missing frame number
// becomes a "non-existing" short-term reference frame, so that the sliding
// window moves as the encoder's did and the frame numbers the slice refers to
// still resolve to the right pictures.
//
// Only the last max_num_ref_frames of them can survive the sliding window, so
// a stream cannot make this allocate more than that however wide the gap.
func (b *dpb) fillGap(sps *syntax.SPS, hdr *syntax.SliceHeader) {
	if hdr.IDR || !b.haveRef || !sps.GapsInFrameNumValueAllowed || len(b.refs) == 0 {
		return
	}
	max := b.maxFrameNum
	next := (b.prevRefFrameNum + 1) % max
	if hdr.FrameNum == b.prevRefFrameNum || hdr.FrameNum == next {
		return
	}
	gap := int((hdr.FrameNum + max - next) % max)
	donor := b.refs[len(b.refs)-1].pic
	for i, fn := 0, next; i < gap; i, fn = i+1, (fn+1)%max {
		if sps.PicOrderCntType != 0 {
			// The order count of the following picture is measured from the
			// last frame number seen, including these.
			if fn < b.prevFrameNum {
				b.prevFrameNumOffset += int(max)
			}
			b.prevFrameNum = fn
		}
		if i >= gap-b.maxNumRefs {
			b.refs = append(b.refs, &refFrame{pic: donor, frameNum: fn, nonExisting: true})
			b.updatePicNums(fn)
			b.slidingWindow()
		}
	}
	b.prevRefFrameNum = (hdr.FrameNum + max - 1) % max
}

func hasMMCO5(hdr *syntax.SliceHeader) bool {
	for _, m := range hdr.MMCOs {
		if m.Op == 5 {
			return true
		}
	}
	return false
}

func (b *dpb) enforceCapacity() {
	n := b.maxNumRefs
	if n < 1 {
		n = 1
	}
	if len(b.refs) <= n {
		return
	}
	b.refs = append(b.refs[:0], b.refs[len(b.refs)-n:]...)
}

func (b *dpb) applyMMCO(hdr *syntax.SliceHeader, current *refFrame) {
	for _, m := range hdr.MMCOs {
		switch m.Op {
		case 1:
			pn := int(hdr.FrameNum) - int(m.DifferenceOfPicNumsMinus1) - 1
			if r := b.shortTermByPicNum(pn); r != nil {
				b.remove(r)
			}
		case 2:
			if r := b.longTermByPicNum(int(m.LongTermPicNum)); r != nil {
				b.remove(r)
			}
		case 3:
			pn := int(hdr.FrameNum) - int(m.DifferenceOfPicNumsMinus1) - 1
			if r := b.shortTermByPicNum(pn); r != nil {
				if old := b.longTermByPicNum(int(m.LongTermFrameIdx)); old != nil {
					b.remove(old)
				}
				r.longTerm = true
				r.longTermIdx = int(m.LongTermFrameIdx)
				if !r.nonExisting {
					r.pic.LongTerm = true
				}
			}
		case 4:
			max := int(m.MaxLongTermFrameIdxPlus1) - 1
			for i := 0; i < len(b.refs); {
				if b.refs[i].longTerm && b.refs[i].longTermIdx > max {
					b.refs = append(b.refs[:i], b.refs[i+1:]...)
					continue
				}
				i++
			}
		case 5:
			b.refs = b.refs[:0]
		case 6:
			if r := b.longTermByPicNum(int(m.LongTermFrameIdx)); r != nil {
				b.remove(r)
			}
			current.longTerm = true
			current.longTermIdx = int(m.LongTermFrameIdx)
			current.pic.LongTerm = true
		}
	}
}

func (b *dpb) slidingWindow() {
	for len(b.refs) > b.maxNumRefs {
		oldest := -1
		var wrap int
		for i, r := range b.refs {
			if r.longTerm {
				continue
			}
			if oldest < 0 || r.frameNumWrap < wrap {
				oldest = i
				wrap = r.frameNumWrap
			}
		}
		if oldest < 0 {
			return
		}
		b.refs = append(b.refs[:oldest], b.refs[oldest+1:]...)
	}
}
