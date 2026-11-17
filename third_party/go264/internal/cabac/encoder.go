package cabac

import (
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/bits"
)

type Encoder struct {
	w            *bits.Writer
	low          uint32
	rng          uint32
	firstBitFlag bool
	bitsOutstand int
	ctx          [NumContexts]context

	estBuf   *bits.Writer
	estTrial *Encoder
}

type State struct {
	ctx          [NumContexts]context
	low          uint32
	rng          uint32
	firstBitFlag bool
	bitsOutstand int
}

func (e *Encoder) Snapshot() State {
	return State{
		ctx:          e.ctx,
		low:          e.low,
		rng:          e.rng,
		firstBitFlag: e.firstBitFlag,
		bitsOutstand: e.bitsOutstand,
	}
}

func (e *Encoder) Restore(s State) {
	e.ctx = s.ctx
	e.low = s.low
	e.rng = s.rng
	e.firstBitFlag = s.firstBitFlag
	e.bitsOutstand = s.bitsOutstand
}

func (e *Encoder) State(ctxIdx int) (state, mps uint8) {
	return e.ctx[ctxIdx].state, e.ctx[ctxIdx].mps
}

func (e *Encoder) Init(w *bits.Writer, sliceQPY int, intra bool, initIDC uint32) error {
	for !w.ByteAligned() {
		w.WriteBit(1)
	}
	var d Decoder
	if intra {
		d.initContexts(&contextInitI, sliceQPY)
	} else {
		if initIDC > 2 {
			return ErrInitIDC
		}
		d.initContexts(contextInitPB[initIDC], sliceQPY)
	}
	e.w = w
	e.ctx = d.ctx
	e.low = 0
	e.rng = 510
	e.firstBitFlag = true
	e.bitsOutstand = 0
	return nil
}

func (e *Encoder) putBit(b uint32) {
	if e.firstBitFlag {
		e.firstBitFlag = false
	} else {
		e.w.WriteBit(b)
	}
	for e.bitsOutstand > 0 {
		e.w.WriteBit(1 - b)
		e.bitsOutstand--
	}
}

func (e *Encoder) renorm() {
	for e.rng < 256 {
		switch {
		case e.low < 256:
			e.putBit(0)
		case e.low >= 512:
			e.low -= 512
			e.putBit(1)
		default:
			e.low -= 256
			e.bitsOutstand++
		}
		e.rng <<= 1
		e.low <<= 1
	}
}

func (e *Encoder) EncodeDecision(ctxIdx int, bin uint32) {
	c := &e.ctx[ctxIdx]
	q := e.rng >> 6 & 3
	lps := uint32(rangeTabLPS[c.state][q])
	e.rng -= lps
	if bin != uint32(c.mps) {
		e.low += e.rng
		e.rng = lps
		if c.state == 0 {
			c.mps = 1 - c.mps
		}
		c.state = transIdxLPS[c.state]
	} else {
		c.state = transIdxMPS[c.state]
	}
	e.renorm()
}

func (e *Encoder) EncodeBypass(bin uint32) {
	e.low <<= 1
	if bin != 0 {
		e.low += e.rng
	}
	if e.low >= 1024 {
		e.putBit(1)
		e.low -= 1024
	} else if e.low < 512 {
		e.putBit(0)
	} else {
		e.low -= 512
		e.bitsOutstand++
	}
}

func (e *Encoder) EncodeTerminate(bin uint32) {
	e.rng -= 2
	if bin != 0 {
		e.low += e.rng
		return
	}
	e.renorm()
}

func (e *Encoder) Finish() {
	e.rng = 2
	e.renorm()
	e.putBit(e.low >> 9 & 1)
	e.w.WriteBits(e.low>>7&3|1, 2)
	e.w.AlignZero()
}

