package decoder

import (
	"fmt"
	"reflect"

	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/bits"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/frame"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/level"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/loopfilter"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/nal"
	"github.com/cloudburrow/cloudburrow/third_party/go264/internal/syntax"
)

type Decoder struct {
	spsMap map[uint32]*syntax.SPS
	ppsMap map[uint32]*syntax.PPS

	scanner *nal.Scanner
	grid    *mbGrid
	cur     *frame.Picture
	sps     *syntax.SPS
	buffer  *dpb
	lastHdr *syntax.SliceHeader

	pending    []*frame.Picture
	maxReorder int

	scal    *scalingTables
	scalSPS *syntax.SPS
	scalPPS *syntax.PPS

	sliceCount int
	limits     Limits
}

type Limits struct {
	MaxFrameMBs  int
	MaxNALBytes  int
	EnforceLevel bool
	// AboveTable (CloudBurrow, #1651) decodes frames larger than any level
	// of Table A-1 allows, up to MaxFrameMBs, as ffmpeg does.
	AboveTable bool
}

func New() *Decoder {
	return &Decoder{
		spsMap:  make(map[uint32]*syntax.SPS),
		ppsMap:  make(map[uint32]*syntax.PPS),
		scanner: nal.NewScanner(),
	}
}

func (d *Decoder) SetLimits(l Limits) { d.limits = l }

func (d *Decoder) Started() bool { return d.buffer != nil || d.cur != nil || len(d.pending) > 0 }

func (d *Decoder) Limits() Limits { return d.limits }

func (d *Decoder) SPS(id uint32) *syntax.SPS { return d.spsMap[id] }

func (d *Decoder) PPS(id uint32) *syntax.PPS { return d.ppsMap[id] }

func (d *Decoder) Decode(data []byte) ([]*frame.Picture, error) {
	d.scanner.Append(data)
	var out []*frame.Picture
	for {
		unit, ok := d.scanner.Next()
		if !ok {
			break
		}
		pics, err := d.handleUnit(unit)
		out = append(out, pics...)
		if err != nil {
			return out, err
		}
	}
	if d.limits.MaxNALBytes > 0 && d.scanner.Buffered() > d.limits.MaxNALBytes {
		return out, fmt.Errorf("%w: %d bytes buffered without a NAL unit boundary, the caller allows %d",
			ErrOverLimit, d.scanner.Buffered(), d.limits.MaxNALBytes)
	}
	return out, nil
}

func (d *Decoder) Flush() ([]*frame.Picture, error) {
	var out []*frame.Picture
	var err error
	if unit, ok := d.scanner.Flush(); ok {
		var pics []*frame.Picture
		pics, err = d.handleUnit(unit)
		out = append(out, pics...)
	}
	out = append(out, d.finishPicture()...)
	out = append(out, d.bump(true)...)
	return out, err
}

func minPOCIndex(pics []*frame.Picture) int {
	best := 0
	for i, p := range pics {
		if p.POC < pics[best].POC {
			best = i
		}
	}
	return best
}

func (d *Decoder) bump(drain bool) []*frame.Picture {
	var out []*frame.Picture
	for len(d.pending) > 0 && (drain || len(d.pending) > d.maxReorder) {
		i := minPOCIndex(d.pending)
		out = append(out, d.pending[i])
		d.pending = append(d.pending[:i], d.pending[i+1:]...)
	}
	return out
}

func (d *Decoder) finishPicture() []*frame.Picture {
	if d.cur == nil {
		return nil
	}
	pic := d.cur
	d.cur = nil
	if d.grid != nil {
		d.conceal(pic)
		loopfilter.Apply(pic, d.grid.widthMBs, d.grid.heightMBs, func(mbx, mby int) *loopfilter.MB {
			m := d.grid.at(mbx, mby)
			if m == nil {
				return nil
			}
			return &m.MB
		})
	}
	pic.ExtendBorders()
	if d.grid != nil {
		pic.Motion = d.grid.motion()
	}
	var out []*frame.Picture
	if d.buffer != nil && d.lastHdr != nil && d.buffer.store(pic, d.lastHdr) {
		// Memory management control operation 5 ends the coded video sequence
		// as far as output is concerned, as an IDR picture does (Annex C.4.4):
		// the pictures before it all come out first, and its own order count
		// restarts at 0, below the ones they carried.
		out = d.bump(true)
		pic.POC = 0
	}
	d.pending = append(d.pending, pic)
	return append(out, d.bump(false)...)
}

