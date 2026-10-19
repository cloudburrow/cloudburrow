package vertexai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/embedding"
	"github.com/cloudburrow/cloudburrow/internal/localai"
)

// PredictionService.EmbedContent (#1244), over REST
// ({model}:embedContent) and gRPC alike, from the same local embedding model
// :predict uses. Both protocols decode into aiplatformpb.EmbedContentRequest
// and call embedContent, so a request is validated, refused and answered the
// same way whichever carried it.
//
// What the model can do is honoured: text content, title, task_type (the
// model card's prompt prefixes), output_dimensionality (the Matryoshka sizes)
// and auto_truncate, whether set at the top level or in embed_content_config.
// What it cannot is refused, naming the field: a non-text part, document_ocr
// and audio_track_extraction (EmbeddingGemma embeds text only).
//
// EmbeddingGemma 2 (#1454) also embeds images: an inline_data part holding a
// PNG or JPEG image, up to embedding.MaxImages of them, alone or between text
// parts, into one vector, as Google's gemini-embedding-2 embeds a Content's
// parts. It embeds a WAV audio clip the same way (#1651), and reads a
// file_data part's gs:// object from this instance's Cloud Storage as if its
// bytes were inline_data. Video, PDF, MP3, document_ocr and
// audio_track_extraction are refused, naming the field.

// embedContentRequestBytes bounds a REST :embedContent body, which may carry
// images as base64: 20 MiB, the inline request size Vertex documents for
// Gemini. gRPC's bound is the transport's 32 MiB.
const embedContentRequestBytes = 20 << 20

// fileDataBytes bounds a file_data object read from Cloud Storage: room for
// Google's 180 seconds of uncompressed stereo audio at 48 kHz.
const fileDataBytes = 64 << 20

// Objects reads objects from this instance's Cloud Storage, for file_data
// parts (#1651): the object's bytes and its content type.
type Objects interface {
	Read(ctx context.Context, bucket, object string, limit int64) ([]byte, string, error)
}

// UseStorage makes EmbedContent read file_data's gs:// objects through objs;
// without it a file_data part says Cloud Storage is not enabled.
func (s *Server) UseStorage(objs Objects) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects = objs
}

// strictJSON rejects a field the request type does not have, as Vertex does.
var strictJSON = protojson.UnmarshalOptions{}

