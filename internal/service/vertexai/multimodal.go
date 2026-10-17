package vertexai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"strings"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/embedding"
	"github.com/cloudburrow/cloudburrow/internal/localai"
)

// Multimodal embeddings (#1454), from the local CLIP model: text and images
// in one vector space, over the two shapes Google documents for them.
//
//   - :predict, multimodalembedding's shape: instances of {text, image:
//     {bytesBase64Encoded | gcsUri, mimeType}, video}, parameters.dimension,
//     and predictions of {textEmbedding, imageEmbedding}.
//   - EmbedContent, with text parts, or one image part as inline_data or as
//     file_data naming a gs:// object in this instance's Cloud Storage.
//
// What CLIP cannot do is refused naming what is missing, never fabricated: a
// video, audio or PDF part needs an encoder it does not have (501), and
// multimodalembedding's 1408 dimensions are wider than its native 512 (501).

// MultimodalModelDisplayName is reported with every multimodal response.
const MultimodalModelDisplayName = "CLIP ViT-B/32, community ONNX int8 conversion " +
	"(Xenova) — NOT a Google model"

// maxMultimodalBytes bounds a multimodal request: a 20 MB image,
// base64-encoded, and room for the rest.
const maxMultimodalBytes = 28 << 20

// ObjectReader reads a Cloud Storage object for a gs:// image.
type ObjectReader func(ctx context.Context, bucket, object string) (io.ReadCloser, error)

// SetObjectReader makes gs:// images readable, from this instance's Cloud
// Storage. Without one a gs:// image is FAILED_PRECONDITION.
func (s *Server) SetObjectReader(r ObjectReader) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects = r
}

// SwapMultimodalEmbedder installs e, or with e nil unloads it with
// unavailable as the reason, and returns the one it replaced to close.
func (s *Server) SwapMultimodalEmbedder(e embedding.MultimodalEmbedder, unavailable error) embedding.MultimodalEmbedder {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.multimodal
	s.multimodal, s.multimodalUnavailable = e, unavailable
	return old
}

// MultimodalLoaded reports whether the multimodal model is served.
func (s *Server) MultimodalLoaded() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.multimodal != nil
}

func isMultimodalModel(requested string) bool {
	return strings.EqualFold(strings.TrimSpace(requested), localai.CLIPONNXID)
}

func (s *Server) multimodalEmbedder() (embedding.MultimodalEmbedder, error) {
	s.mu.RLock()
	e, why := s.multimodal, s.multimodalUnavailable
	s.mu.RUnlock()
	if e != nil {
		return e, nil
	}
	if why == nil {
		why = fmt.Errorf("%s is not loaded; download and load it on the console's Local models page "+
			"(or POST /admin/local-models/%s:load) in a build made with -tags onnx", localai.CLIPONNXID, localai.CLIPONNXID)
	}
	return nil, apierror.Unimplemented("multimodal embeddings are unavailable: %v", why)
}

// MultimodalImage is multimodalembedding's image.
type MultimodalImage struct {
	BytesBase64Encoded string `json:"bytesBase64Encoded,omitempty"`
	GCSURI             string `json:"gcsUri,omitempty"`
	MimeType           string `json:"mimeType,omitempty"`
}

// MultimodalInstance is one multimodalembedding instance.
type MultimodalInstance struct {
	Text  *string          `json:"text,omitempty"`
	Image *MultimodalImage `json:"image,omitempty"`
	// Video is accepted only to be refused by name.
	Video json.RawMessage `json:"video,omitempty"`
}

// MultimodalPredictRequest is the multimodalembedding :predict body.
type MultimodalPredictRequest struct {
	Instances  []MultimodalInstance `json:"instances"`
	Parameters *struct {
		Dimension int `json:"dimension,omitempty"`
	} `json:"parameters,omitempty"`
}

// multimodalDim checks a requested size, refusing 1408 as unimplemented
// rather than invalid: Vertex serves it, this model cannot.
func multimodalDim(field string, dim int) error {
	if dim == 1408 {
		return apierror.Unimplemented("%s 1408 is not served: %s's native size is %d; use 128, 256 or 512",
			field, localai.CLIPONNXID, embedding.CLIPDimensions)
	}
	if err := embedding.ValidateMultimodalDim(dim); err != nil {
		return apierror.InvalidArgument("%s: %v (and 1408 on Vertex)", field, err)
	}
	return nil
}

