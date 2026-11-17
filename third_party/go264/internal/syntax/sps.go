package syntax

import (
	"errors"
	"fmt"

	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/bits"
)

var (
	ErrUnsupported   = errors.New("go264/syntax: unsupported bitstream feature")
	ErrInvalidValue  = errors.New("go264/syntax: syntax element out of range")
	ErrMissingSPS    = errors.New("go264/syntax: referenced SPS is absent")
	ErrMissingPPS    = errors.New("go264/syntax: referenced PPS is absent")
	ErrUnexpectedEnd = errors.New("go264/syntax: bitstream ended inside a syntax structure")
)

const (
	ProfileBaseline = 66
	ProfileMain     = 77
	ProfileExtended = 88
	ProfileHigh     = 100
)

const (
	ChromaMonochrome = 0
	Chroma420        = 1
	Chroma422        = 2
	Chroma444        = 3
)

type HRD struct {
	CPBCntMinus1                       uint32
	BitRateScale                       uint8
	CPBSizeScale                       uint8
	BitRateValueMinus1                 []uint32
	CPBSizeValueMinus1                 []uint32
	CBRFlag                            []bool
	InitialCPBRemovalDelayLengthMinus1 uint8
	CPBRemovalDelayLengthMinus1        uint8
	DPBOutputDelayLengthMinus1         uint8
	TimeOffsetLength                   uint8
}

type VUI struct {
	AspectRatioInfoPresent bool
	AspectRatioIDC         uint8
	SarWidth               uint16
	SarHeight              uint16

	OverscanInfoPresent bool
	OverscanAppropriate bool

	VideoSignalTypePresent  bool
	VideoFormat             uint8
	VideoFullRange          bool
	ColourDescPresent       bool
	ColourPrimaries         uint8
	TransferCharacteristics uint8
	MatrixCoefficients      uint8

	ChromaLocInfoPresent      bool
	ChromaSampleLocTypeTop    uint32
	ChromaSampleLocTypeBottom uint32

	TimingInfoPresent bool
	NumUnitsInTick    uint32
	TimeScale         uint32
	FixedFrameRate    bool

	NalHRDPresent bool
	NalHRD        HRD
	VclHRDPresent bool
	VclHRD        HRD
	LowDelayHRD   bool

	PicStructPresent bool

	BitstreamRestriction         bool
	MotionVectorsOverPicBoundary bool
	MaxBytesPerPicDenom          uint32
	MaxBitsPerMBDenom            uint32
	Log2MaxMvLengthHorizontal    uint32
	Log2MaxMvLengthVertical      uint32
	MaxNumReorderFrames          uint32
	MaxDecFrameBuffering         uint32
}

type SPS struct {
	ProfileIDC                  uint8
	ConstraintSet               uint8
	LevelIDC                    uint8
	ID                          uint32
	ChromaFormatIDC             uint32
	SeparateColourPlane         bool
	BitDepthLumaMinus8          uint32
	BitDepthChromaMinus8        uint32
	QpprimeYZeroTransformBypass bool
	SeqScalingMatrixPresent     bool
	ScalingList4x4              [6][16]uint8
	ScalingList8x8              [6][64]uint8
	ScalingList4x4Present       [6]bool
	ScalingList8x8Present       [6]bool
	UseDefaultScaling4x4        [6]bool
	UseDefaultScaling8x8        [6]bool

	Log2MaxFrameNumMinus4       uint32
	PicOrderCntType             uint32
	Log2MaxPicOrderCntLsbMinus4 uint32
	DeltaPicOrderAlwaysZero     bool
	OffsetForNonRefPic          int32
	OffsetForTopToBottomField   int32
	OffsetForRefFrame           []int32

	MaxNumRefFrames            uint32
	GapsInFrameNumValueAllowed bool
	PicWidthInMbsMinus1        uint32
	PicHeightInMapUnitsMinus1  uint32
	FrameMbsOnly               bool
	MBAdaptiveFrameField       bool
	Direct8x8Inference         bool
	FrameCropping              bool
	FrameCropLeftOffset        uint32
	FrameCropRightOffset       uint32
	FrameCropTopOffset         uint32
	FrameCropBottomOffset      uint32
	VUIPresent                 bool
	VUI                        VUI
}

