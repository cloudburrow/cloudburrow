package vp9hbd

import "github.com/cloudburrow/cloudburrow/third_party/govpx/internal/vp9/tables"

var kfPartitionProbs = tables.KfPartitionProbs

// block holds the spec's variables for the block being decoded.
type block struct {
	miRow, miCol, miSize int
	availU, availL       bool
	segmentID            int
	skip                 bool
	txSize               int
	isInter              bool
	yMode                int
	subModes             [4]int
	uvMode               int
	refFrame             [2]int
	interpFilter         int
	blockMvs             [2][4]mv
	eobTotal             int

	leftRefFrame, aboveRefFrame [2]int
	leftIntra, aboveIntra       bool
	leftSingle, aboveSingle     bool

	// motion vector prediction
	refListMv     [2]mv
	refMvCount    int
	candidateMv   [2]mv
	candidateFrm  [2]int
	modeContext   [4]int
	nearestMv     [2]mv
	nearMv        [2]mv
	bestMv        [2]mv
	mv            [2]mv
	leftMi, aboveMi *modeInfo
}

func (d *Decoder) mi(r, c int) *modeInfo { return &d.mis[r*d.miCols+c] }

func (d *Decoder) decodeBlock(r, c, subsize int) {
	b := &d.b
	b.miRow, b.miCol, b.miSize = r, c, subsize
	b.availU = r > 0
	b.availL = c > d.miColStart
	b.leftMi, b.aboveMi = nil, nil
	if b.availU {
		b.aboveMi = d.mi(r-1, c)
	}
	if b.availL {
		b.leftMi = d.mi(r, c-1)
	}
	if d.h.frameIsIntra {
		d.intraFrameModeInfo()
	} else {
		d.interFrameModeInfo()
	}
	b.eobTotal = 0
	d.residual()
	if b.isInter && subsize >= block8x8 && b.eobTotal == 0 {
		b.skip = true
	}
	for y := range num8x8H[subsize] {
		if r+y >= d.miRows {
			break
		}
		for x := range num8x8W[subsize] {
			if c+x >= d.miCols {
				break
			}
			m := d.mi(r+y, c+x)
			m.skip = b.skip
			m.txSize = int8(b.txSize)
			m.miSize = int8(subsize)
			m.yMode = int8(b.yMode)
			m.segmentID = uint8(b.segmentID)
			m.refFrame = [2]int8{int8(b.refFrame[0]), int8(b.refFrame[1])}
			if b.isInter {
				m.interpFilter = int8(b.interpFilter)
				for l := range 2 {
					m.mv[l] = b.blockMvs[l][3]
					m.subMvs[l] = b.blockMvs[l]
				}
			} else {
				for i := range 4 {
					m.subModes[i] = int8(b.subModes[i])
				}
			}
		}
	}
}

func (d *Decoder) segFeatureActive(feature int) bool {
	return d.seg.enabled && d.seg.featureEnabled[d.b.segmentID][feature]
}

func (d *Decoder) intraFrameModeInfo() {
	b := &d.b
	// intra_segment_id
	b.segmentID = 0
	if d.seg.enabled && d.seg.updateMap {
		b.segmentID = d.bd.tree(segmentTree, d.seg.treeProbs[:])
	}
	d.readSkip()
	d.readTxSize(true)
	b.refFrame = [2]int{intraFrame, noneFrame}
	b.isInter = false
	if b.miSize >= block8x8 {
		above, left := dcPred, dcPred
		if b.availU {
			above = int(b.aboveMi.subModes[2])
		}
		if b.availL {
			left = int(b.leftMi.subModes[1])
		}
		b.yMode = d.bd.tree(intraModeTree, tables.KfYModeProb[above][left][:])
		b.subModes = [4]int{b.yMode, b.yMode, b.yMode, b.yMode}
	} else {
		w, h := num4x4W[b.miSize], num4x4H[b.miSize]
		var mode int
		for idy := 0; idy < 2; idy += h {
			for idx := 0; idx < 2; idx += w {
				var above, left int
				if idy > 0 {
					above = b.subModes[idx]
				} else if b.availU {
					above = int(b.aboveMi.subModes[2+idx])
				} else {
					above = dcPred
				}
				if idx > 0 {
					left = b.subModes[idy*2]
				} else if b.availL {
					left = int(b.leftMi.subModes[1+idy*2])
				} else {
					left = dcPred
				}
				mode = d.bd.tree(intraModeTree, tables.KfYModeProb[above][left][:])
				for y2 := range h {
					for x2 := range w {
						b.subModes[(idy+y2)*2+idx+x2] = mode
					}
				}
			}
		}
		b.yMode = mode
	}
	b.uvMode = d.bd.tree(intraModeTree, tables.KfUvModeProb[b.yMode][:])
}