func (e *Encoder) EstimateBits(fn func(*Encoder)) int {
	if e.estBuf == nil {
		e.estBuf = bits.NewWriterSize(64)
	} else {
		e.estBuf.Reset()
	}
	if e.estTrial == nil {
		e.estTrial = &Encoder{}
	}
	trial := e.estTrial
	trial.w = e.estBuf
	trial.Restore(e.Snapshot())
	beforeOutstand := trial.bitsOutstand
	fn(trial)
	return e.estBuf.BitsWritten() + (trial.bitsOutstand - beforeOutstand)
}

func boolToUint32(b bool) uint32 {
	if b {
		return 1
	}
	return 0
}

func (e *Encoder) intraMBTypeSuffix(base, spread, mbType int) {
	if mbType == 25 {
		e.EncodeTerminate(1)
		return
	}
	e.EncodeTerminate(0)
	rel := mbType - 1
	bit1 := 0
	if rel >= 12 {
		bit1 = 1
		rel -= 12
	}
	e.EncodeDecision(base+1, uint32(bit1))
	bit2 := 0
	if rel >= 4 {
		bit2 = 1
	}
	e.EncodeDecision(base+2, uint32(bit2))
	if bit2 == 1 {
		bit2b := 0
		if rel >= 8 {
			bit2b = 1
			rel -= 8
		} else {
			rel -= 4
		}
		e.EncodeDecision(base+2+spread, uint32(bit2b))
	}
	e.EncodeDecision(base+3+spread, uint32(rel>>1))
	e.EncodeDecision(base+3+2*spread, uint32(rel&1))
}

func (e *Encoder) IntraMBType(inc, mbType int) {
	if mbType == 0 {
		e.EncodeDecision(offMBTypeI+inc, 0)
		return
	}
	e.EncodeDecision(offMBTypeI+inc, 1)
	e.intraMBTypeSuffix(offMBTypeI+2, 1, mbType)
}

func (e *Encoder) MBSkipFlagP(inc int, skip bool) {
	e.EncodeDecision(offMBSkipP+inc, boolToUint32(skip))
}

func (e *Encoder) MBSkipFlagB(inc int, skip bool) {
	e.EncodeDecision(offMBSkipB+inc, boolToUint32(skip))
}

func (e *Encoder) intraMBTypeIn(base, mbType int) {
	if mbType == 0 {
		e.EncodeDecision(base, 0)
		return
	}
	e.EncodeDecision(base, 1)
	e.intraMBTypeSuffix(base, 0, mbType)
}

func (e *Encoder) MBTypeB(inc, mbType int, intra bool) {
	if !intra && mbType == 0 {
		e.EncodeDecision(offMBTypeB+inc, 0)
		return
	}
	e.EncodeDecision(offMBTypeB+inc, 1)
	if !intra && (mbType == 1 || mbType == 2) {
		e.EncodeDecision(offMBTypeB+3, 0)
		e.EncodeDecision(offMBTypeB+5, uint32(mbType-1))
		return
	}
	e.EncodeDecision(offMBTypeB+3, 1)
	bitsVal := 0
	hasExtra := false
	extra := 0
	switch {
	case intra:
		bitsVal = 13
	case mbType <= 10:
		bitsVal = mbType - 3
	case mbType == 11:
		bitsVal = 14
	case mbType == 22:
		bitsVal = 15
	default:
		bitsVal = 8 + (mbType-12)/2
		extra = (mbType - 12) & 1
		hasExtra = true
	}
	e.EncodeDecision(offMBTypeB+4, uint32(bitsVal>>3&1))
	e.EncodeDecision(offMBTypeB+5, uint32(bitsVal>>2&1))
	e.EncodeDecision(offMBTypeB+5, uint32(bitsVal>>1&1))
	e.EncodeDecision(offMBTypeB+5, uint32(bitsVal&1))
	if hasExtra {
		e.EncodeDecision(offMBTypeB+5, uint32(extra))
	}
	if intra {
		e.intraMBTypeIn(offMBTypeIinB, mbType)
	}
}