func (s *SPS) ChromaArrayType() uint32 {
	if s.SeparateColourPlane {
		return 0
	}
	return s.ChromaFormatIDC
}

func (s *SPS) PicWidthInMbs() int { return int(s.PicWidthInMbsMinus1) + 1 }

func (s *SPS) PicHeightInMapUnits() int { return int(s.PicHeightInMapUnitsMinus1) + 1 }

func (s *SPS) FrameHeightInMbs() int {
	h := s.PicHeightInMapUnits()
	if s.FrameMbsOnly {
		return h
	}
	return 2 * h
}

func (s *SPS) Width() int { return s.PicWidthInMbs() * 16 }

func (s *SPS) Height() int { return s.FrameHeightInMbs() * 16 }

func (s *SPS) cropUnits() (x, y int) {
	if s.ChromaArrayType() == 0 {
		x = 1
		y = 2
		if s.FrameMbsOnly {
			y = 1
		}
		return x, y
	}
	subWidthC := 2
	subHeightC := 2
	switch s.ChromaFormatIDC {
	case Chroma422:
		subHeightC = 1
	case Chroma444:
		subWidthC = 1
		subHeightC = 1
	}
	x = subWidthC
	y = subHeightC
	if !s.FrameMbsOnly {
		y *= 2
	}
	return x, y
}

func (s *SPS) CroppedWidth() int {
	if !s.FrameCropping {
		return s.Width()
	}
	cx, _ := s.cropUnits()
	return s.Width() - cx*int(s.FrameCropLeftOffset+s.FrameCropRightOffset)
}

func (s *SPS) CroppedHeight() int {
	if !s.FrameCropping {
		return s.Height()
	}
	_, cy := s.cropUnits()
	return s.Height() - cy*int(s.FrameCropTopOffset+s.FrameCropBottomOffset)
}

func (s *SPS) CropLeft() int {
	if !s.FrameCropping {
		return 0
	}
	cx, _ := s.cropUnits()
	return cx * int(s.FrameCropLeftOffset)
}

func (s *SPS) CropTop() int {
	if !s.FrameCropping {
		return 0
	}
	_, cy := s.cropUnits()
	return cy * int(s.FrameCropTopOffset)
}

func (s *SPS) MaxFrameNum() uint32 { return 1 << (s.Log2MaxFrameNumMinus4 + 4) }

func (s *SPS) MaxPicOrderCntLsb() uint32 { return 1 << (s.Log2MaxPicOrderCntLsbMinus4 + 4) }

func profileHasChromaFormat(profile uint8) bool {
	switch profile {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		return true
	}
	return false
}