func (d *Decoder) readSkip() {
	b := &d.b
	if d.segFeatureActive(segLvlSkip) {
		b.skip = true
		return
	}
	ctx := 0
	if b.availU && b.aboveMi.skip {
		ctx++
	}
	if b.availL && b.leftMi.skip {
		ctx++
	}
	v := d.bd.read(int(d.fc.skip[ctx]))
	d.counts.skip[ctx][v]++
	b.skip = v == 1
}

func (d *Decoder) readTxSize(allowSelect bool) {
	b := &d.b
	maxTx := maxTxSize[b.miSize]
	if allowSelect && d.h.txMode == txModeSelect && b.miSize >= block8x8 {
		above, left := maxTx, maxTx
		if b.availU && !b.aboveMi.skip {
			above = int(b.aboveMi.txSize)
		}
		if b.availL && !b.leftMi.skip {
			left = int(b.leftMi.txSize)
		}
		if !b.availL {
			left = above
		}
		if !b.availU {
			above = left
		}
		ctx := 0
		if above+left > maxTx {
			ctx = 1
		}
		switch maxTx {
		case tx32x32:
			b.txSize = d.bd.tree(tx32Tree, d.fc.tx32[ctx][:])
			d.counts.tx32[ctx][b.txSize]++
		case tx16x16:
			b.txSize = d.bd.tree(tx16Tree, d.fc.tx16[ctx][:])
			d.counts.tx16[ctx][b.txSize]++
		default:
			b.txSize = d.bd.tree(tx8Tree, d.fc.tx8[ctx][:])
			d.counts.tx8[ctx][b.txSize]++
		}
		return
	}
	b.txSize = min(maxTx, txModeToBig[d.h.txMode])
}

func (d *Decoder) interFrameModeInfo() {
	b := &d.b
	b.leftRefFrame = [2]int{intraFrame, noneFrame}
	b.aboveRefFrame = [2]int{intraFrame, noneFrame}
	if b.availL {
		b.leftRefFrame = [2]int{int(b.leftMi.refFrame[0]), int(b.leftMi.refFrame[1])}
	}
	if b.availU {
		b.aboveRefFrame = [2]int{int(b.aboveMi.refFrame[0]), int(b.aboveMi.refFrame[1])}
	}
	b.leftIntra = b.leftRefFrame[0] <= intraFrame
	b.aboveIntra = b.aboveRefFrame[0] <= intraFrame
	b.leftSingle = b.leftRefFrame[1] <= noneFrame
	b.aboveSingle = b.aboveRefFrame[1] <= noneFrame
	d.interSegmentID()
	d.readSkip()
	d.readIsInter()
	d.readTxSize(!b.skip || !b.isInter)
	if b.isInter {
		d.interBlockModeInfo()
	} else {
		d.intraBlockModeInfo()
	}
}

func (d *Decoder) interSegmentID() {
	b := &d.b
	s := &d.seg
	if !s.enabled {
		b.segmentID = 0
		return
	}
	bw, bh := num8x8W[b.miSize], num8x8H[b.miSize]
	xmis := min(d.miCols-b.miCol, bw)
	ymis := min(d.miRows-b.miRow, bh)
	pred := 7
	for y := range ymis {
		for x := range xmis {
			pred = min(pred, int(d.prevSegmentIds[(b.miRow+y)*d.miCols+b.miCol+x]))
		}
	}
	if !s.updateMap {
		b.segmentID = pred
		return
	}
	if s.temporalUpdate {
		ctx := int(d.leftSegPred[b.miRow]) + int(d.aboveSegPred[b.miCol])
		p := d.bd.read(int(s.predProbs[ctx]))
		if p == 1 {
			b.segmentID = pred
		} else {
			b.segmentID = d.bd.tree(segmentTree, s.treeProbs[:])
		}
		for i := range bw {
			d.aboveSegPred[b.miCol+i] = uint8(p)
		}
		for i := range bh {
			d.leftSegPred[b.miRow+i] = uint8(p)
		}
		return
	}
	b.segmentID = d.bd.tree(segmentTree, s.treeProbs[:])
}

