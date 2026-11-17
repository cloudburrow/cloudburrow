package embedding

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/gen2brain/h265/hevc"
	go264 "github.com/oops1/go.264"
)

// Video for EmbeddingGemma 2 (#1722), as Google documents it for
// gemini-embedding-2's EmbedContent: an MP4 or MOV container, sampled at
// 1 frame a second unless video_metadata.fps says otherwise, between
// video_metadata's start_offset and end_offset, up to 120 frames. Each frame
// is taken as a browser shows the video at that time (the frame whose
// presentation time is the latest at or before it, after the track's edit
// list), as Transformers.js's load_video samples it for the model card's
// example, and goes to the vision encoder as EmbeddingGemma2VideoProcessor
// prepares it.
//
// The container is read with github.com/Eyevinn/mp4ff (MIT). H.264 is
// decoded with github.com/oops1/go.264 (BSD 2-Clause; High profile,
// progressive, 8-bit 4:2:0) and H.265 with github.com/gen2brain/h265 (MIT),
// both pure Go. VP9 and AV1, which Google also takes, are refused: the only
// complete pure-Go VP9 decoder (github.com/thesyncim/govpx) needs Go 1.27,
// and no complete AV1 decoder without cgo exists (docs/upstream-evaluation.md).

// VideoMIMETypes are the video types decoded here.
var VideoMIMETypes = []string{"video/mp4", "video/quicktime"}

var videoMIMETypes = map[string]bool{"video/mp4": true, "video/quicktime": true, "video/mov": true}

// IsVideoMIMEType reports whether mt is a video type, served or not.
func IsVideoMIMEType(mt string) bool { return strings.HasPrefix(baseMIME(mt), "video/") }

// MaxVideoFrames is the most frames one video may make: Google's documented
// limit for gemini-embedding-2 (120 seconds at the default 1 frame a second).
const MaxVideoFrames = 120

// DefaultVideoFPS is Google's default sampling rate.
const DefaultVideoFPS = 1.0

// maxVideoPixels bounds a video's frame size: 8K UHD.
const maxVideoPixels = 8192 * 4320

// VideoOptions are a part's video_metadata.
type VideoOptions struct {
	// Start and End bound the interval; End zero means the video's end.
	Start, End time.Duration
	// FPS is the sampling rate; zero means DefaultVideoFPS.
	FPS float64
}

// ErrVideoCodec is a codec Google takes that this build cannot decode.
var ErrVideoCodec = errors.New("unsupported video codec")

// videoTrack is the samples of an MP4's video track.
type videoTrack struct {
	codec     string // the sample entry's type: avc1, hvc1, vp09, av01...
	timescale uint32
	// per sample, in decode order
	samples [][]byte
	pts     []int64 // presentation times after the edit list, in timescale units
	sync    []bool
	end     int64 // the presentation end of the last sample
	// codec configuration
	avcC          *avc.DecConfRec
	hvcC          [][]byte // VPS, SPS and PPS NAL units
	nalLengthSize int
}

// DecodeVideo samples the frames of an MP4 or MOV video.
func DecodeVideo(mimeType string, data []byte, opt VideoOptions) ([]image.Image, error) {
	if !videoMIMETypes[baseMIME(mimeType)] {
		return nil, fmt.Errorf("mime_type %q is not supported; send one of %s", mimeType, strings.Join(VideoMIMETypes, ", "))
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("the video data is empty")
	}
	tr, err := readVideoTrack(data)
	if err != nil {
		return nil, err
	}
	targets, err := sampleTimes(tr, opt)
	if err != nil {
		return nil, err
	}
	// The sample shown at each target time.
	order := make([]int, len(tr.pts)) // sample indexes in presentation order
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return tr.pts[order[a]] < tr.pts[order[b]] })
	want := make([]int, len(targets))
	for i, t := range targets {
		k := sort.Search(len(order), func(j int) bool { return tr.pts[order[j]] > t }) - 1
		if k < 0 {
			k = 0
		}
		want[i] = order[k]
	}
	frames, err := decodeSamples(tr, want)
	if err != nil {
		return nil, err
	}
	out := make([]image.Image, len(want))
	for i, s := range want {
		out[i] = frames[s]
	}
	return out, nil
}

