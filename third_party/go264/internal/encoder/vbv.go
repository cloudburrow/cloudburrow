package encoder

import (
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/nal"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/syntax"
)

const (
	hrdBitRateUnit  = 64
	hrdCPBSizeUnit  = 16
	hrdDelayLength  = 24
	hrdOutputLength = 24
	hrdTimeOffset   = 24
	hrdClockRate    = 90000
)

func hrdBitRate(kbps int) int {
	units := (kbps*1000 + hrdBitRateUnit - 1) / hrdBitRateUnit
	if units < 1 {
		units = 1
	}
	return units * hrdBitRateUnit
}

func hrdCPBSize(kbits int) int {
	units := (kbits*1000 + hrdCPBSizeUnit - 1) / hrdCPBSizeUnit
	if units < 1 {
		units = 1
	}
	return units * hrdCPBSizeUnit
}

func (e *Encoder) applyHRD(sps *syntax.SPS) {
	if !e.rc.vbv {
		return
	}
	h := &sps.VUI.NalHRD
	h.CPBCntMinus1 = 0
	h.BitRateScale = 0
	h.CPBSizeScale = 0
	h.BitRateValueMinus1 = []uint32{uint32(hrdBitRate(e.cfg.VBVMaxrateKbps)/hrdBitRateUnit - 1)}
	h.CPBSizeValueMinus1 = []uint32{uint32(hrdCPBSize(e.cfg.VBVBufferKbits)/hrdCPBSizeUnit - 1)}
	h.CBRFlag = []bool{e.cfg.CBR}
	h.InitialCPBRemovalDelayLengthMinus1 = hrdDelayLength - 1
	h.CPBRemovalDelayLengthMinus1 = hrdDelayLength - 1
	h.DPBOutputDelayLengthMinus1 = hrdOutputLength - 1
	h.TimeOffsetLength = hrdTimeOffset
	sps.VUI.NalHRDPresent = true
	sps.VUI.LowDelayHRD = false
}

func (e *Encoder) cpbFullDelay() uint32 {
	d := hrdClockRate * e.rc.cpbSize / e.rc.cpbRate
	if d < 1 {
		return 1
	}
	if d > float64(1<<hrdDelayLength-1) {
		return 1<<hrdDelayLength - 1
	}
	return uint32(d)
}

func (e *Encoder) bufferingPeriodMessage() syntax.SEIMessage {
	full := e.cpbFullDelay()
	present := uint32(hrdClockRate * e.rc.fill / e.rc.cpbRate)
	if present < 1 {
		present = 1
	}
	if present > full {
		present = full
	}
	return syntax.SEIMessage{
		PayloadType: syntax.SEIPayloadTypeBufferingPeriod,
		BufferingPeriod: &syntax.SEIBufferingPeriod{
			SeqParameterSetID: 0,
			NalHRD: []syntax.SEIInitialCPBDelay{{
				InitialCPBRemovalDelay:       present,
				InitialCPBRemovalDelayOffset: full - present,
			}},
		},
	}
}

func (e *Encoder) picTimingMessage() syntax.SEIMessage {
	return syntax.SEIMessage{
		PayloadType: syntax.SEIPayloadTypePicTiming,
		PicTiming: &syntax.SEIPicTiming{
			CPBRemovalDelay: uint32(2 * (e.cpbFrame - e.cpbAnchor)),
			DPBOutputDelay:  uint32(2 * (e.cfg.BFrames + 1)),
		},
	}
}

func (e *Encoder) pictureSEI(newPeriod, recovery bool) ([]byte, error) {
	var msgs []syntax.SEIMessage
	if e.rc.vbv && newPeriod {
		e.cpbAnchor = e.cpbFrame
		msgs = append(msgs, e.bufferingPeriodMessage())
	}
	if e.rc.vbv {
		msgs = append(msgs, e.picTimingMessage())
	}
	if recovery {
		cnt := e.cfg.IntraRefresh - 1
		if cnt < 0 {
			cnt = 0
		}
		msgs = append(msgs, syntax.SEIMessage{
			PayloadType: syntax.SEIPayloadTypeRecoveryPoint,
			RecoveryPoint: &syntax.SEIRecoveryPoint{
				RecoveryFrameCnt: uint32(cnt),
				ExactMatch:       true,
			},
		})
	}
	if len(msgs) == 0 {
		return nil, nil
	}
	rbsp, err := syntax.WriteSEI(msgs, e.sps, func(uint32) *syntax.SPS { return e.sps })
	if err != nil {
		return nil, err
	}
	return nal.AppendAnnexB(nil, nal.Unit{
		Header: nal.Header{RefIDC: 0, Type: nal.TypeSEI},
		RBSP:   rbsp,
	}, true), nil
}

func appendFiller(dst []byte, n int) []byte {
	if n < 0 {
		return dst
	}
	rbsp := make([]byte, n)
	for i := range rbsp {
		rbsp[i] = 0xFF
	}
	return nal.AppendAnnexB(dst, nal.Unit{
		Header: nal.Header{RefIDC: 0, Type: nal.TypeFillerData},
		RBSP:   append(rbsp, 0x80),
	}, true)
}

const fillerOverhead = 6