func (d *Decoder) readIsInter() {
	b := &d.b
	if d.segFeatureActive(segLvlRefFrame) {
		b.isInter = d.seg.featureData[b.segmentID][segLvlRefFrame] != intraFrame
		return
	}
	var ctx int
	switch {
	case b.availU && b.availL:
		if b.leftIntra && b.aboveIntra {
			ctx = 3
		} else if b.leftIntra || b.aboveIntra {
			ctx = 1
		}
	case b.availU:
		ctx = 2 * btoi(b.aboveIntra)
	case b.availL:
		ctx = 2 * btoi(b.leftIntra)
	}
	v := d.bd.read(int(d.fc.isInter[ctx]))
	d.counts.isInter[ctx][v]++
	b.isInter = v == 1
}

func btoi(v bool) int {
	if v {
		return 1
	}
	return 0
}

func (d *Decoder) intraBlockModeInfo() {
	b := &d.b
	b.refFrame = [2]int{intraFrame, noneFrame}
	if b.miSize >= block8x8 {
		ctx := sizeGroup[b.miSize]
		b.yMode = d.bd.tree(intraModeTree, d.fc.yMode[ctx][:])
		d.counts.intraMode[ctx][b.yMode]++
		b.subModes = [4]int{b.yMode, b.yMode, b.yMode, b.yMode}
	} else {
		w, h := num4x4W[b.miSize], num4x4H[b.miSize]
		var mode int
		for idy := 0; idy < 2; idy += h {
			for idx := 0; idx < 2; idx += w {
				mode = d.bd.tree(intraModeTree, d.fc.yMode[0][:])
				d.counts.intraMode[0][mode]++
				for y2 := range h {
					for x2 := range w {
						b.subModes[(idy+y2)*2+idx+x2] = mode
					}
				}
			}
		}
		b.yMode = mode
	}
	b.uvMode = d.bd.tree(intraModeTree, d.fc.uvMode[b.yMode][:])
	d.counts.uvMode[b.yMode][b.uvMode]++
}

func (d *Decoder) interBlockModeInfo() {
	b := &d.b
	d.readRefFrames()
	for j := range 2 {
		if b.refFrame[j] > intraFrame {
			d.findMvRefs(b.refFrame[j], -1)
			d.findBestRefMvs(j)
		}
	}
	isCompound := b.refFrame[1] > intraFrame
	if d.segFeatureActive(segLvlSkip) {
		b.yMode = zeroMv
	} else if b.miSize >= block8x8 {
		b.yMode = nearestMv + d.readInterMode()
	}
	if d.h.interpFilter == switchable {
		leftInterp, aboveInterp := 3, 3
		if b.availL && b.leftRefFrame[0] > intraFrame {
			leftInterp = int(b.leftMi.interpFilter)
		}
		if b.availU && b.aboveRefFrame[0] > intraFrame {
			aboveInterp = int(b.aboveMi.interpFilter)
		}
		var ctx int
		switch {
		case leftInterp == aboveInterp:
			ctx = leftInterp
		case leftInterp == 3 && aboveInterp != 3:
			ctx = aboveInterp
		case leftInterp != 3 && aboveInterp == 3:
			ctx = leftInterp
		default:
			ctx = 3
		}
		b.interpFilter = d.bd.tree(interpFilterTree, d.fc.interpFilter[ctx][:])
		d.counts.interpFilter[ctx][b.interpFilter]++
	} else {
		b.interpFilter = d.h.interpFilter
	}
	nRefs := 1
	if isCompound {
		nRefs = 2
	}
	if b.miSize < block8x8 {
		w, h := num4x4W[b.miSize], num4x4H[b.miSize]
		for idy := 0; idy < 2; idy += h {
			for idx := 0; idx < 2; idx += w {
				b.yMode = nearestMv + d.readInterMode()
				if b.yMode == nearestMv || b.yMode == nearMv {
					for j := range nRefs {
						d.appendSub8x8Mvs(idy*2+idx, j)
					}
				}
				d.assignMv(isCompound)
				for y2 := range h {
					for x2 := range w {
						blk := (idy+y2)*2 + idx + x2
						for l := range nRefs {
							b.blockMvs[l][blk] = b.mv[l]
						}
					}
				}
			}
		}
	} else {
		d.assignMv(isCompound)
		for l := range nRefs {
			for blk := range 4 {
				b.blockMvs[l][blk] = b.mv[l]
			}
		}
	}
}