// sampleTimes are the presentation times (in tr.timescale units) of the
// frames to take: start, start + 1/fps, ... before end, as load_video takes
// them, and at least one.
func sampleTimes(tr *videoTrack, opt VideoOptions) ([]int64, error) {
	fps := opt.FPS
	if fps == 0 {
		fps = DefaultVideoFPS
	}
	if fps < 0 || math.IsNaN(fps) || math.IsInf(fps, 0) {
		return nil, fmt.Errorf("video_metadata.fps %v must be positive", opt.FPS)
	}
	duration := float64(tr.end) / float64(tr.timescale)
	start, end := opt.Start.Seconds(), duration
	if opt.End > 0 {
		end = math.Min(opt.End.Seconds(), duration)
	}
	if opt.Start < 0 || opt.End < 0 {
		return nil, fmt.Errorf("video_metadata offsets must not be negative")
	}
	if opt.End > 0 && opt.End <= opt.Start {
		return nil, fmt.Errorf("video_metadata.end_offset %v must be after start_offset %v", opt.End, opt.Start)
	}
	if start >= duration {
		return nil, fmt.Errorf("video_metadata.start_offset %v is past the video's end (%.3fs)", opt.Start, duration)
	}
	n := int(math.Floor((end - start) * fps))
	if n > MaxVideoFrames {
		return nil, fmt.Errorf("%.3f seconds at %v frames a second is %d frames; at most %d frames are embedded "+
			"(set video_metadata's end_offset or a lower fps)", end-start, fps, n, MaxVideoFrames)
	}
	if n < 1 {
		n = 1
	}
	out := make([]int64, n)
	for i := range out {
		out[i] = int64(math.Floor((start + float64(i)/fps) * float64(tr.timescale)))
	}
	return out, nil
}

// readVideoTrack reads the first video track of an MP4 or MOV file,
// progressive or fragmented.
func readVideoTrack(data []byte) (tr *videoTrack, err error) {
	defer func() {
		if r := recover(); r != nil {
			tr, err = nil, fmt.Errorf("the data is not a valid MP4 or MOV video: %v", r)
		}
	}()
	f, err := mp4.DecodeFile(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("the data is not a valid MP4 or MOV video: %v", err)
	}
	if f.Moov == nil {
		return nil, fmt.Errorf("the data is not a valid MP4 or MOV video: it has no moov box")
	}
	for _, trak := range f.Moov.Traks {
		if trak.Mdia == nil || trak.Mdia.Hdlr == nil || trak.Mdia.Hdlr.HandlerType != "vide" {
			continue
		}
		return videoTrackOf(f, trak, data)
	}
	return nil, fmt.Errorf("the video has no video track")
}

