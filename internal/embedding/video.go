package embedding

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/mp4"
	go264 "github.com/cloudburrow/cloudburrow/third_party/go264"
	"github.com/cloudburrow/cloudburrow/third_party/govpx"
	"github.com/cloudburrow/cloudburrow/third_party/govpx/vp9hbd"
	"github.com/gen2brain/gav1d/av1"
	"github.com/gen2brain/h265/hevc"
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
// decoded with github.com/oops1/go.264 (copied into third_party/go264,
// BSD 2-Clause; High profile, progressive, 8-bit 4:2:0) and H.265 with
// github.com/gen2brain/h265 (MIT), VP9 (#1731) with github.com/thesyncim/govpx
// (copied into third_party/govpx, BSD 3-Clause; profile 0, 8-bit 4:2:0;
// profiles 1 to 3 with its vp9hbd, #1739)
// and AV1 (#1731) with github.com/gen2brain/gav1d (BSD 2-Clause, a port of
// dav1d; 8 to 12 bits, every chroma format, film grain), all pure Go and
// bit-exact against ffmpeg here (docs/upstream-evaluation.md).

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

// maxVideoSide bounds a video's frame: 16384 on a side, Google's image
// limit for gemini-embedding-2. Google documents no video frame size, so a
// larger frame is ErrNotDecoded (UNIMPLEMENTED), not invalid. Since #1651's
// fourth round each wanted picture is patched as soon as it is decoded and
// its full-size RGB released (DecodeVideoPatched), so a 16K video holds
// one RGB frame at a time beside the decoder's references, not up to 120.
const maxVideoSide = 16384

// maxVideoPixels is maxVideoSide squared.
const maxVideoPixels = maxVideoSide * maxVideoSide

// frameTooLarge is ErrNotDecoded for a w x h frame over maxVideoSide.
func frameTooLarge(w, h int) error {
	if w <= maxVideoSide && h <= maxVideoSide {
		return nil
	}
	return notDecodedf("the video is %dx%d; frames up to %dx%d are decoded", w, h, maxVideoSide, maxVideoSide)
}

// frameConv converts a wanted sample's picture as it is decoded; the
// identity for DecodeVideo, PatchVideoFrame's patches for DecodeVideoPatched.
type frameConv func(image.Image) (image.Image, error)

// keepFrame stores a wanted sample's converted picture in pics (a later
// picture of the same sample replacing it); mk makes the full-size
// picture, and is called only for a wanted sample.
func keepFrame(pics map[int]image.Image, wanted map[int]bool, conv frameConv, s int, mk func() (image.Image, error)) error {
	if !wanted[s] {
		return nil
	}
	img, err := mk()
	if err != nil {
		return err
	}
	if img, err = conv(img); err != nil {
		return err
	}
	pics[s] = img
	return nil
}

// VideoOptions are a part's video_metadata.
type VideoOptions struct {
	// Start and End bound the interval; End zero means the video's end.
	Start, End time.Duration
	// FPS is the sampling rate; zero means DefaultVideoFPS.
	FPS float64
	// Matroska also takes the Matroska and WebM containers
	// (MatroskaMIMETypes), which multimodalembedding@001 takes and
	// gemini-embedding-2 does not (#1651).
	Matroska bool
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
	vpcC          *mp4.VppCBox // VP9's colour description
	av1Config     []byte       // AV1's configOBUs: the sequence header
}

// DecodeVideo samples the frames of an MP4 or MOV video, each at full
// size; DecodeVideoPatched is the form EmbedContent uses.
func DecodeVideo(mimeType string, data []byte, opt VideoOptions) ([]image.Image, error) {
	return decodeVideo(mimeType, data, opt, false)
}

// PatchedFrame is a sampled video frame already prepared for the vision
// encoder (PatchVideoFrame's patches), its full-size picture released. A
// frame VideoFrames drops carries no patches.
type PatchedFrame struct {
	W, H    int
	patches *ImagePatches
}

func (f *PatchedFrame) ColorModel() color.Model { return color.NRGBAModel }
func (f *PatchedFrame) Bounds() image.Rectangle { return image.Rect(0, 0, f.W, f.H) }

// At is black: a patched frame keeps no pixels.
func (f *PatchedFrame) At(x, y int) color.Color { return color.NRGBA{A: 255} }