func (e *Encoder) SubMBTypeB(t int) {
	b := offSubMBTypeB
	switch {
	case t == 0:
		e.EncodeDecision(b, 0)
	case t == 1:
		e.EncodeDecision(b, 1)
		e.EncodeDecision(b+1, 0)
		e.EncodeDecision(b+3, 0)
	case t == 2:
		e.EncodeDecision(b, 1)
		e.EncodeDecision(b+1, 0)
		e.EncodeDecision(b+3, 1)
	case t >= 3 && t <= 6:
		e.EncodeDecision(b, 1)
		e.EncodeDecision(b+1, 1)
		e.EncodeDecision(b+2, 0)
		rel := t - 3
		e.EncodeDecision(b+3, uint32(rel>>1))
		e.EncodeDecision(b+3, uint32(rel&1))
	case t >= 7 && t <= 10:
		e.EncodeDecision(b, 1)
		e.EncodeDecision(b+1, 1)
		e.EncodeDecision(b+2, 1)
		e.EncodeDecision(b+3, 0)
		rel := t - 7
		e.EncodeDecision(b+3, uint32(rel>>1))
		e.EncodeDecision(b+3, uint32(rel&1))
	case t == 11 || t == 12:
		e.EncodeDecision(b, 1)
		e.EncodeDecision(b+1, 1)
		e.EncodeDecision(b+2, 1)
		e.EncodeDecision(b+3, 1)
		e.EncodeDecision(b+3, uint32(t-11))
	}
}

func (e *Encoder) MBTypeP(mbType int, intra bool) {
	if intra {
		e.EncodeDecision(offMBTypeP, 1)
		e.intraMBTypeIn(offMBTypeIinP, mbType)
		return
	}
	e.EncodeDecision(offMBTypeP, 0)
	if mbType == 0 || mbType == 3 {
		e.EncodeDecision(offMBTypeP+1, 0)
		e.EncodeDecision(offMBTypeP+2, boolToUint32(mbType == 3))
		return
	}
	e.EncodeDecision(offMBTypeP+1, 1)
	e.EncodeDecision(offMBTypeP+3, boolToUint32(mbType == 1))
}

func (e *Encoder) SubMBTypeP(t int) {
	switch t {
	case 0:
		e.EncodeDecision(offSubMBTypeP, 1)
	case 1:
		e.EncodeDecision(offSubMBTypeP, 0)
		e.EncodeDecision(offSubMBTypeP+1, 0)
	case 2:
		e.EncodeDecision(offSubMBTypeP, 0)
		e.EncodeDecision(offSubMBTypeP+1, 1)
		e.EncodeDecision(offSubMBTypeP+2, 1)
	case 3:
		e.EncodeDecision(offSubMBTypeP, 0)
		e.EncodeDecision(offSubMBTypeP+1, 1)
		e.EncodeDecision(offSubMBTypeP+2, 0)
	}
}

func (e *Encoder) RefIdx(inc, ref int) {
	ctx := inc
	for i := 0; i < ref; i++ {
		e.EncodeDecision(offRefIdx+ctx, 1)
		ctx = ctx>>2 + 4
	}
	e.EncodeDecision(offRefIdx+ctx, 0)
}

func (e *Encoder) MVD(base, absSum, v int) {
	inc := 0
	if absSum > 2 {
		inc++
	}
	if absSum > 32 {
		inc++
	}
	mag := v
	if mag < 0 {
		mag = -mag
	}
	if mag == 0 {
		e.EncodeDecision(base+inc, 0)
		return
	}
	e.EncodeDecision(base+inc, 1)
	ctxAt := func(i int) int {
		if i > 4 {
			i = 4
		}
		return base + 2 + i
	}
	prefix := mag
	if prefix > 9 {
		prefix = 9
	}
	for i := 1; i < prefix; i++ {
		e.EncodeDecision(ctxAt(i), 1)
	}
	if mag < 9 {
		e.EncodeDecision(ctxAt(prefix), 0)
	} else {
		rest := mag - 9
		k := 3
		for rest >= 1<<uint(k) {
			e.EncodeBypass(1)
			rest -= 1 << uint(k)
			k++
			if k > 24 {
				break
			}
		}
		e.EncodeBypass(0)
		for k--; k >= 0; k-- {
			e.EncodeBypass(uint32(rest>>uint(k)) & 1)
		}
	}
	e.EncodeBypass(boolToUint32(v < 0))
}

