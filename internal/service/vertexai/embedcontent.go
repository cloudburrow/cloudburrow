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
// parts. file_data (a gs:// URI), video, audio, PDF, document_ocr and
// audio_track_extraction are refused, naming the field.

// embedContentRequestBytes bounds a REST :embedContent body, which may carry
// images as base64: 20 MiB, the inline request size Vertex documents for
// Gemini. gRPC's bound is the transport's 32 MiB.
const embedContentRequestBytes = 20 << 20

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
	parts, images, err := embedParts(req.GetContent(), multimodal, name)
	if err != nil {
		return nil, err
	}
	cfg := req.GetEmbedContentConfig()
	if cfg.GetDocumentOcr() {
		if multimodal {
			return nil, apierror.InvalidArgument(
				"embed_content_config.document_ocr is not supported: %s embeds text and images; PDF input is not served", name)
		}
		return nil, apierror.InvalidArgument(
			"embed_content_config.document_ocr is not supported: %s embeds text only", name)
	}
	if cfg.GetAudioTrackExtraction() {
		if multimodal {
			return nil, apierror.InvalidArgument(
				"embed_content_config.audio_track_extraction is not supported: %s embeds text and images; video and audio input are not served", name)
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
	if images > 0 {
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

// embedParts are content's parts: text, and for a multimodal model images.
// Text-only content is joined with newlines into the one text the model
// embeds; images returns how many images there are.
func embedParts(c *aiplatformpb.Content, multimodal bool, name string) ([]embedding.Part, int, error) {
	if c == nil || len(c.GetParts()) == 0 {
		return nil, 0, apierror.InvalidArgument("content is required and must have at least one text part")
	}
	parts := make([]embedding.Part, len(c.GetParts()))
	images := 0
	text := ""
	for i, p := range c.GetParts() {
		switch d := p.GetData().(type) {
		case *aiplatformpb.Part_Text:
			parts[i] = embedding.Part{Text: d.Text}
			text += d.Text
			continue
		case *aiplatformpb.Part_InlineData:
			if !multimodal {
				break
			}
			mt := d.InlineData.GetMimeType()
			if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(mt)), "image/") {
				return nil, 0, apierror.InvalidArgument(
					"content.parts[%d].inline_data.mime_type %q is not supported: %s embeds text and images (%s); video, audio and PDF input are not served",
					i, mt, name, strings.Join(embedding.ImageMIMETypes, ", "))
			}
			img, err := embedding.DecodeImage(mt, d.InlineData.GetData())
			if err != nil {
				return nil, 0, apierror.InvalidArgument("content.parts[%d].inline_data: %v", i, err)
			}
			parts[i] = embedding.Part{Image: img}
			images++
			if images > embedding.MaxImages {
				return nil, 0, apierror.InvalidArgument(
					"content.parts[%d]: at most %d images are embedded in one request", i, embedding.MaxImages)
			}
			continue
		case *aiplatformpb.Part_FileData:
			if !multimodal {
				break
			}
			return nil, 0, apierror.InvalidArgument(
				"content.parts[%d].file_data is not supported: send the image's bytes as inline_data; %s does not read %q",
				i, name, d.FileData.GetFileUri())
		}
		if multimodal {
			return nil, 0, apierror.InvalidArgument(
				"content.parts[%d]: only text and inline_data image parts are supported; %s embeds text and images", i, name)
		}
		return nil, 0, apierror.InvalidArgument(
			"content.parts[%d]: only text parts are supported; %s embeds text only", i, name)
	}
	if images == 0 && strings.TrimSpace(text) == "" {
		return nil, 0, apierror.InvalidArgument("content.parts: the text to embed is empty")
	}
	return parts, images, nil
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
