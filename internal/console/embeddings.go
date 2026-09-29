package console

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// EmbeddingsNote is shown on the Embeddings screen, above the output. The
// maintainer decision for #41 is that this model is labelled everywhere as
// not an official Google artifact; the screen that shows vectors is where a
// user would otherwise assume otherwise.
const EmbeddingsNote = "embeddinggemma-300m-onnx-community is a COMMUNITY ONNX int8 conversion " +
	"of EmbeddingGemma by onnx-community. It is NOT an official Google artifact, and its " +
	"vectors are not guaranteed to match Google's model. The Gemma terms apply."

// embeddingsConfigured reports whether the Embeddings screen is offered.
func (p *Playground) embeddingsConfigured() bool {
	return p.Configured() && p.EmbeddingModel != ""
}

func (p *Playground) predictPath() string {
	return fmt.Sprintf("/v1/projects/console/locations/us-central1/publishers/google/models/%s:predict",
		p.EmbeddingModel)
}

// handleEmbeddingsStatus reports what the screen needs.
func (s *Server) handleEmbeddingsStatus(w http.ResponseWriter, _ *http.Request) {
	p := s.playground
	if !p.embeddingsConfigured() {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false,
			"note": "Start CloudBurrow with -local-ai-embeddings to serve embeddings; see docs/embeddings.md."})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": true,
		"model":      p.EmbeddingModel,
		"endpoint":   "http://" + p.endpoint() + p.predictPath(),
		"official":   false,
		"note":       EmbeddingsNote,
		"taskTypes": []string{"RETRIEVAL_QUERY", "RETRIEVAL_DOCUMENT", "SEMANTIC_SIMILARITY",
			"CLASSIFICATION", "CLUSTERING", "QUESTION_ANSWERING", "FACT_VERIFICATION", "CODE_RETRIEVAL_QUERY"},
		"dimensions": []int{768, 512, 256, 128},
	})
}

// handleEmbeddingsPredict relays the screen's request to the real :predict,
// the same path and body a Vertex client sends, and returns the API's status
// and body unchanged — its 501 when the runtime is not compiled in included.
func (s *Server) handleEmbeddingsPredict(w http.ResponseWriter, r *http.Request) {
	p := s.playground
	if !p.embeddingsConfigured() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "embeddings are not configured"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || !json.Valid(body) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the request body must be JSON"})
		return
	}
	upstream, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		"http://"+p.endpoint()+p.predictPath(), strings.NewReader(string(body)))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	upstream.Header.Set("Content-Type", "application/json")
	resp, err := p.client().Do(upstream)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": "the local AI endpoint is not answering: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)
}
