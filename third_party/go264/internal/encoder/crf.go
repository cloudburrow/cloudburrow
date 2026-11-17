package encoder

import (
	"math"

	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/syntax"
)

func qp2qscale(qp float64) float64 { return 0.85 * math.Exp2((qp-12)/6) }

func qscale2qp(qscale float64) float64 { return 12 + 6*math.Log2(qscale/0.85) }

const (
	crfBaseComplexityPerMB = 44

	crfQCompDefault   = 0.8
	crfIPRatioDefault = 1.4
	crfPBRatioDefault = 1.3

	crfQPStep = 4.0

	crfComplexityFloor = 1.0
	crfQPFloorDrop     = 6.0

	complexityUnknown = -1.0
)

type constantQuality struct {
	baseComplexity  float64
	rateFactorConst float64
	exponent        float64
	ipRatio         float64
	pbRatio         float64
	step            float64
	floor           float64
	last            [5]float64
}

func newConstantQuality(cfg Config) *constantQuality {
	if cfg.RateFactor <= 0 {
		return nil
	}
	base := float64(((cfg.Width+15)/16)*((cfg.Height+15)/16)) * crfBaseComplexityPerMB
	exponent := 1 - cfg.QComp
	c := &constantQuality{
		baseComplexity:  base,
		rateFactorConst: math.Pow(base, exponent) / qp2qscale(cfg.RateFactor),
		exponent:        exponent,
		ipRatio:         cfg.IPRatio,
		pbRatio:         cfg.PBRatio,
		step:            crfQPStep,
		floor:           cfg.RateFactor - crfQPFloorDrop,
	}
	for i := range c.last {
		c.last[i] = -1
	}
	return c
}

func (c *constantQuality) qp(complexity float64, t syntax.SliceType) float64 {
	if complexity < 0 || math.IsInf(complexity, 0) || math.IsNaN(complexity) {
		complexity = c.baseComplexity
	}
	if complexity < crfComplexityFloor {
		complexity = crfComplexityFloor
	}
	qscale := math.Pow(complexity, c.exponent) / c.rateFactorConst
	switch {
	case t.IsI():
		qscale /= c.ipRatio
	case t.IsB():
		qscale *= c.pbRatio
	}
	qp := qscale2qp(qscale)
	if qp < c.floor {
		qp = c.floor
	}
	if last := c.last[t.Base()]; last >= 0 {
		if qp > last+c.step {
			qp = last + c.step
		}
		if qp < last-c.step {
			qp = last - c.step
		}
	}
	return qp
}

func (c *constantQuality) coded(qp int, t syntax.SliceType) {
	c.last[t.Base()] = float64(qp)
}