// DecodeVideoPatched samples the frames of an MP4 or MOV video as
// DecodeVideo does, but patches each frame VideoFrames keeps as soon as it
// is decoded (exactly PatchVideoFrame of DecodeVideo's frame) and keeps
// only the patches: the frames are PatchedFrames.
func DecodeVideoPatched(mimeType string, data []byte, opt VideoOptions) ([]image.Image, error) {
	return decodeVideo(mimeType, data, opt, true)
}

func decodeVideo(mimeType string, data []byte, opt VideoOptions, patch bool) ([]image.Image, error) {
	tr, err := readVideoOf(mimeType, data, opt.Matroska)
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
	if !patch {
		frames, err := decodeSamples(tr, want, func(img image.Image) (image.Image, error) { return img, nil })
		if err != nil {
			return nil, err
		}
		out := make([]image.Image, len(want))
		for i, s := range want {
			out[i] = frames[s]
		}
		return out, nil
	}
	// Only the frames VideoFrames keeps are converted and patched.
	kept := videoFrameIndexes(len(want))
	keptSamples := make([]int, len(kept))
	for i, k := range kept {
		keptSamples[i] = want[k]
	}
	frames, err := decodeSamples(tr, keptSamples, func(img image.Image) (image.Image, error) {
		ip, err := patchImage(img, videoSoftTokens)
		if err != nil {
			return nil, err
		}
		b := img.Bounds()
		return &PatchedFrame{W: b.Dx(), H: b.Dy(), patches: &ip}, nil
	})
	if err != nil {
		return nil, err
	}
	first := frames[keptSamples[0]].(*PatchedFrame)
	out := make([]image.Image, len(want))
	for i := range out {
		out[i] = &PatchedFrame{W: first.W, H: first.H}
	}
	for _, k := range kept {
		out[k] = frames[want[k]]
	}
	return out, nil
}

// VideoDuration is the presentation length of an MP4 or MOV video's first
// video track, for multimodalembedding@001's videoSegmentConfig (#1651),
// whose segments end at the video's end.
func VideoDuration(mimeType string, data []byte, opt VideoOptions) (time.Duration, error) {
	tr, err := readVideoOf(mimeType, data, opt.Matroska)
	if err != nil {
		return 0, err
	}
	return time.Duration(float64(tr.end) / float64(tr.timescale) * float64(time.Second)), nil
}

