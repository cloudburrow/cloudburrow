package modelgarden

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
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
//
// The runtimes of #2148 map the same way:
//
//   - llama.cpp and mlx-lm are OpenAI-compatible servers, as vLLM is, and
//     are mapped as vLLM is: :predict's prompt instances to completions
//     (and chatCompletions instances to chat completions), :rawPredict and
//     chat/completions relaying OpenAI bodies. A GGUF embedding model on
//     llama.cpp takes TEI's {"inputs"} instances and answers one vector per
//     input, through llama.cpp's /v1/embeddings. mlx-lm always serves the
//     model it was started with: a body's "model" is replaced, so a request
//     cannot make it load another repository.
//   - The Hugging Face PyTorch Inference container and ONNX Runtime take
//     the container's own :predict body ({"instances", "parameters"}, each
//     instance the pipeline's inputs) and answer its predictions, as
//     Google's container on Vertex AI does; :rawPredict relays the body,
//     with its content type (an image or a WAV file is one instance), to
//     the container's predict route and answers what it answered. They have
//     no chat completions.

// bodyLimit bounds an engine's answer.
const bodyLimit = 32 << 20

func (m *Manager) post(ctx context.Context, s Serving, route string, body []byte) (*http.Response, error) {
	return m.postTyped(ctx, s, route, body, "application/json")
}

func (m *Manager) postTyped(ctx context.Context, s Serving, route string, body []byte, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.BaseURL+route, bytes.NewReader(body))
	if err != nil {
		return nil, apierror.Internal(err, "building the request: %v", err)
	}
	req.Header.Set("Content-Type", contentType)
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
	if eng, _ := EngineByName(s.Engine); eng.Pipeline {
		got, err := m.call(ctx, s, eng.PredictRoute, body)
		if err != nil {
			return nil, err
		}
		var p struct {
			Predictions []json.RawMessage `json:"predictions"`
		}
		if err := json.Unmarshal(got, &p); err != nil || p.Predictions == nil {
			return nil, apierror.Internal(err, "the %s container answered something that is not predictions", eng.Title)
		}
		out.Predictions = p.Predictions
		return json.Marshal(out)
	}
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
		switch {
		case s.Engine == EngineTEI:
			p, err = m.teiInstance(ctx, s, i, inst, "/embed")
		case s.embeddings():
			p, err = m.teiInstance(ctx, s, i, inst, "/v1/embeddings")
		case s.openAI():
			p, err = m.vllmInstance(ctx, s, i, inst)
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
		if name, _ := inst["model"].(string); name == "" || s.Engine == EngineMLX {
			inst["model"] = s.servedName()
		}
		s.pin(inst)
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
	inst["model"] = s.servedName()
	inst["stream"] = false
	s.pin(inst)
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
		return nil, apierror.Internal(err, "%s answered something that is not a completion", s.title())
	}
	text := c.Choices[0].Text
	if !raw {
		text = "Prompt:\n" + prompt + "\nOutput:\n" + text
	}
	return json.Marshal(text)
}

