package go264

import (
	"bytes"
	"errors"
	"fmt"
	"image"

	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/decoder"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/encoder"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/frame"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/hwaccel"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/level"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/nal"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/syntax"
)

var ErrClosed = errors.New("go264: use of a closed codec")

var ErrOverLimit = decoder.ErrOverLimit

var ErrFrameSize = encoder.ErrFrameSize

type Frame struct {
	Y       []byte
	Cb      []byte
	Cr      []byte
	StrideY int
	StrideC int
	Width   int
	Height  int
}

func (f *Frame) I420Size() int { return f.Width * f.Height * 3 / 2 }

func (f *Frame) AppendI420(dst []byte) []byte {
	for y := 0; y < f.Height; y++ {
		dst = append(dst, f.Y[y*f.StrideY:y*f.StrideY+f.Width]...)
	}
	cw, ch := f.Width/2, f.Height/2
	for _, plane := range [][]byte{f.Cb, f.Cr} {
		for y := 0; y < ch; y++ {
			dst = append(dst, plane[y*f.StrideC:y*f.StrideC+cw]...)
		}
	}
	return dst
}

func frameFromPicture(p *frame.Picture) *Frame {
	x, y := p.CropX, p.CropY
	if x < 0 || x >= p.Width {
		x = 0
	}
	if y < 0 || y >= p.Height {
		y = 0
	}
	width, height := p.CropWidth, p.CropHeight
	if width <= 0 || x+width > p.Width {
		width = p.Width - x
	}
	if height <= 0 || y+height > p.Height {
		height = p.Height - y
	}
	return &Frame{
		Y:       p.Y[p.LumaOffset(x, y):],
		Cb:      p.Cb[p.ChromaOffset(x/2, y/2):],
		Cr:      p.Cr[p.ChromaOffset(x/2, y/2):],
		StrideY: p.StrideY,
		StrideC: p.StrideC,
		Width:   width,
		Height:  height,
	}
}

type EncoderConfig struct {
	Width   int
	Height  int
	FPSNum  int
	FPSDen  int
	GOPSize int
	QP      int

	BitrateKbps int
	RefFrames   int
	BFrames     int

	LongTermReferences int

	CABAC         bool
	MotionSearch  MotionSearch
	ModeDecision  ModeDecision
	Slices        int
	Transform8x8  bool
	ScalingMatrix ScalingMatrix
	Trellis       bool
	ForceSoftware bool

	WeightedPrediction WeightedPrediction
	DirectMode         DirectMode

	IntraRefresh        int
	RepeatParameterSets bool

	Deblocking         DeblockMode
	DeblockAlphaOffset int
	DeblockBetaOffset  int

	VBVBufferKbits int
	VBVMaxrateKbps int
	CBR            bool

	RateFactor float64
	QComp      float64
	IPRatio    float64
	PBRatio    float64

	AQMode     AQMode
	AQStrength float64
}

func (c EncoderConfig) needsSoftware() bool {
	return c.Trellis || c.LongTermReferences > 0 ||
		c.BFrames > 0 || c.ScalingMatrix != ScalingMatrixFlat ||
		c.WeightedPrediction != WeightedPredictionOff ||
		c.DirectMode != DirectSpatial ||
		c.IntraRefresh > 0 || c.Deblocking != DeblockingOn ||
		c.DeblockAlphaOffset != 0 || c.DeblockBetaOffset != 0 ||
		c.VBVBufferKbits > 0 || c.VBVMaxrateKbps > 0 || c.CBR ||
		c.RateFactor > 0 || c.QComp != 0 || c.IPRatio != 0 || c.PBRatio != 0 ||
		c.AQMode != AQOff || c.AQStrength != 0
}

type AQMode = encoder.AQMode

const (
	AQOff      = encoder.AQOff
	AQVariance = encoder.AQVariance
)

type DeblockMode = encoder.DeblockMode

const (
	DeblockingOn              = encoder.DeblockingOn
	DeblockingOff             = encoder.DeblockingOff
	DeblockingNotAcrossSlices = encoder.DeblockingNotAcrossSlices
)

type ScalingMatrix = encoder.ScalingMatrix

const (
	ScalingMatrixFlat = encoder.ScalingMatrixFlat
	ScalingMatrixJVT  = encoder.ScalingMatrixJVT
)

type MotionSearch = encoder.MotionSearch

const (
	MotionSearchFull = encoder.MotionSearchFull
	MotionSearchZero = encoder.MotionSearchZero

	MotionSearchHalf    = encoder.MotionSearchHalf
	MotionSearchInteger = encoder.MotionSearchInteger
)

type WeightedPrediction = encoder.WeightedPrediction