// readVideoOf reads a video's first video track: an MP4 or MOV file, or
// with matroska a Matroska or WebM one.
func readVideoOf(mimeType string, data []byte, matroska bool) (*videoTrack, error) {
	mt := baseMIME(mimeType)
	mkv := matroska && matroskaMIMETypes[mt]
	if !videoMIMETypes[mt] && !mkv {
		types := VideoMIMETypes
		if matroska {
			types = append(append([]string(nil), VideoMIMETypes...), MatroskaMIMETypes...)
		}
		return nil, fmt.Errorf("mime_type %q is not supported; send one of %s", mimeType, strings.Join(types, ", "))
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("the video data is empty")
	}
	if mkv {
		return readMatroskaTrack(data)
	}
	return readVideoTrack(data)
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
		if st.Stsd.VpXX != nil {
			tr.vpcC = st.Stsd.VpXX.VppC
		}
	case "av01":
		if st.Stsd.Av01 != nil && st.Stsd.Av01.Av1C != nil {
			tr.av1Config = st.Stsd.Av01.Av1C.ConfigOBUs
		}
	default:
		return nil, fmt.Errorf("%w %q; Google takes H.264, H.265, VP9 and AV1, and this build decodes those", ErrVideoCodec, tr.codec)
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
func decodeSamples(tr *videoTrack, want []int, conv frameConv) (map[int]image.Image, error) {
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
		wanted := make(map[int]bool, len(groups[k]))
		for _, s := range groups[k] {
			wanted[s] = true
		}
		var (
			pics map[int]image.Image
			err  error
		)
		switch tr.codec {
		case "avc1", "avc3":
			pics, err = decodeH264(tr, k, stop, wanted, conv)
		case "vp09":
			pics, err = decodeVP9(tr, k, stop, wanted, conv)
		case "av01":
			pics, err = decodeAV1(tr, k, stop, wanted, conv)
		default:
			pics, err = decodeH265(tr, k, stop, wanted, conv)
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
func decodeH264(tr *videoTrack, from, stop int, wanted map[int]bool, conv frameConv) (pics map[int]image.Image, err error) {
	defer func() {
		if r := recover(); r != nil {
			pics, err = nil, fmt.Errorf("decoding the H.264 video: %v", r)
		}
	}()
	matrix, full := bt601, false
	if len(tr.avcC.SPSnalus) > 0 {
		if sps, err := avc.ParseSPSNALUnit(tr.avcC.SPSnalus[0], true); err == nil {
			if err := frameTooLarge(int(sps.Width), int(sps.Height)); err != nil {
				return nil, err
			}
			matrix = colourMatrix(sps.VUI != nil, vuiMatrix(sps), int(sps.Height))
			full = sps.VUI != nil && sps.VUI.VideoFullRangeFlag
		}
	}
	d := go264.NewDecoderWithConfig(go264.DecoderConfig{ForceSoftware: true,
		Limits: go264.DecoderLimits{MaxFrameMBs: maxVideoPixels / 256, AboveTable: true}})
	defer d.Close()
	var stream []byte
	start := []byte{0, 0, 0, 1}
	for _, ps := range append(append([][]byte(nil), tr.avcC.SPSnalus...), tr.avcC.PPSnalus...) {
		stream = append(append(stream, start...), ps...)
	}
	// The decoded samples in presentation order: the decoder's pictures
	// come out in that order, each converted before the next Decode
	// reuses its planes.
	idx := make([]int, 0, stop-from)
	for s := from; s < stop; s++ {
		idx = append(idx, s)
	}
	sort.SliceStable(idx, func(a, b int) bool { return tr.pts[idx[a]] < tr.pts[idx[b]] })
	pics = make(map[int]image.Image, len(wanted))
	n := 0
	take := func(fs []*go264.Frame) error {
		for _, f := range fs {
			if n >= len(idx) {
				n++
				continue
			}
			err := keepFrame(pics, wanted, conv, idx[n], func() (image.Image, error) {
				return yuvToNRGBA(yuvPlanes{
					w: f.Width, h: f.Height, y: f.Y, cb: f.Cb, cr: f.Cr, strideY: f.StrideY, strideC: f.StrideC,
					ssX: 1, ssY: 1, depth: 8,
				}, matrix, full), nil
			})
			if err != nil {
				return err
			}
			n++
		}
		return nil
	}
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
		if err := take(fs); err != nil {
			return nil, err
		}
		stream = stream[:0]
	}
	fs, err := d.Flush()
	if err != nil {
		return nil, fmt.Errorf("decoding the H.264 video: %v", err)
	}
	if err := take(fs); err != nil {
		return nil, err
	}
	if n != len(idx) {
		return nil, fmt.Errorf("decoding the H.264 video: %d samples made %d pictures", len(idx), n)
	}
	return pics, nil
}

func vuiMatrix(sps *avc.SPS) int {
	if sps.VUI == nil || !sps.VUI.ColourDescriptionFlag {
		return 2
	}
	return int(sps.VUI.MatrixCoefficients)
}

// decodeH265 decodes samples from to stop (exclusive) of an H.265 track,
// each picture tagged with its sample.
func decodeH265(tr *videoTrack, from, stop int, wanted map[int]bool, conv frameConv) (pics map[int]image.Image, err error) {
	defer func() {
		if r := recover(); r != nil {
			pics, err = nil, fmt.Errorf("decoding the H.265 video: %v", r)
		}
	}()
	d := hevc.Decoder{}
	d.FrameSizeLimit(maxVideoSide * maxVideoSide)
	pics = make(map[int]image.Image, len(wanted))
	// Each picture is converted as the decoder outputs it.
	take := func(ps []*hevc.Picture) error {
		for _, p := range ps {
			if err := frameTooLarge(p.Width, p.Height); err != nil {
				return err
			}
		}
		for _, p := range ps {
			if p.Corrupt {
				continue
			}
			if err := keepFrame(pics, wanted, conv, int(p.Tag), func() (image.Image, error) { return h265Picture(p), nil }); err != nil {
				return err
			}
		}
		return nil
	}
	feed := func(raw []byte, tag uint64) error {
		nal, ok := hevc.ParseNAL(raw)
		if !ok {
			return fmt.Errorf("a NAL unit does not parse")
		}
		nal.Tag = tag
		ps, err := d.DecodeNAL(nal)
		if terr := take(ps); terr != nil {
			return terr
		}
		return err
	}
	for _, ps := range tr.hvcC {
		if err := feed(ps, 0); err != nil {
			if errors.Is(err, ErrNotDecoded) {
				return nil, err
			}
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
				if errors.Is(err, ErrNotDecoded) {
					return nil, err
				}
				return nil, fmt.Errorf("decoding the H.265 video: %v", err)
			}
		}
	}
	if err := take(d.Flush()); err != nil {
		return nil, err
	}
	return pics, nil
}

// h265Picture is a decoded H.265 picture as RGB.
func h265Picture(p *hevc.Picture) image.Image {
	ssX, ssY := 0, 0
	switch p.ChromaFormat {
	case 1:
		ssX, ssY = 1, 1
	case 2:
		ssX = 1
	}
	planes := yuvPlanes{w: p.Width, h: p.Height, strideY: p.StrideY, strideC: p.StrideC,
		ssX: ssX, ssY: ssY, depth: max(p.BitDepth, p.BitDepthC),
		cropX: p.CropX, cropY: p.CropY, cropW: p.CropW, cropH: p.CropH}
	if planes.depth > 8 {
		planes.y16, planes.cb16, planes.cr16 = p.Y16, p.Cb16, p.Cr16
	} else {
		planes.y, planes.cb, planes.cr = p.Y, p.Cb, p.Cr
	}
	if p.ChromaFormat == 0 {
		// Monochrome (4:0:0, #1651): gray, as ffmpeg converts it.
		planes.depth = max(p.BitDepth, 8)
		return grayToNRGBA(planes)
	}
	return yuvToNRGBA(planes, colourMatrix(p.ColorMatrix != 2, int(p.ColorMatrix), p.CropH), p.FullRange)
}

// decodeVP9 decodes samples from to stop (exclusive) of a VP9 track. A
// sample is one shown frame, with any hidden frame (an alternate reference)
// before it in a superframe; VP9 has no reordering, so each sample's picture
// is the one it shows.
func decodeVP9(tr *videoTrack, from, stop int, wanted map[int]bool, conv frameConv) (pics map[int]image.Image, err error) {
	defer func() {
		if r := recover(); r != nil {
			pics, err = nil, fmt.Errorf("decoding the VP9 video: %v", r)
		}
	}()
	d, err := govpx.NewVP9Decoder(govpx.VP9DecoderOptions{MaxWidth: maxVideoSide, MaxHeight: maxVideoSide})
	if err != nil {
		return nil, fmt.Errorf("decoding the VP9 video: %v", err)
	}
	defer d.Close()
	// The colour description vpcC carries (the bitstream's own color_space
	// and color_range, which the container repeats); without one, BT.601
	// or BT.709 by size, limited range, as for H.264 with no VUI.
	full, declared, mc := false, false, 2
	if c := tr.vpcC; c != nil {
		full, declared, mc = c.VideoFullRangeFlag != 0, c.MatrixCoefficients != 2, int(c.MatrixCoefficients)
	}
	if tr.vpcC != nil && tr.vpcC.Profile != 0 {
		return decodeVP9Profiles(tr, from, stop, declared, mc, full, wanted, conv)
	}
	pics = make(map[int]image.Image, len(wanted))
	for s := from; s < stop; s++ {
		if err := d.Decode(tr.samples[s]); err != nil {
			if errors.Is(err, govpx.ErrVP9NotImplemented) {
				// A profile 1 to 3 frame with no vpcC to say so.
				return decodeVP9Profiles(tr, from, stop, declared, mc, full, wanted, conv)
			}
			return nil, fmt.Errorf("decoding the VP9 video's sample %d: %v", s+1, err)
		}
		img, ok := d.NextFrame()
		if !ok {
			continue
		}
		if err := frameTooLarge(img.Width, img.Height); err != nil {
			return nil, err
		}
		// The planes are the decoder's until the next Decode, which
		// yuvToNRGBA's copy precedes.
		err := keepFrame(pics, wanted, conv, s, func() (image.Image, error) {
			return yuvToNRGBA(yuvPlanes{w: img.Width, h: img.Height, y: img.Y, cb: img.U, cr: img.V,
				strideY: img.YStride, strideC: img.UStride, ssX: 1, ssY: 1, depth: 8},
				colourMatrix(declared, mc, img.Height), full), nil
		})
		if err != nil {
			return nil, err
		}
	}
	return pics, nil
}

// decodeVP9Profiles decodes samples from to stop (exclusive) of a VP9
// track in any profile with govpx's vp9hbd (#1739), written for CloudBurrow
// from the VP9 bitstream specification: 8, 10 and 12 bits, 4:2:0, 4:2:2,
// 4:4:0 and 4:4:4, each frame bit-exact against ffmpeg's decode.
func decodeVP9Profiles(tr *videoTrack, from, stop int, declared bool, mc int, full bool,
	wanted map[int]bool, conv frameConv) (map[int]image.Image, error) {
	d := vp9hbd.NewDecoder()
	pics := make(map[int]image.Image, len(wanted))
	for s := from; s < stop; s++ {
		frames, err := d.Decode(tr.samples[s])
		if err != nil {
			return nil, fmt.Errorf("decoding the VP9 video's sample %d: %v", s+1, err)
		}
		if len(frames) == 0 {
			continue
		}
		f := frames[len(frames)-1]
		if err := frameTooLarge(f.Width, f.Height); err != nil {
			return nil, err
		}
		err = keepFrame(pics, wanted, conv, s, func() (image.Image, error) {
			if f.ColorSpace == 7 { // CS_RGB: 4:4:4 planes of G, B and R
				return vp9RGB(f), nil
			}
			return yuvToNRGBA(yuvPlanes{w: f.Width, h: f.Height, y16: f.Planes[0], cb16: f.Planes[1], cr16: f.Planes[2],
				strideY: f.Strides[0], strideC: f.Strides[1], ssX: f.SubsamplingX, ssY: f.SubsamplingY, depth: f.BitDepth},
				colourMatrix(declared, mc, f.Height), full || f.FullRange), nil
		})
		if err != nil {
			return nil, err
		}
	}
	return pics, nil
}

// vp9RGB is a CS_RGB VP9 frame (profile 1 or 3, no subsampling; its planes
// G, B, R) as 8-bit RGB, deeper samples scaled to 0 to 255 and rounded.
func vp9RGB(f *vp9hbd.Frame) image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, f.Width, f.Height))
	maxV := uint32(1)<<f.BitDepth - 1
	to8 := func(v uint16) uint8 { return uint8((min(uint32(v), maxV)*255 + maxV/2) / maxV) }
	for y := range f.Height {
		for x := range f.Width {
			o := img.PixOffset(x, y)
			img.Pix[o] = to8(f.Planes[2][y*f.Strides[2]+x])
			img.Pix[o+1] = to8(f.Planes[0][y*f.Strides[0]+x])
			img.Pix[o+2] = to8(f.Planes[1][y*f.Strides[1]+x])
			img.Pix[o+3] = 255
		}
	}
	return img
}

