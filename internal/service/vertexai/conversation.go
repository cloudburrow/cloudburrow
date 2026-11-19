package vertexai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/png"
	"regexp"
	"strings"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/embedding"
)

// Conversations, system instructions, function calling and image input
// (#1937).
//
// A request is translated to LiteRT-LM's Conversation message format and run
// by cloudburrow_generate's --request_stdin mode, so the model's own chat
// template frames every turn: Gemma 4's, which declares tools, renders
// earlier calls and responses, and parses the calls the model makes in its
// native <|tool_call> syntax. Function calling is therefore the model's
// native tool use, not a prompt this package writes, and the call's
// arguments are constrained by LiteRT-LM's LLGuidance tool-call grammar,
// built from the declared parameter schemas, which also applies
// toolConfig's mode:
//
//	AUTO, VALIDATED         text and/or calls (ToolChoice auto, or allowed
//	                        tools when allowedFunctionNames is set)
//	ANY                     calls only (required; forced when one name)
//	NONE                    text only
//
// Translation, Gemini to LiteRT-LM:
//
//	systemInstruction              {"role":"system"} first preface message
//	user text / image parts        {"role":"user","content":[text|image]}
//	model text / functionCall      {"role":"assistant","content":[text],
//	                                "tool_calls":[{"type":"function",
//	                                "function":{"name","arguments"}}]}
//	functionResponse parts         {"role":"tool","content":[{"name","response"}]}
//
// The last message is the one the model answers; every earlier one is the
// conversation's preface, so a stateless request is replayed exactly.

// Blob is inline bytes (Part.inlineData).
type Blob struct {
	MIMEType    string `json:"mimeType"`
	Data        []byte `json:"data"`
	DisplayName string `json:"displayName,omitempty"`
}

// FileData is a Part.fileData reference.
type FileData struct {
	MIMEType    string `json:"mimeType"`
	FileURI     string `json:"fileUri"`
	DisplayName string `json:"displayName,omitempty"`
}

