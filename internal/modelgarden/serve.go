package modelgarden

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Prediction on a deployed Hugging Face model, in the shapes Google
// documents for Model Garden's vLLM and TEI deployments:
//
//   - :predict on vLLM takes {"instances":[{"prompt", "max_tokens",
//     "temperature", "top_p", "top_k", "raw_response"}]} and answers one
//     string per instance, "Prompt:\n{prompt}\nOutput:\n{text}" (just the
//     text with raw_response). An instance with "@requestFormat":
//     "chatCompletions" carries an OpenAI chat body and answers the chat
//     completion object (PublisherModel deployMetadata.sampleRequest).
//   - :predict on TEI takes {"instances":[{"inputs": ...}]} and answers,
//     per instance, TEI's /embed answer: one vector per input.
//   - :rawPredict relays an OpenAI body ("messages": chat completions,
//     "prompt": completions) to vLLM, or TEI's own {"inputs"} to /embed; a
//     body with "instances" is answered as :predict.
//   - :streamRawPredict and v1beta1 .../chat/completions relay OpenAI
//     bodies to vLLM, streaming its server-sent events when "stream" is set.

// bodyLimit bounds an engine's answer.
const bodyLimit = 32 << 20

func (m *Manager) post(ctx context.Context, s Serving, route string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.BaseURL+route, bytes.NewReader(body))
	if err != nil {
		return nil, apierror.Internal(err, "building the request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.client().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &apierror.Error{Code: codes.Unavailable, Reason: "backendError",
			Message: fmt.Sprintf("the %s engine did not answer: %v", s.Engine, err)}
	}
	return resp, nil
}

// call posts body to route and returns the answer, mapping a refusal.
func (m *Manager) call(ctx context.Context, s Serving, route string, body []byte) ([]byte, error) {
	resp, err := m.post(ctx, s, route, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, bodyLimit))
	if err != nil {
		return nil, &apierror.Error{Code: codes.Unavailable, Reason: "backendError", Message: fmt.Sprintf("reading the answer: %v", err)}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, engineError(resp.StatusCode, b)
	}
	return b, nil
}

