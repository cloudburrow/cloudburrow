package vp9hbd

import "fmt"

// header holds a frame's uncompressed and compressed header (spec 6.2, 6.3).
type header struct {
	profile              int
	showExistingFrame    bool
	frameToShow          int
	frameType            int
	lastFrameType        int
	showFrame            bool
	errorResilient       bool
	intraOnly            bool
	frameIsIntra         bool
	resetFrameContext    int
	bitDepth             int
	colorSpace           int
	colorRange           bool
	ssX, ssY             int
	width, height        int
	refreshFrameFlags    int
	refFrameIdx          [3]int
	refFrameSignBias     [4]int
	allowHighPrecisionMv bool
	interpFilter         int
	refreshFrameContext  bool
	frameParallel        bool
	frameContextIdx      int
	headerSize           int

	lfLevel, lfSharpness int

	baseQIdx                          int
	deltaQYDc, deltaQUvDc, deltaQUvAc int
	lossless                          bool

	tileColsLog2, tileRowsLog2 int

	txMode        int
	referenceMode int
	compFixedRef  int
	compVarRef    [2]int
}

// segmentation and loop filter deltas persist across frames.
type segParams struct {
	enabled, updateMap, temporalUpdate, updateData, absOrDelta bool
	treeProbs                                                  [7]uint8
	predProbs                                                  [3]uint8
	featureEnabled                                             [8][4]bool
	featureData                                                [8][4]int
}

type lfDeltas struct {
	enabled, update bool
	ref             [4]int
	mode            [2]int
}

func (d *Decoder) setupPastIndependence() {
	s := &d.seg
	s.featureEnabled = [8][4]bool{}
	s.featureData = [8][4]int{}
	s.absOrDelta = false
	clear(d.prevSegmentIds)
	d.lf.enabled = true
	d.lf.ref = [4]int{1, 0, -1, -1}
	d.lf.mode = [2]int{}
	d.h.refFrameSignBias = [4]int{}
	d.fc = defaultProbs()
}