func (d *Decoder) readInterMode() int {
	ctx := d.b.modeContext[d.b.refFrame[0]]
	v := d.bd.tree(interModeTree, d.fc.interMode[ctx][:])
	d.counts.interMode[ctx][v]++
	return v
}

func (d *Decoder) readRefFrames() {
	b := &d.b
	h := &d.h
	if d.segFeatureActive(segLvlRefFrame) {
		b.refFrame = [2]int{d.seg.featureData[b.segmentID][segLvlRefFrame], noneFrame}
		return
	}
	compMode := h.referenceMode
	if h.referenceMode == referenceModeSelect {
		ctx := d.compModeCtx()
		compMode = d.bd.read(int(d.fc.compMode[ctx]))
		d.counts.compMode[ctx][compMode]++
	}
	if compMode == compoundReference {
		idx := h.refFrameSignBias[h.compFixedRef]
		ctx := d.compRefCtx()
		v := d.bd.read(int(d.fc.compRef[ctx]))
		d.counts.compRef[ctx][v]++
		b.refFrame[idx] = h.compFixedRef
		b.refFrame[1-idx] = h.compVarRef[v]
		return
	}
	ctx := d.singleRefP1Ctx()
	v := d.bd.read(int(d.fc.singleRef[ctx][0]))
	d.counts.singleRef[ctx][0][v]++
	if v == 1 {
		ctx2 := d.singleRefP2Ctx()
		v2 := d.bd.read(int(d.fc.singleRef[ctx2][1]))
		d.counts.singleRef[ctx2][1][v2]++
		if v2 == 1 {
			b.refFrame[0] = altrefFrame
		} else {
			b.refFrame[0] = goldenFrame
		}
	} else {
		b.refFrame[0] = lastFrame
	}
	b.refFrame[1] = noneFrame
}

func (d *Decoder) compModeCtx() int {
	b := &d.b
	fixed := d.h.compFixedRef
	switch {
	case b.availU && b.availL:
		switch {
		case b.aboveSingle && b.leftSingle:
			return btoi(b.aboveRefFrame[0] == fixed) ^ btoi(b.leftRefFrame[0] == fixed)
		case b.aboveSingle:
			return 2 + btoi(b.aboveRefFrame[0] == fixed || b.aboveIntra)
		case b.leftSingle:
			return 2 + btoi(b.leftRefFrame[0] == fixed || b.leftIntra)
		default:
			return 4
		}
	case b.availU:
		if b.aboveSingle {
			return btoi(b.aboveRefFrame[0] == fixed)
		}
		return 3
	case b.availL:
		if b.leftSingle {
			return btoi(b.leftRefFrame[0] == fixed)
		}
		return 3
	}
	return 1
}

