package level

import (
	"errors"
	"fmt"
	"slices"

	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/syntax"
)

var (
	ErrNoLevel      = errors.New("go264/level: no level carries the stream")
	ErrUnknownLevel = errors.New("go264/level: level_idc is not a level in Table A-1")
	ErrExceedsLevel = errors.New("go264/level: sequence parameter set exceeds the level it declares")
	ErrAboveTable   = errors.New("go264/level: sequence parameter set is larger than any level in Table A-1")
)

type Limits struct {
	IDC       uint8
	MaxMBPS   int
	MaxFS     int
	MaxDpbMbs int
	MaxBR     int
	MaxCPB    int
}

var table = []Limits{
	{10, 1485, 99, 396, 64, 175},
	{11, 3000, 396, 900, 192, 500},
	{12, 6000, 396, 2376, 384, 1000},
	{13, 11880, 396, 2376, 768, 2000},
	{20, 11880, 396, 2376, 2000, 2000},
	{21, 19800, 792, 4752, 4000, 4000},
	{22, 20250, 1620, 8100, 4000, 4000},
	{30, 40500, 1620, 8100, 10000, 10000},
	{31, 108000, 3600, 18000, 14000, 14000},
	{32, 216000, 5120, 20480, 20000, 20000},
	{40, 245760, 8192, 32768, 20000, 25000},
	{41, 245760, 8192, 32768, 50000, 62500},
	{42, 522240, 8704, 34816, 50000, 62500},
	{50, 589824, 22080, 110400, 135000, 135000},
	{51, 983040, 36864, 184320, 240000, 240000},
	{52, 2073600, 36864, 184320, 240000, 240000},
	{60, 4177920, 139264, 696320, 240000, 240000},
	{61, 8355840, 139264, 696320, 480000, 480000},
	{62, 16711680, 139264, 696320, 800000, 800000},
}

func Table() []Limits { return slices.Clone(table) }

func Lookup(idc uint8) (Limits, bool) {
	for _, l := range table {
		if l.IDC == idc {
			return l, true
		}
	}
	return Limits{}, false
}

const IDCLevel1bAlias = 9

var level1b = Limits{IDC: 11, MaxMBPS: 1485, MaxFS: 99, MaxDpbMbs: 396, MaxBR: 128, MaxCPB: 350}

func Level1b() Limits { return level1b }

func Declared(s *syntax.SPS) (Limits, bool) {
	if s.IsLevel1b() {
		return level1b, true
	}
	return Lookup(s.LevelIDC)
}

func (l Limits) DpbFrames(frameMBs int) int {
	if frameMBs <= 0 {
		return 0
	}
	n := l.MaxDpbMbs / frameMBs
	if n > 16 {
		n = 16
	}
	return n
}

func MaxFrameSizeMBs() int {
	n := 0
	for _, l := range table {
		if l.MaxFS > n {
			n = l.MaxFS
		}
	}
	return n
}

func checkGeometry(s *syntax.SPS, maxFS int, kind error, where string) error {
	widthMBs := s.PicWidthInMbs()
	heightMBs := s.FrameHeightInMbs()
	if frameMBs := widthMBs * heightMBs; frameMBs > maxFS {
		return fmt.Errorf("%w: %d macroblocks per frame, %s allows %d", kind, frameMBs, where, maxFS)
	}
	if widthMBs*widthMBs > 8*maxFS {
		return fmt.Errorf("%w: %d macroblocks wide, %s allows %d", kind, widthMBs, where, isqrt(8*maxFS))
	}
	if heightMBs*heightMBs > 8*maxFS {
		return fmt.Errorf("%w: %d macroblocks high, %s allows %d", kind, heightMBs, where, isqrt(8*maxFS))
	}
	return nil
}

func CheckCeiling(s *syntax.SPS) error {
	return checkGeometry(s, MaxFrameSizeMBs(), ErrAboveTable, "the highest level in Table A-1")
}

func CheckSPS(s *syntax.SPS) error {
	l, ok := Declared(s)
	if !ok {
		return fmt.Errorf("%w: level_idc %d", ErrUnknownLevel, s.LevelIDC)
	}
	where := fmt.Sprintf("level %d", s.LevelIDC)
	if err := checkGeometry(s, l.MaxFS, ErrExceedsLevel, where); err != nil {
		return err
	}
	frameMBs := s.PicWidthInMbs() * s.FrameHeightInMbs()
	if dpb := l.DpbFrames(frameMBs); int(s.MaxNumRefFrames) > dpb {
		return fmt.Errorf("%w: max_num_ref_frames %d, %s allows %d at this picture size",
			ErrExceedsLevel, s.MaxNumRefFrames, where, dpb)
	}
	return nil
}

func isqrt(v int) int {
	n := 0
	for (n+1)*(n+1) <= v {
		n++
	}
	return n
}

func cpbBrNalFactor(profileIDC uint8) int64 {
	if profileIDC == syntax.ProfileHigh {
		return 1500
	}
	return 1200
}

func (l Limits) MaxBitsPerSecond(profileIDC uint8) int64 {
	return int64(l.MaxBR) * cpbBrNalFactor(profileIDC)
}

func (l Limits) MaxBufferBits(profileIDC uint8) int64 {
	return int64(l.MaxCPB) * cpbBrNalFactor(profileIDC)
}

type Stream struct {
	Width       int
	Height      int
	FPSNum      int
	FPSDen      int
	RefFrames   int
	PeakKbps    int
	BufferKbits int
	ProfileIDC  uint8
}

func (s Stream) MacroblockWidth() int { return (s.Width + 15) / 16 }

func (s Stream) MacroblockHeight() int { return (s.Height + 15) / 16 }

func Select(s Stream) (uint8, error) {
	if s.Width <= 0 || s.Height <= 0 || s.FPSNum <= 0 || s.FPSDen <= 0 {
		return 0, fmt.Errorf("%w: %dx%d at %d/%d frames per second is not a picture",
			ErrNoLevel, s.Width, s.Height, s.FPSNum, s.FPSDen)
	}
	widthMBs, heightMBs := s.MacroblockWidth(), s.MacroblockHeight()
	frameMBs := widthMBs * heightMBs
	rate := frameMBs * s.FPSNum / s.FPSDen
	bitrate := int64(s.PeakKbps) * 1000
	buffer := int64(s.BufferKbits) * 1000
	for _, l := range table {
		if frameMBs > l.MaxFS || rate > l.MaxMBPS {
			continue
		}
		if widthMBs*widthMBs > 8*l.MaxFS || heightMBs*heightMBs > 8*l.MaxFS {
			continue
		}
		dpbFrames := l.MaxDpbMbs / frameMBs
		if dpbFrames > 16 {
			dpbFrames = 16
		}
		if s.RefFrames > dpbFrames {
			continue
		}
		if bitrate > l.MaxBitsPerSecond(s.ProfileIDC) {
			continue
		}
		if buffer > l.MaxBufferBits(s.ProfileIDC) {
			continue
		}
		return l.IDC, nil
	}
	top := table[len(table)-1]
	return 0, fmt.Errorf("%w: %dx%d at %d/%d frames per second, %d reference frames, %d kbit/s and a %d kbit buffer; "+
		"the highest level allows %d macroblocks, %d macroblocks per second, %d kbit/s and %d kbit",
		ErrNoLevel, s.Width, s.Height, s.FPSNum, s.FPSDen, s.RefFrames, s.PeakKbps, s.BufferKbits,
		top.MaxFS, top.MaxMBPS, top.MaxBitsPerSecond(s.ProfileIDC)/1000, top.MaxBufferBits(s.ProfileIDC)/1000)
}
