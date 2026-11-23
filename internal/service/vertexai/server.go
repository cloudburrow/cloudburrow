package vertexai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
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
	"github.com/cloudburrow/cloudburrow/internal/sysparams"
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
	// videoDecoder decodes multimodalembedding@001's AVI, FLV, MPEG and
	// WMV video (#1651); nil leaves them UNAVAILABLE.
	videoDecoder VideoDecoder

	// garden serves Model Garden's Hugging Face deployments (#1840,
	// modelgarden.go), with their operations and REST transcoder.
	garden *modelgarden.Manager
	// reg serves the Model Registry and Endpoints (#1940, registry.go).
	reg      *registry
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

	// caches are the cached contents (cachedcontent.go, #2107), served over
	// gRPC and through cacheTranscoder.
	cacheOnce       sync.Once
	caches          *cacheStore
	cacheRESTOnce   sync.Once
	cacheTranscoder *rest.Transcoder

	// nb is Colab Enterprise's runtime templates and runtimes
	// (notebookruntimes.go, #2105), served over gRPC and nbTranscoder.
	// Guarded by mu.
	nb           *notebookStore
	nbRESTOnce   sync.Once
	nbTranscoder *rest.Transcoder
	// schedStop ends the Colab Enterprise schedules' loop (schedules.go).
	schedStop chan struct{}
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
	// Context caching's cachedContents, v1 and v1beta1 (cachedcontent.go,
	// #2107).
	for _, v := range []string{"v1", APIVersion} {
		mux.Handle("/"+v+"/projects/{project}/locations/{location}/cachedContents", http.HandlerFunc(s.serveCacheREST))
		mux.Handle("/"+v+"/projects/{project}/locations/{location}/cachedContents/{id}", http.HandlerFunc(s.serveCacheREST))
	}
	// Colab Enterprise's runtime templates and runtimes, and their
	// operations (notebookruntimes.go, #2105).
	// Notebook executions, schedules and the templates' IAM policies
	// (notebookjobs.go, schedules.go, notebookiam.go) too.
	for _, c := range []string{"notebookRuntimeTemplates", "notebookRuntimes", "notebookExecutionJobs", "schedules"} {
		mux.HandleFunc("/v1/projects/{project}/locations/{location}/"+c, s.serveNotebookREST)
		mux.HandleFunc("/v1/projects/{project}/locations/{location}/"+c+"/", s.serveNotebookREST)
	}
	mux.HandleFunc("POST /v1/projects/{project}/locations/{location}/notebookRuntimes:assign", s.serveNotebookREST)
	mux.HandleFunc("POST /v1/projects/{project}/locations/{location}/notebookRuntimeTemplates:setDefault", s.setDefaultREST)
	// Model Garden's OpenAI-compatible path on its endpoints (#1840).
	mux.HandleFunc("POST /v1beta1/projects/{project}/locations/{location}/endpoints/{endpoint}/chat/completions", s.chatCompletionsREST)
	// Everything else: Model Garden's Deploy, Endpoints, Models and
	// operations through the REST transcoder when served (modelgarden.go).
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { s.serveREST(w, r, nil) })
	// The playground page and any browser tab can reach this port (#676).
	return hostguard.Wrap(sysparams.Wrap(mux, sysparams.Options{}))
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

// prepared is a resolved, decoded and validated request, ready to run.
type prepared struct {
	model string
	req   *GenerateContentRequest
	greq  Request
	// cached is the cachedContent the request named, expanded into req
	// (cachedcontent.go, #2107); nil without one.
	cached *cacheUse
}

