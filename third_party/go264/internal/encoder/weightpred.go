package encoder

import (
	"math"

	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/frame"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/mc"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/syntax"
)

type WeightedPrediction uint8

const (
	WeightedPredictionOff WeightedPrediction = iota
	WeightedPredictionExplicit
	WeightedPredictionImplicit
)

const (
	weightNone = iota
	weightExplicit
	weightImplicit
)

const (
	lumaLog2Denom    = 6
	chromaLog2Denom  = 6
	weightSampleStep = 2
	weightGainNum    = 15
	weightGainDen    = 16
)

func clip3(lo, hi, v int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clipSample(v int) int { return clip3(0, 255, v) }

type planeSums struct {
	n            int
	src, ref     int64
	srcSq, refSq int64
}

func accumulateSums(s *planeSums, src []byte, srcStride, srcOff int,
	ref []byte, refStride, refOff, w, h int) {

	for y := 0; y < h; y += weightSampleStep {
		so := srcOff + y*srcStride
		ro := refOff + y*refStride
		for x := 0; x < w; x++ {
			a := int64(src[so+x])
			b := int64(ref[ro+x])
			s.src += a
			s.ref += b
			s.srcSq += a * a
			s.refSq += b * b
			s.n++
		}
	}
}

func fitWeight(s planeSums, denom uint) (int32, int32) {
	unit := int32(1) << denom
	if s.n == 0 {
		return unit, 0
	}
	n := float64(s.n)
	meanS := float64(s.src) / n
	meanR := float64(s.ref) / n
	varS := float64(s.srcSq)/n - meanS*meanS
	varR := float64(s.refSq)/n - meanR*meanR
	scale := 1.0
	if varR > 1 && varS > 0 {
		scale = math.Sqrt(varS / varR)
	}
	w := int32(math.Round(scale * float64(unit)))
	if w < 1 {
		w = 1
	}
	if w > 127 {
		w = 127
	}
	o := int32(math.Round(meanS - float64(w)/float64(unit)*meanR))
	if o < -128 {
		o = -128
	}
	if o > 127 {
		o = 127
	}
	return w, o
}

func applyWeightSample(v int, weight, offset int32, denom uint) int {
	x := v * int(weight)
	x = (x + 1<<(denom-1)) >> denom
	return clipSample(x + int(offset))
}

func compareWeighted(src []byte, srcStride, srcOff int, ref []byte, refStride, refOff, w, h int,
	weight, offset int32, denom uint) (plain, shaped int64) {

	for y := 0; y < h; y += weightSampleStep {
		so := srcOff + y*srcStride
		ro := refOff + y*refStride
		for x := 0; x < w; x++ {
			a := int(src[so+x])
			b := int(ref[ro+x])
			d := a - b
			if d < 0 {
				d = -d
			}
			plain += int64(d)
			d = a - applyWeightSample(b, weight, offset, denom)
			if d < 0 {
				d = -d
			}
			shaped += int64(d)
		}
	}
	return plain, shaped
}

func worthSending(plain, shaped int64, weight, offset int32, denom uint) bool {
	if weight == int32(1)<<denom && offset == 0 {
		return false
	}
	if plain <= 0 {
		return false
	}
	return shaped*weightGainDen < plain*weightGainNum
}

func defaultWeightEntry() syntax.WeightEntry {
	return syntax.WeightEntry{
		LumaWeight:   1 << lumaLog2Denom,
		ChromaWeight: [2]int32{1 << chromaLog2Denom, 1 << chromaLog2Denom},
	}
}

func (e *Encoder) estimateWeightEntry(src, ref *frame.Picture, y0, y1 int) (syntax.WeightEntry, weightMeans) {
	entry := defaultWeightEntry()
	var means weightMeans
	w := e.width()
	h := y1 - y0
	if h <= 0 || ref == nil {
		return entry, means
	}

	var luma planeSums
	accumulateSums(&luma, src.Y, src.StrideY, src.LumaOffset(0, y0),
		ref.Y, ref.StrideY, ref.LumaOffset(0, y0), w, h)
	means.ok = true
	if luma.n > 0 {
		means.ref[0] = float64(luma.ref) / float64(luma.n)
	}
	lw, lo := fitWeight(luma, lumaLog2Denom)
	plain, shaped := compareWeighted(src.Y, src.StrideY, src.LumaOffset(0, y0),
		ref.Y, ref.StrideY, ref.LumaOffset(0, y0), w, h, lw, lo, lumaLog2Denom)
	if worthSending(plain, shaped, lw, lo, lumaLog2Denom) {
		entry.LumaWeightFlag = true
		entry.LumaWeight = lw
		entry.LumaOffset = lo
	}

	cw, ch := w/2, h/2
	cy := y0 / 2
	srcPlanes := [2][]byte{src.Cb, src.Cr}
	refPlanes := [2][]byte{ref.Cb, ref.Cr}
	var cWeight, cOffset [2]int32
	var cPlain, cShaped int64
	trivial := true
	for i := 0; i < 2; i++ {
		var sums planeSums
		accumulateSums(&sums, srcPlanes[i], src.StrideC, src.ChromaOffset(0, cy),
			refPlanes[i], ref.StrideC, ref.ChromaOffset(0, cy), cw, ch)
		if sums.n > 0 {
			means.ref[i+1] = float64(sums.ref) / float64(sums.n)
		}
		cWeight[i], cOffset[i] = fitWeight(sums, chromaLog2Denom)
		p, s := compareWeighted(srcPlanes[i], src.StrideC, src.ChromaOffset(0, cy),
			refPlanes[i], ref.StrideC, ref.ChromaOffset(0, cy), cw, ch,
			cWeight[i], cOffset[i], chromaLog2Denom)
		cPlain += p
		cShaped += s
		if cWeight[i] != int32(1)<<chromaLog2Denom || cOffset[i] != 0 {
			trivial = false
		}
	}
	if !trivial && cPlain > 0 && cShaped*weightGainDen < cPlain*weightGainNum {
		entry.ChromaWeightFlag = true
		entry.ChromaWeight = cWeight
		entry.ChromaOffset = cOffset
	}
	return entry, means
}

func (e *Encoder) sliceRows(p sliceJob) (int, int) {
	y0 := p.firstMB / e.widthMBs * 16
	y1 := ((p.endMB-1)/e.widthMBs + 1) * 16
	if y1 > e.height() {
		y1 = e.height()
	}
	if y0 > y1 {
		y0 = y1
	}
	return y0, y1
}

type weightMeans struct {
	ref [3]float64
	ok  bool
}

func (e *Encoder) weightList(list []*frame.Picture, n, y0, y1 int) ([]syntax.WeightEntry, []weightMeans) {
	out := make([]syntax.WeightEntry, n)
	means := make([]weightMeans, n)
	for i := range out {
		if i < len(list) && list[i] != nil {
			out[i], means[i] = e.estimateWeightEntry(e.src, list[i], y0, y1)
			continue
		}
		out[i] = defaultWeightEntry()
	}
	return out, means
}

func biWeightLimit(denom uint32) int32 {
	if denom == 7 {
		return 127
	}
	return 128
}

func componentWeight(entry *syntax.WeightEntry, c int) int32 {
	if c == 0 {
		return entry.LumaWeight
	}
	return entry.ChromaWeight[c-1]
}

func setComponentWeight(entry *syntax.WeightEntry, c int, w, o int32) {
	if c == 0 {
		entry.LumaWeight, entry.LumaOffset = w, o
		return
	}
	entry.ChromaWeight[c-1], entry.ChromaOffset[c-1] = w, o
}

func reoffset(entry *syntax.WeightEntry, c int, w int32, m weightMeans, denom uint32, unit int32) int32 {
	if !m.ok {
		return 0
	}
	meanS := m.ref[c]*float64(componentWeight(entry, c))/float64(unit) + float64(offsetOf(entry, c))
	o := int32(math.Round(meanS - float64(w)/float64(unit)*m.ref[c]))
	return int32(clip3(-128, 127, int(o)))
}

func offsetOf(entry *syntax.WeightEntry, c int) int32 {
	if c == 0 {
		return entry.LumaOffset
	}
	return entry.ChromaOffset[c-1]
}

func enforceBiWeightLimit(t *syntax.PredWeightTable, m0, m1 []weightMeans) {
	for c := 0; c < 3; c++ {
		denom := t.LumaLog2WeightDenom
		if c > 0 {
			denom = t.ChromaLog2WeightDenom
		}
		unit := int32(1) << denom
		limit := biWeightLimit(denom)
		for i := range t.L0 {
			for j := range t.L1 {
				w0 := componentWeight(&t.L0[i], c)
				w1 := componentWeight(&t.L1[j], c)
				sum := w0 + w1
				if sum >= -128 && sum <= limit {
					continue
				}
				target := limit
				if sum < -128 {
					target = -128
				}
				n0, n1 := shareWeights(w0, w1, target)
				var s0, s1 weightMeans
				if i < len(m0) {
					s0 = m0[i]
				}
				if j < len(m1) {
					s1 = m1[j]
				}
				setComponentWeight(&t.L0[i], c, n0, reoffset(&t.L0[i], c, n0, s0, denom, unit))
				setComponentWeight(&t.L1[j], c, n1, reoffset(&t.L1[j], c, n1, s1, denom, unit))
			}
		}
	}
}

func shareWeights(w0, w1, target int32) (int32, int32) {
	sum := w0 + w1
	if sum == 0 {
		return target / 2, target - target/2
	}
	n0 := int32(math.Round(float64(target) * float64(w0) / float64(sum)))
	n0 = int32(clip3(-128, 127, int(n0)))
	n1 := target - n0
	if n1 < -128 || n1 > 127 {
		n1 = int32(clip3(-128, 127, int(n1)))
		n0 = target - n1
		n0 = int32(clip3(-128, 127, int(n0)))
	}
	return n0, n1
}

func (e *Encoder) weightModeFor(t syntax.SliceType) int {
	switch {
	case e.pps.WeightedPred && (t.IsP() || t.IsSP()):
		return weightExplicit
	case t.IsB() && e.pps.WeightedBipredIDC == 1:
		return weightExplicit
	case t.IsB() && e.pps.WeightedBipredIDC == 2:
		return weightImplicit
	}
	return weightNone
}

func (e *Encoder) fillPredWeightTable(hdr *syntax.SliceHeader, p sliceJob) {
	t := &hdr.PredWeight
	t.LumaLog2WeightDenom = lumaLog2Denom
	t.ChromaLog2WeightDenom = chromaLog2Denom
	y0, y1 := e.sliceRows(p)
	var m0, m1 []weightMeans
	t.L0, m0 = e.weightList(e.refL0, p.active, y0, y1)
	if p.sliceType.IsB() {
		t.L1, m1 = e.weightList(e.refL1, p.activeL1, y0, y1)
		if e.pps.WeightedBipredIDC == 1 {
			enforceBiWeightLimit(t, m0, m1)
		}
	}
	if anyWeightSent(t.L0) || anyWeightSent(t.L1) {
		return
	}
	t.LumaLog2WeightDenom = 0
	t.ChromaLog2WeightDenom = 0
	resetToUnitWeights(t.L0)
	resetToUnitWeights(t.L1)
}

func anyWeightSent(list []syntax.WeightEntry) bool {
	for i := range list {
		if list[i].LumaWeightFlag || list[i].ChromaWeightFlag {
			return true
		}
	}
	return false
}

func resetToUnitWeights(list []syntax.WeightEntry) {
	for i := range list {
		list[i] = syntax.WeightEntry{LumaWeight: 1, ChromaWeight: [2]int32{1, 1}}
	}
}

func (s *mbEncoder) weightEntryIn(list int, ref int8) *syntax.WeightEntry {
	if s.weights == nil {
		return nil
	}
	l := s.weights.L0
	if list == 1 {
		l = s.weights.L1
	}
	if int(ref) < 0 || int(ref) >= len(l) {
		return nil
	}
	return &l[ref]
}

func (s *mbEncoder) implicitWeights(ref0, ref1 int8) (int, int) {
	p0 := s.refPictureIn(0, ref0)
	p1 := s.refPictureIn(1, ref1)
	if p0 == nil || p1 == nil || p0.LongTerm || p1.LongTerm {
		return 32, 32
	}
	td := clip3(-128, 127, p1.POC-p0.POC)
	if td == 0 {
		return 32, 32
	}
	tb := clip3(-128, 127, s.e.rec.POC-p0.POC)
	abs := td / 2
	if abs < 0 {
		abs = -abs
	}
	tx := (16384 + abs) / td
	dist := clip3(-1024, 1023, (tb*tx+32)>>6)
	w1 := dist >> 2
	if w1 < -64 || w1 > 128 {
		return 32, 32
	}
	return 64 - w1, w1
}

func (s *mbEncoder) weightUniRegion(list int, ref int8, x, y, w, h int) {
	if s.weightMode != weightExplicit {
		return
	}
	if ref < 0 {
		ref = 0
	}
	e := s.weightEntryIn(list, ref)
	if e == nil {
		return
	}
	rec := s.e.rec
	mc.WeightUni(rec.Y, rec.StrideY, rec.LumaOffset(x, y), w, h,
		int(e.LumaWeight), int(e.LumaOffset), int(s.weights.LumaLog2WeightDenom))
	cx, cy := x/2, y/2
	cw, ch := w/2, h/2
	logWDC := int(s.weights.ChromaLog2WeightDenom)
	mc.WeightUni(rec.Cb, rec.StrideC, rec.ChromaOffset(cx, cy), cw, ch,
		int(e.ChromaWeight[0]), int(e.ChromaOffset[0]), logWDC)
	mc.WeightUni(rec.Cr, rec.StrideC, rec.ChromaOffset(cx, cy), cw, ch,
		int(e.ChromaWeight[1]), int(e.ChromaOffset[1]), logWDC)
}

func (s *mbEncoder) weightLumaScratch(list int, ref int8, buf []byte, stride, off, w, h int) {
	if s.weightMode != weightExplicit {
		return
	}
	if ref < 0 {
		ref = 0
	}
	e := s.weightEntryIn(list, ref)
	if e == nil {
		return
	}
	mc.WeightUni(buf, stride, off, w, h,
		int(e.LumaWeight), int(e.LumaOffset), int(s.weights.LumaLog2WeightDenom))
}

func (s *mbEncoder) combineLumaBi(dst []byte, dstStride, dstOff int,
	a []byte, aStride, aOff int, b []byte, bStride, bOff int, w, h int, ref [2]int8) {

	switch s.weightMode {
	case weightExplicit:
		e0 := s.weightEntryIn(0, ref[0])
		e1 := s.weightEntryIn(1, ref[1])
		if e0 != nil && e1 != nil {
			mc.WeightBi(dst, dstStride, dstOff, a, aStride, aOff, b, bStride, bOff, w, h,
				int(e0.LumaWeight), int(e1.LumaWeight),
				int(e0.LumaOffset), int(e1.LumaOffset), int(s.weights.LumaLog2WeightDenom))
			return
		}
	case weightImplicit:
		w0, w1 := s.implicitWeights(ref[0], ref[1])
		mc.WeightBi(dst, dstStride, dstOff, a, aStride, aOff, b, bStride, bOff, w, h,
			w0, w1, 0, 0, 5)
		return
	}
	mc.Average(dst, dstStride, dstOff, a, aStride, aOff, b, bStride, bOff, w, h)
}

func (s *mbEncoder) combineBi(seg motionSegment, x, y int) {
	rec := s.e.rec
	cx, cy := x/2, y/2
	cw, ch := seg.w/2, seg.h/2
	a, b := &s.predA, &s.predB
	lumaOff := rec.LumaOffset(x, y)
	chromaOff := rec.ChromaOffset(cx, cy)

	switch s.weightMode {
	case weightExplicit:
		e0 := s.weightEntryIn(0, seg.ref[0])
		e1 := s.weightEntryIn(1, seg.ref[1])
		if e0 == nil || e1 == nil {
			break
		}
		mc.WeightBi(rec.Y, rec.StrideY, lumaOff, a.y[:], 16, 0, b.y[:], 16, 0,
			seg.w, seg.h, int(e0.LumaWeight), int(e1.LumaWeight),
			int(e0.LumaOffset), int(e1.LumaOffset), int(s.weights.LumaLog2WeightDenom))
		logWDC := int(s.weights.ChromaLog2WeightDenom)
		mc.WeightBi(rec.Cb, rec.StrideC, chromaOff, a.cb[:], 8, 0, b.cb[:], 8, 0,
			cw, ch, int(e0.ChromaWeight[0]), int(e1.ChromaWeight[0]),
			int(e0.ChromaOffset[0]), int(e1.ChromaOffset[0]), logWDC)
		mc.WeightBi(rec.Cr, rec.StrideC, chromaOff, a.cr[:], 8, 0, b.cr[:], 8, 0,
			cw, ch, int(e0.ChromaWeight[1]), int(e1.ChromaWeight[1]),
			int(e0.ChromaOffset[1]), int(e1.ChromaOffset[1]), logWDC)
		return

	case weightImplicit:
		w0, w1 := s.implicitWeights(seg.ref[0], seg.ref[1])
		mc.WeightBi(rec.Y, rec.StrideY, lumaOff, a.y[:], 16, 0, b.y[:], 16, 0,
			seg.w, seg.h, w0, w1, 0, 0, 5)
		mc.WeightBi(rec.Cb, rec.StrideC, chromaOff, a.cb[:], 8, 0, b.cb[:], 8, 0,
			cw, ch, w0, w1, 0, 0, 5)
		mc.WeightBi(rec.Cr, rec.StrideC, chromaOff, a.cr[:], 8, 0, b.cr[:], 8, 0,
			cw, ch, w0, w1, 0, 0, 5)
		return
	}

	mc.Average(rec.Y, rec.StrideY, lumaOff, a.y[:], 16, 0, b.y[:], 16, 0, seg.w, seg.h)
	mc.Average(rec.Cb, rec.StrideC, chromaOff, a.cb[:], 8, 0, b.cb[:], 8, 0, cw, ch)
	mc.Average(rec.Cr, rec.StrideC, chromaOff, a.cr[:], 8, 0, b.cr[:], 8, 0, cw, ch)
}
