package vertexai

import (
	"encoding/json"
	"fmt"
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

// Part is one piece of a message: text, an image (inlineData, or fileData
// from this instance's Cloud Storage), a function call or a function
// response (#1937, conversation.go).
//
// The code-execution fields exist so that a request carrying them can be
// *refused* by name. If they were absent, encoding/json would reject them as
// unknown fields, which says less.
type Part struct {
	Text string `json:"text,omitempty"`

	InlineData          *Blob             `json:"inlineData,omitempty"`
	FileData            *FileData         `json:"fileData,omitempty"`
	FunctionCall        *FunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse    *FunctionResponse `json:"functionResponse,omitempty"`
	ExecutableCode      json.RawMessage   `json:"executableCode,omitempty"`
	CodeExecutionResult json.RawMessage   `json:"codeExecutionResult,omitempty"`
	Thought             *bool             `json:"thought,omitempty"`
}

// unsupported returns the name of a populated field a text-only runtime
// (litert_lm_main, which takes one prompt) cannot honour.
func (p Part) unsupported() string {
	switch {
	case p.InlineData != nil:
		return "inlineData"
	case p.FileData != nil:
		return "fileData"
	case p.FunctionCall != nil:
		return "functionCall"
	case p.FunctionResponse != nil:
		return "functionResponse"
	case len(p.ExecutableCode) > 0:
		return "executableCode"
	case len(p.CodeExecutionResult) > 0:
		return "codeExecutionResult"
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
	// ResponseJSONSchema is v1beta1's JSON Schema form of responseSchema.
	ResponseJSONSchema json.RawMessage `json:"responseJsonSchema,omitempty"`
	PresencePenalty    *float32        `json:"presencePenalty,omitempty"`
	FrequencyPenalty   *float32        `json:"frequencyPenalty,omitempty"`
	ResponseLogprobs   *bool           `json:"responseLogprobs,omitempty"`
	Logprobs           *int32          `json:"logprobs,omitempty"`
	ThinkingConfig     *ThinkingConfig `json:"thinkingConfig,omitempty"`
	SpeechConfig       json.RawMessage `json:"speechConfig,omitempty"`
	AudioTimestamp     *bool           `json:"audioTimestamp,omitempty"`
	RoutingConfig      json.RawMessage `json:"routingConfig,omitempty"`
	ResponseModality   []string        `json:"responseModalities,omitempty"`
}

// ThinkingConfig is generationConfig.thinkingConfig (#2107), applied as
// LiteRT-LM's ThinkingConfig: Gemma 4's own thinking mode (its chat template's
// <|think|>, its thought channel) with ThinkingBudgetConstraint's budget.
type ThinkingConfig struct {
	IncludeThoughts *bool  `json:"includeThoughts,omitempty"`
	ThinkingBudget  *int32 `json:"thinkingBudget,omitempty"`
	// ThinkingLevel is refused: Gemma 4's thinking is on or off with a token
	// budget, and mapping LOW or HIGH to a budget would invent a meaning.
	ThinkingLevel string `json:"thinkingLevel,omitempty"`
}

// thinking reports whether thinking is on and its budget (-1 unlimited).
// A thinkingConfig turns the model's thinking mode on unless its
// thinkingBudget is 0 ("0 disables thinking", the reference); without one
// the model answers without thinking, its template's default.
func (g *GenerationConfig) thinking() (on bool, budget int32, include bool) {
	if g == nil || g.ThinkingConfig == nil {
		return false, 0, false
	}
	tc := g.ThinkingConfig
	budget = -1
	if tc.ThinkingBudget != nil {
		budget = *tc.ThinkingBudget
	}
	include = tc.IncludeThoughts != nil && *tc.IncludeThoughts
	return budget != 0, budget, include
}

// maxCandidateCount bounds candidateCount. UNVERIFIED (re-read 2026-11-21:
// https://docs.cloud.google.com/vertex-ai/generative-ai/docs/model-reference/inference
// and .../multimodal/content-generation-parameters, which state no range):
// Gemini's commonly reported limit of 8.
const maxCandidateCount = 8

// candidates is the number of candidates asked for, 1 by default.
func (g *GenerationConfig) candidates() int {
	if g == nil || g.CandidateCount == nil {
		return 1
	}
	return int(*g.CandidateCount)
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
	// N candidates are N independent generations (#2107), each seeded on its
	// own, which needs a runtime that takes a seed.
	add("candidateCount", !options && g.CandidateCount != nil && *g.CandidateCount != 1)
	// A JSON response, with or without a schema, is constrained decoding
	// (cloudburrow_generate's llguidance JSON-schema constraint, #1779).
	add("responseMimeType", g.ResponseMIMEType != "" && !(options && (g.ResponseMIMEType == "application/json" || g.ResponseMIMEType == "text/plain")))
	add("responseSchema", !options && len(g.ResponseSchema) > 0)
	add("responseJsonSchema", !options && len(g.ResponseJSONSchema) > 0)
	// LiteRT-LM's RepetitionPenaltyConfig, through cloudburrow_generate
	// (#1937).
	add("presencePenalty", !options && g.PresencePenalty != nil)
	add("frequencyPenalty", !options && g.FrequencyPenalty != nil)
	// responseLogprobs and logprobs are the model's log-softmax at each
	// decode step, the chosen token and the top candidates, from
	// cloudburrow_generate's --response_logprobs (CloudBurrow's LiteRT-LM
	// decode step observer, #2107).
	add("responseLogprobs", !options && g.ResponseLogprobs != nil && *g.ResponseLogprobs)
	add("logprobs", !options && g.Logprobs != nil)
	add("thinkingConfig", !options && g.ThinkingConfig != nil)
	add("thinkingConfig.thinkingLevel", g.ThinkingConfig != nil && g.ThinkingConfig.ThinkingLevel != "")
	add("speechConfig", len(g.SpeechConfig) > 0)
	add("audioTimestamp", g.AudioTimestamp != nil)
	add("routingConfig", len(g.RoutingConfig) > 0)
	add("responseModalities", len(g.ResponseModality) > 0)
	sort.Strings(out)
	return out
}

// maxLogprobs is Vertex AI's limit on generationConfig.logprobs ("Specify an
// integer value in the range of 1-20", inference reference, re-read
// 2026-11-21).
const maxLogprobs = 20

// maxStopSequences is Vertex AI's limit on generationConfig.stopSequences
// ("A maximum of 5 items in the list", GenerationConfig reference).
const maxStopSequences = 5

// rangeError checks the honoured options against Vertex AI's documented
// ranges (the inference reference's generationConfig: temperature
// 0.0-2.0, topP 0.0-1.0, "Maximum 5 items" of stopSequences; topK and
// maxOutputTokens have no stated range, so only a positive whole topK and
// a positive maxOutputTokens are required; "text/plain isn't supported
// with responseSchema").
//
// UNVERIFIED (re-read 2026-11-18:
// https://docs.cloud.google.com/vertex-ai/generative-ai/docs/model-reference/inference,
// which gives no error text): the error wording.
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
		return apierror.InvalidArgument("Unable to submit request because it has a topK value of %g; topK must be a whole number of at least 1. Update the value and try again.", *g.TopK)
	case g.MaxOutputTokens != nil && *g.MaxOutputTokens < 1:
		return apierror.InvalidArgument("Unable to submit request because it has a maxOutputTokens value of %d; maxOutputTokens must be at least 1. Update the value and try again.", *g.MaxOutputTokens)
	case g.PresencePenalty != nil && !(*g.PresencePenalty >= -2 && *g.PresencePenalty < 2):
		// The reference: "Range: -2.0 to 2.0 (exclusive)".
		return apierror.InvalidArgument("Unable to submit request because it has a presencePenalty value of %g but the supported range is from -2 (inclusive) to 2 (exclusive). Update the value and try again.", *g.PresencePenalty)
	case g.FrequencyPenalty != nil && !(*g.FrequencyPenalty >= -2 && *g.FrequencyPenalty < 2):
		return apierror.InvalidArgument("Unable to submit request because it has a frequencyPenalty value of %g but the supported range is from -2 (inclusive) to 2 (exclusive). Update the value and try again.", *g.FrequencyPenalty)
	case g.CandidateCount != nil && (*g.CandidateCount < 1 || *g.CandidateCount > maxCandidateCount):
		return apierror.InvalidArgument("Unable to submit request because it has a candidateCount value of %d but the supported range is from 1 (inclusive) to %d (inclusive). Update the value and try again.", *g.CandidateCount, maxCandidateCount)
	case g.ThinkingConfig != nil && g.ThinkingConfig.ThinkingBudget != nil && *g.ThinkingConfig.ThinkingBudget < -1:
		// The reference: 0 disables thinking, -1 is dynamic thinking.
		return apierror.InvalidArgument("Unable to submit request because it has a thinkingBudget value of %d; thinkingBudget must be -1 (dynamic), 0 (off) or a positive number of tokens. Update the value and try again.", *g.ThinkingConfig.ThinkingBudget)
	case g.Logprobs != nil && (*g.Logprobs < 1 || *g.Logprobs > maxLogprobs):
		// The reference: "Specify an integer value in the range of 1-20."
		return apierror.InvalidArgument("Unable to submit request because it has a logprobs value of %d but the supported range is from 1 (inclusive) to %d (inclusive). Update the value and try again.", *g.Logprobs, maxLogprobs)
	case g.Logprobs != nil && (g.ResponseLogprobs == nil || !*g.ResponseLogprobs):
		// The reference: "You must enable responseLogprobs to use this
		// parameter."
		return apierror.InvalidArgument("Unable to submit request because logprobs is set but responseLogprobs is not enabled; logprobs requires responseLogprobs to be true. Update the value and try again.")
	case len(g.StopSequences) > maxStopSequences:
		return apierror.InvalidArgument("Unable to submit request because it has %d stopSequences; the maximum is %d. Update the value and try again.", len(g.StopSequences), maxStopSequences)
	}
	for i, s := range g.StopSequences {
		if s == "" {
			return apierror.InvalidArgument("generationConfig.stopSequences[%d] is empty", i)
		}
	}
	if (len(g.ResponseSchema) > 0 || len(g.ResponseJSONSchema) > 0) && g.ResponseMIMEType != "application/json" {
		// Vertex AI: a response schema needs the JSON MIME type.
		return apierror.InvalidArgument("Unable to submit request because responseSchema is set but responseMimeType is not application/json. Learn more: https://cloud.google.com/vertex-ai/generative-ai/docs/multimodal/control-generated-output")
	}
	if len(g.ResponseSchema) > 0 && len(g.ResponseJSONSchema) > 0 {
		return apierror.InvalidArgument("generationConfig.responseSchema and generationConfig.responseJsonSchema cannot both be set")
	}
	if _, err := g.jsonSchema(); err != nil {
		return apierror.InvalidArgument("generationConfig.responseSchema: %v", err)
	}
	return nil
}

