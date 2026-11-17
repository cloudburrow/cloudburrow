package encoder

import (
	"math"

	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/bits"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/cabac"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/cavlc"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/transform"
)

const (
	trellisPasses      = 2
	trellisRateRefused = 1 << 20
)

var (
	invGain4x4 [16]float64
	invGain8x8 [64]float64

	trellisScale4x4 = math.Ldexp(1, -38)
	trellisScale8x8 = math.Ldexp(1, -44)
	trellisScaleDC  = math.Ldexp(1, -44)
	trellisScaleCDC = math.Ldexp(1, -42)
)

func init() {
	const k4 = 1 << 12
	for p := 0; p < 16; p++ {
		var up, down transform.Block
		up[p] = k4
		down[p] = -k4
		transform.Inverse4x4(&up)
		transform.Inverse4x4(&down)
		sum := 0.0
		for i := range up {
			d := float64(up[i]-down[i]) / 2
			sum += d * d
		}
		invGain4x4[p] = sum / (float64(k4) * float64(k4))
	}
	const k8 = 1 << 16
	for p := 0; p < 64; p++ {
		var up, down transform.Block8x8
		up[p] = k8
		down[p] = -k8
		transform.Inverse8x8(&up)
		transform.Inverse8x8(&down)
		sum := 0.0
		for i := range up {
			d := float64(up[i]-down[i]) / 2
			sum += d * d
		}
		invGain8x8[p] = sum / (float64(k8) * float64(k8))
	}
}

type trellisBlock struct {
	n      int
	qbits  uint
	lambda float64
	num    [64]int64
	weight [64]float64
	hi     [64]int32
}

func (t *trellisBlock) prepare(n int, qbits uint, lambda float64) {
	t.n = n
	t.qbits = qbits
	t.lambda = lambda
}

func (t *trellisBlock) set(i int, coef int64, mf, ls int32, gain, scale float64) {
	num := coef * int64(mf)
	t.num[i] = num
	t.weight[i] = float64(ls) * float64(ls) * gain * scale
	half := int64(1) << (t.qbits - 1)
	if num >= 0 {
		t.hi[i] = int32((num + half) >> t.qbits)
	} else {
		t.hi[i] = -int32((-num + half) >> t.qbits)
	}
}

func (t *trellisBlock) distortion(i int, level int32) float64 {
	d := float64(t.num[i] - int64(level)<<t.qbits)
	return d * d * t.weight[i]
}

func abs32(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}

func (t *trellisBlock) refine(levels []int32, rate func() int) {
	live := false
	dist := 0.0
	for i := 0; i < t.n; i++ {
		dist += t.distortion(i, levels[i])
		if t.hi[i] != 0 {
			live = true
		}
	}
	if !live {
		return
	}
	best := dist + t.lambda*float64(rate())
	for pass := 0; pass < trellisPasses; pass++ {
		changed := false
		for i := t.n - 1; i >= 0; i-- {
			limit := abs32(t.hi[i])
			cur := levels[i]
			mag := abs32(cur)
			if limit == 0 && mag == 0 {
				continue
			}
			sign := int32(1)
			if t.hi[i] < 0 || cur < 0 {
				sign = -1
			}
			base := dist - t.distortion(i, cur)
			bestLevel, bestCost, bestDist := cur, best, dist
			for _, m := range [2]int32{mag - 1, mag + 1} {
				if m < 0 || m > limit {
					continue
				}
				cand := sign * m
				if cand == cur {
					continue
				}
				candDist := base + t.distortion(i, cand)
				if candDist >= bestCost {
					continue
				}
				levels[i] = cand
				r := rate()
				if r >= trellisRateRefused {
					continue
				}
				cost := candDist + t.lambda*float64(r)
				if cost < bestCost {
					bestLevel, bestCost, bestDist = cand, cost, candDist
				}
			}
			levels[i] = bestLevel
			if bestLevel != cur {
				best, dist, changed = bestCost, bestDist, true
			}
		}
		if !changed {
			break
		}
	}
}

func (s *mbEncoder) trellisLambda() float64 {
	return lambdaRDTable[s.qpY]
}

func (s *mbEncoder) cavlcRate(levels []int32, nC int) int {
	if s.tw == nil {
		s.tw = bits.NewWriterSize(128)
	}
	s.tw.Reset()
	if err := cavlc.WriteBlock(s.tw, levels, nC); err != nil {
		return trellisRateRefused
	}
	if s.tw.Err() != nil {
		return trellisRateRefused
	}
	return s.tw.BitsWritten()
}

func (s *mbEncoder) blockRate(levels []int32, cat, condA, condB, numC8x8, nC int) func() int {
	if s.cb != nil {
		return func() int {
			return s.cb.EstimateBits(func(t *cabac.Encoder) {
				t.ResidualBlock(levels, cat, condA, condB, numC8x8)
			})
		}
	}
	return func() int { return s.cavlcRate(levels, nC) }
}

