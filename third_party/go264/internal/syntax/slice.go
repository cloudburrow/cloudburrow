package syntax

import (
	"fmt"

	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/bits"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/nal"
)

type SliceType uint8

const (
	SliceP  SliceType = 0
	SliceB  SliceType = 1
	SliceI  SliceType = 2
	SliceSP SliceType = 3
	SliceSI SliceType = 4
)

func (t SliceType) Base() SliceType { return t % 5 }

func (t SliceType) IsI() bool { return t.Base() == SliceI }

func (t SliceType) IsP() bool { return t.Base() == SliceP }

func (t SliceType) IsB() bool { return t.Base() == SliceB }

func (t SliceType) IsSI() bool { return t.Base() == SliceSI }

func (t SliceType) IsSP() bool { return t.Base() == SliceSP }

func (t SliceType) AllSlicesSameType() bool { return t >= 5 }

func (t SliceType) String() string {
	switch t.Base() {
	case SliceP:
		return "P"
	case SliceB:
		return "B"
	case SliceI:
		return "I"
	case SliceSP:
		return "SP"
	case SliceSI:
		return "SI"
	}
	return fmt.Sprintf("slice-type(%d)", uint8(t))
}

type RefPicListModification struct {
	IDC   uint32
	Value uint32
}

type MMCO struct {
	Op                        uint32
	DifferenceOfPicNumsMinus1 uint32
	LongTermPicNum            uint32
	LongTermFrameIdx          uint32
	MaxLongTermFrameIdxPlus1  uint32
}

type WeightEntry struct {
	LumaWeightFlag   bool
	LumaWeight       int32
	LumaOffset       int32
	ChromaWeightFlag bool
	ChromaWeight     [2]int32
	ChromaOffset     [2]int32
}

type PredWeightTable struct {
	LumaLog2WeightDenom   uint32
	ChromaLog2WeightDenom uint32
	L0                    []WeightEntry
	L1                    []WeightEntry
}

type SliceHeader struct {
	FirstMBInSlice uint32
	SliceType      SliceType
	PPSID          uint32
	ColourPlaneID  uint32
	FrameNum       uint32

	FieldPic    bool
	BottomField bool

	IDRPicID uint32

	PicOrderCntLsb         uint32
	DeltaPicOrderCntBottom int32
	DeltaPicOrderCnt       [2]int32

	RedundantPicCnt uint32

	DirectSpatialMvPred bool

	NumRefIdxActiveOverride bool
	NumRefIdxL0ActiveMinus1 uint32
	NumRefIdxL1ActiveMinus1 uint32

	RefPicListModificationL0 []RefPicListModification
	RefPicListModificationL1 []RefPicListModification
	ModificationL0Present    bool
	ModificationL1Present    bool

	PredWeight PredWeightTable

	NoOutputOfPriorPics   bool
	LongTermReference     bool
	AdaptiveRefPicMarking bool
	MMCOs                 []MMCO

	CABACInitIDC uint32

	SliceQPDelta int32
	SPForSwitch  bool
	SliceQSDelta int32

	DisableDeblockingFilterIDC uint32
	SliceAlphaC0OffsetDiv2     int32
	SliceBetaOffsetDiv2        int32

	IDR       bool
	NalRefIDC uint8
}

func (h *SliceHeader) MbaffFrame(s *SPS) bool {
	return s.MBAdaptiveFrameField && !h.FieldPic
}

func (h *SliceHeader) NumRefIdxL0Active(p *PPS) int {
	if h.NumRefIdxActiveOverride {
		return int(h.NumRefIdxL0ActiveMinus1) + 1
	}
	return int(p.NumRefIdxL0DefaultActiveMinus1) + 1
}

func (h *SliceHeader) NumRefIdxL1Active(p *PPS) int {
	if h.NumRefIdxActiveOverride {
		return int(h.NumRefIdxL1ActiveMinus1) + 1
	}
	return int(p.NumRefIdxL1DefaultActiveMinus1) + 1
}

func (h *SliceHeader) QPY(p *PPS) int { return p.SliceQPY(h.SliceQPDelta) }

type ParameterSets interface {
	SPS(id uint32) *SPS
	PPS(id uint32) *PPS
}

