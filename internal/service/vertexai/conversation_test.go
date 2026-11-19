package vertexai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	betapb "cloud.google.com/go/aiplatform/apiv1beta1/aiplatformpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

// convGen is a conversation-capable generator: it records what it was asked
// and answers with scripted chunks, as cloudburrow_generate's JSON lines
// would arrive.
type convGen struct {
	mu     sync.Mutex
	got    []Request
	chunks []Chunk
}

func (g *convGen) Model() string        { return testModel }
func (g *convGen) HonoursOptions() bool { return true }

func (g *convGen) Generate(ctx context.Context, req Request) (<-chan Chunk, func() error, error) {
	g.mu.Lock()
	g.got = append(g.got, req)
	chunks := append([]Chunk(nil), g.chunks...)
	g.mu.Unlock()
	out := make(chan Chunk)
	go func() {
		defer close(out)
		for _, c := range chunks {
			select {
			case out <- c:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, func() error { return nil }, nil
}

func (g *convGen) CountTokens(_ context.Context, req Request) (int, error) {
	g.mu.Lock()
	g.got = append(g.got, req)
	g.mu.Unlock()
	return 42, nil
}

func (g *convGen) ComputeTokens(_ context.Context, texts []string) ([]TokenInfo, error) {
	out := make([]TokenInfo, len(texts))
	for i, t := range texts {
		for j, w := range strings.Fields(t) {
			out[i].IDs = append(out[i].IDs, int64(1000+j))
			out[i].Tokens = append(out[i].Tokens, []byte(w))
		}
	}
	return out, nil
}

func (g *convGen) last(t *testing.T) Request {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.got) == 0 {
		t.Fatal("the generator was never called")
	}
	return g.got[len(g.got)-1]
}

func usageChunks(text string, calls []FunctionCall) []Chunk {
	return []Chunk{
		{Usage: &Usage{PromptTokens: 17, CandidateTokens: -1}},
		{Text: text},
		{ToolCalls: calls},
		{Last: true, FinishReason: FinishReasonStop, Usage: &Usage{PromptTokens: 17, CandidateTokens: 5}},
	}
}

const weatherTool = `"tools":[{"functionDeclarations":[{"name":"get_weather","description":"Weather for a city",
 "parameters":{"type":"OBJECT","properties":{"city":{"type":"STRING"}},"required":["city"]}}]}]`

func TestConversationIsTranslatedToTheModelsMessages(t *testing.T) {
	g := &convGen{chunks: usageChunks("Sunny.", nil)}
	srv := newTestServer(t, g)
	body := `{"systemInstruction":{"parts":[{"text":"Be brief."}]},
	 "contents":[
	  {"role":"user","parts":[{"text":"Weather in Paris?"}]},
	  {"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"city":"Paris"}}}]},
	  {"role":"user","parts":[{"functionResponse":{"name":"get_weather","response":{"sky":"sunny"}}}]}],
	 ` + weatherTool + `,
	 "toolConfig":{"functionCallingConfig":{"mode":"AUTO"}}}`
	resp, got := post(t, srv, vertexPath(testModel, "generateContent"), body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, got)
	}
	rr := g.last(t).Conversation
	if rr == nil {
		t.Fatal("no conversation was sent to the runtime")
	}
	b, _ := json.Marshal(rr)
	var v struct {
		Preface []struct {
			Role      string            `json:"role"`
			Content   []json.RawMessage `json:"content"`
			ToolCalls []struct {
				Function struct {
					Name      string         `json:"name"`
					Arguments map[string]any `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"preface"`
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Name     string         `json:"name"`
				Response map[string]any `json:"response"`
			} `json:"content"`
		} `json:"message"`
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name       string         `json:"name"`
				Parameters map[string]any `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
		ToolChoice struct {
			Type string `json:"type"`
		} `json:"tool_choice"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Preface) != 3 || v.Preface[0].Role != "system" || v.Preface[1].Role != "user" || v.Preface[2].Role != "assistant" {
		t.Fatalf("preface = %s", b)
	}
	if c := v.Preface[2].ToolCalls; len(c) != 1 || c[0].Function.Name != "get_weather" || c[0].Function.Arguments["city"] != "Paris" {
		t.Errorf("the model's call is not replayed as a tool call: %s", b)
	}
	if v.Message.Role != "tool" || len(v.Message.Content) != 1 || v.Message.Content[0].Response["sky"] != "sunny" {
		t.Errorf("the function response is not the answered message: %s", b)
	}
	if len(v.Tools) != 1 || v.Tools[0].Type != "function" || v.Tools[0].Function.Parameters["type"] != "object" {
		t.Errorf("tools not converted to JSON Schema: %s", b)
	}
	if v.ToolChoice.Type != "auto" {
		t.Errorf("tool_choice = %q, want auto", v.ToolChoice.Type)
	}
}

func TestToolConfigModesMapToToolChoice(t *testing.T) {
	cases := map[string]RunnerToolChoice{
		`{"mode":"ANY"}`: {Type: "required"},
		`{"mode":"ANY","allowedFunctionNames":["get_weather"]}`: {Type: "required", Names: []string{"get_weather"}},
		`{"mode":"NONE"}`: {Type: "none"},
		`{"mode":"AUTO"}`: {Type: "auto"},
		`{"mode":"VALIDATED","allowedFunctionNames":["get_weather"]}`: {Type: "allowed", Names: []string{"get_weather"}},
	}
	for cfg, want := range cases {
		g := &convGen{chunks: usageChunks("x", nil)}
		srv := newTestServer(t, g)
		body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` + weatherTool +
			`,"toolConfig":{"functionCallingConfig":` + cfg + `}}`
		resp, got := post(t, srv, vertexPath(testModel, "generateContent"), body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d: %s", cfg, resp.StatusCode, got)
		}
		tc := g.last(t).Conversation.ToolChoice
		if tc == nil || tc.Type != want.Type || strings.Join(tc.Names, ",") != strings.Join(want.Names, ",") {
			t.Errorf("%s: tool_choice = %+v, want %+v", cfg, tc, want)
		}
	}
}

func TestFunctionCallsAndUsageAreFramedAsVertexFramesThem(t *testing.T) {
	g := &convGen{chunks: usageChunks("\n", []FunctionCall{{Name: "get_weather", Args: map[string]any{"city": "Paris"}}})}
	srv := newTestServer(t, g)
	body := `{"contents":[{"role":"user","parts":[{"text":"Weather in Paris?"}]}],` + weatherTool + `}`
	resp, got := post(t, srv, vertexPath(testModel, "generateContent"), body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, got)
	}
	var r GenerateContentResponse
	if err := json.Unmarshal([]byte(got), &r); err != nil {
		t.Fatal(err)
	}
	c := r.Candidates[0]
	if c.Content.Role != "model" || len(c.Content.Parts) != 1 || c.Content.Parts[0].FunctionCall == nil ||
		c.Content.Parts[0].FunctionCall.Name != "get_weather" || c.Content.Parts[0].FunctionCall.Args["city"] != "Paris" {
		t.Fatalf("candidate = %s", got)
	}
	if c.FinishReason != "STOP" {
		t.Errorf("finishReason = %q", c.FinishReason)
	}
	u := r.UsageMetadata
	if u == nil || u.PromptTokenCount != 17 || u.CandidatesTokenCount != 5 || u.TotalTokenCount != 22 {
		t.Fatalf("usageMetadata = %+v", u)
	}
	if len(u.PromptTokensDetails) != 1 || u.PromptTokensDetails[0].Modality != "TEXT" {
		t.Errorf("promptTokensDetails = %+v", u.PromptTokensDetails)
	}
}

func TestStreamCarriesCallsAndFinalUsage(t *testing.T) {
	g := &convGen{chunks: usageChunks("ok", []FunctionCall{{Name: "get_weather", Args: map[string]any{}}})}
	srv := newTestServer(t, g)
	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` + weatherTool + `}`
	resp, got := post(t, srv, vertexPath(testModel, "streamGenerateContent")+"?alt=sse", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, got)
	}
	var events []GenerateContentResponse
	for _, line := range strings.Split(got, "\n") {
		if d, ok := strings.CutPrefix(line, "data: "); ok {
			var r GenerateContentResponse
			if err := json.Unmarshal([]byte(d), &r); err != nil {
				t.Fatalf("event %q: %v", d, err)
			}
			events = append(events, r)
		}
	}
	if len(events) != 3 {
		t.Fatalf("%d events, want text, call, final: %s", len(events), got)
	}
	if events[1].Candidates[0].Content.Parts[0].FunctionCall == nil {
		t.Errorf("the call event carries no functionCall: %s", got)
	}
	last := events[2]
	if last.Candidates[0].FinishReason != "STOP" || last.UsageMetadata == nil || last.UsageMetadata.TotalTokenCount != 22 {
		t.Errorf("final event = %+v", last)
	}
	if events[0].UsageMetadata != nil {
		t.Errorf("an intermediate event carries usageMetadata")
	}
}

func TestStopSequenceKeepsThePromptCount(t *testing.T) {
	g := &convGen{chunks: []Chunk{
		{Usage: &Usage{PromptTokens: 9, CandidateTokens: -1}},
		{Text: "one two END three"},
		{Last: true, FinishReason: FinishReasonStop, Usage: &Usage{PromptTokens: 9, CandidateTokens: 4}},
	}}
	srv := newTestServer(t, g)
	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"stopSequences":["END"]}}`
	resp, got := post(t, srv, vertexPath(testModel, "generateContent"), body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, got)
	}
	var r GenerateContentResponse
	_ = json.Unmarshal([]byte(got), &r)
	if r.Candidates[0].Content.Parts[0].Text != "one two " {
		t.Errorf("text = %q", r.Candidates[0].Content.Parts[0].Text)
	}
	if u := r.UsageMetadata; u == nil || u.PromptTokenCount != 9 || u.CandidatesTokenCount != 0 || u.TotalTokenCount != 9 {
		t.Errorf("usageMetadata = %+v; want the prompt's count and no unmeasured candidate count", u)
	}
}

func pngBase64(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(1, 1, color.RGBA{R: 255, A: 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b.Bytes())
}

func TestImagePartsReachTheVisionEncoder(t *testing.T) {
	g := &convGen{chunks: usageChunks("red", nil)}
	srv := newTestServer(t, g)
	body := `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"` + pngBase64(t) + `"}},{"text":"Colour?"}]}]}`
	resp, got := post(t, srv, vertexPath(testModel, "generateContent"), body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, got)
	}
	req := g.last(t)
	if !req.Vision {
		t.Error("an image request does not load the vision encoder")
	}
	content, _ := req.Conversation.Message["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content = %v", req.Conversation.Message)
	}
	item, _ := content[0].(map[string]any)
	if item["type"] != "image" || item["blob"] == "" {
		t.Errorf("image item = %v", item)
	}
	var r GenerateContentResponse
	_ = json.Unmarshal([]byte(got), &r)
	if r.UsageMetadata == nil || r.UsageMetadata.PromptTokensDetails != nil {
		t.Errorf("an image prompt claims a TEXT-only breakdown: %+v", r.UsageMetadata)
	}
}

func TestConversationRequestsAreValidatedAsVertexValidatesThem(t *testing.T) {
	g := &convGen{chunks: usageChunks("x", nil)}
	srv := newTestServer(t, g)
	user := `{"role":"user","parts":[{"text":"hi"}]}`
	call := `{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{}}}]}`
	cases := map[string]struct {
		body   string
		status int
		want   string
	}{
		"bad role":              {`{"contents":[` + user + `,{"role":"assistant","parts":[{"text":"x"}]},` + user + `]}`, 400, "valid role"},
		"ends on model":         {`{"contents":[` + user + `,{"role":"model","parts":[{"text":"x"}]}]}`, 400, "last turn"},
		"response without call": {`{"contents":[` + user + `,{"role":"user","parts":[{"functionResponse":{"name":"f","response":{}}}]}]}`, 400, "immediately after a function call"},
		"count mismatch": {`{"contents":[` + user + `,{"role":"model","parts":[{"functionCall":{"name":"get_weather"}},{"functionCall":{"name":"get_weather"}}]},` +
			`{"role":"user","parts":[{"functionResponse":{"name":"get_weather","response":{}}}]}],` + weatherTool + `}`, 400, "number of function response parts"},
		"response name mismatch": {`{"contents":[` + user + `,` + call + `,` +
			`{"role":"user","parts":[{"functionResponse":{"name":"get_time","response":{}}}]}],` + weatherTool + `}`, 400, "matches no functionCall"},
		"response id mismatch": {`{"contents":[` + user + `,{"role":"model","parts":[{"functionCall":{"id":"c1","name":"get_weather"}}]},` +
			`{"role":"user","parts":[{"functionResponse":{"id":"c1","name":"get_time","response":{}}}]}],` + weatherTool + `}`, 400, "must match the name of its functionCall"},
		"call not answered":  {`{"contents":[` + user + `,` + call + `,` + user + `]}`, 400, "immediately after a function call"},
		"empty part":         {`{"contents":[{"role":"user","parts":[{}]}]}`, 400, "empty text parameter"},
		"pdf":                {`{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"application/pdf","data":"AA=="}}]}]}`, 400, `application/pdf`},
		"system image":       {`{"systemInstruction":{"parts":[{"inlineData":{"mimeType":"image/png","data":"AA=="}}]},"contents":[` + user + `]}`, 400, "only text"},
		"google search":      {`{"contents":[` + user + `],"tools":[{"googleSearch":{}}]}`, 501, "googleSearch"},
		"bad function name":  {`{"contents":[` + user + `],"tools":[{"functionDeclarations":[{"name":"1bad"}]}]}`, 400, "maximum length of 64"},
		"allowed with AUTO":  {`{"contents":[` + user + `],` + weatherTool + `,"toolConfig":{"functionCallingConfig":{"mode":"AUTO","allowedFunctionNames":["get_weather"]}}}`, 400, "only be set when the mode is ANY"},
		"undeclared allowed": {`{"contents":[` + user + `],` + weatherTool + `,"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["nope"]}}}`, 400, "not a declared function"},
		"bad mode":           {`{"contents":[` + user + `],` + weatherTool + `,"toolConfig":{"functionCallingConfig":{"mode":"SOMETIMES"}}}`, 400, "AUTO, ANY, NONE"},
		"both schemas": {`{"contents":[` + user + `],"tools":[{"functionDeclarations":[{"name":"f","parameters":{"type":"OBJECT"},` +
			`"parametersJsonSchema":{"type":"object"}}]}]}`, 400, "mutually exclusive"},
		"presence range":     {`{"contents":[` + user + `],"generationConfig":{"presencePenalty":2}}`, 400, "presencePenalty"},
		"code execution":     {`{"contents":[{"role":"model","parts":[{"executableCode":{"code":"1"}}]},` + user + `]}`, 501, "executableCode"},
		"safety settings":    {`{"contents":[` + user + `],"safetySettings":[{"category":"HARM_CATEGORY_HARASSMENT","threshold":"BLOCK_NONE"}]}`, 400, "safetySettings"},
		"file without store": {`{"contents":[{"role":"user","parts":[{"fileData":{"mimeType":"image/png","fileUri":"gs://b/o.png"}}]}]}`, 400, "Cloud Storage"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			resp, got := post(t, srv, vertexPath(testModel, "generateContent"), c.body)
			if resp.StatusCode != c.status || !strings.Contains(got, c.want) {
				t.Errorf("status %d, want %d containing %q: %s", resp.StatusCode, c.status, c.want, got)
			}
		})
	}
}

