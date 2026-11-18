package vp9hbd

// loopFilter is spec 8.8.
func (d *Decoder) loopFilter() {
	h := &d.h
	// 8.8.1 loop filter frame init
	var lvlLookup [8][4][2]int
	scale := 1 << uint(h.lfLevel>>5)
	for seg := range 8 {
		lvlSeg := h.lfLevel
		if d.seg.enabled && d.seg.featureEnabled[seg][segLvlAltL] {
			if d.seg.absOrDelta {
				lvlSeg = d.seg.featureData[seg][segLvlAltL]
			} else {
				lvlSeg = d.seg.featureData[seg][segLvlAltL] + h.lfLevel
			}
			lvlSeg = min(63, max(0, lvlSeg))
		}
		if !d.lf.enabled {
			for ref := range 4 {
				for mode := range 2 {
					lvlLookup[seg][ref][mode] = lvlSeg
				}
			}
			continue
		}
		intraLvl := lvlSeg + d.lf.ref[intraFrame]*scale
		lvlLookup[seg][intraFrame][0] = min(63, max(0, intraLvl))
		for ref := lastFrame; ref < 4; ref++ {
			for mode := range 2 {
				interLvl := lvlSeg + d.lf.ref[ref]*scale + d.lf.mode[mode]*scale
				lvlLookup[seg][ref][mode] = min(63, max(0, interLvl))
			}
		}
	}
	for row := 0; row < d.miRows; row += 8 {
		for col := 0; col < d.miCols; col += 8 {
			for plane := range 3 {
				for pass := range 2 {
					d.sbLoopFilter(plane, pass, row, col, &lvlLookup)
				}
			}
		}
	}
}

// sbLoopFilter is spec 8.8.2.
func (d *Decoder) sbLoopFilter(plane, pass, row, col int, lvlLookup *[8][4][2]int) {
	subX, subY := 0, 0
	if plane > 0 {
		subX, subY = d.h.ssX, d.h.ssY
	}
	var dx, dy, sub, edgeLen int
	if pass == 0 {
		dx, dy, sub, edgeLen = 1, 0, subX, 64>>uint(subY)
	} else {
		dx, dy, sub, edgeLen = 0, 1, subY, 64>>uint(subX)
	}
	for edge := 0; edge < 16>>uint(sub); edge++ {
		for i := range edgeLen {
			var x, y int
			if pass == 0 {
				x = col*8 + edge*(4<<uint(subX))
				y = row*8 + (i << uint(subY))
			} else {
				x = col*8 + (i << uint(subX))
				y = row*8 + edge*(4<<uint(subY))
			}
			// onScreen
			if x >= 8*d.miCols || y >= 8*d.miRows || (pass == 0 && x == 0) || (pass == 1 && y == 0) {
				continue
			}
			loopCol := ((x >> 3) >> uint(subX)) << uint(subX)
			loopRow := ((y >> 3) >> uint(subY)) << uint(subY)
			m := d.mi(loopRow, loopCol)
			miSize := int(m.miSize)
			txSz := int(m.txSize)
			if plane > 0 {
				txSz = d.uvTxSize(miSize, txSz)
			}
			sbSize := miSize
			if sub == 1 {
				sbSize = max(block16x16, miSize)
			}
			isIntra := m.refFrame[0] <= intraFrame
			var isBlockEdge bool
			if pass == 0 {
				isBlockEdge = x%(8*num8x8W[sbSize]) == 0
			} else {
				isBlockEdge = y%(8*num8x8H[sbSize]) == 0
			}
			var isTxEdge bool
			if pass == 1 && subX == 1 && d.miCols&1 == 1 && edge&1 == 1 && x+8 >= d.miCols*8 {
				isTxEdge = false
			} else {
				isTxEdge = edge%(1<<uint(txSz)) == 0
			}
			is32Edge := edge%8 == 0
			apply := isBlockEdge || (isTxEdge && isIntra) || (isTxEdge && !m.skip)
			if !apply {
				continue
			}
			// 8.8.3 filter size
			baseSize := min(tx16x16, txSz)
			if txSz == tx4x4 && is32Edge {
				baseSize = tx8x8
			}
			filterSize := baseSize
			if pass == 0 && subX == 1 && baseSize == tx16x16 && x>>3 == d.miCols-1 {
				filterSize = tx8x8
			} else if pass == 1 && subY == 1 && baseSize == tx16x16 && y>>3 == d.miRows-1 {
				filterSize = tx8x8
			}
			// 8.8.4 adaptive filter strength
			mode := int(m.yMode)
			modeType := 0
			if mode == nearestMv || mode == nearMv || mode == newMv {
				modeType = 1
			}
			ref := max(0, int(m.refFrame[0]))
			lvl := lvlLookup[m.segmentID][ref][modeType]
			if lvl == 0 {
				continue
			}
			sharp := d.h.lfSharpness
			shift := 0
			if sharp > 4 {
				shift = 2
			} else if sharp > 0 {
				shift = 1
			}
			var limit int
			if sharp > 0 {
				limit = min(max(lvl>>uint(shift), 1), 9-sharp)
			} else {
				limit = max(1, lvl>>uint(shift))
			}
			blimit := 2*(lvl+2) + limit
			thresh := lvl >> 4
			d.sampleFilter(x>>uint(subX), y>>uint(subY), plane, limit, blimit, thresh, dx, dy, filterSize)
		}
	}
}