func ParseSliceHeader(r *bits.Reader, unit nal.Header, sets ParameterSets) (*SliceHeader, *SPS, *PPS, error) {
	h := &SliceHeader{
		IDR:       unit.Type == nal.TypeSliceIDR,
		NalRefIDC: unit.RefIDC,
	}
	var err error
	if h.FirstMBInSlice, err = r.ReadUE(); err != nil {
		return nil, nil, nil, err
	}
	st, err := r.ReadUE()
	if err != nil {
		return nil, nil, nil, err
	}
	if st > 9 {
		return nil, nil, nil, fmt.Errorf("%w: slice_type %d", ErrInvalidValue, st)
	}
	h.SliceType = SliceType(st)
	if h.PPSID, err = r.ReadUE(); err != nil {
		return nil, nil, nil, err
	}
	pps := sets.PPS(h.PPSID)
	if pps == nil {
		return nil, nil, nil, ErrMissingPPS
	}
	sps := sets.SPS(pps.SPSID)
	if sps == nil {
		return nil, nil, nil, ErrMissingSPS
	}
	if h.IDR && !h.SliceType.IsI() && !h.SliceType.IsSI() {
		return nil, nil, nil, fmt.Errorf("%w: non-intra slice in an IDR picture", ErrInvalidValue)
	}

	if sps.SeparateColourPlane {
		if h.ColourPlaneID, err = r.ReadBits(2); err != nil {
			return nil, nil, nil, err
		}
	}
	if h.FrameNum, err = r.ReadBits(int(sps.Log2MaxFrameNumMinus4) + 4); err != nil {
		return nil, nil, nil, err
	}
	if !sps.FrameMbsOnly {
		if h.FieldPic, err = r.ReadFlag(); err != nil {
			return nil, nil, nil, err
		}
		if h.FieldPic {
			if h.BottomField, err = r.ReadFlag(); err != nil {
				return nil, nil, nil, err
			}
		}
	}
	if h.IDR {
		if h.IDRPicID, err = r.ReadUE(); err != nil {
			return nil, nil, nil, err
		}
		if h.IDRPicID > 65535 {
			return nil, nil, nil, fmt.Errorf("%w: idr_pic_id %d", ErrInvalidValue, h.IDRPicID)
		}
	}
	switch sps.PicOrderCntType {
	case 0:
		if h.PicOrderCntLsb, err = r.ReadBits(int(sps.Log2MaxPicOrderCntLsbMinus4) + 4); err != nil {
			return nil, nil, nil, err
		}
		if pps.BottomFieldPicOrderInFramePresent && !h.FieldPic {
			if h.DeltaPicOrderCntBottom, err = r.ReadSE(); err != nil {
				return nil, nil, nil, err
			}
		}
	case 1:
		if !sps.DeltaPicOrderAlwaysZero {
			if h.DeltaPicOrderCnt[0], err = r.ReadSE(); err != nil {
				return nil, nil, nil, err
			}
			if pps.BottomFieldPicOrderInFramePresent && !h.FieldPic {
				if h.DeltaPicOrderCnt[1], err = r.ReadSE(); err != nil {
					return nil, nil, nil, err
				}
			}
		}
	}
	if pps.RedundantPicCntPresent {
		if h.RedundantPicCnt, err = r.ReadUE(); err != nil {
			return nil, nil, nil, err
		}
	}
	if h.SliceType.IsB() {
		if h.DirectSpatialMvPred, err = r.ReadFlag(); err != nil {
			return nil, nil, nil, err
		}
	}
	h.NumRefIdxL0ActiveMinus1 = pps.NumRefIdxL0DefaultActiveMinus1
	h.NumRefIdxL1ActiveMinus1 = pps.NumRefIdxL1DefaultActiveMinus1
	if h.SliceType.IsP() || h.SliceType.IsSP() || h.SliceType.IsB() {
		if h.NumRefIdxActiveOverride, err = r.ReadFlag(); err != nil {
			return nil, nil, nil, err
		}
		if h.NumRefIdxActiveOverride {
			if h.NumRefIdxL0ActiveMinus1, err = r.ReadUE(); err != nil {
				return nil, nil, nil, err
			}
			if h.SliceType.IsB() {
				if h.NumRefIdxL1ActiveMinus1, err = r.ReadUE(); err != nil {
					return nil, nil, nil, err
				}
			}
		}
		if h.NumRefIdxL0ActiveMinus1 > 31 || h.NumRefIdxL1ActiveMinus1 > 31 {
			return nil, nil, nil, fmt.Errorf("%w: num_ref_idx_active_minus1", ErrInvalidValue)
		}
	}
	if err := parseRefPicListModification(r, h); err != nil {
		return nil, nil, nil, err
	}
	if (pps.WeightedPred && (h.SliceType.IsP() || h.SliceType.IsSP())) ||
		(pps.WeightedBipredIDC == 1 && h.SliceType.IsB()) {
		if err := parsePredWeightTable(r, h, sps); err != nil {
			return nil, nil, nil, err
		}
	}
	if h.NalRefIDC != 0 {
		if err := parseDecRefPicMarking(r, h); err != nil {
			return nil, nil, nil, err
		}
	}
	if pps.CABAC && !h.SliceType.IsI() && !h.SliceType.IsSI() {
		if h.CABACInitIDC, err = r.ReadUE(); err != nil {
			return nil, nil, nil, err
		}
		if h.CABACInitIDC > 2 {
			return nil, nil, nil, fmt.Errorf("%w: cabac_init_idc %d", ErrInvalidValue, h.CABACInitIDC)
		}
	}
	if h.SliceQPDelta, err = r.ReadSE(); err != nil {
		return nil, nil, nil, err
	}
	qp := h.QPY(pps)
	if qp < 0 || qp > 51 {
		return nil, nil, nil, fmt.Errorf("%w: SliceQPY %d", ErrInvalidValue, qp)
	}
	if h.SliceType.IsSP() || h.SliceType.IsSI() {
		if h.SliceType.IsSP() {
			if h.SPForSwitch, err = r.ReadFlag(); err != nil {
				return nil, nil, nil, err
			}
		}
		if h.SliceQSDelta, err = r.ReadSE(); err != nil {
			return nil, nil, nil, err
		}
	}
	if pps.DeblockingFilterControlPresent {
		if h.DisableDeblockingFilterIDC, err = r.ReadUE(); err != nil {
			return nil, nil, nil, err
		}
		if h.DisableDeblockingFilterIDC > 2 {
			return nil, nil, nil, fmt.Errorf("%w: disable_deblocking_filter_idc %d", ErrInvalidValue, h.DisableDeblockingFilterIDC)
		}
		if h.DisableDeblockingFilterIDC != 1 {
			if h.SliceAlphaC0OffsetDiv2, err = r.ReadSE(); err != nil {
				return nil, nil, nil, err
			}
			if h.SliceBetaOffsetDiv2, err = r.ReadSE(); err != nil {
				return nil, nil, nil, err
			}
			if h.SliceAlphaC0OffsetDiv2 < -6 || h.SliceAlphaC0OffsetDiv2 > 6 ||
				h.SliceBetaOffsetDiv2 < -6 || h.SliceBetaOffsetDiv2 > 6 {
				return nil, nil, nil, fmt.Errorf("%w: deblocking filter offset", ErrInvalidValue)
			}
		}
	}
	return h, sps, pps, nil
}