const (
	WeightedPredictionOff      = encoder.WeightedPredictionOff
	WeightedPredictionExplicit = encoder.WeightedPredictionExplicit
	WeightedPredictionImplicit = encoder.WeightedPredictionImplicit
)

type DirectMode = encoder.DirectMode

const (
	DirectSpatial  = encoder.DirectSpatial
	DirectTemporal = encoder.DirectTemporal
)

type ModeDecision = encoder.ModeDecision

const (
	ModeDecisionFast       = encoder.ModeDecisionFast
	ModeDecisionExhaustive = encoder.ModeDecisionExhaustive
)

type RegionKind = encoder.RegionKind

const (
	RegionUnknown = encoder.RegionUnknown
	RegionFill    = encoder.RegionFill
	RegionText    = encoder.RegionText
	RegionImage   = encoder.RegionImage
)

type Region = encoder.Region

type Encoder struct {
	cpu     *encoder.Encoder
	hw      hwaccel.Encoder
	backend string
	closed  bool
	width   int
	height  int

	sps []byte
	pps []byte
}

func (e *Encoder) checkI420(i420 []byte) error {
	if want := e.width * e.height * 3 / 2; len(i420) != want {
		return fmt.Errorf("%w: got %d bytes, want %d", ErrFrameSize, len(i420), want)
	}
	return nil
}

func (e *Encoder) checkFrame(f *Frame) error {
	if f == nil {
		return errors.New("go264: EncodeFrame was given no frame")
	}
	if f.Width != e.width || f.Height != e.height {
		return fmt.Errorf("%w: got %dx%d, want %dx%d", ErrFrameSize, f.Width, f.Height, e.width, e.height)
	}
	cw, ch := f.Width/2, f.Height/2
	if f.StrideY < f.Width || f.StrideC < cw {
		return fmt.Errorf("%w: strides %d and %d are narrower than the picture", ErrFrameSize, f.StrideY, f.StrideC)
	}
	if len(f.Y) < (f.Height-1)*f.StrideY+f.Width ||
		len(f.Cb) < (ch-1)*f.StrideC+cw || len(f.Cr) < (ch-1)*f.StrideC+cw {
		return fmt.Errorf("%w: a plane is shorter than its stride and height need", ErrFrameSize)
	}
	return nil
}

func NewEncoder(cfg EncoderConfig) (*Encoder, error) {
	e := &Encoder{backend: "cpu", width: cfg.Width, height: cfg.Height}
	if !cfg.ForceSoftware && !cfg.needsSoftware() {
		if hw, name, ok := hwaccel.OpenEncoder(hwaccel.EncoderParams{
			Width:   cfg.Width,
			Height:  cfg.Height,
			FPSNum:  cfg.FPSNum,
			FPSDen:  cfg.FPSDen,
			GOPSize: cfg.GOPSize,
			QP:      cfg.QP,

			BitrateKbps: cfg.BitrateKbps,
		}); ok {
			e.hw = hw
			e.backend = name
			return e, nil
		}
	}
	cpu, err := encoder.New(encoder.Config{
		Width:         cfg.Width,
		Height:        cfg.Height,
		FPSNum:        cfg.FPSNum,
		FPSDen:        cfg.FPSDen,
		GOPSize:       cfg.GOPSize,
		QP:            cfg.QP,
		BitrateKbps:   cfg.BitrateKbps,
		RefFrames:     cfg.RefFrames,
		BFrames:       cfg.BFrames,
		CABAC:         cfg.CABAC,
		MotionSearch:  cfg.MotionSearch,
		ModeDecision:  cfg.ModeDecision,
		Slices:        cfg.Slices,
		Transform8x8:  cfg.Transform8x8,
		ScalingMatrix: cfg.ScalingMatrix,
		Trellis:       cfg.Trellis,

		LongTermReferences: cfg.LongTermReferences,

		WeightedPrediction: cfg.WeightedPrediction,
		DirectMode:         cfg.DirectMode,

		IntraRefresh:        cfg.IntraRefresh,
		RepeatParameterSets: cfg.RepeatParameterSets,

		Deblocking:         cfg.Deblocking,
		DeblockAlphaOffset: cfg.DeblockAlphaOffset,
		DeblockBetaOffset:  cfg.DeblockBetaOffset,

		VBVBufferKbits: cfg.VBVBufferKbits,
		VBVMaxrateKbps: cfg.VBVMaxrateKbps,
		CBR:            cfg.CBR,

		RateFactor: cfg.RateFactor,
		QComp:      cfg.QComp,
		IPRatio:    cfg.IPRatio,
		PBRatio:    cfg.PBRatio,

		AQMode:     cfg.AQMode,
		AQStrength: cfg.AQStrength,
	})
	if err != nil {
		return nil, err
	}
	e.cpu = cpu
	return e, nil
}

func (e *Encoder) Backend() string { return e.backend }

