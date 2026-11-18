package vp9hbd

import "github.com/cloudburrow/cloudburrow/third_party/govpx/internal/vp9/tables"

// planeNum4x4 is a plane's size in 4x4 units for a block of size bsize
// (at least 8x8). It follows libvpx (n4_w, n4_h) rather than the spec's
// get_plane_block_size: in 4:2:2 and 4:4:0 an 8x16 or 16x8 block has a
// 4x16 or 16x4 chroma area, for which no block size exists.
func (d *Decoder) planeNum4x4(bsize, plane int) (w, h int) {
	w, h = 2*num8x8W[bsize], 2*num8x8H[bsize]
	if plane > 0 {
		w >>= uint(d.h.ssX)
		h >>= uint(d.h.ssY)
	}
	return w, h
}

// uvTxSize is get_uv_tx_size: the largest transform that fits the chroma
// area, at most the luma transform size (libvpx uv_txsize_lookup).
func (d *Decoder) uvTxSize(miSize, txSize int) int {
	if miSize < block8x8 {
		return tx4x4
	}
	w, h := d.planeNum4x4(miSize, 1)
	m := min(w, h)
	largest := tx4x4
	for largest < tx32x32 && 2<<uint(largest) <= m {
		largest++
	}
	return min(txSize, largest)
}

// residual is spec 6.4.21: prediction, tokens and reconstruction.
func (d *Decoder) residual() {
	b := &d.b
	bsize := max(b.miSize, block8x8)
	for plane := range 3 {
		txSz := b.txSize
		subX, subY := 0, 0
		if plane > 0 {
			txSz = d.uvTxSize(b.miSize, b.txSize)
			subX, subY = d.h.ssX, d.h.ssY
		}
		step := 1 << uint(txSz)
		n4w, n4h := d.planeNum4x4(bsize, plane)
		baseX := (b.miCol * 8) >> uint(subX)
		baseY := (b.miRow * 8) >> uint(subY)
		if b.isInter {
			if b.miSize < block8x8 {
				for y := range n4h {
					for x := range n4w {
						d.predictInter(plane, baseX+4*x, baseY+4*y, 4, 4, y*n4w+x)
					}
				}
			} else {
				d.predictInter(plane, baseX, baseY, n4w*4, n4h*4, 0)
			}
		}
		maxX := (d.miCols * 8) >> uint(subX)
		maxY := (d.miRows * 8) >> uint(subY)
		blockIdx := 0
		above := d.aboveNonzero[plane]
		left := d.leftNonzero[plane]
		for y := 0; y < n4h; y += step {
			for x := 0; x < n4w; x += step {
				startX := baseX + 4*x
				startY := baseY + 4*y
				nonzero := uint8(0)
				if startX < maxX && startY < maxY {
					if !b.isInter {
						d.predictIntra(plane, startX, startY, b.availL || x > 0, b.availU || y > 0, x+step < n4w, txSz, blockIdx)
					}
					if !b.skip {
						eob := d.tokens(plane, startX, startY, txSz, blockIdx)
						if eob > 0 {
							nonzero = 1
							d.reconstruct(plane, startX, startY, txSz, d.txType(plane, txSz, blockIdx))
						}
					}
				}
				for i := range step {
					above[(startX>>2)+i] = nonzero
					left[(startY>>2)+i] = nonzero
				}
				blockIdx++
			}
		}
	}
}

func (d *Decoder) txType(plane, txSz, blockIdx int) int {
	b := &d.b
	if plane > 0 || txSz == tx32x32 {
		return dctDct
	}
	if txSz == tx4x4 {
		if d.h.lossless || b.isInter {
			return dctDct
		}
		if b.miSize < block8x8 {
			return mode2Txfm[b.subModes[blockIdx]]
		}
		return mode2Txfm[b.yMode]
	}
	if b.isInter {
		return dctDct
	}
	return mode2Txfm[b.yMode]
}

func scanFor(txSz, txType int) []int16 {
	switch txSz {
	case tx4x4:
		switch txType {
		case adstDct:
			return tables.RowScan4x4[:]
		case dctAdst:
			return tables.ColScan4x4[:]
		}
		return tables.DefaultScan4x4[:]
	case tx8x8:
		switch txType {
		case adstDct:
			return tables.RowScan8x8[:]
		case dctAdst:
			return tables.ColScan8x8[:]
		}
		return tables.DefaultScan8x8[:]
	case tx16x16:
		switch txType {
		case adstDct:
			return tables.RowScan16x16[:]
		case dctAdst:
			return tables.ColScan16x16[:]
		}
		return tables.DefaultScan16x16[:]
	}
	return tables.DefaultScan32x32[:]
}

