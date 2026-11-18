package syntax

import (
	"fmt"

	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/bits"
)

const ExtendedSAR = 255

func parseHRD(r *bits.Reader, h *HRD) error {
	var err error
	if h.CPBCntMinus1, err = r.ReadUE(); err != nil {
		return err
	}
	if h.CPBCntMinus1 > 31 {
		return fmt.Errorf("%w: cpb_cnt_minus1 %d", ErrInvalidValue, h.CPBCntMinus1)
	}
	v, err := r.ReadBits(4)
	if err != nil {
		return err
	}
	h.BitRateScale = uint8(v)
	if v, err = r.ReadBits(4); err != nil {
		return err
	}
	h.CPBSizeScale = uint8(v)
	n := int(h.CPBCntMinus1) + 1
	h.BitRateValueMinus1 = make([]uint32, n)
	h.CPBSizeValueMinus1 = make([]uint32, n)
	h.CBRFlag = make([]bool, n)
	for i := 0; i < n; i++ {
		if h.BitRateValueMinus1[i], err = r.ReadUE(); err != nil {
			return err
		}
		if h.CPBSizeValueMinus1[i], err = r.ReadUE(); err != nil {
			return err
		}
		if h.CBRFlag[i], err = r.ReadFlag(); err != nil {
			return err
		}
	}
	if v, err = r.ReadBits(5); err != nil {
		return err
	}
	h.InitialCPBRemovalDelayLengthMinus1 = uint8(v)
	if v, err = r.ReadBits(5); err != nil {
		return err
	}
	h.CPBRemovalDelayLengthMinus1 = uint8(v)
	if v, err = r.ReadBits(5); err != nil {
		return err
	}
	h.DPBOutputDelayLengthMinus1 = uint8(v)
	if v, err = r.ReadBits(5); err != nil {
		return err
	}
	h.TimeOffsetLength = uint8(v)
	return nil
}

func writeHRD(w *bits.Writer, h *HRD) {
	w.WriteUE(h.CPBCntMinus1)
	w.WriteBits(uint32(h.BitRateScale), 4)
	w.WriteBits(uint32(h.CPBSizeScale), 4)
	for i := 0; i <= int(h.CPBCntMinus1); i++ {
		w.WriteUE(h.BitRateValueMinus1[i])
		w.WriteUE(h.CPBSizeValueMinus1[i])
		w.WriteFlag(h.CBRFlag[i])
	}
	w.WriteBits(uint32(h.InitialCPBRemovalDelayLengthMinus1), 5)
	w.WriteBits(uint32(h.CPBRemovalDelayLengthMinus1), 5)
	w.WriteBits(uint32(h.DPBOutputDelayLengthMinus1), 5)
	w.WriteBits(uint32(h.TimeOffsetLength), 5)
}

func parseVUI(r *bits.Reader, v *VUI) error {
	var err error
	if v.AspectRatioInfoPresent, err = r.ReadFlag(); err != nil {
		return err
	}
	if v.AspectRatioInfoPresent {
		x, err := r.ReadBits(8)
		if err != nil {
			return err
		}
		v.AspectRatioIDC = uint8(x)
		if v.AspectRatioIDC == ExtendedSAR {
			if x, err = r.ReadBits(16); err != nil {
				return err
			}
			v.SarWidth = uint16(x)
			if x, err = r.ReadBits(16); err != nil {
				return err
			}
			v.SarHeight = uint16(x)
		}
	}
	if v.OverscanInfoPresent, err = r.ReadFlag(); err != nil {
		return err
	}
	if v.OverscanInfoPresent {
		if v.OverscanAppropriate, err = r.ReadFlag(); err != nil {
			return err
		}
	}
	if v.VideoSignalTypePresent, err = r.ReadFlag(); err != nil {
		return err
	}
	if v.VideoSignalTypePresent {
		x, err := r.ReadBits(3)
		if err != nil {
			return err
		}
		v.VideoFormat = uint8(x)
		if v.VideoFullRange, err = r.ReadFlag(); err != nil {
			return err
		}
		if v.ColourDescPresent, err = r.ReadFlag(); err != nil {
			return err
		}
		if v.ColourDescPresent {
			if x, err = r.ReadBits(8); err != nil {
				return err
			}
			v.ColourPrimaries = uint8(x)
			if x, err = r.ReadBits(8); err != nil {
				return err
			}
			v.TransferCharacteristics = uint8(x)
			if x, err = r.ReadBits(8); err != nil {
				return err
			}
			v.MatrixCoefficients = uint8(x)
		}
	}
	if v.ChromaLocInfoPresent, err = r.ReadFlag(); err != nil {
		return err
	}
	if v.ChromaLocInfoPresent {
		if v.ChromaSampleLocTypeTop, err = r.ReadUE(); err != nil {
			return err
		}
		if v.ChromaSampleLocTypeBottom, err = r.ReadUE(); err != nil {
			return err
		}
	}
	if v.TimingInfoPresent, err = r.ReadFlag(); err != nil {
		return err
	}
	if v.TimingInfoPresent {
		if v.NumUnitsInTick, err = r.ReadBits(32); err != nil {
			return err
		}
		if v.TimeScale, err = r.ReadBits(32); err != nil {
			return err
		}
		if v.FixedFrameRate, err = r.ReadFlag(); err != nil {
			return err
		}
	}
	if v.NalHRDPresent, err = r.ReadFlag(); err != nil {
		return err
	}
	if v.NalHRDPresent {
		if err := parseHRD(r, &v.NalHRD); err != nil {
			return err
		}
	}
	if v.VclHRDPresent, err = r.ReadFlag(); err != nil {
		return err
	}
	if v.VclHRDPresent {
		if err := parseHRD(r, &v.VclHRD); err != nil {
			return err
		}
	}
	if v.NalHRDPresent || v.VclHRDPresent {
		if v.LowDelayHRD, err = r.ReadFlag(); err != nil {
			return err
		}
	}
	if v.PicStructPresent, err = r.ReadFlag(); err != nil {
		return err
	}
	if v.BitstreamRestriction, err = r.ReadFlag(); err != nil {
		return err
	}
	if v.BitstreamRestriction {
		if v.MotionVectorsOverPicBoundary, err = r.ReadFlag(); err != nil {
			return err
		}
		if v.MaxBytesPerPicDenom, err = r.ReadUE(); err != nil {
			return err
		}
		if v.MaxBitsPerMBDenom, err = r.ReadUE(); err != nil {
			return err
		}
		if v.Log2MaxMvLengthHorizontal, err = r.ReadUE(); err != nil {
			return err
		}
		if v.Log2MaxMvLengthVertical, err = r.ReadUE(); err != nil {
			return err
		}
		if v.MaxNumReorderFrames, err = r.ReadUE(); err != nil {
			return err
		}
		if v.MaxDecFrameBuffering, err = r.ReadUE(); err != nil {
			return err
		}
	}
	return nil
}