func ParseSPS(rbsp []byte) (*SPS, error) {
	r := bits.NewReader(rbsp)
	s := &SPS{}

	v, err := r.ReadBits(8)
	if err != nil {
		return nil, err
	}
	s.ProfileIDC = uint8(v)
	if v, err = r.ReadBits(8); err != nil {
		return nil, err
	}
	s.ConstraintSet = uint8(v)
	if v, err = r.ReadBits(8); err != nil {
		return nil, err
	}
	s.LevelIDC = uint8(v)
	if s.ID, err = r.ReadUE(); err != nil {
		return nil, err
	}
	if s.ID > 31 {
		return nil, fmt.Errorf("%w: seq_parameter_set_id %d", ErrInvalidValue, s.ID)
	}

	s.ChromaFormatIDC = Chroma420
	if profileHasChromaFormat(s.ProfileIDC) {
		if s.ChromaFormatIDC, err = r.ReadUE(); err != nil {
			return nil, err
		}
		if s.ChromaFormatIDC > 3 {
			return nil, fmt.Errorf("%w: chroma_format_idc %d", ErrInvalidValue, s.ChromaFormatIDC)
		}
		if s.ChromaFormatIDC == Chroma444 {
			if s.SeparateColourPlane, err = r.ReadFlag(); err != nil {
				return nil, err
			}
		}
		if s.BitDepthLumaMinus8, err = r.ReadUE(); err != nil {
			return nil, err
		}
		if s.BitDepthChromaMinus8, err = r.ReadUE(); err != nil {
			return nil, err
		}
		if s.BitDepthLumaMinus8 > 6 || s.BitDepthChromaMinus8 > 6 {
			return nil, fmt.Errorf("%w: bit depth", ErrInvalidValue)
		}
		if s.QpprimeYZeroTransformBypass, err = r.ReadFlag(); err != nil {
			return nil, err
		}
		if s.SeqScalingMatrixPresent, err = r.ReadFlag(); err != nil {
			return nil, err
		}
		if s.SeqScalingMatrixPresent {
			n := 8
			if s.ChromaFormatIDC == Chroma444 {
				n = 12
			}
			for i := 0; i < n; i++ {
				present, err := r.ReadFlag()
				if err != nil {
					return nil, err
				}
				if !present {
					continue
				}
				if i < 6 {
					s.ScalingList4x4Present[i] = true
					if err := readScalingList(r, s.ScalingList4x4[i][:], &s.UseDefaultScaling4x4[i]); err != nil {
						return nil, err
					}
				} else {
					s.ScalingList8x8Present[i-6] = true
					if err := readScalingList(r, s.ScalingList8x8[i-6][:], &s.UseDefaultScaling8x8[i-6]); err != nil {
						return nil, err
					}
				}
			}
		}
	}

	if s.Log2MaxFrameNumMinus4, err = r.ReadUE(); err != nil {
		return nil, err
	}
	if s.Log2MaxFrameNumMinus4 > 12 {
		return nil, fmt.Errorf("%w: log2_max_frame_num_minus4 %d", ErrInvalidValue, s.Log2MaxFrameNumMinus4)
	}
	if s.PicOrderCntType, err = r.ReadUE(); err != nil {
		return nil, err
	}
	switch s.PicOrderCntType {
	case 0:
		if s.Log2MaxPicOrderCntLsbMinus4, err = r.ReadUE(); err != nil {
			return nil, err
		}
		if s.Log2MaxPicOrderCntLsbMinus4 > 12 {
			return nil, fmt.Errorf("%w: log2_max_pic_order_cnt_lsb_minus4 %d", ErrInvalidValue, s.Log2MaxPicOrderCntLsbMinus4)
		}
	case 1:
		if s.DeltaPicOrderAlwaysZero, err = r.ReadFlag(); err != nil {
			return nil, err
		}
		if s.OffsetForNonRefPic, err = r.ReadSE(); err != nil {
			return nil, err
		}
		if s.OffsetForTopToBottomField, err = r.ReadSE(); err != nil {
			return nil, err
		}
		n, err := r.ReadUE()
		if err != nil {
			return nil, err
		}
		if n > 255 {
			return nil, fmt.Errorf("%w: num_ref_frames_in_pic_order_cnt_cycle %d", ErrInvalidValue, n)
		}
		s.OffsetForRefFrame = make([]int32, n)
		for i := range s.OffsetForRefFrame {
			if s.OffsetForRefFrame[i], err = r.ReadSE(); err != nil {
				return nil, err
			}
		}
	case 2:
	default:
		return nil, fmt.Errorf("%w: pic_order_cnt_type %d", ErrInvalidValue, s.PicOrderCntType)
	}

	if s.MaxNumRefFrames, err = r.ReadUE(); err != nil {
		return nil, err
	}
	if s.MaxNumRefFrames > 16 {
		return nil, fmt.Errorf("%w: max_num_ref_frames %d", ErrInvalidValue, s.MaxNumRefFrames)
	}
	if s.GapsInFrameNumValueAllowed, err = r.ReadFlag(); err != nil {
		return nil, err
	}
	if s.PicWidthInMbsMinus1, err = r.ReadUE(); err != nil {
		return nil, err
	}
	if s.PicHeightInMapUnitsMinus1, err = r.ReadUE(); err != nil {
		return nil, err
	}
	if s.PicWidthInMbsMinus1 > 1023 || s.PicHeightInMapUnitsMinus1 > 1023 {
		return nil, fmt.Errorf("%w: picture size in macroblocks", ErrInvalidValue)
	}
	if s.FrameMbsOnly, err = r.ReadFlag(); err != nil {
		return nil, err
	}
	if !s.FrameMbsOnly {
		if s.MBAdaptiveFrameField, err = r.ReadFlag(); err != nil {
			return nil, err
		}
	}
	if s.Direct8x8Inference, err = r.ReadFlag(); err != nil {
		return nil, err
	}
	if s.FrameCropping, err = r.ReadFlag(); err != nil {
		return nil, err
	}
	if s.FrameCropping {
		if s.FrameCropLeftOffset, err = r.ReadUE(); err != nil {
			return nil, err
		}
		if s.FrameCropRightOffset, err = r.ReadUE(); err != nil {
			return nil, err
		}
		if s.FrameCropTopOffset, err = r.ReadUE(); err != nil {
			return nil, err
		}
		if s.FrameCropBottomOffset, err = r.ReadUE(); err != nil {
			return nil, err
		}
		cx, cy := s.cropUnits()
		horizontal := uint64(cx) * (uint64(s.FrameCropLeftOffset) + uint64(s.FrameCropRightOffset))
		vertical := uint64(cy) * (uint64(s.FrameCropTopOffset) + uint64(s.FrameCropBottomOffset))
		if horizontal >= uint64(s.Width()) || vertical >= uint64(s.Height()) {
			return nil, fmt.Errorf("%w: frame cropping removes the whole picture", ErrInvalidValue)
		}
	}
	if s.VUIPresent, err = r.ReadFlag(); err != nil {
		return nil, err
	}
	if s.VUIPresent {
		if err := parseVUI(r, &s.VUI); err != nil {
			return nil, err
		}
	}
	if s.CroppedWidth() <= 0 || s.CroppedHeight() <= 0 {
		return nil, fmt.Errorf("%w: cropping removes the whole picture", ErrInvalidValue)
	}
	return s, nil
}