// decodeAV1 decodes samples from to stop (exclusive) of an AV1 track. A
// sample is a temporal unit, which shows at most one frame; AV1 has no
// reordering in its output, so each sample's picture is the one it shows.
func decodeAV1(tr *videoTrack, from, stop int, wanted map[int]bool, conv frameConv) (pics map[int]image.Image, err error) {
	defer func() {
		if r := recover(); r != nil {
			pics, err = nil, fmt.Errorf("decoding the AV1 video: %v", r)
		}
	}()
	d := &av1.Decoder{FrameSizeLimit: maxVideoSide * maxVideoSide, FrameThreads: 1}
	defer d.Flush()
	// The sequence header av1C carries, ahead of the first temporal unit
	// (a sync sample repeats it).
	if len(tr.av1Config) > 0 {
		if _, err := d.DecodeOBUs(tr.av1Config); err != nil {
			return nil, fmt.Errorf("decoding the AV1 video's configuration: %v", err)
		}
	}
	pics = make(map[int]image.Image, len(wanted))
	for s := from; s < stop; s++ {
		ps, err := d.DecodeOBUs(tr.samples[s])
		if err != nil {
			return nil, fmt.Errorf("decoding the AV1 video's sample %d: %v", s+1, err)
		}
		if len(ps) == 0 {
			continue
		}
		p := ps[len(ps)-1]
		if err := frameTooLarge(p.Width, p.Height); err != nil {
			for _, q := range ps {
				q.Release()
			}
			return nil, err
		}
		err = keepFrame(pics, wanted, conv, s, func() (image.Image, error) { return av1Picture(p) })
		for _, q := range ps {
			q.Release()
		}
		if err != nil {
			return nil, err
		}
	}
	return pics, nil
}

