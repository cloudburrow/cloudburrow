package vertexai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"math"
	"net/http"
	"strings"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/embedding"
	"github.com/cloudburrow/cloudburrow/internal/localai"
)

// multimodalembedding@001's :predict (#1651), served on the local
// multimodal model, EmbeddingGemma 2. Google's model takes one instance of
// text, an image and a video, and answers each with its own embedding:
//
//	{"instances": [{"text": "...", "image": {"bytesBase64Encoded": "..."},
//	   "video": {"gcsUri": "gs://...", "videoSegmentConfig":
//	     {"startOffsetSec": 0, "endOffsetSec": 120, "intervalSec": 16}}}],
//	 "parameters": {"dimension": 512}}
//	→ {"predictions": [{"textEmbedding": [...], "imageEmbedding": [...],
//	   "videoEmbeddings": [{"startOffsetSec": 0, "endOffsetSec": 16, "embedding": [...]}]}]}
//
// as https://docs.cloud.google.com/vertex-ai/generative-ai/docs/embeddings/get-multimodal-embeddings
// documents it (re-read 2026-11-21). The model name stays the local one:
// multimodalembedding@001 is NOT_FOUND, as every Google model ID is, since
// nothing is substituted (doc.go); a client sends this request shape to
// embeddinggemma-2-onnx-community instead.
//
// Served as Google documents: one instance a request; text, image and video
// in any combination; an image as bytesBase64Encoded or a gcsUri in this
// instance's Cloud Storage, BMP, GIF, JPEG or PNG, at most 20 MB; a video
// the same way, one embedding per videoSegmentConfig interval (default 0 to
// 120 seconds every 16, an interval of at least 4 seconds, 2 minutes of
// video analysed a request), its frames at 1 a second, audio not
// considered; dimension 128, 256 or 512 for text and image (video vectors
// are not reduced). What differs, because the local model is not Google's:
// vectors are EmbeddingGemma 2's 768 values, so dimension 1408 (Google's
// default) is UNIMPLEMENTED and an unset dimension gives 768.
//
// Text over Google's 32 tokens is shortened to them, as Google documents
// ("the model internally shortens the input to this length"), counted
// with the local model's own tokenizer (Gemma's SentencePiece), Google's
// being undocumented beyond "approximately 32 words". MP4, MOV, MKV and
// WebM are demuxed and decoded in process (embedding/video.go,
// matroska.go); AVI, FLV, MPEG and WMV, whose codecs no permissive Go
// decoder reads, are decoded by FFmpeg in a pinned container
// (internal/videodecode), sampled and converted here as an MP4's frames
// are.

// mmImageBytes is Google's image limit for multimodalembedding@001: 20 MB.
const mmImageBytes = 20 << 20

// mmVideoObjectBytes bounds a video read from Cloud Storage. Google sets no
// limit there; a larger object is UNIMPLEMENTED.
const mmVideoObjectBytes = 256 << 20

// mmMaxSeconds is how much video one request analyses: 2 minutes.
const mmMaxSeconds = 120

// mmDefaultInterval and mmMinInterval are videoSegmentConfig.intervalSec's
// default and minimum.
const (
	mmDefaultInterval = 16
	mmMinInterval     = 4
)

// mmDimensions are multimodalembedding@001's dimension values; mmGoogleDim
// is its default, which the local model cannot produce.
var mmDimensions = []int{128, 256, 512, 1408}

const mmGoogleDim = 1408

// mmMaxTextTokens is multimodalembedding@001's text length: 32 tokens;
// longer text is shortened to them.
const mmMaxTextTokens = 32

// VideoDecoder decodes the AVI, FLV, MPEG and WMV video the in-process
// decoders do not (internal/videodecode's Docker).
type VideoDecoder interface {
	// Source is a decoder for one request's video; its Close frees it.
	Source(ctx context.Context) ContainerSource
}

// ContainerSource is one request's VideoDecoder.
type ContainerSource interface {
	embedding.ContainerDecoder
	Close() error
}

// SetVideoDecoder makes multimodalembedding@001's request decode AVI, FLV,
// MPEG and WMV video with d; without it they are UNAVAILABLE.
func (s *Server) SetVideoDecoder(d VideoDecoder) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.videoDecoder = d
}