func readScalingList(r *bits.Reader, list []uint8, useDefault *bool) error {
	last := int32(8)
	next := int32(8)
	for i := range list {
		if next != 0 {
			delta, err := r.ReadSE()
			if err != nil {
				return err
			}
			next = (last + delta + 256) % 256
			if i == 0 && next == 0 {
				*useDefault = true
			}
		}
		if next == 0 {
			list[i] = uint8(last)
		} else {
			list[i] = uint8(next)
			last = next
		}
	}
	return nil
}

func writeScalingList(w *bits.Writer, list []uint8, useDefault bool) {
	if useDefault {
		w.WriteSE(-8)
		return
	}
	last := int32(8)
	for _, v := range list {
		delta := int32(v) - last
		if delta > 127 {
			delta -= 256
		} else if delta < -128 {
			delta += 256
		}
		w.WriteSE(delta)
		last = int32(v)
	}
}

func WriteSPS(s *SPS) ([]byte, error) {
	w := bits.NewWriterSize(64)
	w.WriteBits(uint32(s.ProfileIDC), 8)
	w.WriteBits(uint32(s.ConstraintSet), 8)
	w.WriteBits(uint32(s.LevelIDC), 8)
	w.WriteUE(s.ID)

	if profileHasChromaFormat(s.ProfileIDC) {
		w.WriteUE(s.ChromaFormatIDC)
		if s.ChromaFormatIDC == Chroma444 {
			w.WriteFlag(s.SeparateColourPlane)
		}
		w.WriteUE(s.BitDepthLumaMinus8)
		w.WriteUE(s.BitDepthChromaMinus8)
		w.WriteFlag(s.QpprimeYZeroTransformBypass)
		w.WriteFlag(s.SeqScalingMatrixPresent)
		if s.SeqScalingMatrixPresent {
			n := 8
			if s.ChromaFormatIDC == Chroma444 {
				n = 12
			}
			for i := 0; i < n; i++ {
				if i < 6 {
					w.WriteFlag(s.ScalingList4x4Present[i])
					if s.ScalingList4x4Present[i] {
						writeScalingList(w, s.ScalingList4x4[i][:], s.UseDefaultScaling4x4[i])
					}
				} else {
					w.WriteFlag(s.ScalingList8x8Present[i-6])
					if s.ScalingList8x8Present[i-6] {
						writeScalingList(w, s.ScalingList8x8[i-6][:], s.UseDefaultScaling8x8[i-6])
					}
				}
			}
		}
	}

	w.WriteUE(s.Log2MaxFrameNumMinus4)
	w.WriteUE(s.PicOrderCntType)
	switch s.PicOrderCntType {
	case 0:
		w.WriteUE(s.Log2MaxPicOrderCntLsbMinus4)
	case 1:
		w.WriteFlag(s.DeltaPicOrderAlwaysZero)
		w.WriteSE(s.OffsetForNonRefPic)
		w.WriteSE(s.OffsetForTopToBottomField)
		w.WriteUE(uint32(len(s.OffsetForRefFrame)))
		for _, v := range s.OffsetForRefFrame {
			w.WriteSE(v)
		}
	case 2:
	default:
		return nil, fmt.Errorf("%w: pic_order_cnt_type %d", ErrInvalidValue, s.PicOrderCntType)
	}

	w.WriteUE(s.MaxNumRefFrames)
	w.WriteFlag(s.GapsInFrameNumValueAllowed)
	w.WriteUE(s.PicWidthInMbsMinus1)
	w.WriteUE(s.PicHeightInMapUnitsMinus1)
	w.WriteFlag(s.FrameMbsOnly)
	if !s.FrameMbsOnly {
		w.WriteFlag(s.MBAdaptiveFrameField)
	}
	w.WriteFlag(s.Direct8x8Inference)
	w.WriteFlag(s.FrameCropping)
	if s.FrameCropping {
		w.WriteUE(s.FrameCropLeftOffset)
		w.WriteUE(s.FrameCropRightOffset)
		w.WriteUE(s.FrameCropTopOffset)
		w.WriteUE(s.FrameCropBottomOffset)
	}
	w.WriteFlag(s.VUIPresent)
	if s.VUIPresent {
		writeVUI(w, &s.VUI)
	}
	w.WriteRBSPTrailingBits()
	if err := w.Err(); err != nil {
		return nil, err
	}
	return w.Bytes(), nil
}