func (d *Decoder) compRefCtx() int {
	b := &d.b
	h := &d.h
	fixRefIdx := h.refFrameSignBias[h.compFixedRef]
	varRefIdx := 1 - fixRefIdx
	var1 := h.compVarRef[1]
	switch {
	case b.availU && b.availL:
		switch {
		case b.aboveIntra && b.leftIntra:
			return 2
		case b.leftIntra:
			if b.aboveSingle {
				return 1 + 2*btoi(b.aboveRefFrame[0] != var1)
			}
			return 1 + 2*btoi(b.aboveRefFrame[varRefIdx] != var1)
		case b.aboveIntra:
			if b.leftSingle {
				return 1 + 2*btoi(b.leftRefFrame[0] != var1)
			}
			return 1 + 2*btoi(b.leftRefFrame[varRefIdx] != var1)
		default:
			vrfa := b.aboveRefFrame[varRefIdx]
			if b.aboveSingle {
				vrfa = b.aboveRefFrame[0]
			}
			vrfl := b.leftRefFrame[varRefIdx]
			if b.leftSingle {
				vrfl = b.leftRefFrame[0]
			}
			switch {
			case vrfa == vrfl && var1 == vrfa:
				return 0
			case b.leftSingle && b.aboveSingle:
				if (vrfa == h.compFixedRef && vrfl == h.compVarRef[0]) ||
					(vrfl == h.compFixedRef && vrfa == h.compVarRef[0]) {
					return 4
				}
				if vrfa == vrfl {
					return 3
				}
				return 1
			case b.leftSingle || b.aboveSingle:
				vrfc := vrfl
				if b.leftSingle {
					vrfc = vrfa
				}
				rfs := vrfl
				if b.aboveSingle {
					rfs = vrfa
				}
				if vrfc == var1 && rfs != var1 {
					return 1
				}
				if rfs == var1 && vrfc != var1 {
					return 2
				}
				return 4
			case vrfa == vrfl:
				return 4
			default:
				return 2
			}
		}
	case b.availU:
		if b.aboveIntra {
			return 2
		}
		if b.aboveSingle {
			return 3 * btoi(b.aboveRefFrame[0] != var1)
		}
		return 4 * btoi(b.aboveRefFrame[varRefIdx] != var1)
	case b.availL:
		if b.leftIntra {
			return 2
		}
		if b.leftSingle {
			return 3 * btoi(b.leftRefFrame[0] != var1)
		}
		return 4 * btoi(b.leftRefFrame[varRefIdx] != var1)
	}
	return 2
}

func (d *Decoder) singleRefP1Ctx() int {
	b := &d.b
	a, l := b.aboveRefFrame, b.leftRefFrame
	switch {
	case b.availU && b.availL:
		switch {
		case b.aboveIntra && b.leftIntra:
			return 2
		case b.leftIntra:
			if b.aboveSingle {
				return 4 * btoi(a[0] == lastFrame)
			}
			return 1 + btoi(a[0] == lastFrame || a[1] == lastFrame)
		case b.aboveIntra:
			if b.leftSingle {
				return 4 * btoi(l[0] == lastFrame)
			}
			return 1 + btoi(l[0] == lastFrame || l[1] == lastFrame)
		default:
			if b.aboveSingle && b.leftSingle {
				return 2*btoi(a[0] == lastFrame) + 2*btoi(l[0] == lastFrame)
			}
			if !b.aboveSingle && !b.leftSingle {
				return 1 + btoi(a[0] == lastFrame || a[1] == lastFrame || l[0] == lastFrame || l[1] == lastFrame)
			}
			rfs, crf1, crf2 := l[0], a[0], a[1]
			if b.aboveSingle {
				rfs, crf1, crf2 = a[0], l[0], l[1]
			}
			if rfs == lastFrame {
				return 3 + btoi(crf1 == lastFrame || crf2 == lastFrame)
			}
			return btoi(crf1 == lastFrame || crf2 == lastFrame)
		}
	case b.availU:
		if b.aboveIntra {
			return 2
		}
		if b.aboveSingle {
			return 4 * btoi(a[0] == lastFrame)
		}
		return 1 + btoi(a[0] == lastFrame || a[1] == lastFrame)
	case b.availL:
		if b.leftIntra {
			return 2
		}
		if b.leftSingle {
			return 4 * btoi(l[0] == lastFrame)
		}
		return 1 + btoi(l[0] == lastFrame || l[1] == lastFrame)
	}
	return 2
}