// av1Picture converts a decoded AV1 picture to RGB, before the caller
// releases its planes.
func av1Picture(p *av1.Picture) (image.Image, error) {
	planes := yuvPlanes{w: p.Width, h: p.Height, strideY: p.Stride[0], strideC: p.Stride[1], depth: p.BitDepth}
	switch p.Layout {
	case av1.LayoutI420:
		planes.ssX, planes.ssY = 1, 1
	case av1.LayoutI422:
		planes.ssX = 1
	case av1.LayoutI444:
	default: // monochrome: neutral chroma
		cw, ch := (p.Width+1)/2, (p.Height+1)/2
		planes.ssX, planes.ssY, planes.strideC = 1, 1, cw
		mid := 128 << (p.BitDepth - 8)
		if p.BitDepth > 8 {
			planes.cb16 = make([]uint16, cw*ch)
			for i := range planes.cb16 {
				planes.cb16[i] = uint16(mid)
			}
			planes.cr16 = planes.cb16
			planes.strideY /= 2
			planes.y16 = u16(p.Data[0])
		} else {
			planes.cb = bytes.Repeat([]byte{byte(mid)}, cw*ch)
			planes.cr = planes.cb
			planes.y = p.Data[0]
		}
		return yuvToNRGBA(planes, colourMatrix(p.MatrixCoeffs != 2, p.MatrixCoeffs, p.Height), p.FullRange), nil
	}
	if p.BitDepth > 8 {
		// Samples are native-endian uint16, strides in bytes.
		planes.strideY /= 2
		planes.strideC /= 2
		planes.y16, planes.cb16, planes.cr16 = u16(p.Data[0]), u16(p.Data[1]), u16(p.Data[2])
	} else {
		planes.y, planes.cb, planes.cr = p.Data[0], p.Data[1], p.Data[2]
	}
	return yuvToNRGBA(planes, colourMatrix(p.MatrixCoeffs != 2, p.MatrixCoeffs, p.Height), p.FullRange), nil
}

