package vertexai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/embedding"
	"github.com/cloudburrow/cloudburrow/internal/hostguard"
)

// Server serves the supported generateContent subset.
type Server struct {
	gen Generator
	lis listener

	// aliases maps a requested model ID to the generator's model. It is empty
	// by default: an unconfigured alias is an error, never a substitution.
	mu      sync.RWMutex
	aliases map[string]string

	// embedder serves :predict for the one embedding model identity; nil
	// makes every :predict a 501 carrying embedUnavailable.
	embedder         embedding.Embedder
	embedUnavailable error

	// multimodal serves the CLIP model (#1454), loaded on the console's
	// Local models page; objects reads its gs:// images.
	multimodal            embedding.MultimodalEmbedder
	multimodalUnavailable error
	objects               ObjectReader

	// endpoints resolves custom prediction endpoints, reached through
	// ingress with client; nil serves none (see endpoints.go).
	endpoints EndpointResolver
	ingress   func() string
	client    *http.Client
}

// NewServer returns a server. A nil generator is valid and makes every
// generation request fail with FAILED_PRECONDITION, which is the truthful
// answer when no runtime is installed.
func NewServer(gen Generator) *Server {
	return &Server{gen: gen, aliases: map[string]string{}}
}

// NewServerOn returns a server that binds addr when started.
func NewServerOn(addr string, gen Generator) *Server {
	s := NewServer(gen)
	s.lis.addr = addr
	return s
}

// Alias makes requested resolve to the generator's model.
//
// Aliasing is opt-in and recorded, so that answering a request for one model
// with another is a decision someone made and can see, rather than something
// the service did quietly.
func (s *Server) Alias(requested string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.aliases[strings.ToLower(requested)] = s.gen.Model()
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// Both shapes the official SDK produces, verified by inspecting what it
	// sends: the Vertex backend uses a project/location resource path under
	// v1beta1, and the Gemini API backend a bare model path under v1beta.
	mux.HandleFunc("POST /"+APIVersion+"/projects/{project}/locations/{location}/publishers/{publisher}/models/{model}", s.handle)
	mux.HandleFunc("POST /"+GeminiAPIVersion+"/models/{model}", s.handle)
	// Text-embedding :predict and :embedContent, which Vertex clients send
	// under v1 as well.
	mux.HandleFunc("POST /v1/projects/{project}/locations/{location}/publishers/{publisher}/models/{model}", s.handle)
	// Custom prediction containers, as Vertex endpoints (#1193).
	mux.HandleFunc("POST /v1/projects/{project}/locations/{location}/endpoints/{endpoint}", s.handleEndpoint)
	mux.HandleFunc("/", s.notFound)
	// The playground page and any browser tab can reach this port (#676).
	return hostguard.Wrap(mux)
}

// notFound answers an unrouted path with the supported surface rather than an
// empty 404, because "which paths does this serve?" is the first question.
func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	apierror.WriteJSON(w, apierror.NotFound(
		"%s %s is not served; this endpoint serves POST /%s/projects/{project}/locations/{location}/publishers/google/models/{model}:generateContent "+
			"and :streamGenerateContent, and the Gemini API equivalent under /%s/models/{model}",
		r.Method, r.URL.Path, APIVersion, GeminiAPIVersion))
}

// handle dispatches on the verb suffix. net/http's pattern syntax cannot
// express "{model}:generateContent", so the verb travels with the model
// segment and is split off here.
func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	model, verb, ok := strings.Cut(r.PathValue("model"), ":")
	if !ok {
		apierror.WriteJSON(w, apierror.NotFound(
			"missing method: expected {model}:generateContent or {model}:streamGenerateContent"))
		return
	}
	if p := r.PathValue("publisher"); p != "" && p != "google" {
		apierror.WriteJSON(w, apierror.NotFound("publisher %q is not served; only \"google\" is", p))
		return
	}

	if strings.HasPrefix(r.URL.Path, "/v1/") && verb != "predict" && verb != "embedContent" && verb != "rawPredict" && verb != "streamRawPredict" {
		apierror.WriteJSON(w, apierror.Unimplemented(
			"only :predict, :embedContent, :rawPredict and :streamRawPredict are served under /v1; generation is served under /%s", APIVersion))
		return
	}

	switch verb {
	case "generateContent":
		s.generate(w, r, model, false)
	case "streamGenerateContent":
		s.generate(w, r, model, true)
	case "predict":
		if r.PathValue("project") == "" {
			apierror.WriteJSON(w, apierror.Unimplemented(":predict is served only on the Vertex path"))
			return
		}
		s.predict(w, r, model)
	case "embedContent":
		if r.PathValue("project") == "" {
			apierror.WriteJSON(w, apierror.Unimplemented(":embedContent is served only on the Vertex path"))
			return
		}
		s.embedContentREST(w, r, model)
	case "rawPredict":
		if r.PathValue("project") == "" {
			apierror.WriteJSON(w, apierror.Unimplemented(":rawPredict is served only on the Vertex path"))
			return
		}
		s.rawPredictREST(w, r, model)
	case "streamRawPredict":
		if r.PathValue("project") == "" {
			apierror.WriteJSON(w, apierror.Unimplemented(":streamRawPredict is served only on the Vertex path"))
			return
		}
		body, err := readBody(r)
		if err != nil {
			apierror.WriteJSON(w, err)
			return
		}
		s.streamRawPredictREST(w, r, target{project: r.PathValue("project"), location: r.PathValue("location"),
			publisher: "google", model: model}, body)
	case "countTokens":
		// Refused rather than estimated. See the package doc.
		apierror.WriteJSON(w, apierror.Unimplemented(
			"countTokens is not implemented: the runtime exposes no tokenizer to this service, "+
				"and a count computed by any other means would not be the model's"))
	default:
		apierror.WriteJSON(w, apierror.Unimplemented("method %q is not implemented", verb))
	}
}