func (d *Decoder) readUncompressedHeader(r *bitReader) error {
	h := &d.h
	if r.f(2) != 2 {
		return fmt.Errorf("vp9hbd: bad frame marker")
	}
	lo := r.f(1)
	h.profile = r.f(1)<<1 + lo
	if h.profile == 3 && r.f(1) != 0 {
		return errData
	}
	h.showExistingFrame = r.f(1) == 1
	if h.showExistingFrame {
		h.frameToShow = r.f(3)
		h.headerSize = 0
		h.refreshFrameFlags = 0
		h.lfLevel = 0
		return nil
	}
	h.lastFrameType = h.frameType
	h.frameType = r.f(1)
	h.showFrame = r.f(1) == 1
	h.errorResilient = r.f(1) == 1
	if h.frameType == keyFrame {
		if err := frameSyncCode(r); err != nil {
			return err
		}
		if err := h.colorConfig(r); err != nil {
			return err
		}
		h.frameSize(r)
		renderSize(r)
		h.refreshFrameFlags = 0xFF
		h.frameIsIntra = true
		h.intraOnly = false
	} else {
		h.intraOnly = false
		if !h.showFrame {
			h.intraOnly = r.f(1) == 1
		}
		h.frameIsIntra = h.intraOnly
		h.resetFrameContext = 0
		if !h.errorResilient {
			h.resetFrameContext = r.f(2)
		}
		if h.intraOnly {
			if err := frameSyncCode(r); err != nil {
				return err
			}
			if h.profile > 0 {
				if err := h.colorConfig(r); err != nil {
					return err
				}
			} else {
				h.colorSpace = 1
				h.ssX, h.ssY = 1, 1
				h.bitDepth = 8
			}
			h.refreshFrameFlags = r.f(8)
			h.frameSize(r)
			renderSize(r)
		} else {
			h.refreshFrameFlags = r.f(8)
			for i := range 3 {
				h.refFrameIdx[i] = r.f(3)
				h.refFrameSignBias[lastFrame+i] = r.f(1)
			}
			found := false
			for i := range 3 {
				if r.f(1) == 1 {
					ref := d.refs[h.refFrameIdx[i]]
					if ref == nil {
						return fmt.Errorf("vp9hbd: an inter frame refers to an empty reference slot")
					}
					h.width, h.height = ref.width, ref.height
					found = true
					break
				}
			}
			if !found {
				h.frameSize(r)
			}
			renderSize(r)
			h.allowHighPrecisionMv = r.f(1) == 1
			if r.f(1) == 1 {
				h.interpFilter = switchable
			} else {
				h.interpFilter = literalToType[r.f(2)]
			}
			for i := range 3 {
				ref := d.refs[h.refFrameIdx[i]]
				if ref == nil {
					return fmt.Errorf("vp9hbd: an inter frame refers to an empty reference slot")
				}
				if ref.bitDepth != h.bitDepth || ref.ssX != h.ssX || ref.ssY != h.ssY {
					return fmt.Errorf("vp9hbd: a reference frame's format differs from the frame's")
				}
			}
		}
	}
	if !h.errorResilient {
		h.refreshFrameContext = r.f(1) == 1
		h.frameParallel = r.f(1) == 1
	} else {
		h.refreshFrameContext = false
		h.frameParallel = true
	}
	h.frameContextIdx = r.f(2)
	// compute_image_size's side effects (spec 7.2.6) need the frame size,
	// which every path above has set.
	d.computeImageSize()
	if h.frameIsIntra || h.errorResilient {
		d.setupPastIndependence()
		if h.frameType == keyFrame || h.errorResilient || h.resetFrameContext == 3 {
			for i := range 4 {
				d.frameContexts[i] = d.fc
			}
		} else if h.resetFrameContext == 2 {
			d.frameContexts[h.frameContextIdx] = d.fc
		}
		h.frameContextIdx = 0
	}
	// loop_filter_params
	h.lfLevel = r.f(6)
	h.lfSharpness = r.f(3)
	d.lf.enabled = r.f(1) == 1
	d.lf.update = false
	if d.lf.enabled {
		d.lf.update = r.f(1) == 1
		if d.lf.update {
			for i := range 4 {
				if r.f(1) == 1 {
					d.lf.ref[i] = r.s(6)
				}
			}
			for i := range 2 {
				if r.f(1) == 1 {
					d.lf.mode[i] = r.s(6)
				}
			}
		}
	}
	// quantization_params
	h.baseQIdx = r.f(8)
	readDeltaQ := func() int {
		if r.f(1) == 1 {
			return r.s(4)
		}
		return 0
	}
	h.deltaQYDc = readDeltaQ()
	h.deltaQUvDc = readDeltaQ()
	h.deltaQUvAc = readDeltaQ()
	h.lossless = h.baseQIdx == 0 && h.deltaQYDc == 0 && h.deltaQUvDc == 0 && h.deltaQUvAc == 0
	// segmentation_params
	s := &d.seg
	s.enabled = r.f(1) == 1
	s.updateMap, s.temporalUpdate, s.updateData = false, false, false
	if s.enabled {
		s.updateMap = r.f(1) == 1
		if s.updateMap {
			for i := range 7 {
				s.treeProbs[i] = readProb(r)
			}
			s.temporalUpdate = r.f(1) == 1
			for i := range 3 {
				s.predProbs[i] = 255
				if s.temporalUpdate {
					s.predProbs[i] = readProb(r)
				}
			}
		}
		s.updateData = r.f(1) == 1
		if s.updateData {
			s.absOrDelta = r.f(1) == 1
			for i := range 8 {
				for j := range 4 {
					v := 0
					s.featureEnabled[i][j] = r.f(1) == 1
					if s.featureEnabled[i][j] {
						v = r.f(segFeatureBits[j])
						if segFeatureSigned[j] && r.f(1) == 1 {
							v = -v
						}
					}
					s.featureData[i][j] = v
				}
			}
		}
	}
	// tile_info
	minLog2 := 0
	for (64 << uint(minLog2)) < d.sb64Cols {
		minLog2++
	}
	maxLog2 := 1
	for (d.sb64Cols >> uint(maxLog2)) >= 4 {
		maxLog2++
	}
	maxLog2--
	h.tileColsLog2 = minLog2
	for h.tileColsLog2 < maxLog2 {
		if r.f(1) == 1 {
			h.tileColsLog2++
		} else {
			break
		}
	}
	h.tileRowsLog2 = r.f(1)
	if h.tileRowsLog2 == 1 {
		h.tileRowsLog2 += r.f(1)
	}
	h.headerSize = r.f(16)
	return nil
}

func readProb(r *bitReader) uint8 {
	if r.f(1) == 1 {
		return uint8(r.f(8))
	}
	return 255
}