func (d *Decoder) singleRefP2Ctx() int {
	b := &d.b
	a, l := b.aboveRefFrame, b.leftRefFrame
	switch {
	case b.availU && b.availL:
		switch {
		case b.aboveIntra && b.leftIntra:
			return 2
		case b.leftIntra:
			if b.aboveSingle {
				if a[0] == lastFrame {
					return 3
				}
				return 4 * btoi(a[0] == goldenFrame)
			}
			return 1 + 2*btoi(a[0] == goldenFrame || a[1] == goldenFrame)
		case b.aboveIntra:
			if b.leftSingle {
				if l[0] == lastFrame {
					return 3
				}
				return 4 * btoi(l[0] == goldenFrame)
			}
			return 1 + 2*btoi(l[0] == goldenFrame || l[1] == goldenFrame)
		default:
			if b.aboveSingle && b.leftSingle {
				switch {
				case a[0] == lastFrame && l[0] == lastFrame:
					return 3
				case a[0] == lastFrame:
					return 4 * btoi(l[0] == goldenFrame)
				case l[0] == lastFrame:
					return 4 * btoi(a[0] == goldenFrame)
				}
				return 2*btoi(a[0] == goldenFrame) + 2*btoi(l[0] == goldenFrame)
			}
			if !b.aboveSingle && !b.leftSingle {
				if a[0] == l[0] && a[1] == l[1] {
					return 3 * btoi(a[0] == goldenFrame || a[1] == goldenFrame)
				}
				return 2
			}
			rfs, crf1, crf2 := l[0], a[0], a[1]
			if b.aboveSingle {
				rfs, crf1, crf2 = a[0], l[0], l[1]
			}
			switch rfs {
			case goldenFrame:
				return 3 + btoi(crf1 == goldenFrame || crf2 == goldenFrame)
			case altrefFrame:
				return btoi(crf1 == goldenFrame || crf2 == goldenFrame)
			}
			return 1 + 2*btoi(crf1 == goldenFrame || crf2 == goldenFrame)
		}
	case b.availU:
		if b.aboveIntra || (a[0] == lastFrame && b.aboveSingle) {
			return 2
		}
		if b.aboveSingle {
			return 4 * btoi(a[0] == goldenFrame)
		}
		return 3 * btoi(a[0] == goldenFrame || a[1] == goldenFrame)
	case b.availL:
		if b.leftIntra || (l[0] == lastFrame && b.leftSingle) {
			return 2
		}
		if b.leftSingle {
			return 4 * btoi(l[0] == goldenFrame)
		}
		return 3 * btoi(l[0] == goldenFrame || l[1] == goldenFrame)
	}
	return 2
}

func (d *Decoder) assignMv(isCompound bool) {
	b := &d.b
	b.mv[1] = mv{}
	n := 1
	if isCompound {
		n = 2
	}
	for i := range n {
		switch b.yMode {
		case newMv:
			d.readMv(i)
		case nearestMv:
			b.mv[i] = b.nearestMv[i]
		case nearMv:
			b.mv[i] = b.nearMv[i]
		default:
			b.mv[i] = mv{}
		}
	}
}

func useMvHp(m mv) bool {
	return (abs32(m.row)>>3) < 8 && (abs32(m.col)>>3) < 8
}

func abs32(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}

func (d *Decoder) readMv(ref int) {
	b := &d.b
	useHp := d.h.allowHighPrecisionMv && useMvHp(b.bestMv[ref])
	var diff mv
	joint := d.bd.tree(mvJointTree, d.fc.mvJoint[:])
	d.counts.mvJoint[joint]++
	if joint == 2 || joint == 3 {
		diff.row = d.readMvComponent(0, useHp)
	}
	if joint == 1 || joint == 3 {
		diff.col = d.readMvComponent(1, useHp)
	}
	b.mv[ref] = mv{b.bestMv[ref].row + diff.row, b.bestMv[ref].col + diff.col}
}

