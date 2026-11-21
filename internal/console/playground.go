package console

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Playground exposes the local generation endpoint to the console.
//
// It is a **relay**, not a second client. Every request it makes is the same
// HTTP call the official SDK makes, to the same path, with the same body, so a
// screen cannot work while the API is broken. A console that spoke to the
// generator directly would be a second implementation, and the first thing a
// second implementation does is disagree with the first.
type Playground struct {
	// Addr resolves the local generation endpoint's host:port.
	//
	// It is a function rather than a string because the console is
	// constructed before the endpoint binds, and a port assigned by the OS is
	// not known until then. Resolving at request time also means a restart
	// on a different port is picked up rather than cached.
	//
	// Nil, or returning empty, means local AI is not configured and the
	// screen is not offered at all.
	Addr func() string
	// Model is the configured model ID.
	Model string
	// Publisher is recorded so the screen can say what the model actually is.
	Publisher string
	// Community marks a community conversion, which must never be presented
	// as a Google-published model.
	Community bool
	// EmbeddingModel, when set, offers the Embeddings screen for that
	// model identity (#41).
	EmbeddingModel string

	// Client is the HTTP client used for the relay. Nil uses a default with
	// no timeout, because a generation legitimately takes minutes and a
	// deadline here would cut a stream for no reason a user could see.
	Client *http.Client
}

// Configured reports whether local AI is available.
func (p *Playground) Configured() bool { return p != nil && p.endpoint() != "" }

func (p *Playground) endpoint() string {
	if p == nil || p.Addr == nil {
		return ""
	}
	return p.Addr()
}

func (p *Playground) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return http.DefaultClient
}

// generatePath is the Vertex resource path, built exactly as the SDK builds
// it. The project and location are placeholders the endpoint does not
// interpret; they are present because the path shape is the contract.
func (p *Playground) generatePath(verb string) string {
	return fmt.Sprintf("/v1beta1/projects/console/locations/us-central1/publishers/google/models/%s:%s",
		p.Model, verb)
}

// PlaygroundStatus is what the screen needs to render honestly.
type PlaygroundStatus struct {
	Configured bool   `json:"configured"`
	Model      string `json:"model,omitempty"`
	Publisher  string `json:"publisher,omitempty"`
	// Community drives the label. A Gemma result must never be presented as
	// a Gemini result, and a community conversion must never be presented as
	// Google-published.
	Community bool   `json:"community,omitempty"`
	Endpoint  string `json:"endpoint,omitempty"`
	// Ready is a live probe, not a guess from configuration.
	Ready bool `json:"ready"`
	// Unavailable explains a failed probe.
	Unavailable string `json:"unavailable,omitempty"`
	// Supported and Refused are taken from the API's own behaviour rather
	// than written into the client, so the screen cannot offer a control the
	// backend would reject.
	Supported []string `json:"supported"`
	Refused   []string `json:"refused"`
	// Features are the request fields the screen sends beside
	// generationConfig (playgroundFeatures).
	Features []string `json:"features"`
	// Note is displayed above the form.
	Note string `json:"note,omitempty"`
}

// refusedOptions is the list the API refuses, kept here so the screen can
// explain what it does not offer rather than silently omitting it.
//
// A control that is absent with no explanation reads as an oversight; one that
// is absent with a reason reads as a decision. The API is the authority and
// TestPlaygroundRefusalsMatchTheAPI holds these in step with it. Since #1937
// the runtime serves system instructions, multi-turn chat, function calling
// and token counts, which the screen offers (#1962). candidateCount,
// thinkingConfig and cachedContent are served since #2107; the screen does
// not offer them yet (#2107's console follow-up), and the API does not
// refuse them, so they are not listed here.
var refusedOptions = []string{"safetySettings"}

// supportedOptions are the generationConfig fields the screen offers: the
// runtime's cloudburrow_generate applies the sampler, the token limit, the
// repetition penalties and a JSON response schema, and the API ends a stream
// at a stop sequence (#1797, #1779, #1937). TestPlaygroundSupportedOptionsAreHonoured
// holds them in step with the API.
var supportedOptions = []string{
	"temperature", "topP", "topK", "seed", "maxOutputTokens", "stopSequences",
	"presencePenalty", "frequencyPenalty", "responseMimeType", "responseSchema",
}