// jsonSchema is the JSON Schema the response is constrained to: "" for a
// text response, "{}" (any JSON) for application/json without a schema,
// responseJsonSchema as given, or responseSchema (Vertex AI's OpenAPI
// subset) converted to JSON Schema.
func (g *GenerationConfig) jsonSchema() (string, error) {
	if g == nil || g.ResponseMIMEType != "application/json" {
		return "", nil
	}
	switch {
	case len(g.ResponseJSONSchema) > 0:
		var v any
		if err := json.Unmarshal(g.ResponseJSONSchema, &v); err != nil {
			return "", err
		}
		return string(g.ResponseJSONSchema), nil
	case len(g.ResponseSchema) > 0:
		var v map[string]any
		if err := json.Unmarshal(g.ResponseSchema, &v); err != nil {
			return "", err
		}
		out, err := openAPIToJSONSchema(v)
		if err != nil {
			return "", err
		}
		b, err := json.Marshal(out)
		return string(b), err
	}
	return "{}", nil
}

// openAPIToJSONSchema converts Vertex AI's Schema (an OpenAPI 3.0 subset:
// upper-case types, nullable, propertyOrdering) to the JSON Schema the
// runtime's llguidance constraint reads.
func openAPIToJSONSchema(s map[string]any) (map[string]any, error) {
	out := map[string]any{}
	nullable, _ := s["nullable"].(bool)
	for k, v := range s {
		switch k {
		case "type":
			t, _ := v.(string)
			jt, ok := map[string]string{"STRING": "string", "INTEGER": "integer", "NUMBER": "number",
				"BOOLEAN": "boolean", "ARRAY": "array", "OBJECT": "object", "NULL": "null"}[strings.ToUpper(t)]
			if !ok {
				return nil, fmt.Errorf("unknown type %q", t)
			}
			if nullable {
				out["type"] = []string{jt, "null"}
			} else {
				out["type"] = jt
			}
		case "properties":
			props, _ := v.(map[string]any)
			conv := map[string]any{}
			for name, p := range props {
				pm, ok := p.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("property %q is not a schema", name)
				}
				c, err := openAPIToJSONSchema(pm)
				if err != nil {
					return nil, err
				}
				conv[name] = c
			}
			out["properties"] = conv
		case "items":
			im, ok := v.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("items is not a schema")
			}
			c, err := openAPIToJSONSchema(im)
			if err != nil {
				return nil, err
			}
			out["items"] = c
		case "anyOf":
			list, _ := v.([]any)
			var conv []any
			for _, e := range list {
				em, ok := e.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("anyOf holds a non-schema")
				}
				c, err := openAPIToJSONSchema(em)
				if err != nil {
					return nil, err
				}
				conv = append(conv, c)
			}
			out["anyOf"] = conv
		case "enum", "required", "description", "minItems", "maxItems", "minimum", "maximum",
			"minLength", "maxLength", "pattern", "title", "default", "example", "minProperties", "maxProperties":
			out[k] = v
		case "nullable", "propertyOrdering", "format":
			// nullable is folded into type; ordering and format do not
			// constrain the JSON.
		default:
			return nil, fmt.Errorf("unsupported schema field %q", k)
		}
	}
	return out, nil
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
		out.JSONSchema, _ = g.jsonSchema() // validated by rangeError
		out.PresencePenalty = g.PresencePenalty
		out.FrequencyPenalty = g.FrequencyPenalty
		out.ResponseLogprobs = g.ResponseLogprobs != nil && *g.ResponseLogprobs
		if out.ResponseLogprobs && g.Logprobs != nil {
			out.TopLogprobs = *g.Logprobs
		}
		if g.ThinkingConfig != nil {
			on, budget, _ := g.thinking()
			out.Thinking = &on
			out.ThinkingBudget = budget
		}
	}
	return out
}