func iabs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// sampleFilter is spec 8.8.5.
func (d *Decoder) sampleFilter(x, y, plane, limit, blimit, thresh, dx, dy, filterSize int) {
	pl := d.cur.planes[plane]
	stride := d.cur.strides[plane]
	step := dy*stride + dx
	pos := y*stride + x
	at := func(k int) int { return int(pl[pos+k*step]) }
	q0, q1, q2, q3 := at(0), at(1), at(2), at(3)
	p0, p1, p2, p3 := at(-1), at(-2), at(-3), at(-4)
	sh := uint(d.h.bitDepth - 8)
	// filter mask (8.8.5.1)
	threshBd := thresh << sh
	hev := iabs(p1-p0) > threshBd || iabs(q1-q0) > threshBd
	limitBd := limit << sh
	blimitBd := blimit << sh
	if iabs(p3-p2) > limitBd || iabs(p2-p1) > limitBd || iabs(p1-p0) > limitBd ||
		iabs(q1-q0) > limitBd || iabs(q2-q1) > limitBd || iabs(q3-q2) > limitBd ||
		iabs(p0-q0)*2+iabs(p1-q1)/2 > blimitBd {
		return
	}
	one := 1 << sh
	flat := false
	if filterSize >= tx8x8 {
		flat = !(iabs(p1-p0) > one || iabs(q1-q0) > one || iabs(p2-p0) > one ||
			iabs(q2-q0) > one || iabs(p3-p0) > one || iabs(q3-q0) > one)
	}
	flat2 := false
	if filterSize >= tx16x16 {
		q4, q5, q6, q7 := at(4), at(5), at(6), at(7)
		p4, p5, p6, p7 := at(-5), at(-6), at(-7), at(-8)
		flat2 = !(iabs(p7-p0) > one || iabs(q7-q0) > one || iabs(p6-p0) > one ||
			iabs(q6-q0) > one || iabs(p5-p0) > one || iabs(q5-q0) > one ||
			iabs(p4-p0) > one || iabs(q4-q0) > one)
	}
	switch {
	case filterSize == tx4x4 || !flat:
		// narrow filter (8.8.5.2)
		bd := d.h.bitDepth
		lo, hi := -(1 << uint(bd-1)), (1<<uint(bd-1))-1
		c := func(v int) int { return min(max(v, lo), hi) }
		off := 0x80 << sh
		ps1, ps0, qs0, qs1 := p1-off, p0-off, q0-off, q1-off
		filter := 0
		if hev {
			filter = c(ps1 - qs1)
		}
		filter = c(filter + 3*(qs0-ps0))
		filter1 := c(filter+4) >> 3
		filter2 := c(filter+3) >> 3
		pl[pos] = uint16(c(qs0-filter1) + off)
		pl[pos-step] = uint16(c(ps0+filter2) + off)
		if !hev {
			f := (filter1 + 1) >> 1
			pl[pos+step] = uint16(c(qs1-f) + off)
			pl[pos-2*step] = uint16(c(ps1+f) + off)
		}
	case filterSize == tx8x8 || !flat2:
		wideFilter(pl, pos, step, 3)
	default:
		wideFilter(pl, pos, step, 4)
	}
}

// wideFilter is spec 8.8.5.3.
func wideFilter(pl []uint16, pos, step int, log2Size uint) {
	n := (1 << (log2Size - 1)) - 1
	var f [16]int
	for i := -n; i < n; i++ {
		t := int(pl[pos+i*step])
		for j := -n; j <= n; j++ {
			p := min(max(i+j, -(n+1)), n)
			t += int(pl[pos+p*step])
		}
		f[i+n] = (t + (1 << (log2Size - 1))) >> log2Size
	}
	for i := -n; i < n; i++ {
		pl[pos+i*step] = uint16(f[i+n])
	}
}
