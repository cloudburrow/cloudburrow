package encoder

import (
	"math"

	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/syntax"
)

type rateControl struct {
	enabled    bool
	targetBits float64
	bufferSize float64
	buffer     float64
	baseQP     int
	minQP      int
	maxQP      int

	complexityInter float64
	complexityIntra float64

	framesCoded  int
	bitsProduced float64

	crf      *constantQuality
	vbv      bool
	cbr      bool
	cpbSize  float64
	cpbRate  float64
	perFrame float64
	fill     float64
}

func newRateControl(cfg Config) *rateControl {
	rc := &rateControl{
		baseQP: cfg.QP,
		minQP:  8,
		maxQP:  50,
	}
	fps := float64(cfg.FPSNum) / float64(cfg.FPSDen)
	if fps <= 0 {
		fps = 25
	}
	if rc.crf = newConstantQuality(cfg); rc.crf != nil {
		rc.minQP = 0
		rc.maxQP = 51
	}
	if cfg.VBVBufferKbits > 0 && cfg.VBVMaxrateKbps > 0 {
		rc.vbv = true
		rc.cbr = cfg.CBR
		rc.minQP = 0
		rc.maxQP = 51
		rc.cpbRate = float64(hrdBitRate(cfg.VBVMaxrateKbps))
		rc.cpbSize = float64(hrdCPBSize(cfg.VBVBufferKbits))
		rc.perFrame = rc.cpbRate / fps
		rc.fill = rc.cpbSize
	}
	if cfg.BitrateKbps <= 0 && !rc.vbv {
		return rc
	}
	rc.enabled = true
	target := cfg.BitrateKbps
	if target <= 0 {
		target = cfg.VBVMaxrateKbps
	}
	rc.targetBits = float64(target) * 1000 / fps
	rc.bufferSize = rc.targetBits * 8
	return rc
}

func (rc *rateControl) intraTarget() float64 { return rc.targetBits * 4 }

func (rc *rateControl) frameQP(t syntax.SliceType, idr bool, measured float64) int {
	if rc.crf != nil {
		return rc.constantQualityQP(t, idr, measured)
	}
	if rc.vbv {
		return rc.vbvQP(idr)
	}
	if !rc.enabled {
		return rc.baseQP
	}
	complexity := rc.complexityInter
	target := rc.targetBits
	if idr {
		complexity = rc.complexityIntra
		target = rc.intraTarget()
	}
	if complexity <= 0 {
		return rc.baseQP
	}
	target -= rc.buffer * 0.5
	if minimum := rc.targetBits * 0.1; target < minimum {
		target = minimum
	}
	qp := 6 * math.Log2(complexity/target)
	return rc.clamp(int(math.Round(qp)))
}

func (rc *rateControl) constantQualityQP(t syntax.SliceType, idr bool, measured float64) int {
	qp := rc.clamp(int(math.Round(rc.crf.qp(measured, t))))
	if !rc.vbv {
		return qp
	}
	if ceiling := rc.vbvQP(idr); ceiling > qp {
		return ceiling
	}
	return qp
}

func (rc *rateControl) vbvTarget(idr bool) float64 {
	target := rc.targetBits
	if idr {
		target = rc.intraTarget()
	}
	target -= rc.buffer * 0.5
	if floor := rc.minimumBits(); target < floor {
		target = floor
	}
	if ceiling := rc.maximumBits(); target > ceiling {
		target = ceiling
	}
	if target < 1 {
		target = 1
	}
	return target
}

func (rc *rateControl) minimumBits() float64 {
	if !rc.cbr {
		return 0
	}
	need := rc.fill + rc.perFrame - rc.cpbSize
	if need < 0 {
		return 0
	}
	return need
}

func (rc *rateControl) maximumBits() float64 { return rc.fill * 0.85 }

func (rc *rateControl) vbvQP(idr bool) int {
	complexity := rc.complexityInter
	if idr {
		complexity = rc.complexityIntra
	}
	if complexity <= 0 {
		return rc.clamp(rc.baseQP)
	}
	qp := 6 * math.Log2(complexity/rc.vbvTarget(idr))
	return rc.clamp(int(math.Round(qp)))
}

func (rc *rateControl) overBudget(bits int) bool {
	return rc.vbv && float64(bits) > rc.maximumBits()
}

func (rc *rateControl) shrinkQP(bits, qp, attempt int) int {
	step := int(math.Ceil(6 * math.Log2(float64(bits)/rc.maximumBits())))
	if escalation := 1 << uint(attempt); step < escalation {
		step = escalation
	}
	if step < 1 {
		step = 1
	}
	next := qp + step
	if next > 51 {
		next = 51
	}
	return next
}

func (rc *rateControl) padBytes(bits int) int {
	if !rc.vbv || !rc.cbr {
		return 0
	}
	need := rc.minimumBits() - float64(bits)
	if need <= 0 {
		return 0
	}
	n := int(math.Ceil(need / 8))
	if n < fillerOverhead {
		n = fillerOverhead
	}
	if float64(bits+n*8) > rc.fill {
		return 0
	}
	return n
}

func (rc *rateControl) clamp(qp int) int {
	if qp < rc.minQP {
		return rc.minQP
	}
	if qp > rc.maxQP {
		return rc.maxQP
	}
	return qp
}

func (rc *rateControl) update(bits, qp int, t syntax.SliceType, idr, dropped bool) {
	rc.framesCoded++
	rc.bitsProduced += float64(bits)
	if rc.vbv {
		rc.fill -= float64(bits)
		if rc.fill < 0 {
			rc.fill = 0
		}
		rc.fill += rc.perFrame
		if rc.fill > rc.cpbSize {
			rc.fill = rc.cpbSize
		}
	}
	if dropped || (!rc.enabled && rc.crf == nil) {
		return
	}
	if rc.crf != nil {
		rc.crf.coded(qp, t)
	}
	if rc.crf == nil || !t.IsB() {
		observed := float64(bits) * math.Exp2(float64(qp)/6)
		if idr {
			rc.complexityIntra = blend(rc.complexityIntra, observed, 0.5)
		} else {
			rc.complexityInter = blend(rc.complexityInter, observed, 0.4)
			if rc.complexityIntra == 0 {
				rc.complexityIntra = observed * 4
			}
		}
	}
	if !rc.enabled {
		return
	}
	target := rc.targetBits
	if idr {
		target = rc.intraTarget()
	}
	rc.buffer += float64(bits) - target
	if rc.buffer > rc.bufferSize {
		rc.buffer = rc.bufferSize
	}
	if rc.buffer < -rc.bufferSize {
		rc.buffer = -rc.bufferSize
	}
}

func blend(old, observed, weight float64) float64 {
	if old <= 0 {
		return observed
	}
	return old*(1-weight) + observed*weight
}