// GenerateContentRequest is the request body.
type GenerateContentRequest struct {
	Contents []Content `json:"contents"`

	GenerationConfig *GenerationConfig `json:"generationConfig,omitempty"`

	Tools             []Tool          `json:"tools,omitempty"`
	ToolConfig        *ToolConfig     `json:"toolConfig,omitempty"`
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
//
// safetyRatings is omitted: no safety classifier runs locally, and a rating
// nothing measured would be a fabricated one.
type Candidate struct {
	Content      Content `json:"content"`
	FinishReason string  `json:"finishReason,omitempty"`
	Index        int32   `json:"index"`
	// AvgLogprobs and LogprobsResult answer responseLogprobs (#2107): the
	// mean of the chosen tokens' log-probabilities, and each one.
	AvgLogprobs    *float64        `json:"avgLogprobs,omitempty"`
	LogprobsResult *LogprobsResult `json:"logprobsResult,omitempty"`
}

// LogprobsResult is Vertex's per-step log-probabilities. ChosenCandidates
// is the token the sampler chose at each step; TopCandidates (logprobs) the
// most probable tokens of each step, most probable first.
type LogprobsResult struct {
	TopCandidates    []TopCandidates     `json:"topCandidates,omitempty"`
	ChosenCandidates []LogprobsCandidate `json:"chosenCandidates"`
}

// TopCandidates is one step's most probable tokens.
type TopCandidates struct {
	Candidates []LogprobsCandidate `json:"candidates"`
}

// LogprobStep is one decode step: the chosen token and, with logprobs, the
// step's most probable tokens. Every score is the model's log-softmax over
// the step's logits, before sampling.
type LogprobStep struct {
	Chosen LogprobsCandidate
	Top    []LogprobsCandidate
}

// LogprobsCandidate is one token and its log-probability.
type LogprobsCandidate struct {
	Token          string  `json:"token"`
	TokenID        int32   `json:"tokenId"`
	LogProbability float32 `json:"logProbability"`
}

// withLogprobs is r's candidate with steps as its logprobsResult (the top
// candidates when the steps carry them) and the chosen tokens' mean as
// avgLogprobs.
func withLogprobs(r *GenerateContentResponse, steps []LogprobStep) *GenerateContentResponse {
	if r == nil || len(steps) == 0 || len(r.Candidates) == 0 {
		return r
	}
	var sum float64
	res := &LogprobsResult{}
	top := false
	for _, s := range steps {
		top = top || len(s.Top) > 0
	}
	for _, s := range steps {
		sum += float64(s.Chosen.LogProbability)
		res.ChosenCandidates = append(res.ChosenCandidates, s.Chosen)
		if top {
			res.TopCandidates = append(res.TopCandidates, TopCandidates{Candidates: s.Top})
		}
	}
	avg := sum / float64(len(steps))
	r.Candidates[0].AvgLogprobs = &avg
	r.Candidates[0].LogprobsResult = res
	return r
}

// ModalityTokenCount is one entry of usageMetadata's per-modality details.
type ModalityTokenCount struct {
	Modality   string `json:"modality"`
	TokenCount int32  `json:"tokenCount"`
}

// UsageMetadata is the response's token accounting, every count measured by
// the model's own tokenizer (#1937): promptTokenCount is LiteRT-LM's
// Conversation::CountTokens of the rendered conversation (template, tools and
// image tokens included), candidatesTokenCount the tokens the engine decoded.
type UsageMetadata struct {
	PromptTokenCount        int32                `json:"promptTokenCount"`
	CandidatesTokenCount    int32                `json:"candidatesTokenCount,omitempty"`
	TotalTokenCount         int32                `json:"totalTokenCount"`
	CachedContentTokenCount int32                `json:"cachedContentTokenCount,omitempty"`
	ThoughtsTokenCount      int32                `json:"thoughtsTokenCount,omitempty"`
	CacheTokensDetails      []ModalityTokenCount `json:"cacheTokensDetails,omitempty"`
	PromptTokensDetails     []ModalityTokenCount `json:"promptTokensDetails,omitempty"`
	CandidatesTokensDetails []ModalityTokenCount `json:"candidatesTokensDetails,omitempty"`
}

// GenerateContentResponse is the response body.
type GenerateContentResponse struct {
	Candidates    []Candidate    `json:"candidates"`
	UsageMetadata *UsageMetadata `json:"usageMetadata,omitempty"`
	ModelVersion  string         `json:"modelVersion,omitempty"`
	ResponseID    string         `json:"responseId,omitempty"`
}

// maxRequestBytes bounds a request body. A local service still should not be
// made to buffer an arbitrary amount because a client got a loop wrong.
//
// 20 MiB leaves room for inline images (base64 in JSON). UNVERIFIED
// (re-read 2026-11-19, the v1beta1 Blob and GenerateContentRequest protos in
// cloud.google.com/go/aiplatform v1.127.0, which state no size): Vertex AI's
// own request limit.
const maxRequestBytes = 20 << 20

// decodeRequest reads and validates a request.
//
// Unknown fields are rejected. That is the strict choice, and it is the right
// one here: an option this runtime does not implement must not be silently
// dropped, and DisallowUnknownFields is what catches the ones not enumerated
// above — including any Vertex adds after this was written.
func decodeRequest(r io.Reader, options bool) (*GenerateContentRequest, error) {
	req, err := decodeBody(r)
	if err != nil {
		return nil, err
	}
	if err := validate(req, options); err != nil {
		return nil, err
	}
	return req, nil
}

// decodeBody reads a request without validating it.
func decodeBody(r io.Reader) (*GenerateContentRequest, error) {
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
	return &req, nil
}

// validate enforces the documented subset. options marks a runtime that
// takes whole conversations (cloudburrow_generate); without it only a single
// text turn can be served, as litert_lm_main takes one prompt.
func validate(req *GenerateContentRequest, options bool) error {
	switch {
	case len(req.SafetySettings) > 0:
		return unsupportedField("safetySettings")
	case req.CachedContent != "":
		// Expanded by the server (cachedcontent.go) before validation; a
		// request that reaches here with one was not resolved.
		return unsupportedField("cachedContent")
	}
	if !options {
		switch {
		case len(req.Tools) > 0:
			return unsupportedField("tools")
		case req.ToolConfig != nil:
			return unsupportedField("toolConfig")
		case req.SystemInstruction != nil:
			return unsupportedField("systemInstruction")
		}
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
	if options {
		if err := validateTools(req.Tools, req.ToolConfig); err != nil {
			return err
		}
		return validateConversation(req, true)
	}

	if len(req.Contents) == 0 {
		return apierror.InvalidArgument("contents is required and must contain one user turn")
	}
	// Multi-turn needs the conversation runner (cloudburrow_generate); the
	// bare litert_lm_main takes one prompt.
	if len(req.Contents) > 1 {
		return apierror.Unimplemented(
			"multi-turn conversations are not supported by this runtime: litert_lm_main takes one prompt, and joining "+
				"%d turns would need a chat template this service would be inventing; cloudburrow_generate (the default "+
				"runtime) applies the model's own", len(req.Contents))
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

// promptOf joins the text parts of the last turn: the single turn a
// one-prompt runtime serves, and what a generator that reads only Prompt
// (the scripted one, the language and translation helpers) is given.
func promptOf(req *GenerateContentRequest) string {
	if len(req.Contents) == 0 {
		return ""
	}
	var b strings.Builder
	for _, p := range req.Contents[len(req.Contents)-1].Parts {
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

// candidateResponse frames text and function calls: the text part first
// (when there is text), then one functionCall part per call, as Gemini
// orders them.
func candidateResponse(model, text string, calls []FunctionCall, finish string, usage *Usage) *GenerateContentResponse {
	return thoughtResponse(model, "", text, calls, finish, usage)
}

// thoughtResponse is candidateResponse with the model's thoughts first, as a
// part marked thought (includeThoughts, #2107).
func thoughtResponse(model, thought, text string, calls []FunctionCall, finish string, usage *Usage) *GenerateContentResponse {
	var parts []Part
	if thought != "" {
		parts = append(parts, Part{Text: thought, Thought: ptr(true)})
	}
	if text != "" || len(calls) == 0 {
		parts = append(parts, Part{Text: text})
	}
	for i := range calls {
		c := calls[i]
		parts = append(parts, Part{FunctionCall: &c})
	}
	r := &GenerateContentResponse{
		Candidates: []Candidate{{
			Content:      Content{Role: "model", Parts: parts},
			FinishReason: finish,
		}},
		ModelVersion: model,
	}
	r.UsageMetadata = usage.metadata()
	return r
}

// metadata renders measured counts as usageMetadata, or nil when the
// generator measured nothing (a runtime without cloudburrow_generate).
// candidatesTokenCount is omitted when it was not measured: a generation
// ended by a stop sequence is killed before the engine reports its count.
func (u *Usage) metadata() *UsageMetadata {
	if u == nil || u.PromptTokens < 0 {
		return nil
	}
	m := &UsageMetadata{PromptTokenCount: int32(u.PromptTokens), TotalTokenCount: int32(u.PromptTokens)}
	if !u.Images {
		m.PromptTokensDetails = []ModalityTokenCount{{Modality: "TEXT", TokenCount: int32(u.PromptTokens)}}
	}
	// The cached content's tokens are part of promptTokenCount ("When
	// cached_content is set, this is still the total effective prompt size",
	// the v1 GenerateContentResponse.UsageMetadata proto), counted on their
	// own as cachedContentTokenCount.
	if u.CachedTokens > 0 {
		m.CachedContentTokenCount = int32(u.CachedTokens)
		if !u.CachedImages {
			m.CacheTokensDetails = []ModalityTokenCount{{Modality: "TEXT", TokenCount: int32(u.CachedTokens)}}
		}
	}
	if u.CandidateTokens >= 0 {
		cand := u.CandidateTokens
		// Thoughts are decoded tokens too; Vertex counts them apart from the
		// candidates, and totalTokenCount holds both.
		if u.ThoughtTokens > 0 {
			m.ThoughtsTokenCount = int32(u.ThoughtTokens)
			cand = max(cand-u.ThoughtTokens, 0)
			m.TotalTokenCount += int32(u.ThoughtTokens)
		}
		m.CandidatesTokenCount = int32(cand)
		m.TotalTokenCount += int32(cand)
		m.CandidatesTokensDetails = []ModalityTokenCount{{Modality: "TEXT", TokenCount: int32(cand)}}
	}
	return m
}

func ptr[T any](v T) *T { return &v }