func videoTrackOf(f *mp4.File, trak *mp4.TrakBox, data []byte) (*videoTrack, error) {
	md := trak.Mdia
	if md.Mdhd == nil || md.Mdhd.Timescale == 0 || md.Minf == nil || md.Minf.Stbl == nil {
		return nil, fmt.Errorf("the video track has no media header or sample table")
	}
	st := md.Minf.Stbl
	if st.Stsd == nil || len(st.Stsd.Children) == 0 {
		return nil, fmt.Errorf("the video track has no sample description")
	}
	tr := &videoTrack{codec: st.Stsd.Children[0].Type(), timescale: md.Mdhd.Timescale}
	switch tr.codec {
	case "avc1", "avc3":
		if st.Stsd.AvcX == nil || st.Stsd.AvcX.AvcC == nil {
			return nil, fmt.Errorf("the H.264 track has no avcC configuration")
		}
		tr.avcC = &st.Stsd.AvcX.AvcC.DecConfRec
		tr.nalLengthSize = 4
	case "hvc1", "hev1":
		if st.Stsd.HvcX == nil || st.Stsd.HvcX.HvcC == nil {
			return nil, fmt.Errorf("the H.265 track has no hvcC configuration")
		}
		rec := st.Stsd.HvcX.HvcC.DecConfRec
		for _, a := range rec.NaluArrays {
			tr.hvcC = append(tr.hvcC, a.Nalus...)
		}
		tr.nalLengthSize = int(rec.LengthSizeMinusOne) + 1
	case "vp09":
		return nil, fmt.Errorf("%w: VP9 has no decoder without cgo that this build's Go version can compile; send H.264 or H.265", ErrVideoCodec)
	case "av01":
		return nil, fmt.Errorf("%w: AV1 has no complete decoder without cgo; send H.264 or H.265", ErrVideoCodec)
	default:
		return nil, fmt.Errorf("%w %q; Google takes H.264, H.265, VP9 and AV1, and this build decodes H.264 and H.265", ErrVideoCodec, tr.codec)
	}

	// The edit list's first media time is where presentation starts.
	var shift int64
	if trak.Edts != nil && len(trak.Edts.Elst) > 0 {
		for _, e := range trak.Edts.Elst[0].Entries {
			if e.MediaTime >= 0 {
				shift = e.MediaTime
				break
			}
		}
	}
	add := func(dts int64, dur uint32, cto int32, sync bool, sample []byte) {
		pts := dts + int64(cto) - shift
		tr.samples = append(tr.samples, sample)
		tr.pts = append(tr.pts, pts)
		tr.sync = append(tr.sync, sync)
		tr.end = max(tr.end, pts+int64(dur))
	}
	if err := progressiveSamples(st, data, add); err != nil {
		return nil, err
	}
	if f.IsFragmented() {
		var trex *mp4.TrexBox
		if f.Moov.Mvex != nil {
			trex, _ = f.Moov.Mvex.GetTrex(trak.Tkhd.TrackID)
		}
		if trex == nil {
			trex = &mp4.TrexBox{TrackID: trak.Tkhd.TrackID}
		}
		for _, seg := range f.Segments {
			for _, frag := range seg.Fragments {
				fs, err := frag.GetFullSamples(trex)
				if err != nil {
					return nil, fmt.Errorf("the video's fragment: %v", err)
				}
				for _, s := range fs {
					add(int64(s.DecodeTime), s.Dur, s.CompositionTimeOffset, s.IsSync(), s.Data)
				}
			}
		}
	}
	if len(tr.samples) == 0 {
		return nil, fmt.Errorf("the video track has no samples")
	}
	if tr.end <= 0 {
		return nil, fmt.Errorf("the video has no duration")
	}
	return tr, nil
}

// progressiveSamples adds the samples of a sample table, which a fragmented
// file's moov leaves empty.
func progressiveSamples(st *mp4.StblBox, data []byte,
	add func(dts int64, dur uint32, cto int32, sync bool, sample []byte)) error {
	if st.Stsz == nil || st.Stsz.GetNrSamples() == 0 {
		return nil
	}
	if st.Stts == nil || st.Stsc == nil || (st.Stco == nil && st.Co64 == nil) {
		return fmt.Errorf("the video track's sample table is incomplete")
	}
	n := int(st.Stsz.GetNrSamples())
	durs, err := st.Stts.SampleDurations(uint32(n))
	if err != nil {
		return fmt.Errorf("the video track's stts: %v", err)
	}
	var ctts []int32
	if st.Ctts != nil {
		if ctts, err = st.Ctts.CompositionTimeOffsets(uint32(n)); err != nil {
			return fmt.Errorf("the video track's ctts: %v", err)
		}
	}
	var dts int64
	for i := 0; i < n; i++ {
		s := i + 1
		size := uint64(st.Stsz.GetSampleSize(s))
		chunk, first, err := st.Stsc.ChunkNrFromSampleNr(s)
		if err != nil {
			return fmt.Errorf("the video track's stsc: %v", err)
		}
		var off uint64
		if st.Stco != nil {
			off, err = st.Stco.GetOffset(chunk)
		} else {
			off, err = st.Co64.GetOffset(chunk)
		}
		if err != nil {
			return fmt.Errorf("the video track's chunk offsets: %v", err)
		}
		if first < s {
			sum, err := st.Stsz.GetTotalSampleSize(uint32(first), uint32(s-1))
			if err != nil {
				return fmt.Errorf("the video track's stsz: %v", err)
			}
			off += sum
		}
		if off > uint64(len(data)) || size > uint64(len(data))-off {
			return fmt.Errorf("the video's sample %d lies outside the file", s)
		}
		var cto int32
		if ctts != nil {
			cto = ctts[i]
		}
		add(dts, durs[i], cto, st.Stss == nil || st.Stss.IsSyncSample(uint32(s)), data[off:off+size])
		dts += int64(durs[i])
	}
	return nil
}

