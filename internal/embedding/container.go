package embedding

import (
	"errors"
	"fmt"
	"image"
	"sort"
	"time"
)

// The AVI, FLV, MPEG and WMV containers multimodalembedding@001 takes
// (#1651). Their codecs (MPEG-1 and MPEG-2 video, MPEG-4 Part 2, Sorenson
// Spark, VP6, Windows Media Video and VC-1, Motion JPEG...) have no
// permissively licensed Go decoder, so a ContainerDecoder outside this
// package (internal/videodecode: FFmpeg in a pinned container, run as a
// separate program) lists their frames and decodes the sampled ones to raw
// planes. Sampling, colour conversion and patching stay here, exactly as
// for MP4: an H.264 stream remuxed into AVI or FLV gives the MP4's frames
// pixel for pixel.

// ContainerMIMETypes are the video types a ContainerDecoder serves, by
// their canonical MIME type.
var ContainerMIMETypes = []string{"video/x-msvideo", "video/x-flv", "video/mpeg", "video/x-ms-wmv"}

var containerMIMETypes = map[string]bool{
	"video/x-msvideo": true, "video/x-flv": true, "video/mpeg": true, "video/x-ms-wmv": true,
}

// ErrDecoderUnavailable marks a ContainerDecoder that cannot run at all
// (no Docker, or its image would not build): UNAVAILABLE, not the
// request's fault.
var ErrDecoderUnavailable = errors.New("the video decoder is unavailable")

// ContainerDecoder lists and decodes the frames of a video's first video
// stream.
type ContainerDecoder interface {
	// ProbeFrames lists the stream's frames in presentation order, as its
	// decoder outputs them.
	ProbeFrames(data []byte) (*ProbedVideo, error)
	// DecodeFrames decodes the frames at the presentation indexes idx
	// (ascending, distinct) and passes each to yield, in order.
	DecodeFrames(data []byte, idx []int, yield func(i int, f RawFrame) error) error
}

// ProbedVideo is a ContainerDecoder's list of a stream's frames.
type ProbedVideo struct {
	// Codec is the stream's codec, as FFmpeg names it (mpeg2video, wmv3...).
	Codec string
	// Times are each frame's presentation time from the first frame's, and
	// End the last frame's end.
	Times []time.Duration
	End   time.Duration
}

// RawFrame is one decoded frame's planes.
type RawFrame struct {
	W, H int
	// Format is the planes' layout, as FFmpeg names it: yuv420p, yuv422p,
	// yuv444p (each with a j for full range), gray or rgb24.
	Format string
	Pix    []byte
	// Matrix is the stream's ISO/IEC 23091-2 matrix_coefficients, 2 when it
	// declares none; FullRange its full-range flag.
	Matrix    int
	FullRange bool
}

// rawFormats are RawFrame's YUV layouts: chroma shifts and full range.
var rawFormats = map[string]struct {
	ssX, ssY int
	full     bool
}{
	"yuv420p": {1, 1, false}, "yuvj420p": {1, 1, true},
	"yuv422p": {1, 0, false}, "yuvj422p": {1, 0, true},
	"yuv444p": {0, 0, false}, "yuvj444p": {0, 0, true},
}

// RawFormat reports whether a decoder's native pixel format is handed over
// as it is (converted here, as the in-process decoders' pictures are);
// any other is asked for as rgb24.
func RawFormat(f string) bool { _, ok := rawFormats[f]; return ok || f == "gray" }

// RawFrameSize is the bytes of a w × h frame in format f.
func RawFrameSize(f string, w, h int) int {
	if f == "gray" {
		return w * h
	}
	if f == "rgb24" {
		return 3 * w * h
	}
	l := rawFormats[f]
	cw, ch := (w+(1<<l.ssX)-1)>>l.ssX, (h+(1<<l.ssY)-1)>>l.ssY
	return w*h + 2*cw*ch
}

// image converts the frame to RGB as the in-process decoders' frames are.
func (f RawFrame) image() (image.Image, error) {
	if err := frameTooLarge(f.W, f.H); err != nil {
		return nil, err
	}
	if f.W <= 0 || f.H <= 0 {
		return nil, fmt.Errorf("the video's frame is %dx%d", f.W, f.H)
	}
	if len(f.Pix) != RawFrameSize(f.Format, f.W, f.H) {
		return nil, fmt.Errorf("the video's %s frame is %d bytes, not %d", f.Format, len(f.Pix), RawFrameSize(f.Format, f.W, f.H))
	}
	switch f.Format {
	case "rgb24":
		img := image.NewNRGBA(image.Rect(0, 0, f.W, f.H))
		for i := 0; i < f.W*f.H; i++ {
			img.Pix[4*i], img.Pix[4*i+1], img.Pix[4*i+2], img.Pix[4*i+3] = f.Pix[3*i], f.Pix[3*i+1], f.Pix[3*i+2], 255
		}
		return img, nil
	case "gray":
		return grayToNRGBA(yuvPlanes{w: f.W, h: f.H, y: f.Pix, strideY: f.W, depth: 8}), nil
	}
	l, ok := rawFormats[f.Format]
	if !ok {
		return nil, fmt.Errorf("the video's frames are %s", f.Format)
	}
	cw, ch := (f.W+(1<<l.ssX)-1)>>l.ssX, (f.H+(1<<l.ssY)-1)>>l.ssY
	ys, cs := f.W*f.H, cw*ch
	p := yuvPlanes{w: f.W, h: f.H, y: f.Pix[:ys], cb: f.Pix[ys : ys+cs], cr: f.Pix[ys+cs : ys+2*cs],
		strideY: f.W, strideC: cw, ssX: l.ssX, ssY: l.ssY, depth: 8}
	return yuvToNRGBA(p, colourMatrix(true, f.Matrix, f.H), l.full || f.FullRange), nil
}

// readContainerTrack is a ContainerDecoder's frames as a videoTrack, its
// samples in presentation order and each its own random access point.
func readContainerTrack(d ContainerDecoder, data []byte) (*videoTrack, error) {
	pv, err := d.ProbeFrames(data)
	if err != nil {
		return nil, err
	}
	if len(pv.Times) == 0 {
		return nil, fmt.Errorf("the video has no frames")
	}
	tr := &videoTrack{codec: "container", timescale: 1_000_000, ext: d, extData: data,
		pts: make([]int64, len(pv.Times)), sync: make([]bool, len(pv.Times)), end: pv.End.Microseconds()}
	for i, t := range pv.Times {
		tr.pts[i], tr.sync[i] = t.Microseconds(), true
	}
	return tr, nil
}

// decodeContainerSamples decodes tr's frames want through its decoder.
func decodeContainerSamples(tr *videoTrack, want []int, conv frameConv) (map[int]image.Image, error) {
	idx := append([]int(nil), want...)
	sort.Ints(idx)
	k := 0
	for _, i := range idx {
		if k == 0 || idx[k-1] != i {
			idx[k] = i
			k++
		}
	}
	idx = idx[:k]
	out := make(map[int]image.Image, len(idx))
	err := tr.ext.DecodeFrames(tr.extData, idx, func(i int, f RawFrame) error {
		img, err := f.image()
		if err != nil {
			return err
		}
		if img, err = conv(img); err != nil {
			return err
		}
		out[i] = img
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, i := range idx {
		if out[i] == nil {
			return nil, fmt.Errorf("the video's frame %d did not decode", i)
		}
	}
	return out, nil
}