func (d *Decoder) readMvComponent(comp int, useHp bool) int32 {
	f := &d.fc
	c := &d.counts
	sign := d.bd.read(int(f.mvSign[comp]))
	c.mvSign[comp][sign]++
	class := d.bd.tree(mvClassTree, f.mvClass[comp][:])
	c.mvClass[comp][class]++
	var mag int
	if class == 0 {
		c0 := d.bd.read(int(f.mvClass0Bit[comp]))
		c.mvClass0Bit[comp][c0]++
		fr := d.bd.tree(mvFrTree, f.mvClass0Fr[comp][c0][:])
		c.mvClass0Fr[comp][c0][fr]++
		hp := 1
		if useHp {
			hp = d.bd.read(int(f.mvClass0Hp[comp]))
		}
		c.mvClass0Hp[comp][hp]++
		mag = (c0<<3 | fr<<1 | hp) + 1
	} else {
		dd := 0
		for i := range class {
			bit := d.bd.read(int(f.mvBits[comp][i]))
			c.mvBits[comp][i][bit]++
			dd |= bit << uint(i)
		}
		mag = 2 << uint(class+2)
		fr := d.bd.tree(mvFrTree, f.mvFr[comp][:])
		c.mvFr[comp][fr]++
		hp := 1
		if useHp {
			hp = d.bd.read(int(f.mvHp[comp]))
		}
		c.mvHp[comp][hp]++
		mag += (dd<<3 | fr<<1 | hp) + 1
	}
	if sign == 1 {
		return int32(-mag)
	}
	return int32(mag)
}

// Motion vector prediction (spec 6.5).

func (d *Decoder) isInside(r, c int) bool {
	return r >= 0 && r < d.miRows && c >= d.miColStart && c < d.miColEnd
}

func (d *Decoder) findMvRefs(refFrame, blk int) {
	b := &d.b
	b.refMvCount = 0
	differentRefFound := false
	contextCounter := 0
	b.refListMv = [2]mv{}
	search := &tables.MvRefBlocks[b.miSize]
	for i := range 2 {
		cr, cc := b.miRow+int(search[i].Row), b.miCol+int(search[i].Col)
		if d.isInside(cr, cc) {
			m := d.mi(cr, cc)
			differentRefFound = true
			contextCounter += mode2Counter[m.yMode]
			for j := range 2 {
				if int(m.refFrame[j]) == refFrame {
					idx := 3
					if blk >= 0 {
						idx = idxNColToSubblock[blk][btoi(search[i].Col == 0)]
					}
					b.candidateMv[j] = m.subMvs[j][idx]
					d.addMvRefList(j)
					break
				}
			}
		}
	}
	for i := 2; i < 8; i++ {
		cr, cc := b.miRow+int(search[i].Row), b.miCol+int(search[i].Col)
		if d.isInside(cr, cc) {
			differentRefFound = true
			d.ifSameRefFrameAddMv(cr, cc, refFrame, false)
		}
	}
	if d.usePrevFrameMvs {
		d.ifSameRefFrameAddMv(b.miRow, b.miCol, refFrame, true)
	}
	if differentRefFound {
		for i := range 8 {
			cr, cc := b.miRow+int(search[i].Row), b.miCol+int(search[i].Col)
			if d.isInside(cr, cc) {
				d.ifDiffRefFrameAddMv(cr, cc, refFrame, false)
			}
		}
	}
	if d.usePrevFrameMvs {
		d.ifDiffRefFrameAddMv(b.miRow, b.miCol, refFrame, true)
	}
	b.modeContext[refFrame] = counterToContext[contextCounter]
	for i := range 2 {
		b.refListMv[i].row = d.clampMvRow(b.refListMv[i].row, mvBorder)
		b.refListMv[i].col = d.clampMvCol(b.refListMv[i].col, mvBorder)
	}
}

func (d *Decoder) clampMvRow(v int32, border int) int32 {
	b := &d.b
	bh := num8x8H[b.miSize]
	top := -((b.miRow * 8) * 8)
	bottom := ((d.miRows - bh - b.miRow) * 8) * 8
	return int32(min(max(int(v), top-border), bottom+border))
}

func (d *Decoder) clampMvCol(v int32, border int) int32 {
	b := &d.b
	bw := num8x8W[b.miSize]
	left := -((b.miCol * 8) * 8)
	right := ((d.miCols - bw - b.miCol) * 8) * 8
	return int32(min(max(int(v), left-border), right+border))
}