// engineError keeps the engine's own words.
func engineError(status int, b []byte) error {
	msg := strings.TrimSpace(string(b))
	var env struct {
		Error   any    `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &env) == nil {
		switch e := env.Error.(type) {
		case string:
			msg = e
		case map[string]any:
			if s, ok := e["message"].(string); ok {
				msg = s
			}
		}
		if env.Message != "" {
			msg = env.Message
		}
	}
	if len(msg) > 2048 {
		msg = msg[:2048]
	}
	code := codes.Internal
	switch {
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity || status == http.StatusRequestEntityTooLarge:
		code = codes.InvalidArgument
	case status == http.StatusNotFound:
		code = codes.NotFound
	case status == http.StatusTooManyRequests:
		code = codes.ResourceExhausted
	case status == http.StatusServiceUnavailable:
		code = codes.Unavailable
	case status >= 400 && status < 500:
		code = codes.FailedPrecondition
	}
	return &apierror.Error{Code: code, Reason: "endpointError", Message: fmt.Sprintf("the model server answered %d: %s", status, msg)}
}

// predictRequest is Vertex's :predict body.
type predictRequest struct {
	Instances  []json.RawMessage `json:"instances"`
	Parameters json.RawMessage   `json:"parameters,omitempty"`
}

// predictResponse is Vertex's :predict answer.
type predictResponse struct {
	Predictions      []json.RawMessage `json:"predictions"`
	DeployedModelID  string            `json:"deployedModelId"`
	Model            string            `json:"model"`
	ModelDisplayName string            `json:"modelDisplayName"`
	ModelVersionID   string            `json:"modelVersionId"`
}

// Predict answers :predict on endpoint.
func (m *Manager) Predict(ctx context.Context, project, location, endpoint string, body []byte) ([]byte, error) {
	s, err := m.Target(project, location, endpoint)
	if err != nil {
		return nil, err
	}
	var req predictRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, apierror.InvalidArgument("the request is not a prediction request: %v", err)
	}
	if len(req.Instances) == 0 {
		return nil, apierror.InvalidArgument("instances is required and must not be empty")
	}
	var params map[string]any
	if len(req.Parameters) > 0 && string(req.Parameters) != "null" {
		if err := json.Unmarshal(req.Parameters, &params); err != nil {
			return nil, apierror.InvalidArgument("parameters must be an object: %v", err)
		}
	}
	out := predictResponse{DeployedModelID: s.DeployedModelID, Model: s.ModelName, ModelDisplayName: s.ModelDisplay, ModelVersionID: "1"}
	for i, raw := range req.Instances {
		var inst map[string]any
		if err := json.Unmarshal(raw, &inst); err != nil || inst == nil {
			return nil, apierror.InvalidArgument("instances[%d] must be an object", i)
		}
		for k, v := range params {
			if _, ok := inst[k]; !ok {
				inst[k] = v
			}
		}
		var p json.RawMessage
		switch s.Engine {
		case EngineVLLM:
			p, err = m.vllmInstance(ctx, s, i, inst)
		case EngineTEI:
			p, err = m.teiInstance(ctx, s, i, inst)
		default:
			err = apierror.Internal(nil, "unknown engine %q", s.Engine)
		}
		if err != nil {
			return nil, err
		}
		out.Predictions = append(out.Predictions, p)
	}
	return json.Marshal(out)
}

func (m *Manager) vllmInstance(ctx context.Context, s Serving, i int, inst map[string]any) (json.RawMessage, error) {
	if f, _ := inst["@requestFormat"].(string); f != "" {
		if f != "chatCompletions" {
			return nil, apierror.InvalidArgument("instances[%d]: @requestFormat %q is not supported; it is \"chatCompletions\"", i, f)
		}
		delete(inst, "@requestFormat")
		if _, ok := inst["messages"]; !ok {
			return nil, apierror.InvalidArgument("instances[%d]: a chatCompletions instance needs messages", i)
		}
		inst["stream"] = false
		if name, _ := inst["model"].(string); name == "" {
			inst["model"] = s.ServedModel
		}
		b, _ := json.Marshal(inst)
		return m.call(ctx, s, "/v1/chat/completions", b)
	}
	prompt, ok := inst["prompt"].(string)
	if !ok {
		return nil, apierror.InvalidArgument("instances[%d].prompt is required and must be a string", i)
	}
	if l, _ := inst["lora_id"].(string); l != "" {
		return nil, apierror.InvalidArgument("instances[%d].lora_id %q: no LoRA adapter is deployed with this model", i, l)
	}
	if _, ok := inst["multi_modal_data"]; ok {
		return nil, apierror.InvalidArgument("instances[%d].multi_modal_data: multimodal prompts are not served on the completions route; "+
			"send an image_url content part through chat/completions", i)
	}
	raw, _ := inst["raw_response"].(bool)
	delete(inst, "raw_response")
	delete(inst, "lora_id")
	inst["model"] = s.ServedModel
	inst["stream"] = false
	b, _ := json.Marshal(inst)
	got, err := m.call(ctx, s, "/v1/completions", b)
	if err != nil {
		return nil, err
	}
	var c struct {
		Choices []struct {
			Text string `json:"text"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(got, &c); err != nil || len(c.Choices) == 0 {
		return nil, apierror.Internal(err, "vLLM answered something that is not a completion")
	}
	text := c.Choices[0].Text
	if !raw {
		text = "Prompt:\n" + prompt + "\nOutput:\n" + text
	}
	return json.Marshal(text)
}

func (m *Manager) teiInstance(ctx context.Context, s Serving, i int, inst map[string]any) (json.RawMessage, error) {
	in, ok := inst["inputs"]
	if !ok {
		return nil, apierror.InvalidArgument("instances[%d].inputs is required: a string or a list of strings to embed", i)
	}
	switch v := in.(type) {
	case string:
	case []any:
		for _, x := range v {
			if _, ok := x.(string); !ok {
				return nil, apierror.InvalidArgument("instances[%d].inputs must be a string or a list of strings", i)
			}
		}
	default:
		return nil, apierror.InvalidArgument("instances[%d].inputs must be a string or a list of strings", i)
	}
	b, _ := json.Marshal(inst)
	return m.call(ctx, s, "/embed", b)
}

// RawPredict answers :rawPredict: status, content type and body as the
// engine answered.
func (m *Manager) RawPredict(ctx context.Context, project, location, endpoint string, body []byte) (int, string, []byte, error) {
	s, err := m.Target(project, location, endpoint)
	if err != nil {
		return 0, "", nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return 0, "", nil, apierror.InvalidArgument("the request body is not a JSON object: %v", err)
	}
	if _, ok := fields["instances"]; ok {
		out, err := m.Predict(ctx, project, location, endpoint, body)
		if err != nil {
			return 0, "", nil, err
		}
		return http.StatusOK, "application/json", out, nil
	}
	route, body, err := openAIRoute(s, fields, false)
	if err != nil {
		return 0, "", nil, err
	}
	resp, err := m.post(ctx, s, route, body)
	if err != nil {
		return 0, "", nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, bodyLimit))
	if err != nil {
		return 0, "", nil, &apierror.Error{Code: codes.Unavailable, Reason: "backendError", Message: fmt.Sprintf("reading the answer: %v", err)}
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), b, nil
}