// tokens is spec 6.4.24 with the dequantization of 8.6.2 folded in: it
// leaves the dequantized coefficients in d.coefs and returns the number of
// scan positions read (0: the block's first more_coefs was 0).
func (d *Decoder) tokens(plane, startX, startY, txSz, blockIdx int) int {
	b := &d.b
	segEob := 16 << uint(txSz<<1)
	txType := d.txType(plane, txSz, blockIdx)
	scan := scanFor(txSz, txType)
	ref := btoi(b.isInter)
	ptype := btoi(plane > 0)
	probs := &d.fc.coef[txSz][ptype][ref]
	tokCounts := &d.counts.token[txSz][ptype][ref]
	moreCounts := &d.counts.moreCoefs[txSz][ptype][ref]
	// dequantization
	qindex := d.qindex()
	var dcQ, acQ int32
	bd := (d.h.bitDepth - 8) >> 1
	if plane == 0 {
		dcQ = int32(dcQLookup[bd][clamp255(qindex+d.h.deltaQYDc)])
		acQ = int32(acQLookup[bd][clamp255(qindex)])
	} else {
		dcQ = int32(dcQLookup[bd][clamp255(qindex+d.h.deltaQUvDc)])
		acQ = int32(acQLookup[bd][clamp255(qindex+d.h.deltaQUvAc)])
	}
	dqShift := uint(0)
	if txSz == tx32x32 {
		dqShift = 1
	}
	// initial context
	subX, subY := 0, 0
	if plane > 0 {
		subX, subY = d.h.ssX, d.h.ssY
	}
	maxX4 := (2 * d.miCols) >> uint(subX)
	maxY4 := (2 * d.miRows) >> uint(subY)
	x4, y4 := startX>>2, startY>>2
	var above, left uint8
	for i := range 1 << uint(txSz) {
		if x4+i < maxX4 {
			above |= d.aboveNonzero[plane][x4+i]
		}
		if y4+i < maxY4 {
			left |= d.leftNonzero[plane][y4+i]
		}
	}
	ctx := int(above + left)
	var tokenCache [1024]uint8
	n := 4 << uint(txSz)
	log2n := uint(2 + txSz)
	coefs := d.coefs[:n*n]
	clear(coefs)
	checkEob := true
	c := 0
	for ; c < segEob; c++ {
		pos := int(scan[c])
		var band int
		if txSz == tx4x4 {
			band = int(tables.CoefbandTrans4x4[c])
		} else {
			band = int(tables.CoefbandTrans8x8Plus[c])
		}
		if c > 0 {
			// neighbours (9.3.2)
			i, j := pos>>log2n, pos&(n-1)
			var a, a2 int
			switch {
			case i > 0 && j > 0:
				a, a2 = (i-1)*n+j, i*n+j-1
				switch txType {
				case dctAdst:
					a2 = a
				case adstDct:
					a = a2
				}
			case i > 0:
				a = (i-1)*n + j
				a2 = a
			default:
				a = i*n + j - 1
				a2 = a
			}
			ctx = (1 + int(tokenCache[a]) + int(tokenCache[a2])) >> 1
		}
		p := &probs[band][ctx]
		if checkEob {
			more := d.bd.read(int(p[0]))
			moreCounts[band][ctx][more]++
			if more == 0 {
				break
			}
		}
		var token int
		if d.bd.read(int(p[1])) == 0 {
			token = 0
		} else if d.bd.read(int(p[2])) == 0 {
			token = 1
		} else {
			token = d.readHighToken(p[2])
		}
		tokCounts[band][ctx][min(2, token)]++
		tokenCache[pos] = energyClass[token]
		if token == 0 {
			checkEob = false
			continue
		}
		coef := d.readCoef(token)
		q := acQ
		if c == 0 {
			q = dcQ
		}
		v := int32((int64(coef) * int64(q)) >> dqShift)
		if d.bd.read(128) == 1 {
			v = -v
		}
		coefs[pos] = v
		checkEob = true
	}
	if c > 0 {
		b.eobTotal++
	}
	return c
}

// readHighToken reads TWO_TOKEN and above (token_tree from node 2), with
// the Pareto probabilities.
func (d *Decoder) readHighToken(p2 uint8) int {
	pp := &tables.Pareto8Full[p2-1]
	// token_tree nodes 4.. map to pp[node-2]
	tree := [16]int8{2, 6, -2, 4, -3, -4, 8, 10, -5, -6, 12, 14, -7, -8, -9, -10}
	n := 0
	for {
		n = int(tree[n+d.bd.read(int(pp[n>>1]))])
		if n <= 0 {
			return -n
		}
	}
}

func (d *Decoder) readCoef(token int) int {
	cat := extraBits[token][0]
	numExtra := extraBits[token][1]
	coef := extraBits[token][2]
	if token == 10 {
		bd := d.h.bitDepth
		for e := 0; e < bd-8; e++ {
			coef += d.bd.read(255) << uint(5+bd-e)
		}
	}
	for e := range numExtra {
		coef += d.bd.read(catProbs[cat][e]) << uint(numExtra-1-e)
	}
	return coef
}

func clamp255(v int) int { return min(255, max(0, v)) }

func (d *Decoder) qindex() int {
	if d.segFeatureActive(segLvlAltQ) {
		data := d.seg.featureData[d.b.segmentID][segLvlAltQ]
		if !d.seg.absOrDelta {
			data += d.h.baseQIdx
		}
		return clamp255(data)
	}
	return d.h.baseQIdx
}

var (
	dcQLookup = [3]*[256]int16{&tables.DcQLookup8, &tables.DcQLookup10, &tables.DcQLookup12}
	acQLookup = [3]*[256]int16{&tables.AcQLookup8, &tables.AcQLookup10, &tables.AcQLookup12}
)