func parseRefPicListModification(r *bits.Reader, h *SliceHeader) error {
	var err error
	if !h.SliceType.IsI() && !h.SliceType.IsSI() {
		if h.ModificationL0Present, err = r.ReadFlag(); err != nil {
			return err
		}
		if h.ModificationL0Present {
			if h.RefPicListModificationL0, err = parseModificationList(r); err != nil {
				return err
			}
		}
	}
	if h.SliceType.IsB() {
		if h.ModificationL1Present, err = r.ReadFlag(); err != nil {
			return err
		}
		if h.ModificationL1Present {
			if h.RefPicListModificationL1, err = parseModificationList(r); err != nil {
				return err
			}
		}
	}
	return nil
}

const maxModifications = 64

func parseModificationList(r *bits.Reader) ([]RefPicListModification, error) {
	var out []RefPicListModification
	for {
		idc, err := r.ReadUE()
		if err != nil {
			return nil, err
		}
		if idc == 3 {
			return out, nil
		}
		if idc > 3 {
			return nil, fmt.Errorf("%w: modification_of_pic_nums_idc %d", ErrInvalidValue, idc)
		}
		v, err := r.ReadUE()
		if err != nil {
			return nil, err
		}
		out = append(out, RefPicListModification{IDC: idc, Value: v})
		if len(out) > maxModifications {
			return nil, fmt.Errorf("%w: too many reference list modifications", ErrInvalidValue)
		}
	}
}

const maxMMCOs = 64

