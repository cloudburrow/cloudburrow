// Package vp9hbd is a VP9 decoder for every profile: 8, 10 and 12 bits a
// sample, 4:2:0, 4:2:2, 4:4:0 and 4:4:4. It follows the decoding process of
// the VP9 Bitstream & Decoding Process Specification (v0.6, 31 March 2016)
// line by line, with samples held as uint16, and was written for CloudBurrow
// (see ../PROVENANCE.md). It favours clarity over speed.
package vp9hbd

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Frame is a decoded, shown frame.
type Frame struct {
	Width, Height              int // visible size
	BitDepth                   int // 8, 10 or 12
	SubsamplingX, SubsamplingY int
	ColorSpace                 int  // the header's color_space (0..7, 7 = sRGB)
	FullRange                  bool // color_range
	Planes                     [3][]uint16
	Strides                    [3]int // in samples
}

// frameBuf is a decoded frame: its planes cover the whole superblock grid.
type frameBuf struct {
	width, height int
	bitDepth      int
	ssX, ssY      int
	colorSpace    int
	fullRange     bool
	planes        [3][]uint16
	strides       [3]int
}

func (f *frameBuf) frame() *Frame {
	return &Frame{Width: f.width, Height: f.height, BitDepth: f.bitDepth, SubsamplingX: f.ssX, SubsamplingY: f.ssY,
		ColorSpace: f.colorSpace, FullRange: f.fullRange, Planes: f.planes, Strides: f.strides}
}

type mv struct{ row, col int32 }

// modeInfo is what the spec keeps for each 8x8 block (6.4.4).
type modeInfo struct {
	skip         bool
	txSize       int8
	miSize       int8
	yMode        int8
	segmentID    uint8
	refFrame     [2]int8
	interpFilter int8
	mv           [2]mv
	subMvs       [2][4]mv
	subModes     [4]int8
}

type prevMi struct {
	refFrame [2]int8
	mv       [2]mv
}

// Decoder decodes a VP9 stream, one container sample at a time.
type Decoder struct {
	h      header
	seg    segParams
	lf     lfDeltas
	fc     probs
	counts counts

	frameContexts [4]probs
	refs          [8]*frameBuf

	sized                 bool
	lastWidth, lastHeight int
	lastShowFrame         bool
	usePrevFrameMvs       bool

	miRows, miCols, sb64Rows, sb64Cols int

	cur            *frameBuf
	mis            []modeInfo
	prevMis        []prevMi
	prevSegmentIds []uint8

	// contexts
	aboveNonzero   [3][]uint8
	leftNonzero    [3][]uint8
	abovePartition []uint8
	leftPartition  []uint8
	aboveSegPred   []uint8
	leftSegPred    []uint8

	// tile
	bd                   boolDecoder
	miColStart, miColEnd int

	// block (the spec's block-level variables)
	b block

	coefs    [1024]int32
	predBuf  [2][64 * 64]int32
	interBuf [(64*2 + 16 + 8) * 64]int32
}

// NewDecoder returns a decoder.
func NewDecoder() *Decoder {
	d := &Decoder{}
	for i := range d.frameContexts {
		d.frameContexts[i] = defaultProbs()
	}
	d.fc = defaultProbs()
	return d
}

// Decode decodes one container sample (a frame or a superframe) and returns
// the frames it shows, in order. The frames stay valid: the decoder never
// writes to a frame's planes after decoding it.
func (d *Decoder) Decode(data []byte) (shown []*Frame, err error) {
	defer func() {
		if r := recover(); r != nil {
			shown, err = nil, fmt.Errorf("vp9hbd: corrupt VP9 data (%v)", r)
		}
	}()
	for _, f := range splitSuperframe(data) {
		fr, err := d.decodeFrame(f)
		if err != nil {
			return nil, err
		}
		if fr != nil {
			shown = append(shown, fr)
		}
	}
	return shown, nil
}

// splitSuperframe is Annex B.
func splitSuperframe(data []byte) [][]byte {
	n := len(data)
	if n == 0 {
		return nil
	}
	last := data[n-1]
	if last&0xe0 == 0xc0 {
		frames := int(last&7) + 1
		mag := int((last>>3)&3) + 1
		idx := 2 + mag*frames
		if n >= idx && data[n-idx] == last {
			var out [][]byte
			p := n - idx + 1
			off := 0
			for range frames {
				sz := 0
				for b := range mag {
					sz |= int(data[p+b]) << (8 * b)
				}
				p += mag
				if off+sz > n-idx {
					break
				}
				out = append(out, data[off:off+sz])
				off += sz
			}
			return out
		}
	}
	return [][]byte{data}
}