func (e *Encoder) Encode(i420 []byte) ([]byte, error) {
	if e.closed {
		return nil, ErrClosed
	}
	if e.hw != nil {
		if err := e.checkI420(i420); err != nil {
			return nil, err
		}
		return e.fromHardware(e.hw.Encode(i420))
	}
	return e.cpu.Encode(i420)
}

func (e *Encoder) EncodeWithHints(i420 []byte, changed []image.Rectangle, regions []Region) ([]byte, error) {
	if e.closed {
		return nil, ErrClosed
	}
	if e.hw != nil {
		if err := e.checkI420(i420); err != nil {
			return nil, err
		}
		return e.fromHardware(e.hw.Encode(i420))
	}
	return e.cpu.EncodeWithHints(i420, encoder.Hints{Changed: changed, Regions: regions})
}

func (e *Encoder) EncodeFrame(f *Frame, changed []image.Rectangle, regions []Region) ([]byte, error) {
	if e.closed {
		return nil, ErrClosed
	}
	if err := e.checkFrame(f); err != nil {
		return nil, err
	}
	yuv := f.AppendI420(make([]byte, 0, f.I420Size()))
	if e.hw != nil {
		return e.fromHardware(e.hw.Encode(yuv))
	}
	return e.cpu.EncodeWithHints(yuv, encoder.Hints{Changed: changed, Regions: regions})
}

func (e *Encoder) fromHardware(pkt []byte, err error) ([]byte, error) {
	if err != nil || len(pkt) == 0 {
		return pkt, err
	}
	var sps, pps []byte
	idr := false
	for _, u := range nal.SplitAnnexB(pkt) {
		if len(u) == 0 {
			continue
		}
		switch nal.Type(u[0] & 0x1f) {
		case nal.TypeSPS:
			sps = u
		case nal.TypePPS:
			pps = u
		case nal.TypeSliceIDR:
			idr = true
		}
	}
	if sps != nil {
		if restricted := withoutReordering(sps); restricted != nil {
			at := cap(pkt) - cap(sps)
			out := make([]byte, 0, len(pkt)+len(restricted)-len(sps))
			out = append(out, pkt[:at]...)
			out = append(out, restricted...)
			pkt = append(out, pkt[at+len(sps):]...)
			sps = restricted
		}
		e.sps = append(e.sps[:0], sps...)
	}
	if pps != nil {
		e.pps = append(e.pps[:0], pps...)
	}
	if !idr || (sps != nil && pps != nil) || e.sps == nil || e.pps == nil {
		return pkt, nil
	}
	out := make([]byte, 0, 8+len(e.sps)+len(e.pps)+len(pkt))
	out = append(out, 0, 0, 0, 1)
	out = append(out, e.sps...)
	out = append(out, 0, 0, 0, 1)
	out = append(out, e.pps...)
	return append(out, pkt...), nil
}

func withoutReordering(unit []byte) []byte {
	u, err := nal.Parse(unit)
	if err != nil || u.Header.Type != nal.TypeSPS {
		return nil
	}
	sps, err := syntax.ParseSPS(u.RBSP)
	if err != nil || sps.PicOrderCntType != 2 || (sps.VUIPresent && sps.VUI.BitstreamRestriction) {
		return nil
	}
	same, err := syntax.WriteSPS(sps)
	if err != nil || !bytes.Equal(same, u.RBSP) {
		return nil
	}
	sps.RestrictBitstream(0)
	rbsp, err := syntax.WriteSPS(sps)
	if err != nil {
		return nil
	}
	return nal.Escape([]byte{u.Header.Byte()}, rbsp)
}

func (e *Encoder) ForceKeyFrame() {
	if e.cpu != nil {
		e.cpu.ForceKeyFrame()
		return
	}
	if f, ok := e.hw.(hwaccel.KeyFrameForcer); ok {
		f.ForceKeyFrame()
	}
}

func (e *Encoder) Flush() ([]byte, error) {
	if e.closed {
		return nil, ErrClosed
	}
	if e.hw != nil {
		return e.fromHardware(e.hw.Drain())
	}
	return e.cpu.Flush()
}

func (e *Encoder) Close() error {
	if e.closed {
		return nil
	}
	e.closed = true
	if e.hw != nil {
		return e.hw.Close()
	}
	return nil
}

type Decoder struct {
	cpu     *decoder.Decoder
	hw      hwaccel.Decoder
	backend string
	closed  bool
	settled bool
	cfg     DecoderConfig
	open    func(hwaccel.DecoderParams) (hwaccel.Decoder, string, bool)
}

type DecoderLimits = decoder.Limits

type DecoderConfig struct {
	ForceSoftware bool

	Width  int
	Height int

	Limits DecoderLimits
}

func NewDecoder() *Decoder { return NewDecoderWithConfig(DecoderConfig{}) }