func frameSyncCode(r *bitReader) error {
	if r.f(8) != 0x49 || r.f(8) != 0x83 || r.f(8) != 0x42 {
		return fmt.Errorf("vp9hbd: bad frame sync code")
	}
	return nil
}

func (h *header) colorConfig(r *bitReader) error {
	h.bitDepth = 8
	if h.profile >= 2 {
		h.bitDepth = 10
		if r.f(1) == 1 {
			h.bitDepth = 12
		}
	}
	h.colorSpace = r.f(3)
	if h.colorSpace != csRGB {
		h.colorRange = r.f(1) == 1
		if h.profile == 1 || h.profile == 3 {
			h.ssX = r.f(1)
			h.ssY = r.f(1)
			if h.ssX == 1 && h.ssY == 1 {
				return fmt.Errorf("vp9hbd: 4:2:0 in profile %d", h.profile)
			}
			if r.f(1) != 0 {
				return errData
			}
		} else {
			h.ssX, h.ssY = 1, 1
		}
	} else {
		h.colorRange = true
		if h.profile == 1 || h.profile == 3 {
			h.ssX, h.ssY = 0, 0
			if r.f(1) != 0 {
				return errData
			}
		} else {
			return fmt.Errorf("vp9hbd: RGB in profile %d", h.profile)
		}
	}
	return nil
}

func (h *header) frameSize(r *bitReader) {
	h.width = r.f(16) + 1
	h.height = r.f(16) + 1
}

func renderSize(r *bitReader) {
	if r.f(1) == 1 {
		r.f(16)
		r.f(16)
	}
}

// computeImageSize is compute_image_size with its semantics (spec 7.2.6).
func (d *Decoder) computeImageSize() {
	h := &d.h
	d.miCols = (h.width + 7) >> 3
	d.miRows = (h.height + 7) >> 3
	d.sb64Cols = (d.miCols + 7) >> 3
	d.sb64Rows = (d.miRows + 7) >> 3
	first := !d.sized
	same := d.sized && h.width == d.lastWidth && h.height == d.lastHeight
	if !same {
		d.prevSegmentIds = make([]uint8, d.miRows*d.miCols)
	}
	d.usePrevFrameMvs = !first && same && d.lastShowFrame && !h.errorResilient && !h.frameIsIntra
	d.sized = true
	d.lastWidth, d.lastHeight = h.width, h.height
	d.lastShowFrame = h.showFrame
}