type mmRequest struct {
	Instances  []mmInstance  `json:"instances"`
	Parameters *mmParameters `json:"parameters,omitempty"`
}

type mmParameters struct {
	Dimension *int `json:"dimension,omitempty"`
}

type mmInstance struct {
	Text  *string  `json:"text,omitempty"`
	Image *mmMedia `json:"image,omitempty"`
	Video *mmVideo `json:"video,omitempty"`
}

type mmMedia struct {
	BytesBase64Encoded string `json:"bytesBase64Encoded,omitempty"`
	GcsURI             string `json:"gcsUri,omitempty"`
	MimeType           string `json:"mimeType,omitempty"`
}

type mmVideo struct {
	BytesBase64Encoded string           `json:"bytesBase64Encoded,omitempty"`
	GcsURI             string           `json:"gcsUri,omitempty"`
	MimeType           string           `json:"mimeType,omitempty"`
	VideoSegmentConfig *mmSegmentConfig `json:"videoSegmentConfig,omitempty"`
}

type mmSegmentConfig struct {
	StartOffsetSec *int `json:"startOffsetSec,omitempty"`
	EndOffsetSec   *int `json:"endOffsetSec,omitempty"`
	IntervalSec    *int `json:"intervalSec,omitempty"`
}

// isMultimodalPredict reports whether a :predict body is
// multimodalembedding@001's: an instance with text, image or video and no
// text-embedding content.
func isMultimodalPredict(body []byte) bool {
	var probe struct {
		Instances []map[string]json.RawMessage `json:"instances"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return false
	}
	mm := false
	for _, in := range probe.Instances {
		if _, ok := in["content"]; ok {
			return false
		}
		for _, k := range []string{"text", "image", "video"} {
			if _, ok := in[k]; ok {
				mm = true
			}
		}
	}
	return mm
}

// multimodalPredict answers a multimodalembedding@001 :predict on e.
func (s *Server) multimodalPredict(ctx context.Context, e embedding.Embedder, body []byte) (map[string]any, error) {
	if !embedding.EmbedsImages(e) {
		return nil, apierror.InvalidArgument(
			"multimodalembedding@001's instances (text, image, video) are served on %s, the local multimodal model; %s embeds text only (send {\"content\": ...} instances)",
			localai.EmbeddingGemma2ONNXID, e.Model())
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var req mmRequest
	if err := dec.Decode(&req); err != nil {
		return nil, apierror.InvalidArgument(
			"malformed request body: %v; this endpoint serves multimodalembedding@001's documented :predict", err)
	}
	switch n := len(req.Instances); {
	case n == 0:
		return nil, apierror.InvalidArgument("instances must not be empty")
	case n > 1:
		return nil, apierror.InvalidArgument("multimodal embeddings take one instance per request, got %d", n)
	}
	dim := 0
	if p := req.Parameters; p != nil && p.Dimension != nil {
		dim = *p.Dimension
		if !containsInt(mmDimensions, dim) {
			return nil, apierror.InvalidArgument("parameters.dimension %d is not supported; use one of 128, 256, 512 or 1408", dim)
		}
		if dim == mmGoogleDim {
			return nil, apierror.Unimplemented(
				"parameters.dimension 1408: %s's vectors have %d values; use 128, 256 or 512, or leave dimension unset for %d",
				e.Model(), embedding.Dimensions, embedding.Dimensions)
		}
	}
	x := req.Instances[0]
	if x.Text == nil && x.Image == nil && x.Video == nil {
		return nil, apierror.InvalidArgument("instances[0]: set at least one of text, image and video")
	}
	if x.Text != nil && strings.TrimSpace(*x.Text) == "" {
		return nil, apierror.InvalidArgument("instances[0].text must not be empty")
	}
	s.mu.RLock()
	objs := s.objects
	vd := s.videoDecoder
	s.mu.RUnlock()

	// Text and image, reduced to dimension; then the video's segments.
	var reduced []embedding.Input
	if x.Text != nil {
		reduced = append(reduced, embedding.Input{Content: *x.Text, MaxTextTokens: mmMaxTextTokens})
	}
	if x.Image != nil {
		img, err := mmDecodeImage(ctx, x.Image, objs)
		if err != nil {
			return nil, err
		}
		reduced = append(reduced, embedding.Input{Parts: []embedding.Part{{Image: img}}})
	}
	var segs []mmSegment
	if x.Video != nil {
		var err error
		if segs, err = mmDecodeVideo(ctx, x.Video, objs, vd); err != nil {
			return nil, err
		}
	}

	pred := map[string]any{}
	if len(reduced) > 0 {
		out, err := e.Embed(ctx, reduced, dim, true)
		if err != nil {
			return nil, embedFailed(ctx, err)
		}
		i := 0
		if x.Text != nil {
			pred["textEmbedding"] = out[i].Values
			i++
		}
		if x.Image != nil {
			pred["imageEmbedding"] = out[i].Values
		}
	}
	if len(segs) > 0 {
		in := make([]embedding.Input, len(segs))
		for i, sg := range segs {
			in[i] = embedding.Input{Parts: []embedding.Part{{Video: sg.frames}}}
		}
		out, err := e.Embed(ctx, in, 0, true)
		if err != nil {
			return nil, embedFailed(ctx, err)
		}
		ve := make([]map[string]any, len(segs))
		for i, sg := range segs {
			ve[i] = map[string]any{"startOffsetSec": sg.start, "endOffsetSec": sg.end, "embedding": out[i].Values}
		}
		pred["videoEmbeddings"] = ve
	}
	return map[string]any{
		"predictions":      []map[string]any{pred},
		"model":            e.Model(),
		"modelDisplayName": embeddingDisplayName(e.Model()),
	}, nil
}

func embedFailed(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return apierror.InvalidArgument("%v", err)
}

func containsString(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// mmMediaBytes is an image's or video's bytes and the type its source
// declares: bytesBase64Encoded, or a gcsUri read from this instance's Cloud
// Storage.
func mmMediaBytes(ctx context.Context, field, b64, uri, mimeType string, limit int64, objs Objects) ([]byte, string, error) {
	switch {
	case b64 != "" && uri != "":
		return nil, "", apierror.InvalidArgument("%s: set one of bytesBase64Encoded and gcsUri, not both", field)
	case b64 == "" && uri == "":
		return nil, "", apierror.InvalidArgument("%s: set bytesBase64Encoded or gcsUri", field)
	case b64 != "":
		data, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, "", apierror.InvalidArgument("%s.bytesBase64Encoded is not valid base64: %v", field, err)
		}
		return data, mimeType, nil
	}
	rest, ok := strings.CutPrefix(uri, "gs://")
	bucket, object, _ := strings.Cut(rest, "/")
	if !ok || bucket == "" || object == "" {
		return nil, "", apierror.InvalidArgument("%s.gcsUri must be gs://{bucket}/{object}, not %q", field, uri)
	}
	if objs == nil {
		return nil, "", apierror.FailedPrecondition(
			"%s.gcsUri: reading %s needs this instance's Cloud Storage, which is not enabled; start it with the storage service, or send bytesBase64Encoded", field, uri)
	}
	data, ct, err := objs.Read(ctx, bucket, object, limit)
	if err != nil {
		return nil, "", err
	}
	if strings.TrimSpace(mimeType) == "" {
		mimeType = ct
	}
	return data, mimeType, nil
}

// mmImageTypes are the image types multimodalembedding@001 takes, with
// their aliases.
var mmImageTypes = map[string]string{
	"image/bmp": "image/bmp", "image/x-ms-bmp": "image/bmp", "image/x-bmp": "image/bmp",
	"image/gif":  "image/gif",
	"image/jpeg": "image/jpeg", "image/jpg": "image/jpeg", "image/pjpeg": "image/jpeg",
	"image/png": "image/png",
}

func mmDecodeImage(ctx context.Context, m *mmMedia, objs Objects) (image.Image, error) {
	data, mt, err := mmMediaBytes(ctx, "instances[0].image", m.BytesBase64Encoded, m.GcsURI, m.MimeType, mmImageBytes, objs)
	if err != nil {
		return nil, err
	}
	if len(data) > mmImageBytes {
		return nil, apierror.InvalidArgument("instances[0].image is %d bytes; at most 20 MB is embedded", len(data))
	}
	t := sniffMedia(data)
	if base := mmBase(mt); base != "" && base != "application/octet-stream" {
		t = base
	}
	canon, ok := mmImageTypes[t]
	if !ok {
		return nil, apierror.InvalidArgument("instances[0].image: %q is not supported; send BMP, GIF, JPEG or PNG", t)
	}
	var img image.Image
	if canon == "image/gif" {
		img, err = decodeGIF(data)
	} else {
		img, err = embedding.DecodeImage(canon, data)
	}
	if err != nil {
		if errors.Is(err, embedding.ErrNotDecoded) {
			return nil, apierror.Unimplemented("instances[0].image: %v", err)
		}
		return nil, apierror.InvalidArgument("instances[0].image: %v", err)
	}
	return img, nil
}

// decodeGIF is a GIF's first frame, as an animated GIF's image embedding.
func decodeGIF(data []byte) (image.Image, error) {
	cfg, err := gif.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("the data is not a valid GIF image: %v", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > embedding.MaxImageSide || cfg.Height > embedding.MaxImageSide {
		return nil, fmt.Errorf("the image is %dx%d; each side must be 1 to %d pixels", cfg.Width, cfg.Height, embedding.MaxImageSide)
	}
	img, err := gif.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("the data is not a valid GIF image: %v", err)
	}
	return img, nil
}

// mmVideoTypes are the containers multimodalembedding@001 takes, with
// their aliases, mapped to their canonical type.
var mmVideoTypes = map[string]string{
	"video/mp4": "video/mp4", "video/quicktime": "video/quicktime", "video/mov": "video/quicktime",
	"video/x-msvideo": "video/x-msvideo", "video/avi": "video/x-msvideo", "video/msvideo": "video/x-msvideo",
	"video/x-flv": "video/x-flv", "video/flv": "video/x-flv",
	"video/x-matroska": "video/x-matroska", "video/mkv": "video/x-matroska",
	"video/webm": "video/webm",
	"video/mpeg": "video/mpeg", "video/mpg": "video/mpeg", "video/mp2t": "video/mpeg",
	"video/x-ms-wmv": "video/x-ms-wmv", "video/wmv": "video/x-ms-wmv", "video/x-ms-asf": "video/x-ms-wmv",
}

// mmSegment is one videoSegmentConfig interval and its sampled frames.
type mmSegment struct {
	start, end int
	frames     []image.Image
}

func mmDecodeVideo(ctx context.Context, v *mmVideo, objs Objects, vd VideoDecoder) ([]mmSegment, error) {
	const field = "instances[0].video"
	data, mt, err := mmMediaBytes(ctx, field, v.BytesBase64Encoded, v.GcsURI, v.MimeType, mmVideoObjectBytes, objs)
	if err != nil {
		if e := apierror.From(err); e != nil && e.Code == codes.InvalidArgument && strings.Contains(e.Message, "larger than") {
			return nil, apierror.Unimplemented("%s: %s; this build reads at most %d MiB of video", field, e.Message, mmVideoObjectBytes>>20)
		}
		return nil, err
	}
	if len(data) == 0 {
		return nil, apierror.InvalidArgument("%s: the video data is empty", field)
	}
	t := sniffMedia(data)
	if base := mmBase(mt); base != "" && base != "application/octet-stream" {
		t = base
	}
	canon, ok := mmVideoTypes[t]
	if !ok {
		return nil, apierror.InvalidArgument(
			"%s: %q is not supported; send AVI, FLV, MKV, MOV, MP4, MPEG, MPG, WEBM or WMV", field, t)
	}
	opt := embedding.VideoOptions{FPS: embedding.DefaultVideoFPS, Matroska: true}
	if containsString(embedding.ContainerMIMETypes, canon) {
		if vd == nil {
			return nil, &apierror.Error{Code: codes.Unavailable, Reason: "backendError", Message: fmt.Sprintf(
				"%s: %s video is decoded by the video decoder container, which this instance does not run; send MP4, MOV, MKV or WebM", field, canon)}
		}
		src := vd.Source(ctx)
		defer src.Close()
		opt.Container = src
	}

	start, interval := 0, mmDefaultInterval
	end := -1
	if c := v.VideoSegmentConfig; c != nil {
		if c.StartOffsetSec != nil {
			start = *c.StartOffsetSec
		}
		if c.EndOffsetSec != nil {
			end = *c.EndOffsetSec
		}
		if c.IntervalSec != nil {
			interval = *c.IntervalSec
		}
	}
	switch {
	case start < 0:
		return nil, apierror.InvalidArgument("%s.videoSegmentConfig.startOffsetSec %d must not be negative", field, start)
	case end >= 0 && end <= start:
		return nil, apierror.InvalidArgument(
			"%s.videoSegmentConfig.endOffsetSec %d must be after startOffsetSec %d", field, end, start)
	case interval < mmMinInterval:
		return nil, apierror.InvalidArgument(
			"%s.videoSegmentConfig.intervalSec %d is below the minimum of %d seconds", field, interval, mmMinInterval)
	}
	// Two minutes are analysed a request, from startOffsetSec.
	if end < 0 || end-start > mmMaxSeconds {
		end = start + mmMaxSeconds
	}
	dur, err := embedding.VideoDuration(canon, data, opt)
	if err != nil {
		return nil, videoError(field, err)
	}
	if float64(start) >= dur.Seconds() {
		return nil, apierror.InvalidArgument(
			"%s.videoSegmentConfig.startOffsetSec %d is past the video's end (%.3fs)", field, start, dur.Seconds())
	}
	// Segments end at the video's end, to the nearest second (Google's
	// offsets are whole seconds).
	last := max(int(math.Round(dur.Seconds())), start+1)
	if end > last {
		end = last
	}
	var segs []mmSegment
	for t := start; t < end; t += interval {
		e := min(t+interval, end)
		opt.Start, opt.End = time.Duration(t)*time.Second, time.Duration(e)*time.Second
		frames, err := embedding.DecodeVideoPatched(canon, data, opt)
		if err != nil {
			return nil, videoError(field, err)
		}
		segs = append(segs, mmSegment{start: t, end: e, frames: frames})
	}
	return segs, nil
}

func videoError(field string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, embedding.ErrDecoderUnavailable) {
		return &apierror.Error{Code: codes.Unavailable, Reason: "backendError", Message: fmt.Sprintf("%s: %v", field, err)}
	}
	if errors.Is(err, embedding.ErrNotDecoded) || errors.Is(err, embedding.ErrVideoCodec) {
		return apierror.Unimplemented("%s: %v", field, err)
	}
	return apierror.InvalidArgument("%s: %v", field, err)
}

// mmBase is a MIME type without parameters, lower-cased.
func mmBase(mt string) string {
	mt = strings.ToLower(strings.TrimSpace(mt))
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	return mt
}

// sniffMedia is the type of an image or video from its first bytes, for a
// request that names none (multimodalembedding@001 documents no mimeType).
func sniffMedia(b []byte) string {
	switch {
	case len(b) >= 12 && string(b[4:8]) == "ftyp":
		if string(b[8:12]) == "qt  " {
			return "video/quicktime"
		}
		return "video/mp4"
	case len(b) >= 8 && (string(b[4:8]) == "moov" || string(b[4:8]) == "mdat" || string(b[4:8]) == "wide" ||
		string(b[4:8]) == "free" || string(b[4:8]) == "skip"):
		return "video/quicktime"
	case bytes.HasPrefix(b, []byte("FLV\x01")):
		return "video/x-flv"
	case bytes.HasPrefix(b, []byte{0x30, 0x26, 0xB2, 0x75, 0x8E, 0x66, 0xCF, 0x11}):
		return "video/x-ms-wmv"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "AVI ":
		return "video/x-msvideo"
	case bytes.HasPrefix(b, []byte{0x1A, 0x45, 0xDF, 0xA3}):
		if bytes.Contains(b[:min(len(b), 64)], []byte("webm")) {
			return "video/webm"
		}
		return "video/x-matroska"
	case bytes.HasPrefix(b, []byte{0, 0, 1, 0xBA}) || bytes.HasPrefix(b, []byte{0, 0, 1, 0xB3}):
		return "video/mpeg"
	case len(b) > 376 && b[0] == 0x47 && b[188] == 0x47 && b[376] == 0x47:
		return "video/mpeg" // an MPEG transport stream
	}
	return mmBase(http.DetectContentType(b))
}