func (d *Decoder) decodeFrame(data []byte) (*Frame, error) {
	if len(data) == 0 {
		return nil, nil
	}
	r := &bitReader{data: data}
	if err := d.readUncompressedHeader(r); err != nil {
		return nil, err
	}
	h := &d.h
	if h.showExistingFrame {
		ref := d.refs[h.frameToShow]
		if ref == nil {
			return nil, errors.New("vp9hbd: show_existing_frame of an empty slot")
		}
		return ref.frame(), nil
	}
	// trailing_bits
	r.pos = (r.pos + 7) &^ 7
	if h.headerSize == 0 {
		return nil, errors.New("vp9hbd: no compressed header")
	}
	start := r.pos >> 3
	if start+h.headerSize > len(data) {
		return nil, errData
	}
	// load_probs(ctx), load_probs2(ctx)
	d.fc = d.frameContexts[h.frameContextIdx]
	d.counts = counts{}
	if err := d.bd.init(data[start : start+h.headerSize]); err != nil {
		return nil, err
	}
	d.readCompressedHeader(&d.bd)
	d.allocFrame()
	if err := d.decodeTiles(data[start+h.headerSize:]); err != nil {
		return nil, err
	}
	if h.lfLevel != 0 {
		d.loopFilter()
	}
	// refresh_probs
	if !h.errorResilient && !h.frameParallel {
		cur := d.fc
		saved := d.frameContexts[h.frameContextIdx]
		d.fc = saved
		d.fc.tx8, d.fc.tx16, d.fc.tx32, d.fc.skip = cur.tx8, cur.tx16, cur.tx32, cur.skip
		d.adaptCoefProbs()
		if !h.frameIsIntra {
			d.fc.tx8, d.fc.tx16, d.fc.tx32, d.fc.skip = saved.tx8, saved.tx16, saved.tx32, saved.skip
			d.adaptNoncoefProbs()
		} else {
			// load_probs loads every table but tx and skip: the
			// others keep the saved values, which an intra frame's
			// compressed header cannot change anyway.
			_ = cur
		}
	}
	if h.refreshFrameContext {
		d.frameContexts[h.frameContextIdx] = d.fc
	}
	// spec 8.1 step 3
	if d.seg.enabled && d.seg.updateMap {
		for i, m := range d.mis {
			d.prevSegmentIds[i] = m.segmentID
		}
	}
	// reference update (8.10)
	for i := range 8 {
		if (h.refreshFrameFlags>>uint(i))&1 == 1 {
			d.refs[i] = d.cur
		}
	}
	if len(d.prevMis) != len(d.mis) {
		d.prevMis = make([]prevMi, len(d.mis))
	}
	for i, m := range d.mis {
		d.prevMis[i] = prevMi{refFrame: m.refFrame, mv: m.mv}
	}
	if h.showFrame {
		return d.cur.frame(), nil
	}
	return nil, nil
}

// allocFrame starts a new current frame and the per-frame arrays.
func (d *Decoder) allocFrame() {
	h := &d.h
	f := &frameBuf{width: h.width, height: h.height, bitDepth: h.bitDepth, ssX: h.ssX, ssY: h.ssY,
		colorSpace: h.colorSpace, fullRange: h.colorRange}
	w, ht := d.sb64Cols*64, d.sb64Rows*64
	for p := range 3 {
		pw, ph := w, ht
		if p > 0 {
			pw, ph = w>>uint(h.ssX), ht>>uint(h.ssY)
		}
		f.strides[p] = pw
		f.planes[p] = make([]uint16, pw*ph)
	}
	d.cur = f
	n := d.miRows * d.miCols
	if cap(d.mis) >= n {
		d.mis = d.mis[:n]
		clear(d.mis)
	} else {
		d.mis = make([]modeInfo, n)
	}
	if len(d.prevSegmentIds) != n {
		d.prevSegmentIds = make([]uint8, n)
	}
	if len(d.prevMis) != n {
		d.prevMis = make([]prevMi, n)
	}
}