func parseDecRefPicMarking(r *bits.Reader, h *SliceHeader) error {
	var err error
	if h.IDR {
		if h.NoOutputOfPriorPics, err = r.ReadFlag(); err != nil {
			return err
		}
		h.LongTermReference, err = r.ReadFlag()
		return err
	}
	if h.AdaptiveRefPicMarking, err = r.ReadFlag(); err != nil {
		return err
	}
	if !h.AdaptiveRefPicMarking {
		return nil
	}
	for {
		op, err := r.ReadUE()
		if err != nil {
			return err
		}
		if op == 0 {
			return nil
		}
		if op > 6 {
			return fmt.Errorf("%w: memory_management_control_operation %d", ErrInvalidValue, op)
		}
		m := MMCO{Op: op}
		switch op {
		case 1, 3:
			if m.DifferenceOfPicNumsMinus1, err = r.ReadUE(); err != nil {
				return err
			}
		case 2:
			if m.LongTermPicNum, err = r.ReadUE(); err != nil {
				return err
			}
		}
		switch op {
		case 3, 6:
			if m.LongTermFrameIdx, err = r.ReadUE(); err != nil {
				return err
			}
		case 4:
			if m.MaxLongTermFrameIdxPlus1, err = r.ReadUE(); err != nil {
				return err
			}
		}
		h.MMCOs = append(h.MMCOs, m)
		if len(h.MMCOs) > maxMMCOs {
			return fmt.Errorf("%w: too many memory management operations", ErrInvalidValue)
		}
	}
}

func parsePredWeightTable(r *bits.Reader, h *SliceHeader, sps *SPS) error {
	t := &h.PredWeight
	var err error
	if t.LumaLog2WeightDenom, err = r.ReadUE(); err != nil {
		return err
	}
	if t.LumaLog2WeightDenom > 7 {
		return fmt.Errorf("%w: luma_log2_weight_denom %d", ErrInvalidValue, t.LumaLog2WeightDenom)
	}
	hasChroma := sps.ChromaArrayType() != 0
	if hasChroma {
		if t.ChromaLog2WeightDenom, err = r.ReadUE(); err != nil {
			return err
		}
		if t.ChromaLog2WeightDenom > 7 {
			return fmt.Errorf("%w: chroma_log2_weight_denom %d", ErrInvalidValue, t.ChromaLog2WeightDenom)
		}
	}
	defLuma := int32(1) << t.LumaLog2WeightDenom
	defChroma := int32(1) << t.ChromaLog2WeightDenom
	if t.L0, err = parseWeightList(r, int(h.NumRefIdxL0ActiveMinus1)+1, hasChroma, defLuma, defChroma); err != nil {
		return err
	}
	if h.SliceType.IsB() {
		if t.L1, err = parseWeightList(r, int(h.NumRefIdxL1ActiveMinus1)+1, hasChroma, defLuma, defChroma); err != nil {
			return err
		}
	}
	return nil
}

func inWeightRange(v int32) bool { return v >= -128 && v <= 127 }

func parseWeightList(r *bits.Reader, n int, hasChroma bool, defLuma, defChroma int32) ([]WeightEntry, error) {
	out := make([]WeightEntry, n)
	for i := range out {
		e := &out[i]
		var err error
		if e.LumaWeightFlag, err = r.ReadFlag(); err != nil {
			return nil, err
		}
		if e.LumaWeightFlag {
			if e.LumaWeight, err = r.ReadSE(); err != nil {
				return nil, err
			}
			if e.LumaOffset, err = r.ReadSE(); err != nil {
				return nil, err
			}
			if !inWeightRange(e.LumaWeight) || !inWeightRange(e.LumaOffset) {
				return nil, fmt.Errorf("%w: luma weight %d offset %d", ErrInvalidValue, e.LumaWeight, e.LumaOffset)
			}
		} else {
			e.LumaWeight = defLuma
		}
		if !hasChroma {
			continue
		}
		if e.ChromaWeightFlag, err = r.ReadFlag(); err != nil {
			return nil, err
		}
		if e.ChromaWeightFlag {
			for j := 0; j < 2; j++ {
				if e.ChromaWeight[j], err = r.ReadSE(); err != nil {
					return nil, err
				}
				if e.ChromaOffset[j], err = r.ReadSE(); err != nil {
					return nil, err
				}
				if !inWeightRange(e.ChromaWeight[j]) || !inWeightRange(e.ChromaOffset[j]) {
					return nil, fmt.Errorf("%w: chroma weight %d offset %d",
						ErrInvalidValue, e.ChromaWeight[j], e.ChromaOffset[j])
				}
			}
		} else {
			e.ChromaWeight[0] = defChroma
			e.ChromaWeight[1] = defChroma
		}
	}
	return out, nil
}