func (e *Encoder) IntraChromaPredMode(inc, mode int) {
	if mode == 0 {
		e.EncodeDecision(offIntraChromaPred+inc, 0)
		return
	}
	e.EncodeDecision(offIntraChromaPred+inc, 1)
	if mode == 1 {
		e.EncodeDecision(offIntraChromaPred+3, 0)
		return
	}
	e.EncodeDecision(offIntraChromaPred+3, 1)
	if mode == 2 {
		e.EncodeDecision(offIntraChromaPred+3, 0)
		return
	}
	e.EncodeDecision(offIntraChromaPred+3, 1)
}

func (e *Encoder) Intra4x4PredMode(predMode, mode int) {
	if mode == predMode {
		e.EncodeDecision(offPrevIntraPred, 1)
		return
	}
	e.EncodeDecision(offPrevIntraPred, 0)
	rem := mode
	if mode > predMode {
		rem = mode - 1
	}
	e.EncodeDecision(offRemIntraPred, uint32(rem&1))
	e.EncodeDecision(offRemIntraPred, uint32(rem>>1&1))
	e.EncodeDecision(offRemIntraPred, uint32(rem>>2&1))
}

func (e *Encoder) CodedBlockPatternLuma(leftCBP, topCBP, cbp int) {
	ctx := boolToInt(leftCBP&0x02 == 0) + 2*boolToInt(topCBP&0x04 == 0)
	e.EncodeDecision(offCodedBlockPatLum+ctx, uint32(cbp&1))
	ctx = boolToInt(cbp&0x01 == 0) + 2*boolToInt(topCBP&0x08 == 0)
	e.EncodeDecision(offCodedBlockPatLum+ctx, uint32(cbp>>1&1))
	ctx = boolToInt(leftCBP&0x08 == 0) + 2*boolToInt(cbp&0x01 == 0)
	e.EncodeDecision(offCodedBlockPatLum+ctx, uint32(cbp>>2&1))
	ctx = boolToInt(cbp&0x04 == 0) + 2*boolToInt(cbp&0x02 == 0)
	e.EncodeDecision(offCodedBlockPatLum+ctx, uint32(cbp>>3&1))
}

func (e *Encoder) CodedBlockPatternChroma(leftCBP, topCBP, v int) {
	a := leftCBP >> 4 & 3
	b := topCBP >> 4 & 3
	ctx := 0
	if a > 0 {
		ctx++
	}
	if b > 0 {
		ctx += 2
	}
	e.EncodeDecision(offCodedBlockPatChr+ctx, boolToUint32(v > 0))
	if v == 0 {
		return
	}
	ctx = 4
	if a == 2 {
		ctx++
	}
	if b == 2 {
		ctx += 2
	}
	e.EncodeDecision(offCodedBlockPatChr+ctx, uint32(v-1))
}

func (e *Encoder) MBQPDelta(delta int, prevNonZero bool) {
	mapped := 0
	switch {
	case delta > 0:
		mapped = 2*delta - 1
	case delta < 0:
		mapped = -2 * delta
	}
	e.EncodeDecision(offMBQPDelta+boolToInt(prevNonZero), boolToUint32(mapped > 0))
	if mapped == 0 {
		return
	}
	ctx := 2
	for i := 1; i < mapped; i++ {
		e.EncodeDecision(offMBQPDelta+ctx, 1)
		ctx = 3
	}
	e.EncodeDecision(offMBQPDelta+ctx, 0)
}