func (d *Decoder) decodeTiles(data []byte) error {
	h := &d.h
	tileCols := 1 << uint(h.tileColsLog2)
	tileRows := 1 << uint(h.tileRowsLog2)
	// clear_above_context
	aw := d.sb64Cols*16 + 32
	for p := range 3 {
		d.aboveNonzero[p] = make([]uint8, aw)
		d.leftNonzero[p] = make([]uint8, d.sb64Rows*16+32)
	}
	d.abovePartition = make([]uint8, d.sb64Cols*8+8)
	d.leftPartition = make([]uint8, d.sb64Rows*8+8)
	d.aboveSegPred = make([]uint8, d.sb64Cols*8+8)
	d.leftSegPred = make([]uint8, d.sb64Rows*8+8)
	for tileRow := range tileRows {
		for tileCol := range tileCols {
			lastTile := tileRow == tileRows-1 && tileCol == tileCols-1
			var size int
			if lastTile {
				size = len(data)
			} else {
				if len(data) < 4 {
					return errData
				}
				size = int(binary.BigEndian.Uint32(data))
				data = data[4:]
				if size > len(data) {
					return errData
				}
			}
			miRowStart := tileOffset(tileRow, d.miRows, h.tileRowsLog2)
			miRowEnd := tileOffset(tileRow+1, d.miRows, h.tileRowsLog2)
			d.miColStart = tileOffset(tileCol, d.miCols, h.tileColsLog2)
			d.miColEnd = tileOffset(tileCol+1, d.miCols, h.tileColsLog2)
			if err := d.bd.init(data[:size]); err != nil {
				return err
			}
			for r := miRowStart; r < miRowEnd; r += 8 {
				// clear_left_context
				for p := range 3 {
					clear(d.leftNonzero[p])
				}
				clear(d.leftPartition)
				clear(d.leftSegPred)
				for c := d.miColStart; c < d.miColEnd; c += 8 {
					d.decodePartition(r, c, block64x64)
				}
			}
			data = data[size:]
		}
	}
	return nil
}

func tileOffset(tileNum, mis, log2 int) int {
	sbs := (mis + 7) >> 3
	offset := ((tileNum * sbs) >> uint(log2)) << 3
	return min(offset, mis)
}

func (d *Decoder) decodePartition(r, c, bsize int) {
	if r >= d.miRows || c >= d.miCols {
		return
	}
	num8x8 := num8x8W[bsize]
	half := num8x8 >> 1
	hasRows := (r + half) < d.miRows
	hasCols := (c + half) < d.miCols
	// partition context (9.3.2)
	bsl := miWidthLog2[bsize]
	boffset := miWidthLog2[block64x64] - bsl
	above, left := 0, 0
	for i := range num8x8 {
		above |= int(d.abovePartition[c+i])
		left |= int(d.leftPartition[(r&7)+i])
	}
	a := 0
	if above&(1<<uint(boffset)) != 0 {
		a = 1
	}
	l := 0
	if left&(1<<uint(boffset)) != 0 {
		l = 1
	}
	ctx := bsl*4 + l*2 + a
	var probs []uint8
	if d.h.frameIsIntra {
		probs = kfPartitionProbs[ctx][:]
	} else {
		probs = d.fc.partition[ctx][:]
	}
	var partition int
	switch {
	case hasRows && hasCols:
		partition = d.bd.tree(partitionTree, probs)
	case hasCols:
		if d.bd.read(int(probs[1])) == 1 {
			partition = partSplit
		} else {
			partition = partHorz
		}
	case hasRows:
		if d.bd.read(int(probs[2])) == 1 {
			partition = partSplit
		} else {
			partition = partVert
		}
	default:
		partition = partSplit
	}
	if !d.h.frameIsIntra {
		d.counts.partition[ctx][partition]++
	}
	subsize := subsizeLookup[partition][bsize]
	switch {
	case subsize < block8x8 || partition == partNone:
		d.decodeBlock(r, c, subsize)
	case partition == partHorz:
		d.decodeBlock(r, c, subsize)
		if hasRows {
			d.decodeBlock(r+half, c, subsize)
		}
	case partition == partVert:
		d.decodeBlock(r, c, subsize)
		if hasCols {
			d.decodeBlock(r, c+half, subsize)
		}
	default:
		d.decodePartition(r, c, subsize)
		d.decodePartition(r, c+half, subsize)
		d.decodePartition(r+half, c, subsize)
		d.decodePartition(r+half, c+half, subsize)
	}
	if bsize == block8x8 || partition != partSplit {
		for i := range num8x8 {
			d.abovePartition[c+i] = uint8(15 >> uint(bWidthLog2[subsize]))
			d.leftPartition[(r&7)+i] = uint8(15 >> uint(bHeightLog2[subsize]))
		}
	}
}
