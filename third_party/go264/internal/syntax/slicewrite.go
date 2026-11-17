package syntax

import (
	"fmt"

	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/bits"
)

func WriteSliceHeader(w *bits.Writer, h *SliceHeader, sps *SPS, pps *PPS) error {
	w.WriteUE(h.FirstMBInSlice)
	w.WriteUE(uint32(h.SliceType))
	w.WriteUE(h.PPSID)
	if sps.SeparateColourPlane {
		w.WriteBits(h.ColourPlaneID, 2)
	}
	w.WriteBits(h.FrameNum, int(sps.Log2MaxFrameNumMinus4)+4)
	if !sps.FrameMbsOnly {
		w.WriteFlag(h.FieldPic)
		if h.FieldPic {
			w.WriteFlag(h.BottomField)
		}
	}
	if h.IDR {
		w.WriteUE(h.IDRPicID)
	}
	switch sps.PicOrderCntType {
	case 0:
		w.WriteBits(h.PicOrderCntLsb, int(sps.Log2MaxPicOrderCntLsbMinus4)+4)
		if pps.BottomFieldPicOrderInFramePresent && !h.FieldPic {
			w.WriteSE(h.DeltaPicOrderCntBottom)
		}
	case 1:
		if !sps.DeltaPicOrderAlwaysZero {
			w.WriteSE(h.DeltaPicOrderCnt[0])
			if pps.BottomFieldPicOrderInFramePresent && !h.FieldPic {
				w.WriteSE(h.DeltaPicOrderCnt[1])
			}
		}
	}
	if pps.RedundantPicCntPresent {
		w.WriteUE(h.RedundantPicCnt)
	}
	if h.SliceType.IsB() {
		w.WriteFlag(h.DirectSpatialMvPred)
	}
	if h.SliceType.IsP() || h.SliceType.IsSP() || h.SliceType.IsB() {
		w.WriteFlag(h.NumRefIdxActiveOverride)
		if h.NumRefIdxActiveOverride {
			w.WriteUE(h.NumRefIdxL0ActiveMinus1)
			if h.SliceType.IsB() {
				w.WriteUE(h.NumRefIdxL1ActiveMinus1)
			}
		}
	}
	writeRefPicListModification(w, h)
	if (pps.WeightedPred && (h.SliceType.IsP() || h.SliceType.IsSP())) ||
		(pps.WeightedBipredIDC == 1 && h.SliceType.IsB()) {
		writePredWeightTable(w, h, sps)
	}
	if h.NalRefIDC != 0 {
		writeDecRefPicMarking(w, h)
	}
	if pps.CABAC && !h.SliceType.IsI() && !h.SliceType.IsSI() {
		w.WriteUE(h.CABACInitIDC)
	}
	w.WriteSE(h.SliceQPDelta)
	if h.SliceType.IsSP() || h.SliceType.IsSI() {
		if h.SliceType.IsSP() {
			w.WriteFlag(h.SPForSwitch)
		}
		w.WriteSE(h.SliceQSDelta)
	}
	if pps.DeblockingFilterControlPresent {
		w.WriteUE(h.DisableDeblockingFilterIDC)
		if h.DisableDeblockingFilterIDC != 1 {
			w.WriteSE(h.SliceAlphaC0OffsetDiv2)
			w.WriteSE(h.SliceBetaOffsetDiv2)
		}
	}
	if err := w.Err(); err != nil {
		return fmt.Errorf("go264/syntax: writing slice header: %w", err)
	}
	return nil
}

func writeRefPicListModification(w *bits.Writer, h *SliceHeader) {
	if !h.SliceType.IsI() && !h.SliceType.IsSI() {
		w.WriteFlag(h.ModificationL0Present)
		if h.ModificationL0Present {
			writeModificationList(w, h.RefPicListModificationL0)
		}
	}
	if h.SliceType.IsB() {
		w.WriteFlag(h.ModificationL1Present)
		if h.ModificationL1Present {
			writeModificationList(w, h.RefPicListModificationL1)
		}
	}
}

func writeModificationList(w *bits.Writer, list []RefPicListModification) {
	for _, m := range list {
		w.WriteUE(m.IDC)
		w.WriteUE(m.Value)
	}
	w.WriteUE(3)
}

func writeDecRefPicMarking(w *bits.Writer, h *SliceHeader) {
	if h.IDR {
		w.WriteFlag(h.NoOutputOfPriorPics)
		w.WriteFlag(h.LongTermReference)
		return
	}
	w.WriteFlag(h.AdaptiveRefPicMarking)
	if !h.AdaptiveRefPicMarking {
		return
	}
	for _, m := range h.MMCOs {
		w.WriteUE(m.Op)
		switch m.Op {
		case 1, 3:
			w.WriteUE(m.DifferenceOfPicNumsMinus1)
		case 2:
			w.WriteUE(m.LongTermPicNum)
		}
		switch m.Op {
		case 3, 6:
			w.WriteUE(m.LongTermFrameIdx)
		case 4:
			w.WriteUE(m.MaxLongTermFrameIdxPlus1)
		}
	}
	w.WriteUE(0)
}

func writePredWeightTable(w *bits.Writer, h *SliceHeader, sps *SPS) {
	t := &h.PredWeight
	w.WriteUE(t.LumaLog2WeightDenom)
	hasChroma := sps.ChromaArrayType() != 0
	if hasChroma {
		w.WriteUE(t.ChromaLog2WeightDenom)
	}
	writeWeightList(w, t.L0, hasChroma)
	if h.SliceType.IsB() {
		writeWeightList(w, t.L1, hasChroma)
	}
}

func writeWeightList(w *bits.Writer, list []WeightEntry, hasChroma bool) {
	for i := range list {
		e := &list[i]
		w.WriteFlag(e.LumaWeightFlag)
		if e.LumaWeightFlag {
			w.WriteSE(e.LumaWeight)
			w.WriteSE(e.LumaOffset)
		}
		if !hasChroma {
			continue
		}
		w.WriteFlag(e.ChromaWeightFlag)
		if e.ChromaWeightFlag {
			for j := 0; j < 2; j++ {
				w.WriteSE(e.ChromaWeight[j])
				w.WriteSE(e.ChromaOffset[j])
			}
		}
	}
}