// openAIRoute picks the engine route for an OpenAI-style body and fills in
// the served model when the body names none (vLLM's model may be "").
func openAIRoute(s Serving, fields map[string]json.RawMessage, stream bool) (string, []byte, error) {
	route := ""
	switch s.Engine {
	case EngineTEI:
		if _, ok := fields["inputs"]; ok {
			route = "/embed"
		} else if _, ok := fields["input"]; ok {
			route = "/v1/embeddings"
		} else {
			return "", nil, apierror.InvalidArgument(`a Text Embeddings Inference endpoint takes {"inputs": ...} (or {"instances": [...]})`)
		}
		if stream {
			return "", nil, apierror.FailedPrecondition("Text Embeddings Inference does not stream; use :predict or :rawPredict")
		}
	case EngineVLLM:
		if _, ok := fields["messages"]; ok {
			route = "/v1/chat/completions"
		} else if _, ok := fields["prompt"]; ok {
			route = "/v1/completions"
		} else {
			return "", nil, apierror.InvalidArgument(`a vLLM endpoint takes an OpenAI chat completions body ({"messages": [...]}), ` +
				`a completions body ({"prompt": ...}) or {"instances": [...]}`)
		}
		var name string
		if raw, ok := fields["model"]; !ok || json.Unmarshal(raw, &name) != nil || name == "" {
			fields["model"], _ = json.Marshal(s.ServedModel)
		}
		if stream {
			fields["stream"] = json.RawMessage("true")
		}
	}
	b, err := json.Marshal(fields)
	return route, b, err
}

// Stream relays an OpenAI body to vLLM and calls emit with each server-sent
// event, or once with the whole answer when the body does not stream.
// forceStream is set for :streamRawPredict.
func (m *Manager) Stream(ctx context.Context, project, location, endpoint string, body []byte, forceStream bool,
	emit func(contentType string, data []byte) error) error {
	s, err := m.Target(project, location, endpoint)
	if err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return apierror.InvalidArgument("the request body is not a JSON object: %v", err)
	}
	if s.Engine == EngineTEI && !forceStream {
		if _, ok := fields["messages"]; ok {
			return apierror.FailedPrecondition("endpoint %s serves a Text Embeddings Inference model, which has no chat completions", endpoint)
		}
	}
	if _, ok := fields["instances"]; ok && forceStream {
		out, err := m.Predict(ctx, project, location, endpoint, body)
		if err != nil {
			return err
		}
		return emit("application/json", out)
	}
	if s.Engine == EngineVLLM && !forceStream {
		if _, ok := fields["messages"]; !ok {
			return apierror.InvalidArgument("messages is required")
		}
	}
	route, b, err := openAIRoute(s, fields, forceStream)
	if err != nil {
		return err
	}
	resp, err := m.post(ctx, s, route, b)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	ct := resp.Header.Get("Content-Type")
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		got, _ := io.ReadAll(io.LimitReader(resp.Body, bodyLimit))
		return engineError(resp.StatusCode, got)
	}
	if !strings.HasPrefix(ct, "text/event-stream") {
		got, err := io.ReadAll(io.LimitReader(resp.Body, bodyLimit))
		if err != nil {
			return &apierror.Error{Code: codes.Unavailable, Reason: "backendError", Message: fmt.Sprintf("reading the answer: %v", err)}
		}
		return emit(ct, got)
	}
	// One event per emit, framed as the engine framed it.
	r := bufio.NewReader(resp.Body)
	var ev bytes.Buffer
	for {
		line, err := r.ReadBytes('\n')
		ev.Write(line)
		if bytes.Equal(bytes.TrimRight(line, "\r\n"), nil) && ev.Len() > len(line) || (err != nil && ev.Len() > 0) {
			if e := emit("text/event-stream", bytes.Clone(ev.Bytes())); e != nil {
				return e
			}
			ev.Reset()
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return &apierror.Error{Code: codes.Unavailable, Reason: "backendError", Message: fmt.Sprintf("reading the stream: %v", err)}
		}
	}
}
