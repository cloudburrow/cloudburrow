package encoder

import (
	"math"

	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/bits"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/cabac"
)

var lambdaRDTable [52]float64

func init() {
	for qp := 0; qp < 52; qp++ {
		lambdaRDTable[qp] = 0.85 * math.Pow(2, float64(qp-12)/3)
	}
}

func (s *mbEncoder) mbSSD() int {
	total := 0
	for y := 0; y < 16; y++ {
		src := s.e.src.Y[s.e.src.LumaOffset(s.mbx*16, s.mby*16+y):]
		rec := s.e.rec.Y[s.e.rec.LumaOffset(s.mbx*16, s.mby*16+y):]
		for x := 0; x < 16; x++ {
			d := int(src[x]) - int(rec[x])
			total += d * d
		}
	}
	for y := 0; y < 8; y++ {
		so := s.e.src.ChromaOffset(s.mbx*8, s.mby*8+y)
		ro := s.e.rec.ChromaOffset(s.mbx*8, s.mby*8+y)
		for x := 0; x < 8; x++ {
			d := int(s.e.src.Cb[so+x]) - int(s.e.rec.Cb[ro+x])
			total += d * d
			d = int(s.e.src.Cr[so+x]) - int(s.e.rec.Cr[ro+x])
			total += d * d
		}
	}
	return total
}

func (s *mbEncoder) trialBits(write func() error) (int, error) {
	saved := s.w
	savedPrevQP := s.prevQP
	savedQP := s.qpY
	savedMBQP := s.cur.QPY
	if s.trialW == nil {
		s.trialW = bits.NewWriterSize(512)
	} else {
		s.trialW.Reset()
	}
	scratch := s.trialW
	s.w = scratch
	err := write()
	s.w = saved
	s.prevQP = savedPrevQP
	s.qpY = savedQP
	s.cur.QPY = savedMBQP
	if err != nil {
		return 0, err
	}
	if e := scratch.Err(); e != nil {
		return 0, e
	}
	return scratch.BitsWritten(), nil
}

func (s *mbEncoder) trialBitsCABAC(write func()) int {
	savedPrevQP := s.prevQP
	savedQP := s.qpY
	savedMBQP := s.cur.QPY
	savedDelta := s.prevQPDeltaNonZero
	savedLumaDC := s.cur.cbfLumaDC
	savedChromaDC := s.cur.cbfChromaDC
	real := s.cb
	n := real.EstimateBits(func(trial *cabac.Encoder) {
		s.cb = trial
		write()
	})
	s.cb = real
	s.prevQP = savedPrevQP
	s.qpY = savedQP
	s.cur.QPY = savedMBQP
	s.prevQPDeltaNonZero = savedDelta
	s.cur.cbfLumaDC = savedLumaDC
	s.cur.cbfChromaDC = savedChromaDC
	return n
}

func rdCost(ssd, bits int, lambda float64) float64 {
	return float64(ssd) + lambda*float64(bits)
}

type interCandidate struct {
	kind  int
	t8    bool
	parts []partResult
	subs  []subResult
}

func (s *mbEncoder) applyInterCandidate(c interCandidate) {
	s.reset()
	s.cur.kind = c.kind
	s.cur.Transform8x8 = c.t8
	s.cur.Intra = false
	s.cur.skipped = false
	s.parts = c.parts
	s.subs = c.subs
	if c.kind == mbTypeP8x8 {
		s.applySubMotion(c.subs)
		s.compensateSubMBs(c.subs)
	} else {
		s.clearMotion()
		for i, p := range partitionsFor(c.kind) {
			s.storePartitionMotion(p, c.parts[i].mv, c.parts[i].ref)
			s.storeMVD(p.x, p.y, p.w, p.h, c.parts[i].mvd)
		}
		s.compensatePartitions(c.kind, c.parts)
	}
	s.quantiseInterLuma()
	s.quantiseInterChroma()
	s.reconstructInterLuma()
	s.reconstructInterChroma()
}

func (s *mbEncoder) evaluateInter(c interCandidate, lambda float64) (float64, error) {
	s.applyInterCandidate(c)
	ssd := s.mbSSD()
	if s.cb != nil {
		inc := s.skipFlagInc()
		n := s.trialBitsCABAC(func() {
			s.cb.MBSkipFlagP(inc, false)
			s.writeInterMBCABAC()
		})
		return rdCost(ssd, n, lambda), nil
	}
	n, err := s.trialBits(s.writeInterMB)
	if err != nil {
		return 0, err
	}
	return rdCost(ssd, n, lambda), nil
}

func (s *mbEncoder) skipLeavesNoResidual(mv [2]int16) bool {
	s.reset()
	s.cur.kind = mbTypePSkip
	s.cur.Intra = false
	s.clearMotion()
	s.setMotion(mv)
	s.motionCompensateMB(mv)
	s.quantiseInterLuma()
	s.quantiseInterChroma()
	if s.cur.cbpLuma != 0 || s.cur.cbpChroma != 0 {
		return false
	}
	s.cur.skipped = true
	s.cur.NzY = [16]uint8{}
	s.cur.nzCb = [4]uint8{}
	s.cur.nzCr = [4]uint8{}
	return true
}

func (s *mbEncoder) applySkip(mv [2]int16) {
	s.reset()
	s.cur.kind = mbTypePSkip
	s.cur.Intra = false
	s.cur.skipped = true
	s.cur.cbpLuma = 0
	s.cur.cbpChroma = 0
	s.cur.NzY = [16]uint8{}
	s.cur.nzCb = [4]uint8{}
	s.cur.nzCr = [4]uint8{}
	s.setMotion(mv)
	s.motionCompensateMB(mv)
}

func (s *mbEncoder) evaluateSkip(mv [2]int16, lambda float64) float64 {
	s.applySkip(mv)
	bits := 1
	if s.cb != nil {
		inc := s.skipFlagInc()
		bits = s.trialBitsCABAC(func() { s.cb.MBSkipFlagP(inc, true) })
	}
	return rdCost(s.mbSSD(), bits, lambda)
}

func (s *mbEncoder) evaluateIntra(typeOffset uint32, lambda float64) (float64, error) {
	s.encodeIntraModes()
	ssd := s.mbSSD()
	if s.cb != nil {
		inc := s.skipFlagInc()
		n := s.trialBitsCABAC(func() {
			s.cb.MBSkipFlagP(inc, false)
			s.writeIntraMBCABAC(true)
		})
		return rdCost(ssd, n, lambda), nil
	}
	n, err := s.trialBits(func() error { return s.writeIntraMB(typeOffset) })
	if err != nil {
		return 0, err
	}
	return rdCost(ssd, n, lambda), nil
}
