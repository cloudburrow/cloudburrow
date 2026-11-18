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
//
// options reports whether the generator applies the sampler and the token
// limit (cloudburrow_generate, #1797). Without it those are refused too: the
// image's litert_lm_main takes no sampler flag, and before LiteRT-LM a789b7d0
// it accepted --max_output_tokens and ignored it (309 tokens decoded at a
// limit of 8, measured). stopSequences are applied by this package (stop.go),
// so they need only a generator that streams, which every one does.
func (g *GenerationConfig) unsupportedConfig(options bool) []string {
	if g == nil {
		return nil
	}
	var out []string
	add := func(name string, set bool) {
		if set {
			out = append(out, name)
		}
	}
	add("maxOutputTokens", !options && g.MaxOutputTokens != nil)
	add("temperature", !options && g.Temperature != nil)
	add("topP", !options && g.TopP != nil)
	add("topK", !options && g.TopK != nil)
	add("seed", !options && g.Seed != nil)
	add("candidateCount", g.CandidateCount != nil && *g.CandidateCount != 1)
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

// maxStopSequences is Vertex AI's limit on generationConfig.stopSequences
// ("A maximum of 5 items in the list", GenerationConfig reference).
const maxStopSequences = 5

// rangeError checks the honoured options against Vertex AI's documented
// ranges (GenerationConfig reference: temperature 0.0-2.0, topP 0.0-1.0,
// topK at least 1, maxOutputTokens at least 1, at most 5 stopSequences).
//
// UNVERIFIED: the error wording; Vertex's text for an out-of-range value is
// not in its reference docs.
func (g *GenerationConfig) rangeError() error {
	if g == nil {
		return nil
	}
	switch {
	case g.Temperature != nil && !(*g.Temperature >= 0 && *g.Temperature <= 2):
		return apierror.InvalidArgument("Unable to submit request because it has a temperature value of %g but the supported range is from 0 (inclusive) to 2.0001 (exclusive). Update the value and try again.", *g.Temperature)
	case g.TopP != nil && !(*g.TopP >= 0 && *g.TopP <= 1):
		return apierror.InvalidArgument("Unable to submit request because it has a topP value of %g but the supported range is from 0 (inclusive) to 1.0001 (exclusive). Update the value and try again.", *g.TopP)
	case g.TopK != nil && (*g.TopK < 1 || *g.TopK != float32(int32(*g.TopK))):
		return apierror.InvalidArgument("Unable to submit request because it has a topK value of %g but the supported range is from 1 (inclusive) to 41 (exclusive). Update the value and try again.", *g.TopK)
	case g.MaxOutputTokens != nil && *g.MaxOutputTokens < 1:
		return apierror.InvalidArgument("Unable to submit request because it has a maxOutputTokens value of %d but the supported range is from 1 (inclusive) to 65537 (exclusive). Update the value and try again.", *g.MaxOutputTokens)
	case len(g.StopSequences) > maxStopSequences:
		return apierror.InvalidArgument("Unable to submit request because it has %d stopSequences; the maximum is %d. Update the value and try again.", len(g.StopSequences), maxStopSequences)
	}
	for i, s := range g.StopSequences {
		if s == "" {
			return apierror.InvalidArgument("generationConfig.stopSequences[%d] is empty", i)
		}
	}
	return nil
}

// generatorRequest is the request a generator is asked for.
func generatorRequest(req *GenerateContentRequest) Request {
	out := Request{Prompt: promptOf(req)}
	if g := req.GenerationConfig; g != nil {
		out.MaxOutputTokens = g.MaxOutputTokens
		out.Temperature = g.Temperature
		out.TopP = g.TopP
		out.Seed = g.Seed
		if g.TopK != nil {
			k := int32(*g.TopK)
			out.TopK = &k
		}
	}
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

// FinishReason values, spelled as Vertex spells them. MAX_TOKENS is emitted
// when cloudburrow_generate reports it decoded maxOutputTokens tokens; STOP
// otherwise, including at a stop sequence, as Vertex reports one.
const (
	FinishReasonStop      = "STOP"
	FinishReasonMaxTokens = "MAX_TOKENS"
)

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
func decodeRequest(r io.Reader, options bool) (*GenerateContentRequest, error) {
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
	if err := validate(&req, options); err != nil {
		return nil, err
	}
	return &req, nil
}

// validate enforces the documented subset.
func validate(req *GenerateContentRequest, options bool) error {
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

	if names := req.GenerationConfig.unsupportedConfig(options); len(names) > 0 {
		return apierror.InvalidArgument(
			"generationConfig.%s cannot be honoured: the LiteRT-LM runtime applies no such control, "+
				"and accepting it would report a setting that was never applied. "+
				"See docs/generation.md for the options this endpoint applies",
			strings.Join(names, ", generationConfig."))
	}
	if err := req.GenerationConfig.rangeError(); err != nil {
		return err
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