func NewDecoderWithConfig(cfg DecoderConfig) *Decoder {
	d := &Decoder{backend: "cpu", cfg: cfg, cpu: decoder.New(), open: hwaccel.OpenDecoder}
	d.cpu.SetLimits(cfg.Limits)
	if cfg.ForceSoftware {
		d.settled = true
		return d
	}
	if cfg.Width > 0 && cfg.Height > 0 && d.openHardware(cfg.Width, cfg.Height) {
		d.cpu = nil
	}
	return d
}

func (d *Decoder) openHardware(width, height int) bool {
	d.settled = true
	hw, name, ok := d.open(hwaccel.DecoderParams{Width: width, Height: height})
	if !ok {
		return false
	}
	d.hw = hw
	d.backend = name
	return true
}

func admissible(sps *syntax.SPS, limits DecoderLimits) bool {
	if level.CheckCeiling(sps) != nil {
		return false
	}
	if limits.EnforceLevel && level.CheckSPS(sps) != nil {
		return false
	}
	return limits.MaxFrameMBs <= 0 || sps.PicWidthInMbs()*sps.FrameHeightInMbs() <= limits.MaxFrameMBs
}

func sequenceStart(annexB []byte, limits DecoderLimits) (at, width, height int, idr bool) {
	at = -1
	for _, ebsp := range nal.SplitAnnexB(annexB) {
		typ := nal.Type(ebsp[0] & 0x1f)
		if at >= 0 {
			if typ == nal.TypeSliceIDR {
				return at, width, height, true
			}
			continue
		}
		if typ != nal.TypeSPS {
			continue
		}
		u, err := nal.Parse(ebsp)
		if err != nil {
			continue
		}
		sps, err := syntax.ParseSPS(u.RBSP)
		if err != nil || !admissible(sps, limits) {
			continue
		}
		at = cap(annexB) - cap(ebsp) - 3
		width, height = sps.CroppedWidth(), sps.CroppedHeight()
	}
	return at, width, height, false
}

func (d *Decoder) handOver(annexB []byte, at, width, height int) ([]*Frame, error) {
	started := d.cpu.Started()
	if !d.openHardware(width, height) {
		pics, err := d.cpu.Decode(annexB)
		return d.convert(pics), err
	}
	var out []*Frame
	var errs []error
	if started {
		pics, err := d.cpu.Decode(annexB[:at])
		out = append(out, d.convert(pics)...)
		errs = append(errs, err)
		pics, err = d.cpu.Flush()
		out = append(out, d.convert(pics)...)
		errs = append(errs, err)
		annexB = annexB[at:]
	}
	d.cpu = nil
	pics, err := d.hw.Decode(annexB)
	out = append(out, fromHardware(pics)...)
	return out, errors.Join(append(errs, err)...)
}

func (d *Decoder) Backend() string { return d.backend }

func (d *Decoder) Decode(annexB []byte) ([]*Frame, error) {
	if d.closed {
		return nil, ErrClosed
	}
	if !d.settled {
		if at, w, h, idr := sequenceStart(annexB, d.cfg.Limits); at >= 0 && (idr || !d.cpu.Started()) {
			return d.handOver(annexB, at, w, h)
		}
	}
	if d.hw != nil {
		pics, err := d.hw.Decode(annexB)
		return fromHardware(pics), err
	}
	pics, err := d.cpu.Decode(annexB)
	return d.convert(pics), err
}

func (d *Decoder) Flush() ([]*Frame, error) {
	if d.closed {
		return nil, ErrClosed
	}
	if d.hw != nil {
		pics, err := d.hw.Flush()
		return fromHardware(pics), err
	}
	pics, err := d.cpu.Flush()
	return d.convert(pics), err
}

func (d *Decoder) convert(pics []*frame.Picture) []*Frame {
	if len(pics) == 0 {
		return nil
	}
	out := make([]*Frame, 0, len(pics))
	for _, p := range pics {
		out = append(out, frameFromPicture(p))
	}
	return out
}

func (d *Decoder) Close() error {
	if d.closed {
		return nil
	}
	d.closed = true
	if d.hw != nil {
		return d.hw.Close()
	}
	return nil
}

func Backends() []string {
	out := []string{"cpu"}
	out = append(out, hwaccel.Available()...)
	return out
}

func fromHardware(pics []*hwaccel.Picture) []*Frame {
	if len(pics) == 0 {
		return nil
	}
	out := make([]*Frame, 0, len(pics))
	for _, p := range pics {
		out = append(out, &Frame{
			Y: p.Y, Cb: p.Cb, Cr: p.Cr,
			StrideY: p.StrideY, StrideC: p.StrideC,
			Width: p.Width, Height: p.Height,
		})
	}
	return out
}