// Features are the request fields beside generationConfig the screen sends
// (#1962): a conversation's earlier turns, a system instruction, function
// declarations with their mode, image parts and the token counts.
var playgroundFeatures = []string{
	"contents", "systemInstruction", "tools", "toolConfig", "inlineData", "fileData",
	"countTokens", "computeTokens", "usageMetadata",
}

// Status probes the endpoint and reports what the screen may offer.
func (p *Playground) Status(ctx context.Context) PlaygroundStatus {
	if !p.Configured() {
		return PlaygroundStatus{
			Configured: false,
			Note: "Local AI is not configured. Start CloudBurrow with -local-ai-model to " +
				"enable it; see docs/generation.md.",
		}
	}
	st := PlaygroundStatus{
		Configured: true,
		Model:      p.Model,
		Publisher:  p.Publisher,
		Community:  p.Community,
		Endpoint:   p.endpoint(),
		Supported:  supportedOptions,
		Refused:    refusedOptions,
		Features:   playgroundFeatures,
	}
	if p.Community {
		st.Note = "This model is a COMMUNITY conversion, not published by Google. " +
			"Its output is not a Gemini result and is not labelled as one."
	}

	// Readiness is established by asking the endpoint something cheap and
	// well-defined: a request for a model it does not serve must come back
	// 404. That proves the service is answering and routing, without
	// starting a generation that would take seconds and load a model.
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodPost,
		"http://"+p.endpoint()+"/v1beta1/projects/console/locations/us-central1/publishers/google/models/__readiness_probe__:generateContent",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"probe"}]}]}`))
	if err != nil {
		st.Unavailable = err.Error()
		return st
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client().Do(req)
	if err != nil {
		st.Unavailable = "the local generation endpoint is not answering: " + err.Error()
		return st
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode == http.StatusNotFound {
		st.Ready = true
		return st
	}
	st.Unavailable = fmt.Sprintf("the endpoint answered %d to a readiness probe, want 404", resp.StatusCode)
	return st
}

// handlePlayground reports status.
func (s *Server) handlePlayground(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.playground.Status(r.Context()))
}

// playgroundRequest is what the screen sends.
type playgroundRequest struct {
	// Prompt is a single user turn's text, sent when Contents is empty.
	Prompt string `json:"prompt"`
	// Contents is the whole conversation (#1962): the earlier user and
	// model turns, function calls and responses, and the new turn with its
	// image parts, relayed as the request's contents for the API to
	// validate.
	Contents []json.RawMessage `json:"contents,omitempty"`
	// SystemInstruction, Tools and ToolConfig are relayed as the request's
	// own fields (#1962).
	SystemInstruction json.RawMessage `json:"systemInstruction,omitempty"`
	Tools             json.RawMessage `json:"tools,omitempty"`
	ToolConfig        json.RawMessage `json:"toolConfig,omitempty"`
	// Stream picks the method: true (the default, when absent) relays
	// :streamGenerateContent, as StreamGenerateContent streams it, and false
	// relays the unary :generateContent, as GenerateContent answers (#1257).
	Stream *bool `json:"stream,omitempty"`
	// Raw sends the same body as RawPredict on the publisher model: true
	// relays :streamRawPredict (StreamRawPredict, #1285) when streaming and
	// :rawPredict (RawPredict, #1284) otherwise.
	Raw bool `json:"raw,omitempty"`
	// GenerationConfig is relayed as the request's generationConfig, for
	// the API to apply or refuse (supportedOptions).
	GenerationConfig map[string]any `json:"generationConfig,omitempty"`
}

// maxPlaygroundRequest bounds what the screen sends: Vertex AI's own
// request limit, which an inline image counts against.
const maxPlaygroundRequest = 20 << 20

// body is the API request the screen's fields make: the same body the SDK
// sends.
func (b playgroundRequest) body() (map[string]any, string) {
	req := map[string]any{}
	if len(b.Contents) > 0 {
		req["contents"] = b.Contents
	} else {
		if strings.TrimSpace(b.Prompt) == "" {
			return nil, "a prompt is required"
		}
		req["contents"] = []map[string]any{{
			"role":  "user",
			"parts": []map[string]any{{"text": b.Prompt}},
		}}
	}
	for k, v := range map[string]json.RawMessage{"systemInstruction": b.SystemInstruction, "tools": b.Tools, "toolConfig": b.ToolConfig} {
		if len(v) > 0 && string(v) != "null" {
			req[k] = v
		}
	}
	if len(b.GenerationConfig) > 0 {
		req["generationConfig"] = b.GenerationConfig
	}
	return req, ""
}

// maxRelayBytes bounds a relayed unary answer or refusal.
const maxRelayBytes = 1 << 20

// handlePlaygroundGenerate relays a generation to the real API and streams the
// result back as server-sent events.
//
// The relay is deliberately thin: it forwards the same body to the same path
// and copies bytes back. It does not parse candidates, so it cannot reshape
// what the API said, and it cannot invent a response when the API failed.
func (s *Server) handlePlaygroundGenerate(w http.ResponseWriter, r *http.Request) {
	p := s.playground
	if !p.Configured() {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "local AI is not configured",
		})
		return
	}

	var body playgroundRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxPlaygroundRequest)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed request: " + err.Error()})
		return
	}
	// The exact body the SDK sends, built here so the screen exercises the
	// same request shape the compatibility tests do.
	req, msg := body.body()
	if msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	payload, err := json.Marshal(req)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	stream := body.Stream == nil || *body.Stream
	target := p.generatePath("streamGenerateContent") + "?alt=sse"
	switch {
	case body.Raw && stream:
		target = p.generatePath("streamRawPredict")
	case body.Raw:
		target = p.generatePath("rawPredict")
	case !stream:
		target = p.generatePath("generateContent")
	}
	upstream, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		"http://"+p.endpoint()+target, strings.NewReader(string(payload)))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	upstream.Header.Set("Content-Type", "application/json")

	resp, err := p.client().Do(upstream)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": "the local generation endpoint is not answering: " + err.Error(),
		})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK || !stream {
		// The unary answer, and any refusal, is relayed as the API wrote it.
		// The API's own message reaches the screen, unchanged. A generic
		// failure here would hide the one useful thing — which field was
		// refused, or which model is actually served.
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxRelayBytes))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(msg)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "cannot stream"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Copied in small reads and flushed, so the screen sees tokens as the
	// model produces them. Buffering here would make the streaming endpoint
	// look like a slow unary one.
	buf := make([]byte, 512)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			flusher.Flush()
		}
		if err != nil {
			return
		}
	}
}

// handlePlaygroundTokens relays the prompt's countTokens or computeTokens
// (#1962), the call the SDK's models.count_tokens and compute_tokens make,
// to the same publisher model path, and the API's answer back unchanged:
// the screen's token count and its tokenizer pane.
func (s *Server) handlePlaygroundTokens(w http.ResponseWriter, r *http.Request) {
	p := s.playground
	if !p.Configured() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "local AI is not configured"})
		return
	}
	verb := r.URL.Query().Get("verb")
	if verb != "countTokens" && verb != "computeTokens" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "verb must be countTokens or computeTokens"})
		return
	}
	var body playgroundRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxPlaygroundRequest)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed request: " + err.Error()})
		return
	}
	req, msg := body.body()
	if msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	// CountTokensRequest takes contents, systemInstruction, tools and
	// generationConfig; ComputeTokensRequest contents alone.
	delete(req, "toolConfig")
	if verb == "computeTokens" {
		req = map[string]any{"contents": req["contents"]}
	}
	payload, err := json.Marshal(req)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	upstream, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		"http://"+p.endpoint()+p.generatePath(verb), strings.NewReader(string(payload)))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	upstream.Header.Set("Content-Type", "application/json")
	resp, err := p.client().Do(upstream)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": "the local generation endpoint is not answering: " + err.Error(),
		})
		return
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, maxRelayBytes))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)
}