func writeVUI(w *bits.Writer, v *VUI) {
	w.WriteFlag(v.AspectRatioInfoPresent)
	if v.AspectRatioInfoPresent {
		w.WriteBits(uint32(v.AspectRatioIDC), 8)
		if v.AspectRatioIDC == ExtendedSAR {
			w.WriteBits(uint32(v.SarWidth), 16)
			w.WriteBits(uint32(v.SarHeight), 16)
		}
	}
	w.WriteFlag(v.OverscanInfoPresent)
	if v.OverscanInfoPresent {
		w.WriteFlag(v.OverscanAppropriate)
	}
	w.WriteFlag(v.VideoSignalTypePresent)
	if v.VideoSignalTypePresent {
		w.WriteBits(uint32(v.VideoFormat), 3)
		w.WriteFlag(v.VideoFullRange)
		w.WriteFlag(v.ColourDescPresent)
		if v.ColourDescPresent {
			w.WriteBits(uint32(v.ColourPrimaries), 8)
			w.WriteBits(uint32(v.TransferCharacteristics), 8)
			w.WriteBits(uint32(v.MatrixCoefficients), 8)
		}
	}
	w.WriteFlag(v.ChromaLocInfoPresent)
	if v.ChromaLocInfoPresent {
		w.WriteUE(v.ChromaSampleLocTypeTop)
		w.WriteUE(v.ChromaSampleLocTypeBottom)
	}
	w.WriteFlag(v.TimingInfoPresent)
	if v.TimingInfoPresent {
		w.WriteBits(v.NumUnitsInTick, 32)
		w.WriteBits(v.TimeScale, 32)
		w.WriteFlag(v.FixedFrameRate)
	}
	w.WriteFlag(v.NalHRDPresent)
	if v.NalHRDPresent {
		writeHRD(w, &v.NalHRD)
	}
	w.WriteFlag(v.VclHRDPresent)
	if v.VclHRDPresent {
		writeHRD(w, &v.VclHRD)
	}
	if v.NalHRDPresent || v.VclHRDPresent {
		w.WriteFlag(v.LowDelayHRD)
	}
	w.WriteFlag(v.PicStructPresent)
	w.WriteFlag(v.BitstreamRestriction)
	if v.BitstreamRestriction {
		w.WriteFlag(v.MotionVectorsOverPicBoundary)
		w.WriteUE(v.MaxBytesPerPicDenom)
		w.WriteUE(v.MaxBitsPerMBDenom)
		w.WriteUE(v.Log2MaxMvLengthHorizontal)
		w.WriteUE(v.Log2MaxMvLengthVertical)
		w.WriteUE(v.MaxNumReorderFrames)
		w.WriteUE(v.MaxDecFrameBuffering)
	}
}

func (s *SPS) RestrictBitstream(reorder int) {
	s.VUIPresent = true
	v := &s.VUI
	v.BitstreamRestriction = true
	v.MotionVectorsOverPicBoundary = true
	v.MaxBytesPerPicDenom = 2
	v.MaxBitsPerMBDenom = 1
	v.Log2MaxMvLengthHorizontal = 15
	v.Log2MaxMvLengthVertical = 15
	v.MaxNumReorderFrames = uint32(reorder)
	v.MaxDecFrameBuffering = uint32(max(int(s.MaxNumRefFrames), reorder, 1))
}
