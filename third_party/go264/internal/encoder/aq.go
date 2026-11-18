package encoder

import (
	"math"

	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/frame"
)

type AQMode uint8

const (
	AQOff AQMode = iota
	AQVariance
)

const (
	aqAdjustmentLimit = 6
	aqOffsetLimit     = 10
	aqStrengthDefault = 1.0
)

func lumaEnergy(p *frame.Picture, mbx, mby int) float64 {
	off := p.LumaOffset(mbx*16, mby*16)
	sum, square := 0, 0
	for y := 0; y < 16; y++ {
		row := p.Y[off+y*p.StrideY:]
		for x := 0; x < 16; x++ {
			v := int(row[x])
			sum += v
			square += v * v
		}
	}
	return float64(square) - float64(sum)*float64(sum)/256
}

func (e *Encoder) adaptiveQuant(h *frameHints) *frameHints {
	if e.cfg.AQMode == AQOff {
		return h
	}
	n := e.widthMBs * e.heightMBs
	if cap(e.aqEnergy) < n {
		e.aqEnergy = make([]float64, n)
	}
	energy := e.aqEnergy[:n]
	total := 0.0
	for mby := 0; mby < e.heightMBs; mby++ {
		for mbx := 0; mbx < e.widthMBs; mbx++ {
			v := math.Log2(lumaEnergy(e.src, mbx, mby) + 1)
			energy[mby*e.widthMBs+mbx] = v
			total += v
		}
	}
	mean := total / float64(n)
	drift := 0.0
	for i, v := range energy {
		adjust := clampFloat(-e.cfg.AQStrength*(v-mean), -aqAdjustmentLimit, aqAdjustmentLimit)
		energy[i] = adjust
		drift += adjust
	}
	drift /= float64(n)
	if h == nil {
		h = &frameHints{widthMBs: e.widthMBs, heightMBs: e.heightMBs}
	}
	if h.qpOffset == nil {
		h.qpOffset = make([]int8, n)
	}
	for i, adjust := range energy {
		rounded := clampInt(int(math.Round(adjust-drift)), -aqAdjustmentLimit, aqAdjustmentLimit)
		h.qpOffset[i] = int8(clampInt(int(h.qpOffset[i])+rounded, -aqOffsetLimit, aqOffsetLimit))
	}
	return h
}

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