func (d *Decoder) handleUnit(ebsp []byte) ([]*frame.Picture, error) {
	u, err := nal.Parse(ebsp)
	if err != nil {
		return nil, err
	}
	switch u.Type {
	case nal.TypeSPS:
		sps, err := syntax.ParseSPS(u.RBSP)
		if err != nil {
			return nil, err
		}
		if err := d.checkSupported(sps); err != nil {
			return nil, err
		}
		if old, ok := d.spsMap[sps.ID]; ok && reflect.DeepEqual(old, sps) {
			return nil, nil
		}
		d.spsMap[sps.ID] = sps
		return nil, nil

	case nal.TypePPS:
		pps, err := syntax.ParsePPS(u.RBSP, d.SPS)
		if err != nil {
			return nil, err
		}
		if old, ok := d.ppsMap[pps.ID]; ok && reflect.DeepEqual(old, pps) {
			return nil, nil
		}
		d.ppsMap[pps.ID] = pps
		return nil, nil

	case nal.TypeSliceIDR, nal.TypeSliceNonIDR:
		return d.decodeSlice(u)
	}
	return nil, nil
}

func (d *Decoder) checkSupported(sps *syntax.SPS) error {
	if sps.ChromaArrayType() != syntax.Chroma420 {
		return fmt.Errorf("%w: chroma format %d", ErrUnsupported, sps.ChromaFormatIDC)
	}
	if sps.BitDepthLumaMinus8 != 0 || sps.BitDepthChromaMinus8 != 0 {
		return fmt.Errorf("%w: bit depth above 8", ErrUnsupported)
	}
	if !sps.FrameMbsOnly {
		return fmt.Errorf("%w: field or MBAFF coding", ErrUnsupported)
	}
	if sps.QpprimeYZeroTransformBypass {
		return fmt.Errorf("%w: lossless transform bypass", ErrUnsupported)
	}
	if err := level.CheckCeiling(sps); err != nil && !(d.limits.AboveTable && d.limits.MaxFrameMBs > 0) {
		return fmt.Errorf("%w: %s", ErrOverLimit, err)
	}
	if d.limits.EnforceLevel {
		if err := level.CheckSPS(sps); err != nil {
			return fmt.Errorf("%w: %s", ErrOverLimit, err)
		}
	}
	if d.limits.MaxFrameMBs > 0 {
		if n := sps.PicWidthInMbs() * sps.FrameHeightInMbs(); n > d.limits.MaxFrameMBs {
			return fmt.Errorf("%w: %d macroblocks per frame, the caller allows %d",
				ErrOverLimit, n, d.limits.MaxFrameMBs)
		}
	}
	return nil
}

func (d *Decoder) scalingFor(sps *syntax.SPS, pps *syntax.PPS) *scalingTables {
	if d.scalSPS == sps && d.scalPPS == pps && d.scal != nil {
		return d.scal
	}
	d.scal = buildScalingTables(sps, pps)
	d.scalSPS = sps
	d.scalPPS = pps
	return d.scal
}

func (d *Decoder) startsPicture(hdr *syntax.SliceHeader, sps *syntax.SPS) bool {
	prev := d.lastHdr
	if d.cur == nil || prev == nil || d.sps != sps {
		return true
	}
	if hdr.FirstMBInSlice == 0 {
		if m := d.grid.at(0, 0); m != nil && m.Decoded {
			return true
		}
	}
	switch {
	case hdr.FrameNum != prev.FrameNum,
		hdr.PPSID != prev.PPSID,
		(hdr.NalRefIDC == 0) != (prev.NalRefIDC == 0),
		hdr.IDR != prev.IDR,
		hdr.IDR && hdr.IDRPicID != prev.IDRPicID:
		return true
	}
	switch sps.PicOrderCntType {
	case 0:
		return hdr.PicOrderCntLsb != prev.PicOrderCntLsb ||
			hdr.DeltaPicOrderCntBottom != prev.DeltaPicOrderCntBottom
	case 1:
		return hdr.DeltaPicOrderCnt != prev.DeltaPicOrderCnt
	}
	return false
}

func (d *Decoder) conceal(pic *frame.Picture) {
	var src *frame.Picture
	if d.buffer != nil {
		for i := len(d.buffer.refs) - 1; i >= 0; i-- {
			if r := d.buffer.refs[i]; !r.nonExisting && r.pic != pic &&
				r.pic.Width == pic.Width && r.pic.Height == pic.Height {
				src = r.pic
				break
			}
		}
	}
	for mby := 0; mby < d.grid.heightMBs; mby++ {
		for mbx := 0; mbx < d.grid.widthMBs; mbx++ {
			if d.grid.at(mbx, mby).Decoded {
				continue
			}
			concealMB(pic, src, mbx, mby)
		}
	}
}