// u16 reads native-endian (little-endian on every platform Go builds
// CloudBurrow for) 16-bit samples.
func u16(b []byte) []uint16 {
	out := make([]uint16, len(b)/2)
	for i := range out {
		out[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	return out
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

// grayToNRGBA converts a monochrome picture's luma plane to RGB as ffmpeg's
// swscale converts gray to rgb24 (measured with ffmpeg 9): each sample is
// its own grey level, whatever the stream's range, a deeper one rounded to
// 8 bits (Y / 4, to nearest, for 10 bits).
func grayToNRGBA(p yuvPlanes) *image.NRGBA {
	x0, y0, w, h := 0, 0, p.w, p.h
	if p.cropW > 0 && p.cropH > 0 {
		x0, y0, w, h = p.cropX, p.cropY, p.cropW, p.cropH
	}
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	shift := p.depth - 8
	for yy := 0; yy < h; yy++ {
		row := img.Pix[yy*img.Stride:]
		for xx := 0; xx < w; xx++ {
			v := int(p.luma(x0+xx, y0+yy))
			if shift > 0 {
				v = min((v+1<<(shift-1))>>shift, 255)
			}
			row[4*xx], row[4*xx+1], row[4*xx+2], row[4*xx+3] = uint8(v), uint8(v), uint8(v), 255
		}
	}
	return img
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

// videoSoftTokens and videoMaxFrames are the model's video_processor
// values: max_soft_tokens per frame and max_frames.
const (
	videoSoftTokens = 140
	videoMaxFrames  = 32
)

// VideoFrames are the frames the video processor keeps of a video's: all of
// them up to videoMaxFrames, or that many taken uniformly
// (np.linspace(0, n-1, max_frames).astype(int)), as
// EmbeddingGemma2VideoProcessor's sample_frames does with
// overflow_strategy "uniform".
func VideoFrames(frames []image.Image) []image.Image {
	if len(frames) <= videoMaxFrames {
		return frames
	}
	idx := videoFrameIndexes(len(frames))
	out := make([]image.Image, len(idx))
	for i, k := range idx {
		out[i] = frames[k]
	}
	return out
}

// videoFrameIndexes are the indexes of the frames VideoFrames keeps of n.
func videoFrameIndexes(n int) []int {
	if n <= videoMaxFrames {
		out := make([]int, n)
		for i := range out {
			out[i] = i
		}
		return out
	}
	step := float64(n-1) / float64(videoMaxFrames-1)
	out := make([]int, videoMaxFrames)
	for i := range out {
		k := int(math.Floor(float64(i) * step))
		if i == videoMaxFrames-1 {
			k = n - 1
		}
		out[i] = k
	}
	return out
}

// PatchVideoFrame prepares a video frame for the vision encoder, with the
// video processor's budget of soft tokens.
func PatchVideoFrame(img image.Image) (ImagePatches, error) {
	if f, ok := img.(*PatchedFrame); ok {
		if f.patches == nil {
			return ImagePatches{}, fmt.Errorf("the video frame was dropped (VideoFrames does not keep it)")
		}
		return *f.patches, nil
	}
	return patchImage(img, videoSoftTokens)
}