func (e *Encoder) EndOfSlice(end bool) { e.EncodeTerminate(boolToUint32(end)) }

func (e *Encoder) CodedBlockFlag(cat, condTermA, condTermB int, flag bool) {
	inc := condTermA + 2*condTermB
	e.EncodeDecision(offCodedBlockFlag+catOffsetCodedBlockFlag[cat]+inc, boolToUint32(flag))
}

func (e *Encoder) absLevelInc(cat, binIdx, numGt1, numEq1 int) int {
	base := offAbsLevel + catOffsetAbsLevel[cat]
	if binIdx == 0 {
		if numGt1 != 0 {
			return base
		}
		v := 1 + numEq1
		if v > 4 {
			v = 4
		}
		return base + v
	}
	limit := 4
	if cat == CatChromaDC {
		limit = 3
	}
	v := numGt1
	if v > limit {
		v = limit
	}
	return base + 5 + v
}

func (e *Encoder) absLevelMinus1(cat, numGt1, numEq1, v int) {
	prefix := v
	if prefix > 14 {
		prefix = 14
	}
	for i := 0; i < prefix; i++ {
		e.EncodeDecision(e.absLevelInc(cat, i, numGt1, numEq1), 1)
	}
	if prefix < 14 {
		e.EncodeDecision(e.absLevelInc(cat, prefix, numGt1, numEq1), 0)
		return
	}
	rest := v - 14
	k := 0
	for rest >= 1<<uint(k) {
		e.EncodeBypass(1)
		rest -= 1 << uint(k)
		k++
		if k > 24 {
			break
		}
	}
	e.EncodeBypass(0)
	for k--; k >= 0; k-- {
		e.EncodeBypass(uint32(rest>>uint(k)) & 1)
	}
}

func (e *Encoder) ResidualBlock(coeffs []int32, cat, condTermA, condTermB, numC8x8 int) {
	maxNumCoeff := len(coeffs)
	last := -1
	for i := maxNumCoeff - 1; i >= 0; i-- {
		if coeffs[i] != 0 {
			last = i
			break
		}
	}
	if last == -1 {
		e.CodedBlockFlag(cat, condTermA, condTermB, false)
		return
	}
	e.CodedBlockFlag(cat, condTermA, condTermB, true)
	numCoeff := last + 1
	for i := 0; i < numCoeff-1; i++ {
		sigInc := significanceInc(cat, i, numC8x8)
		significant := coeffs[i] != 0
		e.EncodeDecision(offSignificant+catOffsetSignificant[cat]+sigInc, boolToUint32(significant))
		if significant {
			e.EncodeDecision(offLastSignificant+catOffsetSignificant[cat]+sigInc, 0)
		}
	}
	if numCoeff < maxNumCoeff {
		sigInc := significanceInc(cat, numCoeff-1, numC8x8)
		e.EncodeDecision(offSignificant+catOffsetSignificant[cat]+sigInc, 1)
		e.EncodeDecision(offLastSignificant+catOffsetSignificant[cat]+sigInc, 1)
	}
	numGt1, numEq1 := 0, 0
	for i := numCoeff - 1; i >= 0; i-- {
		if coeffs[i] == 0 {
			continue
		}
		abs := coeffs[i]
		if abs < 0 {
			abs = -abs
		}
		e.absLevelMinus1(cat, numGt1, numEq1, int(abs)-1)
		if abs > 1 {
			numGt1++
		} else {
			numEq1++
		}
		e.EncodeBypass(boolToUint32(coeffs[i] < 0))
	}
}

func (e *Encoder) EstimateResidualBlockBits(coeffs []int32, cat, condTermA, condTermB, numC8x8 int) int {
	return e.EstimateBits(func(t *Encoder) {
		t.ResidualBlock(coeffs, cat, condTermA, condTermB, numC8x8)
	})
}
