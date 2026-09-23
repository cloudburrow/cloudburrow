package vertexai

import (
	"encoding/json"
	"io"
	"sort"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// APIVersion is the version this package serves, and the one the Go Gen AI SDK
// sends for the Vertex backend by default. Recorded here rather than assumed:
// the SDK sets v1beta1 for Vertex and v1beta for the Gemini API, which is why
// both prefixes are routed.
const (
	APIVersion       = "v1beta1"
	GeminiAPIVersion = "v1beta"
)

// Part is one piece of a message. Only text is supported.
//
// The other fields exist so that a request carrying them can be *refused*. If
// they were absent, encoding/json would discard them silently and an image
// would be answered as though it had been read.
type Part struct {
	Text string `json:"text,omitempty"`

	InlineData       json.RawMessage `json:"inlineData,omitempty"`
	FileData         json.RawMessage `json:"fileData,omitempty"`
	FunctionCall     json.RawMessage `json:"functionCall,omitempty"`
	FunctionResponse json.RawMessage `json:"functionResponse,omitempty"`
	ExecutableCode   json.RawMessage `json:"executableCode,omitempty"`
	Thought          *bool           `json:"thought,omitempty"`
}

// unsupported returns the name of a populated field this runtime cannot honour.
func (p Part) unsupported() string {
	switch {
	case len(p.InlineData) > 0:
		return "inlineData"
	case len(p.FileData) > 0:
		return "fileData"
	case len(p.FunctionCall) > 0:
		return "functionCall"
	case len(p.FunctionResponse) > 0:
		return "functionResponse"
	case len(p.ExecutableCode) > 0:
		return "executableCode"
	case p.Thought != nil:
		return "thought"
	}
	return ""
}

// Content is one turn.
type Content struct {
	Role  string `json:"role,omitempty"`
	Parts []Part `json:"parts,omitempty"`
}

// GenerationConfig is the generation options block.
//
// Every field is a pointer so that "absent" and "set to the zero value" are
// distinguishable. temperature=0 is a meaningful request, and treating it as
// unset would silently ignore it — the exact failure this package exists to
// avoid.
type GenerationConfig struct {
	MaxOutputTokens  *int32          `json:"maxOutputTokens,omitempty"`
	CandidateCount   *int32          `json:"candidateCount,omitempty"`
	Temperature      *float32        `json:"temperature,omitempty"`
	TopP             *float32        `json:"topP,omitempty"`
	TopK             *float32        `json:"topK,omitempty"`
	Seed             *int32          `json:"seed,omitempty"`
	StopSequences    []string        `json:"stopSequences,omitempty"`
	ResponseMIMEType string          `json:"responseMimeType,omitempty"`
	ResponseSchema   json.RawMessage `json:"responseSchema,omitempty"`
	PresencePenalty  *float32        `json:"presencePenalty,omitempty"`
	FrequencyPenalty *float32        `json:"frequencyPenalty,omitempty"`
	ResponseLogprobs *bool           `json:"responseLogprobs,omitempty"`
	Logprobs         *int32          `json:"logprobs,omitempty"`
	ThinkingConfig   json.RawMessage `json:"thinkingConfig,omitempty"`
	SpeechConfig     json.RawMessage `json:"speechConfig,omitempty"`
	AudioTimestamp   *bool           `json:"audioTimestamp,omitempty"`
	RoutingConfig    json.RawMessage `json:"routingConfig,omitempty"`
	ResponseModality []string        `json:"responseModalities,omitempty"`
}

// unsupportedConfig lists every populated option the runtime cannot honour.
//
// It returns all of them rather than the first, because a caller fixing one at
// a time learns their configuration is unsupported one round trip per field.
func (g *GenerationConfig) unsupportedConfig() []string {
	if g == nil {
		return nil
	}
	var out []string
	add := func(name string, set bool) {
		if set {
			out = append(out, name)
		}
	}
	// maxOutputTokens is refused on measured evidence, not on absence: the
	// runtime accepts --max_output_tokens and ignores it. The same prompt
	// decoded 309 tokens at a limit of 8, at 40, and with no limit at all.
	// A flag that is accepted and does nothing is the worst case for a
	// caller, because the request looks honoured.
	add("maxOutputTokens", g.MaxOutputTokens != nil)
	add("candidateCount", g.CandidateCount != nil && *g.CandidateCount != 1)
	add("temperature", g.Temperature != nil)
	add("topP", g.TopP != nil)
	add("topK", g.TopK != nil)
	add("seed", g.Seed != nil)
	add("stopSequences", len(g.StopSequences) > 0)
	add("responseMimeType", g.ResponseMIMEType != "")
	add("responseSchema", len(g.ResponseSchema) > 0)
	add("presencePenalty", g.PresencePenalty != nil)
	add("frequencyPenalty", g.FrequencyPenalty != nil)
	add("responseLogprobs", g.ResponseLogprobs != nil)
	add("logprobs", g.Logprobs != nil)
	add("thinkingConfig", len(g.ThinkingConfig) > 0)
	add("speechConfig", len(g.SpeechConfig) > 0)
	add("audioTimestamp", g.AudioTimestamp != nil)
	add("routingConfig", len(g.RoutingConfig) > 0)
	add("responseModalities", len(g.ResponseModality) > 0)
	sort.Strings(out)
	return out
}

// GenerateContentRequest is the request body.
type GenerateContentRequest struct {
	Contents []Content `json:"contents"`

	GenerationConfig *GenerationConfig `json:"generationConfig,omitempty"`

	Tools             json.RawMessage `json:"tools,omitempty"`
	ToolConfig        json.RawMessage `json:"toolConfig,omitempty"`
	SafetySettings    json.RawMessage `json:"safetySettings,omitempty"`
	SystemInstruction *Content        `json:"systemInstruction,omitempty"`
	CachedContent     string          `json:"cachedContent,omitempty"`
	Labels            json.RawMessage `json:"labels,omitempty"`
}

// FinishReason values, spelled as Vertex spells them.
// Only STOP is ever emitted. MAX_TOKENS is absent deliberately: nothing in
// this path can stop generation early, so a constant for it would suggest a
// reason that cannot occur.
const FinishReasonStop = "STOP"

// Candidate is one generated result.
type Candidate struct {
	Content      Content `json:"content"`
	FinishReason string  `json:"finishReason,omitempty"`
	Index        int32   `json:"index"`
}

// GenerateContentResponse is the response body.
//
// UsageMetadata is deliberately absent. See the package doc: a token count we
// did not measure is worse than no token count.
type GenerateContentResponse struct {
	Candidates   []Candidate `json:"candidates"`
	ModelVersion string      `json:"modelVersion,omitempty"`
	ResponseID   string      `json:"responseId,omitempty"`
}

// maxRequestBytes bounds a request body. A local service still should not be
// made to buffer an arbitrary amount because a client got a loop wrong.
const maxRequestBytes = 1 << 20

// decodeRequest reads and validates a request.
//
// Unknown fields are rejected. That is the strict choice, and it is the right
// one here: an option this runtime does not implement must not be silently
// dropped, and DisallowUnknownFields is what catches the ones not enumerated
// above — including any Vertex adds after this was written.
func decodeRequest(r io.Reader) (*GenerateContentRequest, error) {
	dec := json.NewDecoder(io.LimitReader(r, maxRequestBytes))
	dec.DisallowUnknownFields()

	var req GenerateContentRequest
	if err := dec.Decode(&req); err != nil {
		if strings.Contains(err.Error(), "unknown field") {
			return nil, apierror.InvalidArgument(
				"%s; this endpoint serves a documented subset of generateContent and refuses fields it cannot honour", err)
		}
		return nil, apierror.InvalidArgument("malformed request body: %v", err)
	}
	if err := validate(&req); err != nil {
		return nil, err
	}
	return &req, nil
}

// validate enforces the documented subset.
func validate(req *GenerateContentRequest) error {
	switch {
	case len(req.Tools) > 0:
		return unsupportedField("tools")
	case len(req.ToolConfig) > 0:
		return unsupportedField("toolConfig")
	case len(req.SafetySettings) > 0:
		return unsupportedField("safetySettings")
	case req.SystemInstruction != nil:
		return unsupportedField("systemInstruction")
	case req.CachedContent != "":
		return unsupportedField("cachedContent")
	}

	if names := req.GenerationConfig.unsupportedConfig(); len(names) > 0 {
		return apierror.InvalidArgument(
			"generationConfig.%s cannot be honoured: the LiteRT-LM runtime applies no such control, "+
				"and accepting it would report a setting that was never applied. "+
				"This endpoint supports no generation options at all — see docs/generation.md",
			strings.Join(names, ", generationConfig."))
	}

	if len(req.Contents) == 0 {
		return apierror.InvalidArgument("contents is required and must contain one user turn")
	}
	// Multi-turn needs a chat template to join the turns, and inventing one
	// would change what the model sees in a way the caller cannot inspect.
	// The runtime applies the template bundled with the model to a single
	// prompt, so a single turn is what can be served faithfully.
	if len(req.Contents) > 1 {
		return apierror.Unimplemented(
			"multi-turn conversations are not supported: the runtime takes one prompt and applies "+
				"the model's own template to it, and joining %d turns would require a chat template "+
				"this service would be inventing", len(req.Contents))
	}

	c := req.Contents[0]
	if c.Role != "" && c.Role != "user" {
		return apierror.InvalidArgument("contents[0].role = %q; only \"user\" is supported", c.Role)
	}
	if len(c.Parts) == 0 {
		return apierror.InvalidArgument("contents[0].parts is required")
	}
	for i, p := range c.Parts {
		if name := p.unsupported(); name != "" {
			return apierror.InvalidArgument(
				"contents[0].parts[%d].%s: this endpoint is text-only; %s is not supported", i, name, name)
		}
	}
	if strings.TrimSpace(promptOf(req)) == "" {
		return apierror.InvalidArgument("contents[0].parts contains no text")
	}

	return nil
}

func unsupportedField(name string) error {
	return apierror.InvalidArgument(
		"%s is not supported by this endpoint; it serves a documented subset of generateContent "+
			"and refuses what it cannot perform rather than ignoring it", name)
}

// promptOf joins the text parts of the single supported turn.
func promptOf(req *GenerateContentRequest) string {
	if len(req.Contents) == 0 {
		return ""
	}
	var b strings.Builder
	for _, p := range req.Contents[0].Parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// textResponse frames generated text as Vertex frames it.
func textResponse(model, text, finish string) *GenerateContentResponse {
	return &GenerateContentResponse{
		Candidates: []Candidate{{
			Content:      Content{Role: "model", Parts: []Part{{Text: text}}},
			FinishReason: finish,
			Index:        0,
		}},
		ModelVersion: model,
	}
}