// multimodalPredict serves :predict on the multimodal model.
func (s *Server) multimodalPredict(ctx context.Context, body io.Reader) (map[string]any, error) {
	e, err := s.multimodalEmbedder()
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(body, maxMultimodalBytes+1))
	if err != nil {
		return nil, apierror.InvalidArgument("reading the request body: %v", err)
	}
	if len(raw) > maxMultimodalBytes {
		return nil, apierror.InvalidArgument("the request is larger than %d bytes", maxMultimodalBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var req MultimodalPredictRequest
	if err := dec.Decode(&req); err != nil {
		return nil, apierror.InvalidArgument(
			"malformed request body: %v; this endpoint serves multimodalembedding's :predict (text, image, parameters.dimension)", err)
	}
	if len(req.Instances) != 1 {
		return nil, apierror.InvalidArgument("exactly one instance is allowed per request, got %d", len(req.Instances))
	}
	dim := 0
	if req.Parameters != nil {
		dim = req.Parameters.Dimension
	}
	if err := multimodalDim("parameters.dimension", dim); err != nil {
		return nil, err
	}
	x := req.Instances[0]
	if len(x.Video) > 0 && string(x.Video) != "null" {
		return nil, apierror.Unimplemented("instances[0].video is not served: %s has no video encoder", localai.CLIPONNXID)
	}
	if x.Text == nil && x.Image == nil {
		return nil, apierror.InvalidArgument("instances[0] must set at least one of text and image")
	}
	pred := map[string]any{}
	if x.Text != nil {
		if strings.TrimSpace(*x.Text) == "" {
			return nil, apierror.InvalidArgument("instances[0].text is empty")
		}
		o, err := e.EmbedText(ctx, *x.Text, dim)
		if err != nil {
			return nil, embedFailure(ctx, err)
		}
		pred["textEmbedding"] = o.Values
	}
	if x.Image != nil {
		img, err := s.predictImage(ctx, x.Image)
		if err != nil {
			return nil, err
		}
		o, err := e.EmbedImage(ctx, img, dim)
		if err != nil {
			return nil, embedFailure(ctx, err)
		}
		pred["imageEmbedding"] = o.Values
	}
	return map[string]any{
		"predictions":      []map[string]any{pred},
		"model":            e.Model(),
		"modelDisplayName": MultimodalModelDisplayName,
	}, nil
}

func embedFailure(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return apierror.InvalidArgument("%v", err)
}

// predictImage decodes multimodalembedding's image: inline bytes or a gs://
// object, exactly one.
func (s *Server) predictImage(ctx context.Context, im *MultimodalImage) (image.Image, error) {
	switch {
	case im.BytesBase64Encoded != "" && im.GCSURI != "":
		return nil, apierror.InvalidArgument("instances[0].image: set one of bytesBase64Encoded and gcsUri, not both")
	case im.BytesBase64Encoded != "":
		data, err := base64.StdEncoding.DecodeString(im.BytesBase64Encoded)
		if err != nil {
			return nil, apierror.InvalidArgument("instances[0].image.bytesBase64Encoded is not base64: %v", err)
		}
		return decodeImage("instances[0].image", data, im.MimeType)
	case im.GCSURI != "":
		data, err := s.readGCS(ctx, "instances[0].image.gcsUri", im.GCSURI)
		if err != nil {
			return nil, err
		}
		return decodeImage("instances[0].image", data, im.MimeType)
	}
	return nil, apierror.InvalidArgument("instances[0].image: set bytesBase64Encoded or gcsUri")
}

func decodeImage(field string, data []byte, mimeType string) (image.Image, error) {
	if mt := strings.ToLower(mimeType); mt == "image/bmp" || mt == "image/webp" {
		return nil, apierror.Unimplemented("%s: %s is not decoded here; send PNG, JPEG or GIF", field, mimeType)
	}
	img, err := embedding.DecodeImage(data, mimeType)
	if err != nil {
		return nil, apierror.InvalidArgument("%s: %v", field, err)
	}
	return img, nil
}

// readGCS reads a gs:// object from this instance's Cloud Storage.
func (s *Server) readGCS(ctx context.Context, field, uri string) ([]byte, error) {
	rest, ok := strings.CutPrefix(uri, "gs://")
	bucket, object, _ := strings.Cut(rest, "/")
	if !ok || bucket == "" || object == "" {
		return nil, apierror.InvalidArgument("%s %q: want gs://BUCKET/OBJECT", field, uri)
	}
	s.mu.RLock()
	read := s.objects
	s.mu.RUnlock()
	if read == nil {
		return nil, apierror.FailedPrecondition("%s %q: this instance's Cloud Storage is not available to the local AI endpoint", field, uri)
	}
	rc, err := read(ctx, bucket, object)
	if err != nil {
		var ae *apierror.Error
		if errors.As(err, &ae) {
			return nil, err
		}
		return nil, apierror.FailedPrecondition("%s %q: %v", field, uri, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, embedding.MaxImageBytes+1))
	if err != nil {
		return nil, apierror.FailedPrecondition("%s %q: %v", field, uri, err)
	}
	return data, nil
}

// multimodalEmbedContent serves EmbedContent on the multimodal model: all
// text parts (joined, as the text model does), or exactly one image part.
func (s *Server) multimodalEmbedContent(ctx context.Context, req *aiplatformpb.EmbedContentRequest) (*aiplatformpb.EmbedContentResponse, error) {
	e, err := s.multimodalEmbedder()
	if err != nil {
		return nil, err
	}
	cfg := req.GetEmbedContentConfig()
	if cfg.GetDocumentOcr() {
		return nil, apierror.Unimplemented("embed_content_config.document_ocr is not served: %s has no document encoder", localai.CLIPONNXID)
	}
	if cfg.GetAudioTrackExtraction() {
		return nil, apierror.Unimplemented("embed_content_config.audio_track_extraction is not served: %s has no audio encoder", localai.CLIPONNXID)
	}
	if t, _, err := pick("task_type", req.TaskType, cfg.GetTaskType, cfg != nil && cfg.TaskType != nil); err != nil {
		return nil, err
	} else if t != aiplatformpb.EmbedContentRequest_UNSPECIFIED {
		return nil, apierror.Unimplemented("task_type %v is not served: %s was not trained with task prompts", t, localai.CLIPONNXID)
	}
	if title, _, err := pick("title", req.Title, cfg.GetTitle, cfg != nil && cfg.Title != nil); err != nil {
		return nil, err
	} else if title != "" {
		return nil, apierror.Unimplemented("title is not served: %s was not trained with titles", localai.CLIPONNXID)
	}
	dim, _, err := pick("output_dimensionality", req.OutputDimensionality, cfg.GetOutputDimensionality,
		cfg != nil && cfg.OutputDimensionality != nil)
	if err != nil {
		return nil, err
	}
	if err := multimodalDim("output_dimensionality", int(dim)); err != nil {
		return nil, err
	}
	auto, set, err := pick("auto_truncate", req.AutoTruncate, cfg.GetAutoTruncate, cfg != nil && cfg.AutoTruncate != nil)
	if err != nil {
		return nil, err
	}
	if !set {
		auto = true
	}

	parts := req.GetContent().GetParts()
	if len(parts) == 0 {
		return nil, apierror.InvalidArgument("content is required and must have at least one part")
	}
	var texts []string
	var img image.Image
	for i, p := range parts {
		field := fmt.Sprintf("content.parts[%d]", i)
		var mime string
		var data []byte
		switch d := p.GetData().(type) {
		case *aiplatformpb.Part_Text:
			texts = append(texts, d.Text)
			continue
		case *aiplatformpb.Part_InlineData:
			mime, data = d.InlineData.GetMimeType(), d.InlineData.GetData()
		case *aiplatformpb.Part_FileData:
			mime = d.FileData.GetMimeType()
		default:
			return nil, apierror.InvalidArgument("%s: only text, inline_data and file_data parts are embedded", field)
		}
		if err := mediaSupported(field, mime); err != nil {
			return nil, err
		}
		if img != nil {
			return nil, apierror.Unimplemented("%s: %s embeds one image per request; it cannot combine images into one embedding", field, localai.CLIPONNXID)
		}
		if fd := p.GetFileData(); fd != nil {
			if data, err = s.readGCS(ctx, field+".file_data.file_uri", fd.GetFileUri()); err != nil {
				return nil, err
			}
		}
		if img, err = decodeImage(field, data, mime); err != nil {
			return nil, err
		}
	}
	if img != nil && len(texts) > 0 {
		return nil, apierror.Unimplemented("content mixes text and an image: %s has separate text and image encoders, "+
			"so it cannot embed both as one input; embed each in its own request", localai.CLIPONNXID)
	}

	var o embedding.Output
	if img != nil {
		o, err = e.EmbedImage(ctx, img, int(dim))
	} else {
		text := strings.Join(texts, "\n")
		if strings.TrimSpace(text) == "" {
			return nil, apierror.InvalidArgument("content.parts: the text to embed is empty")
		}
		o, err = e.EmbedText(ctx, text, int(dim))
		if err == nil && o.Truncated && !auto {
			return nil, apierror.InvalidArgument("the text is longer than %s's %d tokens and auto_truncate is false",
				localai.CLIPONNXID, embedding.CLIPMaxTokens)
		}
	}
	if err != nil {
		return nil, embedFailure(ctx, err)
	}
	resp := &aiplatformpb.EmbedContentResponse{
		Embedding: &aiplatformpb.EmbedContentResponse_Embedding{Values: o.Values},
		Truncated: o.Truncated,
	}
	if o.TokenCount > 0 {
		resp.UsageMetadata = &aiplatformpb.UsageMetadata{PromptTokenCount: int32(o.TokenCount), TotalTokenCount: int32(o.TokenCount)}
	}
	return resp, nil
}

// mediaSupported refuses a part CLIP has no encoder for, naming the missing
// encoder, and an image type that is not decoded.
func mediaSupported(field, mime string) error {
	mt := strings.ToLower(strings.TrimSpace(mime))
	switch {
	case mt == "":
		return apierror.InvalidArgument("%s: mime_type is required", field)
	case strings.HasPrefix(mt, "video/"):
		return apierror.Unimplemented("%s: %s is not served: %s has no video encoder", field, mime, localai.CLIPONNXID)
	case strings.HasPrefix(mt, "audio/"):
		return apierror.Unimplemented("%s: %s is not served: %s has no audio encoder", field, mime, localai.CLIPONNXID)
	case mt == "application/pdf":
		return apierror.Unimplemented("%s: %s is not served: %s has no document encoder", field, mime, localai.CLIPONNXID)
	case mt == "image/png", mt == "image/jpeg", mt == "image/jpg", mt == "image/gif", mt == "image/bmp", mt == "image/webp":
		return nil
	}
	return apierror.InvalidArgument("%s: mime_type %q is not an image, video, audio or PDF type", field, mime)
}