// decodeSamples decodes the pictures of the samples in want, starting each
// at the sync sample before it, and returns them by sample index.
func decodeSamples(tr *videoTrack, want []int) (map[int]image.Image, error) {
	// Group the wanted samples by the sync sample they are decoded from.
	groups := map[int][]int{}
	for _, s := range want {
		// The sync sample before it, in decode order, that is presented no
		// later: a leading picture of an open GOP (H.265's RASL) is decoded
		// from the random access point before its own.
		k := s
		for k > 0 && !(tr.sync[k] && tr.pts[k] <= tr.pts[s]) {
			k--
		}
		groups[k] = append(groups[k], s)
	}
	starts := make([]int, 0, len(groups))
	for k := range groups {
		starts = append(starts, k)
	}
	sort.Ints(starts)
	out := map[int]image.Image{}
	for _, k := range starts {
		last := 0
		for _, s := range groups[k] {
			last = max(last, s)
		}
		// A picture's references precede it in decode order, so decoding
		// to the last wanted sample and flushing outputs every one.
		stop := last + 1
		var (
			pics map[int]image.Image
			err  error
		)
		if tr.avcC != nil {
			pics, err = decodeH264(tr, k, stop)
		} else {
			pics, err = decodeH265(tr, k, stop)
		}
		if err != nil {
			return nil, err
		}
		for _, s := range groups[k] {
			p, ok := pics[s]
			if !ok {
				return nil, fmt.Errorf("the video's frame %d did not decode", s)
			}
			out[s] = p
		}
	}
	return out, nil
}

// nalUnits splits a sample into its length-prefixed NAL units.
func nalUnits(sample []byte, lengthSize int) ([][]byte, error) {
	var out [][]byte
	for len(sample) > 0 {
		if len(sample) < lengthSize {
			return nil, fmt.Errorf("a sample ends inside a NAL unit's length")
		}
		var n uint64
		for _, b := range sample[:lengthSize] {
			n = n<<8 | uint64(b)
		}
		sample = sample[lengthSize:]
		if n > uint64(len(sample)) {
			return nil, fmt.Errorf("a NAL unit runs past its sample")
		}
		out = append(out, sample[:n])
		sample = sample[n:]
	}
	return out, nil
}

// decodeH264 decodes samples from to stop (exclusive) of an H.264 track. The
// decoder hands its pictures back in display order, which is the samples'
// presentation order.
func decodeH264(tr *videoTrack, from, stop int) (pics map[int]image.Image, err error) {
	defer func() {
		if r := recover(); r != nil {
			pics, err = nil, fmt.Errorf("decoding the H.264 video: %v", r)
		}
	}()
	matrix, full := bt601, false
	if len(tr.avcC.SPSnalus) > 0 {
		if sps, err := avc.ParseSPSNALUnit(tr.avcC.SPSnalus[0], true); err == nil {
			if sps.Width*sps.Height > maxVideoPixels {
				return nil, fmt.Errorf("the video is %dx%d; at most %d pixels a frame are decoded", sps.Width, sps.Height, maxVideoPixels)
			}
			matrix = colourMatrix(sps.VUI != nil, vuiMatrix(sps), int(sps.Height))
			full = sps.VUI != nil && sps.VUI.VideoFullRangeFlag
		}
	}
	d := go264.NewDecoderWithConfig(go264.DecoderConfig{ForceSoftware: true,
		Limits: go264.DecoderLimits{MaxFrameMBs: maxVideoPixels / 256}})
	defer d.Close()
	var stream []byte
	start := []byte{0, 0, 0, 1}
	for _, ps := range append(append([][]byte(nil), tr.avcC.SPSnalus...), tr.avcC.PPSnalus...) {
		stream = append(append(stream, start...), ps...)
	}
	var frames []*go264.Frame
	for s := from; s < stop; s++ {
		nals, err := nalUnits(tr.samples[s], tr.nalLengthSize)
		if err != nil {
			return nil, fmt.Errorf("the video's sample %d: %v", s+1, err)
		}
		for _, nal := range nals {
			stream = append(append(stream, start...), nal...)
		}
		fs, err := d.Decode(stream)
		if err != nil {
			return nil, fmt.Errorf("decoding the H.264 video: %v", err)
		}
		frames = append(frames, copyFrames(fs)...)
		stream = stream[:0]
	}
	fs, err := d.Flush()
	if err != nil {
		return nil, fmt.Errorf("decoding the H.264 video: %v", err)
	}
	frames = append(frames, copyFrames(fs)...)
	// The decoded samples in presentation order.
	idx := make([]int, 0, stop-from)
	for s := from; s < stop; s++ {
		idx = append(idx, s)
	}
	sort.SliceStable(idx, func(a, b int) bool { return tr.pts[idx[a]] < tr.pts[idx[b]] })
	if len(frames) != len(idx) {
		return nil, fmt.Errorf("decoding the H.264 video: %d samples made %d pictures", len(idx), len(frames))
	}
	pics = make(map[int]image.Image, len(idx))
	for i, s := range idx {
		f := frames[i]
		pics[s] = yuvToNRGBA(yuvPlanes{
			w: f.Width, h: f.Height, y: f.Y, cb: f.Cb, cr: f.Cr, strideY: f.StrideY, strideC: f.StrideC,
			ssX: 1, ssY: 1, depth: 8,
		}, matrix, full)
	}
	return pics, nil
}