var maxDpbMbsByLevel = [...]struct {
	levelIDC    uint8
	constraint3 bool
	maxDpbMbs   int
}{
	{10, false, 396},
	{11, true, 396},
	{9, false, 396},
	{11, false, 900},
	{12, false, 2376},
	{13, false, 2376},
	{20, false, 2376},
	{21, false, 4752},
	{22, false, 8100},
	{30, false, 8100},
	{31, false, 18000},
	{32, false, 20480},
	{40, false, 32768},
	{41, false, 32768},
	{42, false, 34816},
	{50, false, 110400},
	{51, false, 184320},
	{52, false, 184320},
	{60, false, 696320},
	{61, false, 696320},
	{62, false, 696320},
}

func (s *SPS) IsLevel1b() bool {
	if s.LevelIDC == 9 {
		return true
	}
	return !profileHasChromaFormat(s.ProfileIDC) && s.LevelIDC == 11 && s.ConstraintSet&0x10 != 0
}

func (s *SPS) maxDpbMbs() int {
	if s.IsLevel1b() {
		return 396
	}
	best := 0
	for _, e := range maxDpbMbsByLevel {
		if e.levelIDC == s.LevelIDC && !e.constraint3 && e.levelIDC != 9 {
			return e.maxDpbMbs
		}
		if e.maxDpbMbs > best {
			best = e.maxDpbMbs
		}
	}
	return best
}

func (s *SPS) MaxDpbFrames() int {
	frameMbs := s.PicWidthInMbs() * s.FrameHeightInMbs()
	if frameMbs <= 0 {
		return 1
	}
	n := s.maxDpbMbs() / frameMbs
	if n > maxBufferedFrames {
		n = maxBufferedFrames
	}
	if n < 1 {
		n = 1
	}
	return n
}

const maxBufferedFrames = 16

func (s *SPS) MaxNumReorder() int {
	if s.VUIPresent && s.VUI.BitstreamRestriction {
		n := int(s.VUI.MaxNumReorderFrames)
		if n > maxBufferedFrames || n < 0 {
			return maxBufferedFrames
		}
		return n
	}
	if s.PicOrderCntType == 2 || s.intraProfile() {
		return 0
	}
	return s.MaxDpbFrames()
}

func (s *SPS) intraProfile() bool {
	if s.ConstraintSet&0x10 == 0 {
		return false
	}
	switch s.ProfileIDC {
	case 44, 86, 100, 110, 122, 244:
		return true
	}
	return false
}