// FunctionCall is a Part.functionCall: the model asks the caller to run
// a declared function.
type FunctionCall struct {
	ID   string         `json:"id,omitempty"`
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

// FunctionResponse is a Part.functionResponse: the caller's result.
type FunctionResponse struct {
	ID       string         `json:"id,omitempty"`
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

// Tool is one entry of the request's tools. Only function declarations run
// locally; the fields that name Google-hosted tools exist so they can be
// refused by name rather than rejected as unknown.
type Tool struct {
	FunctionDeclarations  []FunctionDeclaration `json:"functionDeclarations,omitempty"`
	Retrieval             json.RawMessage       `json:"retrieval,omitempty"`
	GoogleSearch          json.RawMessage       `json:"googleSearch,omitempty"`
	GoogleSearchRetrieval json.RawMessage       `json:"googleSearchRetrieval,omitempty"`
	GoogleMaps            json.RawMessage       `json:"googleMaps,omitempty"`
	EnterpriseWebSearch   json.RawMessage       `json:"enterpriseWebSearch,omitempty"`
	CodeExecution         json.RawMessage       `json:"codeExecution,omitempty"`
	URLContext            json.RawMessage       `json:"urlContext,omitempty"`
	ComputerUse           json.RawMessage       `json:"computerUse,omitempty"`
}

// FunctionDeclaration declares a function the model may call.
type FunctionDeclaration struct {
	Name                 string          `json:"name"`
	Description          string          `json:"description,omitempty"`
	Parameters           json.RawMessage `json:"parameters,omitempty"`
	ParametersJSONSchema json.RawMessage `json:"parametersJsonSchema,omitempty"`
	Response             json.RawMessage `json:"response,omitempty"`
	ResponseJSONSchema   json.RawMessage `json:"responseJsonSchema,omitempty"`
}

// ToolConfig is the request's toolConfig.
type ToolConfig struct {
	FunctionCallingConfig *FunctionCallingConfig `json:"functionCallingConfig,omitempty"`
	RetrievalConfig       json.RawMessage        `json:"retrievalConfig,omitempty"`
}

// FunctionCallingConfig is toolConfig.functionCallingConfig.
type FunctionCallingConfig struct {
	Mode                        string   `json:"mode,omitempty"`
	AllowedFunctionNames        []string `json:"allowedFunctionNames,omitempty"`
	StreamFunctionCallArguments *bool    `json:"streamFunctionCallArguments,omitempty"`
}

// RunnerRequest is cloudburrow_generate's --request_stdin input.
type RunnerRequest struct {
	Preface    []map[string]any  `json:"preface"`
	Tools      []json.RawMessage `json:"tools,omitempty"`
	ToolChoice *RunnerToolChoice `json:"tool_choice,omitempty"`
	Message    map[string]any    `json:"message"`
	CountOnly  bool              `json:"count_only,omitempty"`
	Compute    []string          `json:"compute,omitempty"`
}

// RunnerToolChoice is LiteRT-LM's ToolChoice, as the runner reads it.
type RunnerToolChoice struct {
	Type  string   `json:"type"`
	Names []string `json:"names,omitempty"`
}

// functionNameRE is FunctionDeclaration.name's documented form: "Must start
// with a letter or an underscore. Must be a-z, A-Z, 0-9, or contain
// underscores, dots and dashes, with a maximum length of 64."
var functionNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.\-]{0,63}$`)

// validRoles is Vertex AI's error for a role other than user or model.
// UNVERIFIED (docs re-read 2026-11-19, still unanswered: https://docs.cloud.google.com/vertex-ai/generative-ai/docs/model-reference/inference):
// the wording is Gemini's as commonly reported; the reference documents
// the two roles, user and model, not the message.
const validRoles = "Please use a valid role: user, model."

// validateTools checks tools and toolConfig against the documented shapes
// and refuses the Google-hosted tools by name.
func validateTools(tools []Tool, tc *ToolConfig) error {
	declared := map[string]bool{}
	for i, t := range tools {
		for _, hosted := range []struct {
			name string
			set  bool
		}{
			{"retrieval", len(t.Retrieval) > 0}, {"googleSearch", len(t.GoogleSearch) > 0},
			{"googleSearchRetrieval", len(t.GoogleSearchRetrieval) > 0}, {"googleMaps", len(t.GoogleMaps) > 0},
			{"enterpriseWebSearch", len(t.EnterpriseWebSearch) > 0}, {"codeExecution", len(t.CodeExecution) > 0},
			{"urlContext", len(t.URLContext) > 0}, {"computerUse", len(t.ComputerUse) > 0},
		} {
			if hosted.set {
				return apierror.Unimplemented(
					"tools[%d].%s is not supported: it is a Google-hosted tool, and this endpoint runs only the local model; "+
						"declare functions (functionDeclarations) and run them yourself", i, hosted.name)
			}
		}
		for j, fd := range t.FunctionDeclarations {
			at := fmt.Sprintf("tools[%d].functionDeclarations[%d]", i, j)
			if !functionNameRE.MatchString(fd.Name) {
				return apierror.InvalidArgument(
					"%s.name %q is invalid: it must start with a letter or an underscore and contain only a-z, A-Z, 0-9, "+
						"underscores, dots and dashes, with a maximum length of 64", at, fd.Name)
			}
			if declared[fd.Name] {
				// UNVERIFIED (docs re-read 2026-11-19, still unanswered:
				// https://docs.cloud.google.com/vertex-ai/generative-ai/docs/model-reference/function-calling):
				// the reference does not say duplicates are refused.
				return apierror.InvalidArgument("%s.name %q is declared more than once", at, fd.Name)
			}
			declared[fd.Name] = true
			if len(fd.Parameters) > 0 && len(fd.ParametersJSONSchema) > 0 {
				return apierror.InvalidArgument("%s: parameters and parametersJsonSchema are mutually exclusive", at)
			}
			if _, err := fd.jsonParameters(); err != nil {
				return apierror.InvalidArgument("%s.parameters: %v", at, err)
			}
		}
	}
	if tc == nil {
		return nil
	}
	if len(tc.RetrievalConfig) > 0 {
		return apierror.Unimplemented("toolConfig.retrievalConfig is not supported: it configures Google-hosted retrieval tools")
	}
	fc := tc.FunctionCallingConfig
	if fc == nil {
		return nil
	}
	switch fc.Mode {
	case "", "MODE_UNSPECIFIED", "AUTO", "ANY", "NONE", "VALIDATED":
	default:
		return apierror.InvalidArgument("toolConfig.functionCallingConfig.mode %q is not one of AUTO, ANY, NONE, VALIDATED", fc.Mode)
	}
	if fc.StreamFunctionCallArguments != nil && *fc.StreamFunctionCallArguments {
		return apierror.Unimplemented(
			"toolConfig.functionCallingConfig.streamFunctionCallArguments is not supported: the runtime parses a call once it is complete")
	}
	if len(fc.AllowedFunctionNames) > 0 {
		if fc.Mode != "ANY" && fc.Mode != "VALIDATED" {
			// The reference: "Only set when the mode is ANY" (VALIDATED
			// documents the same limiting).
			// UNVERIFIED (docs re-read 2026-11-19, still unanswered: https://docs.cloud.google.com/vertex-ai/generative-ai/docs/model-reference/function-calling):
			// the error wording.
			return apierror.InvalidArgument(
				"toolConfig.functionCallingConfig.allowedFunctionNames should only be set when the mode is ANY or VALIDATED")
		}
		for _, n := range fc.AllowedFunctionNames {
			if !declared[n] {
				return apierror.InvalidArgument(
					"toolConfig.functionCallingConfig.allowedFunctionNames: %q is not a declared function", n)
			}
		}
	}
	return nil
}

// jsonParameters is the declaration's parameters as JSON Schema: the
// OpenAPI-subset Schema converted, or parametersJsonSchema as given.
func (fd FunctionDeclaration) jsonParameters() (json.RawMessage, error) {
	switch {
	case len(fd.ParametersJSONSchema) > 0:
		var v map[string]any
		if err := json.Unmarshal(fd.ParametersJSONSchema, &v); err != nil {
			return nil, fmt.Errorf("parametersJsonSchema is not a JSON Schema object: %v", err)
		}
		return fd.ParametersJSONSchema, nil
	case len(fd.Parameters) > 0:
		var v map[string]any
		if err := json.Unmarshal(fd.Parameters, &v); err != nil {
			return nil, err
		}
		out, err := openAPIToJSONSchema(v)
		if err != nil {
			return nil, err
		}
		return json.Marshal(out)
	}
	return nil, nil
}

// runnerTools converts declarations to the tools LiteRT-LM's preface takes.
func runnerTools(tools []Tool) ([]json.RawMessage, error) {
	var out []json.RawMessage
	for _, t := range tools {
		for _, fd := range t.FunctionDeclarations {
			params, err := fd.jsonParameters()
			if err != nil {
				return nil, err
			}
			fn := map[string]any{"name": fd.Name, "description": fd.Description}
			if params != nil {
				fn["parameters"] = params
			} else {
				fn["parameters"] = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			b, err := json.Marshal(map[string]any{"type": "function", "function": fn})
			if err != nil {
				return nil, err
			}
			out = append(out, b)
		}
	}
	return out, nil
}

// runnerToolChoice maps functionCallingConfig to LiteRT-LM's ToolChoice.
func runnerToolChoice(tc *ToolConfig) *RunnerToolChoice {
	if tc == nil || tc.FunctionCallingConfig == nil {
		return nil
	}
	fc := tc.FunctionCallingConfig
	switch fc.Mode {
	case "ANY":
		return &RunnerToolChoice{Type: "required", Names: fc.AllowedFunctionNames}
	case "NONE":
		return &RunnerToolChoice{Type: "none"}
	case "VALIDATED":
		if len(fc.AllowedFunctionNames) > 0 {
			return &RunnerToolChoice{Type: "allowed", Names: fc.AllowedFunctionNames}
		}
	}
	return &RunnerToolChoice{Type: "auto"}
}

// partKind names the one populated field of a part, or "" for none.
func (p Part) kind() string {
	n, kind := 0, ""
	set := func(name string, ok bool) {
		if ok {
			n++
			kind = name
		}
	}
	set("text", p.Text != "")
	set("inlineData", p.InlineData != nil)
	set("fileData", p.FileData != nil)
	set("functionCall", p.FunctionCall != nil)
	set("functionResponse", p.FunctionResponse != nil)
	set("executableCode", len(p.ExecutableCode) > 0)
	set("codeExecutionResult", len(p.CodeExecutionResult) > 0)
	if n > 1 {
		return "several"
	}
	return kind
}

// validateConversation checks contents and systemInstruction for a runtime
// that takes whole conversations. generate is false for countTokens, which
// counts any conversation, including one that ends with a model turn.
func validateConversation(req *GenerateContentRequest, generate bool) error {
	if si := req.SystemInstruction; si != nil {
		if len(si.Parts) == 0 {
			return apierror.InvalidArgument("systemInstruction.parts is required")
		}
		for i, p := range si.Parts {
			if k := p.kind(); k != "text" {
				return apierror.InvalidArgument("systemInstruction.parts[%d]: only text is supported in a system instruction", i)
			}
		}
	}
	if len(req.Contents) == 0 {
		return apierror.InvalidArgument("contents is required and must contain at least one user turn")
	}
	var pendingCalls []*FunctionCall // calls of the previous model turn awaiting responses
	for i, c := range req.Contents {
		role := c.Role
		if role == "" && len(req.Contents) == 1 {
			role = "user"
		}
		if role != "user" && role != "model" && role != "function" {
			return apierror.InvalidArgument("contents[%d].role = %q. %s", i, c.Role, validRoles)
		}
		if len(c.Parts) == 0 {
			return apierror.InvalidArgument("contents[%d].parts is required", i)
		}
		var calls []*FunctionCall
		var responses []*FunctionResponse
		for j, p := range c.Parts {
			at := fmt.Sprintf("contents[%d].parts[%d]", i, j)
			switch k := p.kind(); k {
			case "":
				// UNVERIFIED (docs re-read 2026-11-19, still unanswered:
				// https://docs.cloud.google.com/vertex-ai/generative-ai/docs/model-reference/inference):
				// Gemini's wording for an empty part; the reference does not
				// say how a part with no data is refused.
				return apierror.InvalidArgument("Unable to submit request because it has an empty text parameter. Add a value to the parameter and try again. (%s)", at)
			case "several":
				return apierror.InvalidArgument("%s: a part holds exactly one of text, inlineData, fileData, functionCall or functionResponse", at)
			case "executableCode", "codeExecutionResult":
				return apierror.Unimplemented("%s.%s is not supported: code execution is a Google-hosted tool", at, k)
			case "functionCall":
				if role != "model" {
					return apierror.InvalidArgument("%s: functionCall parts belong to model turns", at)
				}
				if p.FunctionCall.Name == "" {
					return apierror.InvalidArgument("%s.functionCall.name is required", at)
				}
				calls = append(calls, p.FunctionCall)
			case "functionResponse":
				if role == "model" {
					return apierror.InvalidArgument("%s: functionResponse parts belong to user turns", at)
				}
				if p.FunctionResponse.Name == "" {
					return apierror.InvalidArgument("%s.functionResponse.name is required", at)
				}
				responses = append(responses, p.FunctionResponse)
			case "inlineData", "fileData":
				if role == "model" {
					return apierror.InvalidArgument("%s.%s: media parts belong to user turns", at, k)
				}
				mt := p.InlineData
				var mime string
				if mt != nil {
					mime = mt.MIMEType
				} else {
					mime = p.FileData.MIMEType
				}
				if err := imageMIMEType(at+"."+k, mime); err != nil {
					return err
				}
			}
			if p.Thought != nil && *p.Thought {
				return apierror.InvalidArgument("%s: thought parts are not accepted; this model returns none", at)
			}
		}
		// The function-calling guide: "you must return all corresponding
		// functionResponse parts together in a single Content object in the
		// next turn", "Don't interleave functionCall and functionResponse
		// parts across separate turns", and "The functionResponse.name must
		// still match the corresponding functionCall.name, or the request
		// fails with a 400 error"; a response pairs with its call by id when
		// both carry one, otherwise with an unmatched call of the same name.
		// UNVERIFIED (docs re-read 2026-11-19, still unanswered:
		// https://docs.cloud.google.com/vertex-ai/generative-ai/docs/multimodal/function-calling):
		// the wording is Gemini's as commonly reported, not documented.
		if len(responses) > 0 {
			if len(pendingCalls) == 0 {
				return apierror.InvalidArgument("Please ensure that function response turn comes immediately after a function call turn. (contents[%d])", i)
			}
			if len(responses) != len(pendingCalls) {
				return apierror.InvalidArgument("Please ensure that the number of function response parts is equal to the number of function call parts of the function call turn. (contents[%d]: %d responses, %d calls)", i, len(responses), len(pendingCalls))
			}
			if err := matchFunctionResponses(i, pendingCalls, responses); err != nil {
				return err
			}
		} else if len(pendingCalls) > 0 && role != "model" {
			return apierror.InvalidArgument("Please ensure that function response turn comes immediately after a function call turn. (contents[%d])", i)
		}
		pendingCalls = calls
	}
	last := req.Contents[len(req.Contents)-1]
	if generate && last.Role == "model" {
		// UNVERIFIED (docs re-read 2026-11-19, still unanswered:
		// https://docs.cloud.google.com/vertex-ai/generative-ai/docs/model-reference/inference):
		// contents "contains conversation history and the latest request",
		// and no rule says how it must end; the model answers the last user
		// turn, so a conversation ending on the model's own turn has nothing
		// to answer.
		return apierror.InvalidArgument("contents[%d]: the last turn must be a user turn (or function responses), not a model turn", len(req.Contents)-1)
	}
	return nil
}

// matchFunctionResponses pairs each function response of contents[i] with
// a call of the previous model turn, by id when both carry one, otherwise
// with the first unmatched call of the same name, and refuses a response
// whose name matches no call (the function-calling guide's 400).
func matchFunctionResponses(i int, calls []*FunctionCall, responses []*FunctionResponse) error {
	used := make([]bool, len(calls))
	for _, r := range responses {
		k := -1
		if r.ID != "" {
			for j, c := range calls {
				if !used[j] && c.ID == r.ID {
					k = j
					break
				}
			}
			if k >= 0 && calls[k].Name != r.Name {
				return apierror.InvalidArgument(
					"contents[%d]: functionResponse %q (id %q) must match the name of its functionCall, %q", i, r.Name, r.ID, calls[k].Name)
			}
		}
		if k < 0 {
			for j, c := range calls {
				if !used[j] && c.Name == r.Name {
					k = j
					break
				}
			}
		}
		if k < 0 {
			return apierror.InvalidArgument(
				"contents[%d]: functionResponse %q matches no functionCall of the function call turn", i, r.Name)
		}
		used[k] = true
	}
	return nil
}

// imageMIMEType accepts the image types Vertex AI documents for Gemini
// (image/png, image/jpeg, image/webp, image/heic, image/heif) and refuses
// every other type by name.
func imageMIMEType(at, mt string) error {
	m := strings.ToLower(strings.TrimSpace(mt))
	switch m {
	case "image/png", "image/jpeg", "image/jpg", "image/webp", "image/heic", "image/heif":
		return nil
	case "":
		return apierror.InvalidArgument("%s.mimeType is required", at)
	}
	return apierror.InvalidArgument(
		"%s: MIME type %q is not supported here; the local model reads images (image/png, image/jpeg, image/webp, image/heic, image/heif). "+
			"Audio, video and PDF input are not served by this endpoint", at, mt)
}

// toRunner translates a validated request to the runner's conversation,
// reading fileData's gs:// objects through objs. It reports whether an image
// is present, which loads the model's vision encoder.
func toRunner(ctx context.Context, req *GenerateContentRequest, objs Objects) (*RunnerRequest, bool, error) {
	var msgs []map[string]any
	if si := req.SystemInstruction; si != nil {
		var b strings.Builder
		for _, p := range si.Parts {
			b.WriteString(p.Text)
		}
		msgs = append(msgs, map[string]any{"role": "system", "content": []any{textItem(b.String())}})
	}
	vision := false
	for i, c := range req.Contents {
		if c.Role == "model" {
			m := map[string]any{"role": "assistant"}
			var content []any
			var calls []any
			for _, p := range c.Parts {
				switch {
				case p.FunctionCall != nil:
					args := p.FunctionCall.Args
					if args == nil {
						args = map[string]any{}
					}
					calls = append(calls, map[string]any{"type": "function",
						"function": map[string]any{"name": p.FunctionCall.Name, "arguments": args}})
				default:
					content = append(content, textItem(p.Text))
				}
			}
			if content == nil {
				content = []any{}
			}
			m["content"] = content
			if len(calls) > 0 {
				m["tool_calls"] = calls
			}
			msgs = append(msgs, m)
			continue
		}
		var responses, content []any
		for j, p := range c.Parts {
			switch {
			case p.FunctionResponse != nil:
				resp := p.FunctionResponse.Response
				if resp == nil {
					resp = map[string]any{}
				}
				responses = append(responses, map[string]any{"name": p.FunctionResponse.Name, "response": resp})
			case p.InlineData != nil || p.FileData != nil:
				item, err := imageItem(ctx, fmt.Sprintf("contents[%d].parts[%d]", i, j), j, p, objs)
				if err != nil {
					return nil, false, err
				}
				content = append(content, item)
				vision = true
			default:
				content = append(content, textItem(p.Text))
			}
		}
		if len(responses) > 0 {
			msgs = append(msgs, map[string]any{"role": "tool", "content": responses})
		}
		if len(content) > 0 {
			msgs = append(msgs, map[string]any{"role": "user", "content": content})
		}
	}
	tools, err := runnerTools(req.Tools)
	if err != nil {
		return nil, false, apierror.InvalidArgument("tools: %v", err)
	}
	out := &RunnerRequest{Preface: msgs[:len(msgs)-1], Message: msgs[len(msgs)-1], Tools: tools}
	if out.Preface == nil {
		out.Preface = []map[string]any{}
	}
	if len(tools) > 0 {
		out.ToolChoice = runnerToolChoice(req.ToolConfig)
	}
	return out, vision, nil
}

func textItem(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

// imageItem decodes an image part and hands it to the runtime as PNG, which
// its decoder (stb_image) reads whatever format the caller sent: WebP and
// HEIC/HEIF are decoded by the same code EmbedContent uses.
func imageItem(ctx context.Context, at string, i int, p Part, objs Objects) (map[string]any, error) {
	var mt string
	var data []byte
	if p.InlineData != nil {
		mt, data = p.InlineData.MIMEType, p.InlineData.Data
	} else {
		var err error
		mt, data, err = readFileData(ctx, i, &aiplatformpb.FileData{MimeType: p.FileData.MIMEType, FileUri: p.FileData.FileURI}, objs)
		if err != nil {
			return nil, err
		}
		if p.FileData.MIMEType != "" {
			mt = p.FileData.MIMEType
		}
	}
	if err := imageMIMEType(at, mt); err != nil {
		return nil, err
	}
	img, err := embedding.DecodeImage(mt, data)
	if err != nil {
		return nil, apierror.InvalidArgument("%s: the %s image cannot be decoded: %v", at, mt, err)
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		return nil, apierror.Internal(err, "%s: re-encoding the image failed: %v", at, err)
	}
	return map[string]any{"type": "image", "blob": base64.StdEncoding.EncodeToString(b.Bytes())}, nil
}
