package vertexai

import (
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
	if !strings.EqualFold(strings.TrimSpace(requested), localai.EmbeddingGemmaONNXID) {
		apierror.WriteJSON(w, apierror.NotFound(
			"model %q is not served for :predict; the only embedding model here is %q, "+
				"a community ONNX conversion of EmbeddingGemma that is not an official Google artifact. "+
				"No substitution is performed",
			requested, localai.EmbeddingGemmaONNXID))
		return
	}
	s.mu.RLock()
	e, why := s.embedder, s.embedUnavailable
	s.mu.RUnlock()
	if e == nil {
		if why == nil {
			why = errors.New("the embedding endpoint is not enabled; start with -local-ai-embeddings-dir")
		}
		apierror.WriteJSON(w, apierror.Unimplemented("embeddings are unavailable: %v", why))
		return
	}

	dec := json.NewDecoder(io.LimitReader(r.Body, maxRequestBytes))
	dec.DisallowUnknownFields()
	var req PredictRequest
	if err := dec.Decode(&req); err != nil {
		apierror.WriteJSON(w, apierror.InvalidArgument(
			"malformed request body: %v; this endpoint serves a documented subset of text-embedding :predict", err))
		return
	}
	if len(req.Instances) == 0 {
		apierror.WriteJSON(w, apierror.InvalidArgument("instances must not be empty"))
		return
	}
	if len(req.Instances) > maxEmbedInstances {
		apierror.WriteJSON(w, apierror.InvalidArgument("at most %d instances per request, got %d",
			maxEmbedInstances, len(req.Instances)))
		return
	}
	auto, dim := true, 0
	if p := req.Parameters; p != nil {
		if p.AutoTruncate != nil {
			auto = *p.AutoTruncate
		}
		dim = p.OutputDimensionality
	}
	if err := embedding.ValidateDim(dim); err != nil {
		apierror.WriteJSON(w, apierror.InvalidArgument("%v", err))
		return
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
			apierror.WriteJSON(w, apierror.InvalidArgument("instances[%d]: %v", i, err))
			return
		}
		chars += utf8.RuneCountInString(x.Content)
	}

	out, err := e.Embed(r.Context(), in, dim, auto)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		apierror.WriteJSON(w, apierror.InvalidArgument("%v", err))
		return
	}

	preds := make([]map[string]any, len(out))
	for i, o := range out {
		preds[i] = map[string]any{"embeddings": map[string]any{
			"values":     o.Values,
			"statistics": map[string]any{"token_count": o.TokenCount, "truncated": o.Truncated},
		}}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(ProvenanceHeader, "community; not an official Google artifact")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"predictions":      preds,
		"metadata":         map[string]any{"billableCharacterCount": chars},
		"model":            e.Model(),
		"modelDisplayName": EmbeddingModelDisplayName,
	})
}
