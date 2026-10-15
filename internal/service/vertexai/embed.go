package vertexai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/embedding"
	"github.com/cloudburrow/cloudburrow/internal/localai"
)

// EmbeddingModelDisplayName is reported with every embedding response, so a
// client that logs the response records what actually produced the vectors.
const EmbeddingModelDisplayName = "EmbeddingGemma 300M, community ONNX int8 conversion " +
	"(onnx-community) — NOT an official Google artifact"

// ProvenanceHeader carries the model's provenance on every embedding response.
const ProvenanceHeader = "X-CloudBurrow-Model-Provenance"

// SetEmbedder installs the embedding runtime. unavailable, when e is nil,
// is the reason given with the 501 every :predict then receives.
func (s *Server) SetEmbedder(e embedding.Embedder, unavailable error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.embedder, s.embedUnavailable = e, unavailable
}

// SwapEmbedder installs e, or with e nil unloads the runtime with
// unavailable as the reason, and returns the embedder it replaced for the
// caller to close (#1306).
func (s *Server) SwapEmbedder(e embedding.Embedder, unavailable error) embedding.Embedder {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.embedder
	s.embedder, s.embedUnavailable = e, unavailable
	return old
}

// EmbedderLoaded reports whether an embedding runtime is installed.
func (s *Server) EmbedderLoaded() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.embedder != nil
}

// PredictRequest is the Vertex text-embedding :predict subset served.
type PredictRequest struct {
	Instances  []PredictInstance `json:"instances"`
	Parameters *PredictParams    `json:"parameters,omitempty"`
}

// PredictInstance is one text. Both spellings of task_type are accepted,
// because Vertex documents snake_case and clients send either.
type PredictInstance struct {
	Content       string `json:"content"`
	TaskType      string `json:"task_type,omitempty"`
	TaskTypeCamel string `json:"taskType,omitempty"`
	Title         string `json:"title,omitempty"`
}

// PredictParams are the supported parameters.
type PredictParams struct {
	AutoTruncate         *bool `json:"autoTruncate,omitempty"`
	OutputDimensionality int   `json:"outputDimensionality,omitempty"`
}

// maxEmbedInstances bounds a request. Vertex's limit for text embeddings is
// 250; this runtime embeds on a CPU one at a time, so it is lower.
const maxEmbedInstances = 32

func (s *Server) predict(w http.ResponseWriter, r *http.Request, requested string) {
	resp, err := s.embedPredict(r.Context(), requested, r.Body)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		apierror.WriteJSON(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(ProvenanceHeader, provenanceValue)
	_ = json.NewEncoder(w).Encode(resp)
}

// provenanceValue is ProvenanceHeader's value, sent as gRPC metadata too.
const provenanceValue = "community; not an official Google artifact"

// checkEmbeddingModel refuses any model but the one embedding identity
// served, for :predict and embedContent alike. No substitution is performed.
func checkEmbeddingModel(requested, verb string) error {
	if strings.EqualFold(strings.TrimSpace(requested), localai.EmbeddingGemmaONNXID) {
		return nil
	}
	return apierror.NotFound(
		"model %q is not served for %s; the only embedding model here is %q, "+
			"a community ONNX conversion of EmbeddingGemma that is not an official Google artifact. "+
			"No substitution is performed",
		requested, verb, localai.EmbeddingGemmaONNXID)
}

// embeddingsUnavailable is the 501 every embedding method answers when the
// runtime is not installed; why is the reason SetEmbedder was given.
func embeddingsUnavailable(why error) error {
	if why == nil {
		why = errors.New("the embedding endpoint is not enabled; start with -local-ai-embeddings-dir")
	}
	return apierror.Unimplemented("embeddings are unavailable: %v", why)
}

// embedPredict serves one text-embedding :predict, for the JSON route and the
// gRPC PredictionService alike, so the two answer the same request the same
// way. body is the JSON request.
func (s *Server) embedPredict(ctx context.Context, requested string, body io.Reader) (map[string]any, error) {
	e, err := s.embedderFor(requested, ":predict")
	if err != nil {
		return nil, err
	}

	dec := json.NewDecoder(io.LimitReader(body, maxRequestBytes))
	dec.DisallowUnknownFields()
	var req PredictRequest
	if err := dec.Decode(&req); err != nil {
		return nil, apierror.InvalidArgument(
			"malformed request body: %v; this endpoint serves a documented subset of text-embedding :predict", err)
	}
	if len(req.Instances) == 0 {
		return nil, apierror.InvalidArgument("instances must not be empty")
	}
	if len(req.Instances) > maxEmbedInstances {
		return nil, apierror.InvalidArgument("at most %d instances per request, got %d",
			maxEmbedInstances, len(req.Instances))
	}
	auto, dim := true, 0
	if p := req.Parameters; p != nil {
		if p.AutoTruncate != nil {
			auto = *p.AutoTruncate
		}
		dim = p.OutputDimensionality
	}
	if err := embedding.ValidateDim(dim); err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	in := make([]embedding.Input, len(req.Instances))
	chars := 0
	for i, x := range req.Instances {
		tt := x.TaskType
		if tt == "" {
			tt = x.TaskTypeCamel
		}
		in[i] = embedding.Input{Content: x.Content, TaskType: tt, Title: x.Title}
		if _, err := embedding.Prompt(in[i]); err != nil {
			return nil, apierror.InvalidArgument("instances[%d]: %v", i, err)
		}
		chars += utf8.RuneCountInString(x.Content)
	}

	out, err := e.Embed(ctx, in, dim, auto)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, apierror.InvalidArgument("%v", err)
	}

	preds := make([]map[string]any, len(out))
	for i, o := range out {
		preds[i] = map[string]any{"embeddings": map[string]any{
			"values":     o.Values,
			"statistics": map[string]any{"token_count": o.TokenCount, "truncated": o.Truncated},
		}}
	}
	return map[string]any{
		"predictions":      preds,
		"metadata":         map[string]any{"billableCharacterCount": chars},
		"model":            e.Model(),
		"modelDisplayName": EmbeddingModelDisplayName,
	}, nil
}