// embedContentREST serves POST .../models/{model}:embedContent.
func (s *Server) embedContentREST(w http.ResponseWriter, r *http.Request, requested string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, embedContentRequestBytes))
	if err != nil {
		apierror.WriteJSON(w, apierror.InvalidArgument("reading the request body: %v", err))
		return
	}
	req := &aiplatformpb.EmbedContentRequest{}
	if err := strictJSON.Unmarshal(body, req); err != nil {
		apierror.WriteJSON(w, apierror.InvalidArgument(
			"malformed request body: %v; this endpoint serves a documented subset of embedContent", err))
		return
	}
	// The model travels in the path; a body that names another is refused.
	if m := req.GetModel(); m != "" {
		t, err := embedContentModel(m)
		if err != nil {
			apierror.WriteJSON(w, err)
			return
		}
		if t.model != requested {
			apierror.WriteJSON(w, apierror.InvalidArgument("model %q in the body does not match the path's model %q", m, requested))
			return
		}
	}
	resp, err := s.embedContent(r.Context(), requested, req)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		apierror.WriteJSON(w, err)
		return
	}
	out, err := protojson.Marshal(resp)
	if err != nil {
		apierror.WriteJSON(w, apierror.Internal(err, "encoding the response: %v", err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(ProvenanceHeader, provenanceValue)
	_, _ = w.Write(out)
}

// embedContent embeds one EmbedContentRequest. requested is the model ID,
// from the REST path or the gRPC request's resource name.
func (s *Server) embedContent(ctx context.Context, requested string, req *aiplatformpb.EmbedContentRequest) (*aiplatformpb.EmbedContentResponse, error) {
	e, err := s.embedderFor(requested, ":embedContent")
	if err != nil {
		return nil, err
	}

	name := embeddingDisplayName(e.Model())
	multimodal := embedding.EmbedsImages(e)
	cfg := req.GetEmbedContentConfig()
	if cfg.GetDocumentOcr() {
		if multimodal {
			return nil, apierror.InvalidArgument(
				"embed_content_config.document_ocr is not supported: %s embeds text, images and audio; PDF input is not served", name)
		}
		return nil, apierror.InvalidArgument(
			"embed_content_config.document_ocr is not supported: %s embeds text only", name)
	}
	if cfg.GetAudioTrackExtraction() {
		if multimodal {
			return nil, apierror.InvalidArgument(
				"embed_content_config.audio_track_extraction is not supported: %s embeds text, images and audio; video input is not served", name)
		}
		return nil, apierror.InvalidArgument(
			"embed_content_config.audio_track_extraction is not supported: %s embeds text only", name)
	}

	// Each option may be set at the top level (deprecated) or in
	// embed_content_config; both set to different values is ambiguous.
	title, _, err := pick("title", req.Title, cfg.GetTitle, cfg != nil && cfg.Title != nil)
	if err != nil {
		return nil, err
	}
	task, _, err := pick("task_type", req.TaskType, cfg.GetTaskType, cfg != nil && cfg.TaskType != nil)
	if err != nil {
		return nil, err
	}
	dim, _, err := pick("output_dimensionality", req.OutputDimensionality, cfg.GetOutputDimensionality,
		cfg != nil && cfg.OutputDimensionality != nil)
	if err != nil {
		return nil, err
	}
	auto, set, err := pick("auto_truncate", req.AutoTruncate, cfg.GetAutoTruncate, cfg != nil && cfg.AutoTruncate != nil)
	if err != nil {
		return nil, err
	}
	s.mu.RLock()
	objs := s.objects
	s.mu.RUnlock()
	parts, media, err := embedParts(ctx, req.GetContent(), mediaSupport{images: multimodal, audio: embedding.EmbedsAudio(e), objects: objs}, name)
	if err != nil {
		return nil, err
	}
	if !set {
		auto = true // Vertex's default
	}

	if err := embedding.ValidateDim(int(dim)); err != nil {
		return nil, apierror.InvalidArgument("output_dimensionality: %v", err)
	}
	taskName := ""
	if task != aiplatformpb.EmbedContentRequest_UNSPECIFIED {
		taskName = task.String()
	}
	in := embedding.Input{TaskType: taskName, Title: title}
	if _, err := embedding.Prompt(embedding.Input{Content: "x", TaskType: taskName}); err != nil {
		return nil, apierror.InvalidArgument("task_type: %v", err)
	}
	if media > 0 {
		in.Parts = parts
	} else {
		texts := make([]string, len(parts))
		for i, p := range parts {
			texts[i] = p.Text
		}
		in.Content = strings.Join(texts, "\n")
	}

	out, err := e.Embed(ctx, []embedding.Input{in}, int(dim), auto)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, apierror.InvalidArgument("%v", err)
	}
	o := out[0]
	return &aiplatformpb.EmbedContentResponse{
		Embedding: &aiplatformpb.EmbedContentResponse_Embedding{Values: o.Values},
		UsageMetadata: &aiplatformpb.UsageMetadata{
			PromptTokenCount: int32(o.TokenCount),
			TotalTokenCount:  int32(o.TokenCount),
		},
		Truncated: o.Truncated,
	}, nil
}

// mediaSupport is what a model embeds besides text, and where file_data
// parts are read from.
type mediaSupport struct {
	images, audio bool
	objects       Objects
}

// what names what the model embeds, for an error.
func (m mediaSupport) what() string {
	switch {
	case m.audio:
		return "text, images and audio"
	case m.images:
		return "text and images"
	}
	return "text only"
}

// embedParts are content's parts: text, and for a multimodal model images
// and audio clips, inline or read from Cloud Storage. Text-only content is
// joined with newlines into the one text the model embeds; media returns how
// many images and clips there are.
func embedParts(ctx context.Context, c *aiplatformpb.Content, m mediaSupport, name string) ([]embedding.Part, int, error) {
	if c == nil || len(c.GetParts()) == 0 {
		return nil, 0, apierror.InvalidArgument("content is required and must have at least one text part")
	}
	parts := make([]embedding.Part, len(c.GetParts()))
	images, clips := 0, 0
	samples := 0 // of audio, all clips together
	text := ""
	for i, p := range c.GetParts() {
		field, mt := "", ""
		var data []byte
		switch d := p.GetData().(type) {
		case *aiplatformpb.Part_Text:
			parts[i] = embedding.Part{Text: d.Text}
			text += d.Text
			continue
		case *aiplatformpb.Part_InlineData:
			field, mt, data = "inline_data", d.InlineData.GetMimeType(), d.InlineData.GetData()
		case *aiplatformpb.Part_FileData:
			if !m.images {
				break
			}
			var err error
			field = "file_data"
			if mt, data, err = readFileData(ctx, i, d.FileData, m.objects); err != nil {
				return nil, 0, err
			}
		}
		if field == "" || !m.images {
			if m.images {
				return nil, 0, apierror.InvalidArgument(
					"content.parts[%d]: only text, inline_data and file_data parts are supported; %s embeds %s", i, name, m.what())
			}
			return nil, 0, apierror.InvalidArgument(
				"content.parts[%d]: only text parts are supported; %s embeds text only", i, name)
		}
		switch {
		case strings.HasPrefix(strings.ToLower(strings.TrimSpace(mt)), "image/"):
			img, err := embedding.DecodeImage(mt, data)
			if err != nil {
				return nil, 0, apierror.InvalidArgument("content.parts[%d].%s: %v", i, field, err)
			}
			parts[i] = embedding.Part{Image: img}
			images++
			if images > embedding.MaxImages {
				return nil, 0, apierror.InvalidArgument(
					"content.parts[%d]: at most %d images are embedded in one request", i, embedding.MaxImages)
			}
		case m.audio && embedding.IsAudioMIMEType(mt):
			clip, err := embedding.DecodeAudio(mt, data)
			if err != nil {
				return nil, 0, apierror.InvalidArgument("content.parts[%d].%s: %v", i, field, err)
			}
			parts[i] = embedding.Part{Audio: clip}
			clips++
			if samples += len(clip); samples > embedding.MaxAudioSeconds*embedding.AudioSampleRate {
				return nil, 0, apierror.InvalidArgument(
					"content.parts[%d]: the audio adds up to %.1f seconds; at most %d seconds are embedded in one request",
					i, float64(samples)/embedding.AudioSampleRate, embedding.MaxAudioSeconds)
			}
		default:
			types := append(append([]string(nil), embedding.ImageMIMETypes...), embedding.AudioMIMETypes...)
			notServed := "video, MP3 and PDF input are not served"
			if !m.audio {
				types, notServed = embedding.ImageMIMETypes, "video, audio and PDF input are not served"
			}
			return nil, 0, apierror.InvalidArgument(
				"content.parts[%d].%s.mime_type %q is not supported: %s embeds %s (%s); %s",
				i, field, mt, name, m.what(), strings.Join(types, ", "), notServed)
		}
	}
	if images+clips == 0 && strings.TrimSpace(text) == "" {
		return nil, 0, apierror.InvalidArgument("content.parts: the text to embed is empty")
	}
	return parts, images + clips, nil
}

// readFileData reads a file_data part's gs:// object from this instance's
// Cloud Storage, with its mime_type, or the object's content type when the
// part has none.
func readFileData(ctx context.Context, i int, fd *aiplatformpb.FileData, objs Objects) (string, []byte, error) {
	uri := fd.GetFileUri()
	rest, ok := strings.CutPrefix(uri, "gs://")
	bucket, object, _ := strings.Cut(rest, "/")
	if !ok || bucket == "" || object == "" {
		return "", nil, apierror.InvalidArgument(
			"content.parts[%d].file_data.file_uri must be gs://{bucket}/{object}, not %q", i, uri)
	}
	if objs == nil {
		return "", nil, apierror.FailedPrecondition(
			"content.parts[%d].file_data: reading %s needs this instance's Cloud Storage, which is not enabled; start it with the storage service, or send the bytes as inline_data", i, uri)
	}
	data, contentType, err := objs.Read(ctx, bucket, object, fileDataBytes)
	if err != nil {
		return "", nil, err
	}
	mt := fd.GetMimeType()
	if strings.TrimSpace(mt) == "" {
		mt = contentType
	}
	return mt, data, nil
}

// pick reconciles an option set at the top level of EmbedContentRequest with
// the same option in embed_content_config.
func pick[T comparable](field string, top *T, fromCfg func() T, inCfg bool) (v T, set bool, err error) {
	switch {
	case top != nil && inCfg && *top != fromCfg():
		return v, false, apierror.InvalidArgument(
			"%s and embed_content_config.%s are both set, to different values; set one", field, field)
	case inCfg:
		return fromCfg(), true, nil
	case top != nil:
		return *top, true, nil
	}
	return v, false, nil
}

// embedderFor checks the requested model and returns the runtime, with the
// same NOT_FOUND and 501 :predict gives.
func (s *Server) embedderFor(requested, verb string) (embedding.Embedder, error) {
	id, err := embeddingModelID(requested, verb)
	if err != nil {
		return nil, err
	}
	s.mu.RLock()
	e, why := s.embedders[id], s.embedUnavailable[id]
	s.mu.RUnlock()
	if e == nil && why == nil && id == localai.EmbeddingGemma2ONNXID {
		why = errors.New(id + " is not loaded: download and load it on the console's Vertex AI → Local models page " +
			"(or POST /admin/local-models/" + id + "/download, then /load), in an instance started with -local-ai-embeddings")
	}
	if e == nil {
		return nil, embeddingsUnavailable(why)
	}
	return e, nil
}

// embedContentModel is the model ID in an EmbedContentRequest.model, which
// is a publisher model resource name.
func embedContentModel(name string) (target, error) {
	t, err := parseTarget("model", name)
	if err != nil {
		return t, err
	}
	if t.model == "" {
		return t, apierror.InvalidArgument(
			"model %q: EmbedContent is served only on publisher models (projects/{p}/locations/{l}/publishers/google/models/{m})", name)
	}
	return t, t.checkPublisher()
}