// prepare resolves the model, decodes the JSON request, expands the cached
// content it names and validates the result. It is the one entry point for
// the JSON routes and the gRPC PredictionService, unary and streaming alike,
// so every transport resolves, refuses and honours the same fields the same
// way.
func (s *Server) prepare(ctx context.Context, requested string, body io.Reader) (*prepared, error) {
	model, err := s.resolveModel(requested)
	if err != nil {
		return nil, err
	}
	options := honoursOptions(s.gen)
	req, err := decodeBody(body)
	if err != nil {
		return nil, err
	}
	cached, err := s.expandCachedContent(ctx, req, model)
	if err != nil {
		return nil, err
	}
	if err := validate(req, options); err != nil {
		return nil, err
	}
	greq := generatorRequest(req)
	if options {
		if greq.Conversation, greq.Vision, err = toRunner(ctx, req, s.objects); err != nil {
			return nil, err
		}
	}
	return &prepared{model: model, req: req, greq: greq, cached: cached}, nil
}

// run starts one generation of a prepared request.
func (s *Server) run(ctx context.Context, p *prepared, greq Request) (<-chan Chunk, func() error, error) {
	// The generation runs under its own context so a stop sequence can end
	// it without ending the request.
	gctx, cancel := context.WithCancel(ctx)
	chunks, errFn, err := s.gen.Generate(gctx, greq)
	if err != nil {
		cancel()
		return nil, nil, apierror.Internal(err, "starting the local runtime failed")
	}
	var stops []string
	if p.req.GenerationConfig != nil {
		stops = p.req.GenerationConfig.StopSequences
	}
	if greq.Vision {
		chunks = markImages(chunks)
	}
	_, _, include := p.req.GenerationConfig.thinking()
	if p.cached != nil || !include {
		chunks = annotateUsage(chunks, p.cached, include)
	}
	return applyStops(ctx, cancel, chunks, stops), errFn, nil
}