func (s *mbEncoder) refine4x4(levels []int32, coef *transform.Block, start, qp int, qs, ls *scale4x4, rate func() int) {
	qbits := uint(15 + qp/6)
	mf := &qs[qp%6]
	scale := &ls[qp%6]
	n := 16 - start
	s.tb.prepare(n, qbits, s.trellisLambda())
	for i := 0; i < n; i++ {
		p := transform.ZigZagIndex(i + start)
		s.tb.set(i, int64(coef[p]), mf[p], scale[p], invGain4x4[p], trellisScale4x4)
	}
	s.tb.refine(levels, rate)
}

func (s *mbEncoder) refine8x8(levels []int32, coef *transform.Block8x8, qp int,
	qs *transform.QuantScale8x8, ls *transform.LevelScale8x8, rate func() int) {
	qbits := uint(16 + qp/6)
	mf := &qs[qp%6]
	scale := &ls[qp%6]
	s.tb.prepare(64, qbits, s.trellisLambda())
	for i := 0; i < 64; i++ {
		p := int(transform.ZigZagScan8x8[i])
		s.tb.set(i, int64(coef[p]), mf[p], scale[p], invGain8x8[p], trellisScale8x8)
	}
	s.tb.refine(levels, rate)
}

func (s *mbEncoder) refineDC(levels []int32, coef []int32, order []int, qp int,
	qs, ls *scale4x4, gain, scale float64, rate func() int) {
	qbits := uint(16 + qp/6)
	mf := qs[qp%6][0]
	lv := ls[qp%6][0]
	s.tb.prepare(len(order), qbits, s.trellisLambda())
	for i, p := range order {
		s.tb.set(i, int64(coef[p]), mf, lv, gain, scale)
	}
	s.tb.refine(levels, rate)
}

var (
	lumaDCOrder   [16]int
	chromaDCOrder = [4]int{0, 1, 2, 3}
)

func init() {
	for i := 0; i < 16; i++ {
		lumaDCOrder[i] = transform.ZigZagIndex(i)
	}
}

func (s *mbEncoder) trellisLuma4x4(blk int, coef *transform.Block, start int, intra bool, cat int) {
	levels := s.lumaScan[blk][start:]
	var condA, condB int
	if s.cb != nil {
		condA, condB = s.lumaCBFContext(blk)
	}
	rate := s.blockRate(levels, cat, condA, condB, 1, s.lumaNC(blk))
	s.refine4x4(levels, coef, start, s.qpY, s.e.lumaQuant4x4(intra), s.e.lumaLevel4x4(intra), rate)
}

func (s *mbEncoder) trellisLumaDC(coef *transform.Block) {
	levels := s.lumaDCScan[:]
	var condA, condB int
	if s.cb != nil {
		condA, condB = s.lumaDCCBFContext()
	}
	rate := s.blockRate(levels, cabac.CatIntra16x16DC, condA, condB, 1, s.lumaNC(0))
	s.refineDC(levels, coef[:], lumaDCOrder[:], s.qpY,
		s.e.lumaQuant4x4(true), s.e.lumaLevel4x4(true),
		16*invGain4x4[0], trellisScaleDC, rate)
}

func (s *mbEncoder) trellisChromaDC(plane, qpc int, coef *transform.ChromaDC, intra bool) {
	levels := s.chromaDC[plane][:]
	var condA, condB int
	if s.cb != nil {
		condA, condB = s.chromaDCCBFContext(plane)
	}
	rate := s.blockRate(levels, cabac.CatChromaDC, condA, condB, 1, -1)
	s.refineDC(levels, coef[:], chromaDCOrder[:], qpc,
		s.e.chromaQuant4x4(intra, plane), s.e.chromaLevel4x4(intra, plane),
		4*invGain4x4[0], trellisScaleCDC, rate)
}

func (s *mbEncoder) trellisChromaAC(plane, blk, qpc int, coef *transform.Block, intra bool) {
	levels := s.chromaScan[plane][blk][1:]
	var condA, condB int
	if s.cb != nil {
		condA, condB = s.chromaACCBFContext(plane, blk)
	}
	rate := s.blockRate(levels, cabac.CatChromaAC, condA, condB, 1, s.chromaNC(plane, blk))
	s.refine4x4(levels, coef, 1, qpc, s.e.chromaQuant4x4(intra, plane), s.e.chromaLevel4x4(intra, plane), rate)
}

func (s *mbEncoder) trellisLuma8x8(i8 int, coef *transform.Block8x8, intra bool) {
	levels := s.luma8x8Scan[i8][:]
	var rate func() int
	if s.cb != nil {
		block := &s.luma8x8Scan[i8]
		rate = func() int {
			return s.cb.EstimateBits(func(t *cabac.Encoder) { t.ResidualBlock8x8(block) })
		}
	} else {
		var nC [4]int
		for i4 := 0; i4 < 4; i4++ {
			nC[i4] = s.lumaNC(i8*4 + i4)
		}
		rate = func() int {
			total := 0
			for i4 := 0; i4 < 4; i4++ {
				sub := s.sub4x4Of8x8(i8, i4)
				r := s.cavlcRate(sub[:], nC[i4])
				if r >= trellisRateRefused {
					return trellisRateRefused
				}
				total += r
			}
			return total
		}
	}
	s.refine8x8(levels, coef, s.qpY, s.e.quantScale8x8(intra), s.e.levelScale8x8(intra), rate)
}