// resolveModel maps a requested model to the one that will run, refusing
// anything that is not the configured model or an explicit alias.
func (s *Server) resolveModel(requested string) (string, error) {
	if s.gen == nil {
		return "", apierror.FailedPrecondition(
			"no local generation runtime is configured: build it with `make litert-lm` and start " +
				"CloudBurrow with the model path set (see docs/generation.md)")
	}
	actual := s.gen.Model()
	req := strings.ToLower(strings.TrimSpace(requested))
	if req == strings.ToLower(actual) {
		return actual, nil
	}
	s.mu.RLock()
	target, ok := s.aliases[req]
	s.mu.RUnlock()
	if ok {
		return target, nil
	}
	return "", apierror.NotFound(
		"model %q is not available here; this endpoint serves %q. "+
			"No substitution is performed: a Gemini request is not answered by a Gemma model "+
			"unless an alias is configured explicitly",
		requested, actual)
}

func (s *Server) generate(w http.ResponseWriter, r *http.Request, requested string, stream bool) {
	if !stream {
		resp, err := s.generateUnary(r.Context(), requested, r.Body)
		if err != nil {
			apierror.WriteJSON(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
		return
	}
	model, chunks, errFn, err := s.startGeneration(r.Context(), requested, r.Body)
	if err != nil {
		apierror.WriteJSON(w, err)
		return
	}
	s.writeStream(w, r, model, chunks, errFn)
}

// startGeneration resolves the model, decodes and validates the JSON request
// and starts the generator. It is the one entry point for the JSON routes and
// the gRPC PredictionService, unary and streaming alike, so every transport
// resolves, refuses and honours the same fields the same way.
func (s *Server) startGeneration(ctx context.Context, requested string, body io.Reader) (string, <-chan Chunk, func() error, error) {
	model, err := s.resolveModel(requested)
	if err != nil {
		return "", nil, nil, err
	}
	req, err := decodeRequest(body)
	if err != nil {
		return "", nil, nil, err
	}
	chunks, errFn, err := s.gen.Generate(ctx, Request{Prompt: promptOf(req)})
	if err != nil {
		return "", nil, nil, apierror.Internal(err, "starting the local runtime failed")
	}
	return model, chunks, errFn, nil
}

// generateUnary serves one generateContent, for the JSON route and the gRPC
// PredictionService alike, so the two answer the same request the same way.
// body is the JSON request; the stream is assembled into one response.
func (s *Server) generateUnary(ctx context.Context, requested string, body io.Reader) (*GenerateContentResponse, error) {
	model, chunks, errFn, err := s.startGeneration(ctx, requested, body)
	if err != nil {
		return nil, err
	}
	var text strings.Builder
	finish := FinishReasonStop
	for c := range chunks {
		if c.Last {
			finish = c.FinishReason
			continue
		}
		text.WriteString(c.Text)
	}
	if err := generationError(ctx, errFn); err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return textResponse(model, strings.TrimRight(text.String(), "\n"), finish), nil
}

// writeStream writes server-sent events in the shape the SDK parses.
//
// Each event is one complete GenerateContentResponse, which is how Vertex
// streams: a client that reads only the first event still has a valid
// response. The finish reason rides the last event and no other.
func (s *Server) writeStream(w http.ResponseWriter, r *http.Request, model string,
	chunks <-chan Chunk, errFn func() error) {

	flusher, ok := w.(http.Flusher)
	if !ok {
		apierror.WriteJSON(w, apierror.Internal(nil, "the response writer cannot stream"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	wrote := false
	for c := range chunks {
		var resp *GenerateContentResponse
		if c.Last {
			// A final event with an empty text part carries the finish
			// reason. Vertex sends one; the SDK expects to see the reason
			// on a candidate rather than inferring it from the stream
			// ending.
			resp = textResponse(model, "", c.FinishReason)
		} else {
			resp = textResponse(model, c.Text, "")
		}
		b, err := json.Marshal(resp)
		if err != nil {
			return
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return // the client went away
		}
		flusher.Flush()
		wrote = true
	}

	// An error after bytes are on the wire cannot become a status code. It is
	// sent as a final event so the client sees a failure rather than a stream
	// that merely stopped.
	if err := generationError(r.Context(), errFn); err != nil {
		if !wrote {
			// Nothing written yet, so a real status code is still possible.
			apierror.WriteJSON(w, err)
			return
		}
		// The event carries the body WriteJSON would have sent: Google's
		// status mapping, with the canonical code's name in status.
		fmt.Fprintf(w, "data: %s\n\n", apierror.MarshalJSON(err))
		flusher.Flush()
	}
}

// generationError reports cancellation as cancellation and anything else as
// what it was. A client that hung up has not caused a server error.
func generationError(ctx context.Context, errFn func() error) error {
	if err := ctx.Err(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return apierror.From(fmt.Errorf("the request deadline passed before generation finished"))
		}
		return nil // the client cancelled; there is nobody to tell
	}
	if err := errFn(); err != nil {
		// The cause is carried into the message rather than kept internal.
		// This runtime is the developer's own subprocess on their own
		// machine: its stderr is the only actionable part of the failure,
		// and hiding it would leave them with "internal error" for a
		// missing model file.
		return apierror.Internal(err, "local generation failed: %v", err)
	}
	return nil
}

// Model reports the model that will run, for startup output.
func (s *Server) Model() string {
	if s.gen == nil {
		return ""
	}
	return s.gen.Model()
}