func (m *Manager) teiInstance(ctx context.Context, s Serving, i int, inst map[string]any, route string) (json.RawMessage, error) {
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
	if route == "/embed" {
		b, _ := json.Marshal(inst)
		return m.call(ctx, s, route, b)
	}
	// llama.cpp's OpenAI embeddings, answered as TEI answers: one vector
	// per input.
	b, _ := json.Marshal(map[string]any{"model": s.servedName(), "input": in})
	got, err := m.call(ctx, s, route, b)
	if err != nil {
		return nil, err
	}
	var e struct {
		Data []struct {
			Index     int             `json:"index"`
			Embedding json.RawMessage `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(got, &e); err != nil || len(e.Data) == 0 {
		return nil, apierror.Internal(err, "llama.cpp answered something that is not embeddings")
	}
	sort.Slice(e.Data, func(a, b int) bool { return e.Data[a].Index < e.Data[b].Index })
	vecs := make([]json.RawMessage, len(e.Data))
	for j, d := range e.Data {
		vecs[j] = d.Embedding
	}
	return json.Marshal(vecs)
}

// openAI reports whether the endpoint's engine is an OpenAI-compatible
// server.
func (s Serving) openAI() bool {
	e, _ := EngineByName(s.Engine)
	return e.OpenAI
}

// embeddings reports whether the endpoint serves an embedding model on an
// OpenAI-compatible server (a GGUF embedding model on llama.cpp).
func (s Serving) embeddings() bool {
	return s.Engine == EngineLlamaCpp && (s.Task == "feature-extraction" || s.Task == "sentence-similarity")
}

func (s Serving) title() string {
	e, _ := EngineByName(s.Engine)
	return e.Title
}

// servedName is the model name the engine serves under: the Hugging Face
// ID (vLLM's --served-model-name, llama.cpp's --alias), or mlx-lm's
// "default_model", the model it was started with.
func (s Serving) servedName() string {
	if s.Engine == EngineMLX {
		return "default_model"
	}
	return s.ServedModel
}

// pin keeps an mlx-lm request on the model it serves: mlx-lm loads the
// model, adapter or draft model a request names, from the Hub if need be.
func (s Serving) pin(fields map[string]any) {
	if s.Engine != EngineMLX {
		return
	}
	fields["model"] = "default_model"
	delete(fields, "adapters")
	delete(fields, "adapter")
	delete(fields, "draft_model")
}

// RawPredict answers :rawPredict: status, content type and body as the
// engine answered.
func (m *Manager) RawPredict(ctx context.Context, project, location, endpoint string, body []byte, contentType string) (int, string, []byte, error) {
	s, err := m.Target(project, location, endpoint)
	if err != nil {
		return 0, "", nil, err
	}
	if eng, _ := EngineByName(s.Engine); eng.Pipeline {
		return m.relayRaw(ctx, s, eng.PredictRoute, body, contentType)
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
	return m.relayRaw(ctx, s, route, body, "application/json")
}

// relayRaw posts body to route with its content type and answers the
// engine's status, content type and body.
func (m *Manager) relayRaw(ctx context.Context, s Serving, route string, body []byte, contentType string) (int, string, []byte, error) {
	if contentType == "" {
		contentType = "application/json"
	}
	resp, err := m.postTyped(ctx, s, route, body, contentType)
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
	case EngineVLLM, EngineLlamaCpp, EngineMLX:
		if _, ok := fields["messages"]; ok {
			route = "/v1/chat/completions"
		} else if _, ok := fields["prompt"]; ok {
			route = "/v1/completions"
		} else if _, ok := fields["input"]; ok && s.embeddings() {
			route = "/v1/embeddings"
		} else {
			return "", nil, apierror.InvalidArgument(`a %s endpoint takes an OpenAI chat completions body ({"messages": [...]}), `+
				`a completions body ({"prompt": ...}) or {"instances": [...]}`, s.title())
		}
		if s.embeddings() && route != "/v1/embeddings" {
			return "", nil, apierror.FailedPrecondition("the endpoint serves an embedding model on llama.cpp, which takes "+
				`{"input": ...} or {"instances": [{"inputs": ...}]}, not %s`, route)
		}
		var name string
		if raw, ok := fields["model"]; !ok || json.Unmarshal(raw, &name) != nil || name == "" || s.Engine == EngineMLX {
			fields["model"], _ = json.Marshal(s.servedName())
		}
		if s.Engine == EngineMLX {
			delete(fields, "adapters")
			delete(fields, "adapter")
			delete(fields, "draft_model")
		}
		if stream {
			fields["stream"] = json.RawMessage("true")
		}
	default:
		return "", nil, apierror.FailedPrecondition("the %s container has no OpenAI-compatible routes; use :predict or :rawPredict", s.title())
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
	if (s.Engine == EngineTEI || s.embeddings()) && !forceStream {
		if _, ok := fields["messages"]; ok {
			return apierror.FailedPrecondition("endpoint %s serves an embedding model on %s, which has no chat completions", endpoint, s.title())
		}
	}
	if eng, _ := EngineByName(s.Engine); eng.Pipeline {
		if !forceStream {
			return apierror.FailedPrecondition("endpoint %s serves a %s model, which has no chat completions", endpoint, eng.Title)
		}
		if _, ok := fields["instances"]; !ok {
			return apierror.InvalidArgument(`the %s container takes {"instances": [...], "parameters": {...}}`, eng.Title)
		}
	}
	if _, ok := fields["instances"]; ok && forceStream {
		out, err := m.Predict(ctx, project, location, endpoint, body)
		if err != nil {
			return err
		}
		return emit("application/json", out)
	}
	if s.openAI() && !s.embeddings() && !forceStream {
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