// copyFrames copies go.264's frames, whose planes the decoder reuses.
func copyFrames(fs []*go264.Frame) []*go264.Frame {
	out := make([]*go264.Frame, len(fs))
	for i, f := range fs {
		c := *f
		c.Y = append([]byte(nil), f.Y...)
		c.Cb = append([]byte(nil), f.Cb...)
		c.Cr = append([]byte(nil), f.Cr...)
		out[i] = &c
	}
	return out
}

func vuiMatrix(sps *avc.SPS) int {
	if sps.VUI == nil || !sps.VUI.ColourDescriptionFlag {
		return 2
	}
	return int(sps.VUI.MatrixCoefficients)
}

// decodeH265 decodes samples from to stop (exclusive) of an H.265 track,
// each picture tagged with its sample.
func decodeH265(tr *videoTrack, from, stop int) (pics map[int]image.Image, err error) {
	defer func() {
		if r := recover(); r != nil {
			pics, err = nil, fmt.Errorf("decoding the H.265 video: %v", r)
		}
	}()
	d := hevc.Decoder{}
	d.FrameSizeLimit(maxVideoPixels)
	var out []*hevc.Picture
	feed := func(raw []byte, tag uint64) error {
		nal, ok := hevc.ParseNAL(raw)
		if !ok {
			return fmt.Errorf("a NAL unit does not parse")
		}
		nal.Tag = tag
		ps, err := d.DecodeNAL(nal)
		out = append(out, ps...)
		return err
	}
	for _, ps := range tr.hvcC {
		if err := feed(ps, 0); err != nil {
			return nil, fmt.Errorf("decoding the H.265 video's parameter sets: %v", err)
		}
	}
	for s := from; s < stop; s++ {
		nals, err := nalUnits(tr.samples[s], tr.nalLengthSize)
		if err != nil {
			return nil, fmt.Errorf("the video's sample %d: %v", s+1, err)
		}
		for _, n := range nals {
			if err := feed(n, uint64(s)); err != nil {
				return nil, fmt.Errorf("decoding the H.265 video: %v", err)
			}
		}
	}
	out = append(out, d.Flush()...)
	pics = make(map[int]image.Image, len(out))
	for _, p := range out {
		if p.Corrupt {
			continue
		}
		ssX, ssY := 0, 0
		switch p.ChromaFormat {
		case 1:
			ssX, ssY = 1, 1
		case 2:
			ssX = 1
		case 0:
			return nil, fmt.Errorf("monochrome H.265 video is not supported")
		}
		planes := yuvPlanes{w: p.Width, h: p.Height, strideY: p.StrideY, strideC: p.StrideC,
			ssX: ssX, ssY: ssY, depth: max(p.BitDepth, p.BitDepthC),
			cropX: p.CropX, cropY: p.CropY, cropW: p.CropW, cropH: p.CropH}
		if planes.depth > 8 {
			planes.y16, planes.cb16, planes.cr16 = p.Y16, p.Cb16, p.Cr16
		} else {
			planes.y, planes.cb, planes.cr = p.Y, p.Cb, p.Cr
		}
		pics[int(p.Tag)] = yuvToNRGBA(planes, colourMatrix(p.ColorMatrix != 2, int(p.ColorMatrix), p.CropH), p.FullRange)
	}
	return pics, nil
}

