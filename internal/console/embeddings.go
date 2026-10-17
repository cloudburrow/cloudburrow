package console

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/localai"
)

// EmbeddingsNote is shown on the Embeddings screen, above the output. The
// maintainer decision for #41 is that this model is labelled everywhere as
// not an official Google artifact; the screen that shows vectors is where a
// user would otherwise assume otherwise.
const EmbeddingsNote = "embeddinggemma-300m-onnx-community is a COMMUNITY ONNX int8 conversion " +
	"of EmbeddingGemma by onnx-community. It is NOT an official Google artifact, and its " +
	"vectors are not guaranteed to match Google's model. The Gemma terms apply."

// MultimodalNote is shown on the Embeddings screen's image and text card.
const MultimodalNote = localai.CLIPONNXID + " is a COMMUNITY ONNX int8 conversion of OpenAI's " +
	"CLIP ViT-B/32 by Xenova, NOT a Google model: Vertex's multimodalembedding is not published. " +
	"Text and images share its 512-dimensional space; it has no video, audio or document encoder. " +
	"Download and load it on the Local models page."

// embeddingsConfigured reports whether the Embeddings screen is offered.
func (p *Playground) embeddingsConfigured() bool {
	return p.Configured() && p.EmbeddingModel != ""
}

func (p *Playground) predictPath() string { return p.embeddingPath("predict") }

// embeddingPath is the real Vertex path of one embedding method: predict
// (text-embedding :predict, a batch), embedContent (#1244, one content) or
// rawPredict (#1284, the :predict body sent unwrapped).
func (p *Playground) embeddingPath(method string) string {
	return p.embeddingPathFor(p.EmbeddingModel, method)
}

// embeddingPathFor is embeddingPath for a model the user named (#1305).
func (p *Playground) embeddingPathFor(model, method string) string {
	return fmt.Sprintf("/v1/projects/console/locations/us-central1/publishers/google/models/%s:%s",
		model, method)
}

// notServedEmbeddingModels are the Vertex embedding models a user is likely
// to type (#1305). The screen offers them so that what happens is visible:
// the API answers 404 for each, naming the one model served, and never
// substitutes it.
var notServedEmbeddingModels = []string{
	"text-embedding-005", "text-embedding-004", "text-multilingual-embedding-002",
	"textembedding-gecko@003", "gemini-embedding-001",
}

// embeddingModelName is the shape of a publisher model ID, so a typed name
// can only ever change the last path segment.
var embeddingModelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,127}$`)

// embeddingMethods are the methods the screen can send.
var embeddingMethods = []string{"predict", "embedContent", "rawPredict"}

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
		"methods":    embeddingMethods,
		"endpoints": map[string]string{
			"predict":      "http://" + p.endpoint() + p.embeddingPath("predict"),
			"embedContent": "http://" + p.endpoint() + p.embeddingPath("embedContent"),
			"rawPredict":   "http://" + p.endpoint() + p.embeddingPath("rawPredict"),
		},
		"official": false,
		"note":     EmbeddingsNote,
		"taskTypes": []string{"RETRIEVAL_QUERY", "RETRIEVAL_DOCUMENT", "SEMANTIC_SIMILARITY",
			"CLASSIFICATION", "CLUSTERING", "QUESTION_ANSWERING", "FACT_VERIFICATION", "CODE_RETRIEVAL_QUERY"},
		"dimensions": []int{768, 512, 256, 128},
		// Names to try; the API refuses each with 404 and no substitution.
		"notServed": notServedEmbeddingModels,
		// The image and text card (#1454): multimodalembedding's :predict
		// shape, on the local CLIP model, loaded on the Local models page.
		"multimodal": map[string]any{
			"model":      localai.CLIPONNXID,
			"endpoint":   "http://" + p.endpoint() + p.embeddingPathFor(localai.CLIPONNXID, "predict"),
			"dimensions": []int{512, 256, 128},
			"note":       MultimodalNote,
		},
	})
}

// handleEmbeddingsPredict relays the screen's request to the real :predict,
// to :embedContent with ?method=embedContent (#1244), or to :rawPredict with
// ?method=rawPredict (#1284), the same path and
// body a Vertex client sends, and returns the API's status and body
// unchanged — its 501 when the runtime is not compiled in included.
func (s *Server) handleEmbeddingsPredict(w http.ResponseWriter, r *http.Request) {
	p := s.playground
	if !p.embeddingsConfigured() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "embeddings are not configured"})
		return
	}
	method := r.URL.Query().Get("method")
	if method == "" {
		method = "predict"
	}
	if !slices.Contains(embeddingMethods, method) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("method %q is not one of %v", method, embeddingMethods)})
		return
	}
	// ?model= sends the request for another model ID (#1305), so that the
	// API's own 404 for text-embedding-005 or textembedding-gecko is what the
	// screen shows; nothing is substituted here either.
	model := r.URL.Query().Get("model")
	if model == "" {
		model = p.EmbeddingModel
	}
	if !embeddingModelName.MatchString(model) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("model %q is not a publisher model ID", model)})
		return
	}
	// An inline image travels base64-encoded, so the bound is the
	// multimodal model's 20 MB image limit, encoded, rather than 1 MB.
	body, err := io.ReadAll(io.LimitReader(r.Body, 28<<20))
	if err != nil || !json.Valid(body) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the request body must be JSON"})
		return
	}
	upstream, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		"http://"+p.endpoint()+p.embeddingPathFor(model, method), strings.NewReader(string(body)))
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
