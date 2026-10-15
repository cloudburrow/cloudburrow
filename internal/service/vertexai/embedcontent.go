package vertexai

import (
	"context"
	"io"
	"net/http"
	"strings"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/embedding"
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

// strictJSON rejects a field the request type does not have, as Vertex does.
var strictJSON = protojson.UnmarshalOptions{}

// embedContentREST serves POST .../models/{model}:embedContent.
func (s *Server) embedContentREST(w http.ResponseWriter, r *http.Request, requested string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes))
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

	text, err := embedText(req.GetContent())
	if err != nil {
		return nil, err
	}
	cfg := req.GetEmbedContentConfig()
	if cfg.GetDocumentOcr() {
		return nil, apierror.InvalidArgument(
			"embed_content_config.document_ocr is not supported: %s embeds text only", EmbeddingModelDisplayName)
	}
	if cfg.GetAudioTrackExtraction() {
		return nil, apierror.InvalidArgument(
			"embed_content_config.audio_track_extraction is not supported: %s embeds text only", EmbeddingModelDisplayName)
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
	in := embedding.Input{Content: text, TaskType: taskName, Title: title}
	if _, err := embedding.Prompt(in); err != nil {
		return nil, apierror.InvalidArgument("task_type: %v", err)
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

// embedText is the text of content: every part must be text, and the parts
// are joined with newlines into the one text the model embeds.
func embedText(c *aiplatformpb.Content) (string, error) {
	if c == nil || len(c.GetParts()) == 0 {
		return "", apierror.InvalidArgument("content is required and must have at least one text part")
	}
	texts := make([]string, len(c.GetParts()))
	for i, p := range c.GetParts() {
		if _, ok := p.GetData().(*aiplatformpb.Part_Text); !ok {
			return "", apierror.InvalidArgument(
				"content.parts[%d]: only text parts are supported; %s embeds text only", i, EmbeddingModelDisplayName)
		}
		texts[i] = p.GetText()
	}
	text := strings.Join(texts, "\n")
	if strings.TrimSpace(text) == "" {
		return "", apierror.InvalidArgument("content.parts: the text to embed is empty")
	}
	return text, nil
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
	if err := checkEmbeddingModel(requested, verb); err != nil {
		return nil, err
	}
	s.mu.RLock()
	e, why := s.embedder, s.embedUnavailable
	s.mu.RUnlock()
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