func TestPenaltiesReachTheRuntime(t *testing.T) {
	g := &convGen{chunks: usageChunks("x", nil)}
	srv := newTestServer(t, g)
	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"presencePenalty":0.5,"frequencyPenalty":-1}}`
	resp, got := post(t, srv, vertexPath(testModel, "generateContent"), body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, got)
	}
	req := g.last(t)
	if req.PresencePenalty == nil || *req.PresencePenalty != 0.5 || req.FrequencyPenalty == nil || *req.FrequencyPenalty != -1 {
		t.Errorf("penalties = %v, %v", req.PresencePenalty, req.FrequencyPenalty)
	}
	flags := strings.Join(runnerFlags(req), " ")
	if !strings.Contains(flags, "--presence_penalty=0.5") || !strings.Contains(flags, "--frequency_penalty=-1") || !strings.Contains(flags, "--request_stdin") {
		t.Errorf("runner flags = %s", flags)
	}
}

func TestCountAndComputeTokens(t *testing.T) {
	g := &convGen{}
	srv := newTestServer(t, g)
	for _, v := range []string{"v1", APIVersion} {
		path := "/" + v + "/projects/p/locations/us-central1/publishers/google/models/" + testModel
		resp, got := post(t, srv, path+":countTokens", `{"contents":[{"role":"user","parts":[{"text":"hello world"}]}],`+weatherTool+`}`)
		if resp.StatusCode != http.StatusOK || !strings.Contains(got, `"totalTokens":42`) || !strings.Contains(got, `"modality":"TEXT"`) {
			t.Errorf("%s countTokens: %d %s", v, resp.StatusCode, got)
		}
		if rr := g.last(t).Conversation; rr == nil || len(rr.Tools) != 1 {
			t.Errorf("%s countTokens did not count the tools", v)
		}
		resp, got = post(t, srv, path+":computeTokens", `{"contents":[{"role":"user","parts":[{"text":"hello world"}]}]}`)
		var r ComputeTokensResponse
		if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(got), &r) != nil || len(r.TokensInfo) != 1 {
			t.Fatalf("%s computeTokens: %d %s", v, resp.StatusCode, got)
		}
		ti := r.TokensInfo[0]
		if ti.Role != "user" || strings.Join(ti.TokenIDs, ",") != "1000,1001" || string(ti.Tokens[1]) != "world" {
			t.Errorf("%s tokensInfo = %+v", v, ti)
		}
		if !strings.Contains(got, `"tokenIds":["1000","1001"]`) {
			t.Errorf("tokenIds are not int64 strings: %s", got)
		}
	}
}

func TestCountTokensNeedsTheModelsTokenizer(t *testing.T) {
	srv := newTestServer(t, &EchoGenerator{ID: testModel})
	resp, got := post(t, srv, vertexPath(testModel, "countTokens"), helloBody)
	if resp.StatusCode != http.StatusNotImplemented || !strings.Contains(got, "tokenizer") {
		t.Errorf("status %d: %s", resp.StatusCode, got)
	}
}

func TestBetaPredictionServiceBridgesToV1(t *testing.T) {
	g := &convGen{chunks: usageChunks("hello", nil)}
	s := NewServer(g)
	b := &betaPrediction{s: s}
	model := "projects/p/locations/us-central1/publishers/google/models/" + testModel
	resp, err := b.GenerateContent(context.Background(), &betapb.GenerateContentRequest{
		Model:    model,
		Contents: []*betapb.Content{{Role: "user", Parts: []*betapb.Part{{Data: &betapb.Part_Text{Text: "hi"}}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetCandidates()[0].GetContent().GetParts()[0].GetText() != "hello" || resp.GetUsageMetadata().GetTotalTokenCount() != 22 {
		t.Errorf("response = %v", resp)
	}
	ct, err := b.CountTokens(context.Background(), &betapb.CountTokensRequest{
		Endpoint: model,
		Contents: []*betapb.Content{{Role: "user", Parts: []*betapb.Part{{Data: &betapb.Part_Text{Text: "hi"}}}}},
	})
	if err != nil || ct.GetTotalTokens() != 42 {
		t.Errorf("CountTokens = %v, %v", ct, err)
	}
	// A field the subset does not serve is refused by name, not dropped.
	_, err = b.GenerateContent(context.Background(), &betapb.GenerateContentRequest{
		Model:            model,
		Contents:         []*betapb.Content{{Role: "user", Parts: []*betapb.Part{{Data: &betapb.Part_Text{Text: "hi"}}}}},
		ModelArmorConfig: &betapb.ModelArmorConfig{PromptTemplateName: "x"},
	})
	if c := status.Code(err); c != codes.Unimplemented && c != codes.InvalidArgument {
		t.Errorf("an unserved field: %v", err)
	}
}

func TestV1GRPCFunctionCallRoundTrip(t *testing.T) {
	g := &convGen{chunks: usageChunks("", []FunctionCall{{Name: "get_weather", Args: map[string]any{"city": "Oslo"}}})}
	p := &predictionService{s: NewServer(g)}
	args, _ := structpb.NewStruct(map[string]any{"city": "Oslo"})
	resp, err := p.GenerateContent(context.Background(), &aiplatformpb.GenerateContentRequest{
		Model: "projects/p/locations/us-central1/publishers/google/models/" + testModel,
		Contents: []*aiplatformpb.Content{
			{Role: "user", Parts: []*aiplatformpb.Part{{Data: &aiplatformpb.Part_Text{Text: "Oslo?"}}}},
			{Role: "model", Parts: []*aiplatformpb.Part{{Data: &aiplatformpb.Part_FunctionCall{FunctionCall: &aiplatformpb.FunctionCall{Name: "get_weather", Args: args}}}}},
			{Role: "user", Parts: []*aiplatformpb.Part{{Data: &aiplatformpb.Part_FunctionResponse{FunctionResponse: &aiplatformpb.FunctionResponse{Name: "get_weather", Response: args}}}}},
		},
		Tools: []*aiplatformpb.Tool{{FunctionDeclarations: []*aiplatformpb.FunctionDeclaration{{Name: "get_weather"}}}},
		ToolConfig: &aiplatformpb.ToolConfig{FunctionCallingConfig: &aiplatformpb.FunctionCallingConfig{
			Mode: aiplatformpb.FunctionCallingConfig_ANY}},
	})
	if err != nil {
		t.Fatal(err)
	}
	fc := resp.GetCandidates()[0].GetContent().GetParts()[0].GetFunctionCall()
	if fc.GetName() != "get_weather" || fc.GetArgs().GetFields()["city"].GetStringValue() != "Oslo" {
		t.Errorf("functionCall = %v", fc)
	}
	if tc := g.last(t).Conversation.ToolChoice; tc == nil || tc.Type != "required" {
		t.Errorf("ANY over gRPC = %+v", tc)
	}
}

func TestRunnerLinesBecomeChunks(t *testing.T) {
	in := `{"prompt_tokens":12}
{"text":"Hel"}
{"text":"lo"}
{"tool_calls":[{"type":"function","function":{"name":"f","arguments":{"a":1}}},{"type":"function","function":{"name":"g","arguments":"{\"b\":true}"}}]}
`
	out := make(chan Chunk, 10)
	u := &Usage{PromptTokens: -1, CandidateTokens: -1}
	streamJSONLines(context.Background(), strings.NewReader(in), out, u)
	close(out)
	var got []Chunk
	for c := range out {
		got = append(got, c)
	}
	if len(got) != 4 || got[0].Usage == nil || got[0].Usage.PromptTokens != 12 || got[1].Text != "Hel" || got[2].Text != "lo" {
		t.Fatalf("chunks = %+v", got)
	}
	calls := got[3].ToolCalls
	if len(calls) != 2 || calls[0].Name != "f" || calls[0].Args["a"] != float64(1) || calls[1].Args["b"] != true {
		t.Errorf("calls = %+v", calls)
	}
	if u.PromptTokens != 12 {
		t.Errorf("usage = %+v", u)
	}
}