// Compressed header (spec 6.3).
func (d *Decoder) readCompressedHeader(b *boolDecoder) {
	h := &d.h
	f := &d.fc
	if h.lossless {
		h.txMode = only4x4
	} else {
		h.txMode = b.literal(2)
		if h.txMode == allow32x32 {
			h.txMode += b.literal(1)
		}
	}
	if h.txMode == txModeSelect {
		for i := range 2 {
			f.tx8[i][0] = diffUpdateProb(b, f.tx8[i][0])
		}
		for i := range 2 {
			for j := range 2 {
				f.tx16[i][j] = diffUpdateProb(b, f.tx16[i][j])
			}
		}
		for i := range 2 {
			for j := range 3 {
				f.tx32[i][j] = diffUpdateProb(b, f.tx32[i][j])
			}
		}
	}
	for t := 0; t <= txModeToBig[h.txMode]; t++ {
		if b.literal(1) == 1 {
			for i := range 2 {
				for j := range 2 {
					for k := range 6 {
						maxL := 6
						if k == 0 {
							maxL = 3
						}
						for l := range maxL {
							for m := range 3 {
								f.coef[t][i][j][k][l][m] = diffUpdateProb(b, f.coef[t][i][j][k][l][m])
							}
						}
					}
				}
			}
		}
	}
	for i := range 3 {
		f.skip[i] = diffUpdateProb(b, f.skip[i])
	}
	if h.frameIsIntra {
		return
	}
	for i := range 7 {
		for j := range 3 {
			f.interMode[i][j] = diffUpdateProb(b, f.interMode[i][j])
		}
	}
	if h.interpFilter == switchable {
		for j := range 4 {
			for i := range 2 {
				f.interpFilter[j][i] = diffUpdateProb(b, f.interpFilter[j][i])
			}
		}
	}
	for i := range 4 {
		f.isInter[i] = diffUpdateProb(b, f.isInter[i])
	}
	// frame_reference_mode
	compAllowed := false
	for i := 1; i < 3; i++ {
		if h.refFrameSignBias[i+1] != h.refFrameSignBias[1] {
			compAllowed = true
		}
	}
	h.referenceMode = singleReference
	if compAllowed {
		if b.literal(1) == 1 {
			if b.literal(1) == 0 {
				h.referenceMode = compoundReference
			} else {
				h.referenceMode = referenceModeSelect
			}
			sb := h.refFrameSignBias
			switch {
			case sb[lastFrame] == sb[goldenFrame]:
				h.compFixedRef, h.compVarRef = altrefFrame, [2]int{lastFrame, goldenFrame}
			case sb[lastFrame] == sb[altrefFrame]:
				h.compFixedRef, h.compVarRef = goldenFrame, [2]int{lastFrame, altrefFrame}
			default:
				h.compFixedRef, h.compVarRef = lastFrame, [2]int{goldenFrame, altrefFrame}
			}
		}
	}
	if h.referenceMode == referenceModeSelect {
		for i := range 5 {
			f.compMode[i] = diffUpdateProb(b, f.compMode[i])
		}
	}
	if h.referenceMode != compoundReference {
		for i := range 5 {
			f.singleRef[i][0] = diffUpdateProb(b, f.singleRef[i][0])
			f.singleRef[i][1] = diffUpdateProb(b, f.singleRef[i][1])
		}
	}
	if h.referenceMode != singleReference {
		for i := range 5 {
			f.compRef[i] = diffUpdateProb(b, f.compRef[i])
		}
	}
	for i := range 4 {
		for j := range 9 {
			f.yMode[i][j] = diffUpdateProb(b, f.yMode[i][j])
		}
	}
	for i := range 16 {
		for j := range 3 {
			f.partition[i][j] = diffUpdateProb(b, f.partition[i][j])
		}
	}
	// mv_probs
	for j := range 3 {
		f.mvJoint[j] = updateMvProb(b, f.mvJoint[j])
	}
	for i := range 2 {
		f.mvSign[i] = updateMvProb(b, f.mvSign[i])
		for j := range 10 {
			f.mvClass[i][j] = updateMvProb(b, f.mvClass[i][j])
		}
		f.mvClass0Bit[i] = updateMvProb(b, f.mvClass0Bit[i])
		for j := range 10 {
			f.mvBits[i][j] = updateMvProb(b, f.mvBits[i][j])
		}
	}
	for i := range 2 {
		for j := range 2 {
			for k := range 3 {
				f.mvClass0Fr[i][j][k] = updateMvProb(b, f.mvClass0Fr[i][j][k])
			}
		}
		for k := range 3 {
			f.mvFr[i][k] = updateMvProb(b, f.mvFr[i][k])
		}
	}
	if h.allowHighPrecisionMv {
		for i := range 2 {
			f.mvClass0Hp[i] = updateMvProb(b, f.mvClass0Hp[i])
			f.mvHp[i] = updateMvProb(b, f.mvHp[i])
		}
	}
}

func diffUpdateProb(b *boolDecoder, p uint8) uint8 {
	if b.read(252) == 0 {
		return p
	}
	delta := decodeTermSubexp(b)
	return invRemapProb(delta, int(p))
}

func decodeTermSubexp(b *boolDecoder) int {
	if b.literal(1) == 0 {
		return b.literal(4)
	}
	if b.literal(1) == 0 {
		return b.literal(4) + 16
	}
	if b.literal(1) == 0 {
		return b.literal(5) + 32
	}
	v := b.literal(7)
	if v < 65 {
		return v + 64
	}
	return (v << 1) - 1 + b.literal(1)
}

func invRemapProb(delta, m int) uint8 {
	v := int(invMapTable[min(delta, 254)])
	m--
	if (m << 1) <= 255 {
		m = 1 + invRecenterNonneg(v, m)
	} else {
		m = 255 - invRecenterNonneg(v, 255-1-m)
	}
	return uint8(m)
}

func invRecenterNonneg(v, m int) int {
	if v > 2*m {
		return v
	}
	if v&1 != 0 {
		return m - ((v + 1) >> 1)
	}
	return m + (v >> 1)
}

func updateMvProb(b *boolDecoder, p uint8) uint8 {
	if b.read(252) == 1 {
		return uint8(b.literal(7)<<1 | 1)
	}
	return p
}