func (d *Decoder) addMvRefList(j int) {
	b := &d.b
	if b.refMvCount >= 2 {
		return
	}
	if b.refMvCount > 0 && b.candidateMv[j] == b.refListMv[0] {
		return
	}
	b.refListMv[b.refMvCount] = b.candidateMv[j]
	b.refMvCount++
}

func (d *Decoder) getBlockMv(r, c, j int, usePrev bool) {
	b := &d.b
	if usePrev {
		p := &d.prevMis[r*d.miCols+c]
		b.candidateMv[j] = p.mv[j]
		b.candidateFrm[j] = int(p.refFrame[j])
	} else {
		m := d.mi(r, c)
		b.candidateMv[j] = m.mv[j]
		b.candidateFrm[j] = int(m.refFrame[j])
	}
}

func (d *Decoder) ifSameRefFrameAddMv(r, c, refFrame int, usePrev bool) {
	b := &d.b
	for j := range 2 {
		d.getBlockMv(r, c, j, usePrev)
		if b.candidateFrm[j] == refFrame {
			d.addMvRefList(j)
			return
		}
	}
}

func (d *Decoder) ifDiffRefFrameAddMv(r, c, refFrame int, usePrev bool) {
	b := &d.b
	for j := range 2 {
		d.getBlockMv(r, c, j, usePrev)
	}
	mvsSame := b.candidateMv[0] == b.candidateMv[1]
	if b.candidateFrm[0] > intraFrame && b.candidateFrm[0] != refFrame {
		d.scaleMv(0, refFrame)
		d.addMvRefList(0)
	}
	if b.candidateFrm[1] > intraFrame && b.candidateFrm[1] != refFrame && !mvsSame {
		d.scaleMv(1, refFrame)
		d.addMvRefList(1)
	}
}

func (d *Decoder) scaleMv(j, refFrame int) {
	b := &d.b
	if d.h.refFrameSignBias[b.candidateFrm[j]] != d.h.refFrameSignBias[refFrame] {
		b.candidateMv[j].row *= -1
		b.candidateMv[j].col *= -1
	}
}

func (d *Decoder) findBestRefMvs(refList int) {
	b := &d.b
	for i := range 2 {
		dr, dc := b.refListMv[i].row, b.refListMv[i].col
		if !d.h.allowHighPrecisionMv || !useMvHp(b.refListMv[i]) {
			if dr&1 != 0 {
				if dr > 0 {
					dr--
				} else {
					dr++
				}
			}
			if dc&1 != 0 {
				if dc > 0 {
					dc--
				} else {
					dc++
				}
			}
		}
		b.refListMv[i].row = d.clampMvRow(dr, (borderInPixel-interpExtend)<<3)
		b.refListMv[i].col = d.clampMvCol(dc, (borderInPixel-interpExtend)<<3)
	}
	b.nearestMv[refList] = b.refListMv[0]
	b.nearMv[refList] = b.refListMv[1]
	b.bestMv[refList] = b.refListMv[0]
}

func (d *Decoder) appendSub8x8Mvs(blk, refList int) {
	b := &d.b
	var sub [2]mv
	d.findMvRefs(b.refFrame[refList], blk)
	dst := 0
	switch {
	case blk == 0:
		sub[0], sub[1] = b.refListMv[0], b.refListMv[1]
		dst = 2
	case blk <= 2:
		sub[0] = b.blockMvs[refList][0]
		dst = 1
	default:
		sub[0] = b.blockMvs[refList][2]
		dst = 1
		for idx := 1; idx >= 0 && dst < 2; idx-- {
			if b.blockMvs[refList][idx] != sub[0] {
				sub[dst] = b.blockMvs[refList][idx]
				dst++
			}
		}
	}
	for n := 0; n < 2 && dst < 2; n++ {
		if b.refListMv[n] != sub[0] {
			sub[dst] = b.refListMv[n]
			dst++
		}
	}
	if dst < 2 {
		sub[dst] = mv{}
	}
	b.nearestMv[refList] = sub[0]
	b.nearMv[refList] = sub[1]
}