// startGeneration prepares a request and starts its one generation, for the
// streaming routes. candidateCount > 1 is refused there, as Vertex AI
// documents it: "Specifying multiple candidates is a Preview feature that
// works with generateContent (streamGenerateContent is not supported)"
// (content-generation-parameters, re-read 2026-11-21).
func (s *Server) startGeneration(ctx context.Context, requested string, body io.Reader) (string, <-chan Chunk, func() error, error) {
	p, err := s.prepare(ctx, requested, body)
	if err != nil {
		return "", nil, nil, err
	}
	if n := p.req.GenerationConfig.candidates(); n > 1 {
		return "", nil, nil, apierror.InvalidArgument(
			"Unable to submit request because candidateCount is %d; multiple candidates are supported by generateContent only, not streamGenerateContent", n)
	}
	chunks, errFn, err := s.run(ctx, p, p.greq)
	if err != nil {
		return "", nil, nil, err
	}
	return p.model, chunks, errFn, nil
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

// annotateUsage adds the cached content's token count to the usage, and drops
// the thoughts unless includeThoughts asked for them (their tokens are
// counted either way).
func annotateUsage(in <-chan Chunk, cached *cacheUse, include bool) <-chan Chunk {
	out := make(chan Chunk)
	go func() {
		defer close(out)
		for c := range in {
			if c.Usage != nil && cached != nil {
				u := *c.Usage
				u.CachedTokens = cached.tokens
				u.CachedImages = cached.images
				c.Usage = &u
			}
			if c.Thought != "" && !include {
				if c.Text == "" && c.Usage == nil && len(c.ToolCalls) == 0 && !c.Last {
					continue
				}
				c.Thought = ""
			}
			out <- c
		}
	}()
	return out
}

// candidateSeed is candidate i's seed: the request's seed plus i, so a
// seeded request is repeatable candidate by candidate, or a fresh random
// seed for each when the request sets none, so the candidates are
// independent samples rather than one sample repeated.
func candidateSeed(seed *int32, i int) *int32 {
	var v int32
	if seed != nil {
		v = *seed + int32(i)
	} else {
		v = rand.Int32()
	}
	return &v
}

// generateUnary serves one generateContent, for the JSON route and the gRPC
// PredictionService alike, so the two answer the same request the same way.
// body is the JSON request; each candidate's stream is assembled into one
// response. candidateCount N runs N independent generations, one after
// another (the runtime is one model in one process), each its own sample
// (candidateSeed); the prompt is counted once and the candidates' tokens
// summed, as Vertex bills them ("charged for the output tokens of all
// candidates, but are only charged once for the input tokens").
func (s *Server) generateUnary(ctx context.Context, requested string, body io.Reader) (*GenerateContentResponse, error) {
	p, err := s.prepare(ctx, requested, body)
	if err != nil {
		return nil, err
	}
	n := p.req.GenerationConfig.candidates()
	var resp *GenerateContentResponse
	var total *Usage
	for i := 0; i < n; i++ {
		greq := p.greq
		if n > 1 {
			greq.Seed = candidateSeed(p.greq.Seed, i)
		}
		chunks, errFn, err := s.run(ctx, p, greq)
		if err != nil {
			return nil, err
		}
		r, usage, err := collectCandidate(ctx, p.model, chunks, errFn)
		if err != nil {
			return nil, err
		}
		r.Candidates[0].Index = int32(i)
		total = addUsage(total, usage)
		if resp == nil {
			resp = r
			continue
		}
		resp.Candidates = append(resp.Candidates, r.Candidates[0])
	}
	resp.UsageMetadata = total.metadata()
	return resp, nil
}

// addUsage sums the candidates' decoded tokens over one prompt.
func addUsage(total, u *Usage) *Usage {
	if total == nil || u == nil {
		if u == nil {
			return total
		}
		c := *u
		return &c
	}
	switch {
	case total.CandidateTokens < 0 || u.CandidateTokens < 0:
		total.CandidateTokens = -1
	default:
		total.CandidateTokens += u.CandidateTokens
	}
	total.ThoughtTokens += u.ThoughtTokens
	return total
}

// collectCandidate assembles one generation's stream into a one-candidate response.
func collectCandidate(ctx context.Context, model string, chunks <-chan Chunk, errFn func() error) (*GenerateContentResponse, *Usage, error) {
	var text, thought strings.Builder
	var calls []FunctionCall
	var steps []LogprobStep
	var usage *Usage
	finish := FinishReasonStop
	for c := range chunks {
		steps = append(steps, c.Logprobs...)
		if c.Usage != nil {
			usage = c.Usage
		}
		if c.Last {
			finish = c.FinishReason
			continue
		}
		text.WriteString(c.Text)
		thought.WriteString(c.Thought)
		calls = append(calls, c.ToolCalls...)
	}
	if err := generationError(ctx, errFn); err != nil {
		return nil, nil, err
	}
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	t := strings.TrimRight(text.String(), "\n")
	if len(calls) > 0 {
		// Text around a call is whitespace the template leaves; a call
		// alone is answered as Gemini answers one, with no text part.
		t = strings.TrimSpace(t)
	}
	return withLogprobs(thoughtResponse(model, strings.TrimSpace(thought.String()), t, calls, finish, usage), steps), usage, nil
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
		return withLogprobs(candidateResponse(model, "", nil, c.FinishReason, c.Usage), c.Logprobs)
	case c.Thought != "":
		return &GenerateContentResponse{Candidates: []Candidate{{
			Content: Content{Role: "model", Parts: []Part{{Text: c.Thought, Thought: ptr(true)}}}}}, ModelVersion: model}
	case len(c.ToolCalls) > 0:
		return withLogprobs(candidateResponse(model, "", c.ToolCalls, "", nil), c.Logprobs)
	case c.Usage != nil && c.Text == "":
		return nil
	}
	return withLogprobs(textResponse(model, c.Text, ""), c.Logprobs)
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

// Accelerator names where generation runs (ExecGenerator.Accelerator),
// empty for a generator that does not say.
func (s *Server) Accelerator() string {
	if a, ok := s.gen.(interface{ Accelerator() string }); ok {
		return a.Accelerator()
	}
	return ""
}

// serveCacheREST serves the cachedContents paths through their transcoder.
func (s *Server) serveCacheREST(w http.ResponseWriter, r *http.Request) {
	s.cacheREST().ServeHTTP(w, r)
}
