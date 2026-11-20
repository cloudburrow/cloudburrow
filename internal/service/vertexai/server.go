package vertexai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/cmek"
	"github.com/cloudburrow/cloudburrow/internal/embedding"
	"github.com/cloudburrow/cloudburrow/internal/hostguard"
	"github.com/cloudburrow/cloudburrow/internal/lro"
	"github.com/cloudburrow/cloudburrow/internal/modelgarden"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// Server serves the supported generateContent subset.
type Server struct {
	gen Generator
	lis listener

	// aliases maps a requested model ID to the generator's model. It is empty
	// by default: an unconfigured alias is an error, never a substitution.
	mu      sync.RWMutex
	aliases map[string]string

	// embedders serve :predict and :embedContent, by embedding model ID
	// (EmbeddingGemma and EmbeddingGemma 2, #1454); a model with none
	// answers a 501 carrying its embedUnavailable reason.
	embedders        map[string]embedding.Embedder
	embedUnavailable map[string]error

	// endpoints resolves custom prediction endpoints, reached through
	// ingress with client; nil serves none (see endpoints.go).
	endpoints EndpointResolver
	ingress   func() string
	client    *http.Client

	// objects reads file_data's gs:// objects (#1651); nil without Cloud
	// Storage.
	objects Objects

	// garden serves Model Garden's Hugging Face deployments (#1840,
	// modelgarden.go), with their operations and REST transcoder.
	garden   *modelgarden.Manager
	ops      *lro.Store
	restOnce sync.Once
	rest     *rest.Transcoder

	// docai is Document AI's processors (documentai.go, #1799); nil until
	// first used or SetDocumentAIState. Guarded by mu.
	docai *docAIStore
	// speech is Speech-to-Text's recognizers (speechrecognizers.go,
	// #1799); nil until first used or SetSpeechState. Guarded by mu.
	speech *speechStore
	// keys are Document AI's and Speech-to-Text's customer-managed keys
	// (cmek.go, #1950), set by UseCMEK.
	keys atomic.Pointer[cmek.Keys]
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
	// Cloud Vision's text features, for BigQuery's ML.ANNOTATE_IMAGE
	// (vision.go, #1773).
	mux.HandleFunc("POST /v1/images:annotate", s.annotateImages)
	// Speech-to-Text v2's recognize, for BigQuery's ML.TRANSCRIBE
	// (speech.go, #1799).
	mux.HandleFunc("POST /v2/projects/{project}/locations/{location}/recognizers/{recognizer}", s.recognize)
	// Translation v3's translateText and detectLanguage, for BigQuery's
	// ML.TRANSLATE (translate.go, #1799).
	mux.HandleFunc("POST /v3/projects/{project}/locations/{location}", s.translation)
	// Natural Language v1's documents methods, for BigQuery's
	// ML.UNDERSTAND_TEXT (language.go, #1799).
	for _, v := range []string{"analyzeEntities", "analyzeEntitySentiment", "analyzeSentiment", "analyzeSyntax", "classifyText", "annotateText", "moderateText"} {
		mux.HandleFunc("POST /v1/documents:"+v, s.language)
	}
	// Model Garden's OpenAI-compatible path on its endpoints (#1840).
	mux.HandleFunc("POST /v1beta1/projects/{project}/locations/{location}/endpoints/{endpoint}/chat/completions", s.chatCompletionsREST)
	// Everything else: Model Garden's Deploy, Endpoints, Models and
	// operations through the REST transcoder when served (modelgarden.go).
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { s.serveREST(w, r, nil) })
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

	if strings.HasPrefix(r.URL.Path, "/v1/") && verb != "predict" && verb != "embedContent" && verb != "rawPredict" && verb != "streamRawPredict" &&
		verb != "countTokens" && verb != "computeTokens" {
		apierror.WriteJSON(w, apierror.Unimplemented(
			"only :predict, :embedContent, :rawPredict, :streamRawPredict, :countTokens and :computeTokens are served under /v1; generation is served under /%s", APIVersion))
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
	case "countTokens", "computeTokens":
		// Counted by the model's tokenizer in the runtime (tokens.go).
		s.tokensREST(w, r, model, verb)
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
	req, err := decodeRequest(body, honoursOptions(s.gen))
	if err != nil {
		return "", nil, nil, err
	}
	greq := generatorRequest(req)
	if honoursOptions(s.gen) {
		if greq.Conversation, greq.Vision, err = toRunner(ctx, req, s.objects); err != nil {
			return "", nil, nil, err
		}
	}
	// The generation runs under its own context so a stop sequence can end
	// it without ending the request.
	gctx, cancel := context.WithCancel(ctx)
	chunks, errFn, err := s.gen.Generate(gctx, greq)
	if err != nil {
		cancel()
		return "", nil, nil, apierror.Internal(err, "starting the local runtime failed")
	}
	var stops []string
	if req.GenerationConfig != nil {
		stops = req.GenerationConfig.StopSequences
	}
	if greq.Vision {
		chunks = markImages(chunks)
	}
	return model, applyStops(ctx, cancel, chunks, stops), errFn, nil
}

// markImages flags the usage of a prompt with images, whose per-modality
// split the runtime does not report, so no TEXT-only breakdown is claimed.
func markImages(in <-chan Chunk) <-chan Chunk {
	out := make(chan Chunk)
	go func() {
		defer close(out)
		for c := range in {
			if c.Usage != nil {
				u := *c.Usage
				u.Images = true
				c.Usage = &u
			}
			out <- c
		}
	}()
	return out
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
	var calls []FunctionCall
	var usage *Usage
	finish := FinishReasonStop
	for c := range chunks {
		if c.Usage != nil {
			usage = c.Usage
		}
		if c.Last {
			finish = c.FinishReason
			continue
		}
		text.WriteString(c.Text)
		calls = append(calls, c.ToolCalls...)
	}
	if err := generationError(ctx, errFn); err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	t := strings.TrimRight(text.String(), "\n")
	if len(calls) > 0 {
		// Text around a call is whitespace the template leaves; a call
		// alone is answered as Gemini answers one, with no text part.
		t = strings.TrimSpace(t)
	}
	return candidateResponse(model, t, calls, finish, usage), nil
}

// chunkResponse frames one streamed chunk, or returns nil for a chunk that
// carries only the prompt's token count. A final event with an empty text
// part carries the finish reason and usageMetadata: Vertex sends one, and the
// SDK expects to see the reason on a candidate rather than inferring it from
// the stream ending. UNVERIFIED (re-read 2026-11-19, the v1beta1
// GenerateContentResponse proto in cloud.google.com/go/aiplatform v1.127.0,
// which does not say): what Vertex's intermediate events carry as
// usageMetadata; here only the final one has it, with every count.
func chunkResponse(model string, c Chunk) *GenerateContentResponse {
	switch {
	case c.Last:
		return candidateResponse(model, "", nil, c.FinishReason, c.Usage)
	case len(c.ToolCalls) > 0:
		return candidateResponse(model, "", c.ToolCalls, "", nil)
	case c.Usage != nil && c.Text == "":
		return nil
	}
	return textResponse(model, c.Text, "")
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
		resp := chunkResponse(model, c)
		if resp == nil {
			continue
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