func concealMB(dst, src *frame.Picture, mbx, mby int) {
	for y := 0; y < 16; y++ {
		at := dst.LumaOffset(mbx*16, mby*16+y)
		row := dst.Y[at : at+16]
		if src != nil {
			copy(row, src.Y[at:at+16])
		} else {
			for i := range row {
				row[i] = 128
			}
		}
	}
	for y := 0; y < 8; y++ {
		at := dst.ChromaOffset(mbx*8, mby*8+y)
		for _, planes := range [2][2][]byte{{dst.Cb, srcPlane(src, true)}, {dst.Cr, srcPlane(src, false)}} {
			row := planes[0][at : at+8]
			if planes[1] != nil {
				copy(row, planes[1][at:at+8])
			} else {
				for i := range row {
					row[i] = 128
				}
			}
		}
	}
}

func srcPlane(p *frame.Picture, cb bool) []byte {
	switch {
	case p == nil:
		return nil
	case cb:
		return p.Cb
	}
	return p.Cr
}

func (d *Decoder) decodeSlice(u nal.Unit) ([]*frame.Picture, error) {
	r := bits.NewReader(u.RBSP)
	hdr, sps, pps, err := syntax.ParseSliceHeader(r, u.Header, d)
	if err != nil {
		return nil, err
	}
	if hdr.RedundantPicCnt > 0 {
		return nil, nil
	}
	weighted := pps.WeightedPred && (hdr.SliceType.IsP() || hdr.SliceType.IsSP())
	var out []*frame.Picture
	if d.startsPicture(hdr, sps) {
		out = append(out, d.finishPicture()...)
		if hdr.IDR {
			out = append(out, d.bump(true)...)
		}
		d.maxReorder = sps.MaxNumReorder()
		if d.sps != sps || d.buffer == nil {
			d.buffer = newDPB(sps)
		}
		if hdr.IDR {
			d.buffer.refs = d.buffer.refs[:0]
		}
		d.buffer.fillGap(sps, hdr)
		d.sps = sps
		d.cur = frame.NewPicture(sps.PicWidthInMbs(), sps.FrameHeightInMbs())
		d.cur.FrameNum = hdr.FrameNum
		d.cur.IDR = hdr.IDR
		d.cur.CropX = sps.CropLeft()
		d.cur.CropY = sps.CropTop()
		d.cur.CropWidth = sps.CroppedWidth()
		d.cur.CropHeight = sps.CroppedHeight()
		d.cur.POC = d.buffer.computePOC(sps, hdr)
		d.grid = d.freshGrid(sps.PicWidthInMbs(), sps.FrameHeightInMbs())
		d.sliceCount = 0
	}
	d.sliceCount++
	d.lastHdr = hdr

	d.buffer.updatePicNums(hdr.FrameNum)
	active := hdr.NumRefIdxL0Active(pps)
	activeL1 := hdr.NumRefIdxL1Active(pps)
	var refList, refListL1 []*frame.Picture
	switch {
	case hdr.SliceType.IsP() || hdr.SliceType.IsSP():
		refList = d.buffer.buildListP(hdr, active)
		if len(refList) == 0 {
			return out, fmt.Errorf("%w: P slice with an empty reference list", ErrCorrupt)
		}
	case hdr.SliceType.IsB():
		refList, refListL1 = d.buffer.buildListsB(hdr, d.cur.POC, active, activeL1)
		if len(refList) == 0 || len(refListL1) == 0 {
			return out, fmt.Errorf("%w: B slice with an empty reference list", ErrCorrupt)
		}
	}

	sd := &sliceDecoder{
		r:               r,
		sps:             sps,
		pps:             pps,
		hdr:             hdr,
		pic:             d.cur,
		grid:            d.grid,
		refList:         refList,
		refListL1:       refListL1,
		numRefIdxActive: active,
		numRefIdxL1:     activeL1,
		scal:            d.scalingFor(sps, pps),
		directSpatial:   hdr.DirectSpatialMvPred,
		direct8x8:       sps.Direct8x8Inference,
		sliceID:         d.sliceCount,
	}
	if len(refListL1) != 0 {
		sd.colocated = refListL1[0]
		sd.colShortTerm = !refListL1[0].LongTerm
	}
	switch {
	case weighted:
		sd.weights = &hdr.PredWeight
		sd.weightMode = weightExplicit
	case hdr.SliceType.IsB() && pps.WeightedBipredIDC == 1:
		sd.weights = &hdr.PredWeight
		sd.weightMode = weightExplicit
	case hdr.SliceType.IsB() && pps.WeightedBipredIDC == 2:
		sd.weightMode = weightImplicit
	}
	if err := sd.run(); err != nil {
		return out, err
	}
	return out, nil
}