// YUV to RGB, as a browser draws a video frame on a canvas: the matrix the
// stream declares (BT.709 for an undeclared HD stream, BT.601 below 720
// lines), limited or full range, chroma upsampled bilinearly.

type yuvMatrix struct{ kr, kb float64 }

var (
	bt601  = yuvMatrix{0.299, 0.114}
	bt709  = yuvMatrix{0.2126, 0.0722}
	bt2020 = yuvMatrix{0.2627, 0.0593}
)

// colourMatrix maps ISO/IEC 23091-2 matrix_coefficients to a matrix.
func colourMatrix(declared bool, mc, height int) yuvMatrix {
	if declared {
		switch mc {
		case 1:
			return bt709
		case 5, 6:
			return bt601
		case 9, 10:
			return bt2020
		}
	}
	if height >= 720 {
		return bt709
	}
	return bt601
}

type yuvPlanes struct {
	w, h                       int
	y, cb, cr                  []uint8
	y16, cb16, cr16            []uint16
	strideY, strideC           int
	ssX, ssY                   int // chroma subsampling shifts
	depth                      int
	cropX, cropY, cropW, cropH int // zero cropW: no cropping
}

func (p yuvPlanes) luma(x, y int) float64 {
	if p.y16 != nil {
		return float64(p.y16[y*p.strideY+x])
	}
	return float64(p.y[y*p.strideY+x])
}

func (p yuvPlanes) chroma(x, y int) (float64, float64) {
	i := y*p.strideC + x
	if p.cb16 != nil {
		return float64(p.cb16[i]), float64(p.cr16[i])
	}
	return float64(p.cb[i]), float64(p.cr[i])
}

func yuvToNRGBA(p yuvPlanes, m yuvMatrix, full bool) *image.NRGBA {
	x0, y0, w, h := 0, 0, p.w, p.h
	if p.cropW > 0 && p.cropH > 0 {
		x0, y0, w, h = p.cropX, p.cropY, p.cropW, p.cropH
	}
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	maxV := float64(int(1)<<p.depth - 1)
	scale := float64(int(1) << (p.depth - 8))
	yOff, yRange, cRange := 16*scale, 219*scale, 224*scale
	if full {
		yOff, yRange, cRange = 0, maxV, maxV
	}
	half := 128 * scale
	kg := 1 - m.kr - m.kb
	cw := (p.w + (1 << p.ssX) - 1) >> p.ssX
	ch := (p.h + (1 << p.ssY) - 1) >> p.ssY
	for yy := 0; yy < h; yy++ {
		sy := y0 + yy
		// chroma sample position, centred vertically for 4:2:0
		fy := (float64(sy)+0.5)/float64(int(1)<<p.ssY) - 0.5
		cy0 := int(math.Floor(fy))
		wy := fy - float64(cy0)
		cy1 := min(max(cy0+1, 0), ch-1)
		cy0 = min(max(cy0, 0), ch-1)
		row := img.Pix[yy*img.Stride:]
		for xx := 0; xx < w; xx++ {
			sx := x0 + xx
			fx := (float64(sx)+0.5)/float64(int(1)<<p.ssX) - 0.5
			cx0 := int(math.Floor(fx))
			wx := fx - float64(cx0)
			cx1 := min(max(cx0+1, 0), cw-1)
			cx0 = min(max(cx0, 0), cw-1)
			b00, r00 := p.chroma(cx0, cy0)
			b01, r01 := p.chroma(cx1, cy0)
			b10, r10 := p.chroma(cx0, cy1)
			b11, r11 := p.chroma(cx1, cy1)
			cb := (b00*(1-wx)+b01*wx)*(1-wy) + (b10*(1-wx)+b11*wx)*wy
			cr := (r00*(1-wx)+r01*wx)*(1-wy) + (r10*(1-wx)+r11*wx)*wy
			yv := (p.luma(sx, sy) - yOff) / yRange
			u := (cb - half) / cRange
			v := (cr - half) / cRange
			r := yv + 2*(1-m.kr)*v
			b := yv + 2*(1-m.kb)*u
			g := (yv - m.kr*r - m.kb*b) / kg
			o := xx * 4
			row[o] = unit8(r)
			row[o+1] = unit8(g)
			row[o+2] = unit8(b)
			row[o+3] = 0xff
		}
	}
	return img
}

func unit8(v float64) uint8 {
	v = math.Round(v * 255)
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}
