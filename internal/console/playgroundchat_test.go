package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/service/vertexai"
)

// TestPlaygroundRelaysTheConversation (#1962): the Studio's chat sends its
// earlier turns, a system instruction and function declarations with their
// mode as the request's own fields, and the real vertexai server turns them
// into the runtime's conversation: the system instruction and earlier turns
// as the preface, the declarations as tools, the mode as tool_choice. A
// model's function call comes back as a functionCall part, with the
// response's usageMetadata.
func TestPlaygroundRelaysTheConversation(t *testing.T) {
	const model = "gemma-4-e2b-it-community"
	var mu sync.Mutex
	var got vertexai.Request
	gen := &vertexai.ScriptedGenerator{ID: model, Options: true,
		Calls: []vertexai.FunctionCall{{Name: "get_weather", Args: map[string]any{"city": "Paris"}}},
		Usage: &vertexai.Usage{PromptTokens: 7, CandidateTokens: 3},
		Seen:  func(r vertexai.Request) { mu.Lock(); got = r; mu.Unlock() }}
	api := httptest.NewServer(vertexai.NewServer(gen).Handler())
	defer api.Close()
	srv := newPlaygroundServer(t, &Playground{Addr: func() string { return strings.TrimPrefix(api.URL, "http://") }, Model: model})

	resp, body := postJSON(t, srv, "/api/ai/playground", `{"stream":false,
		"systemInstruction":{"parts":[{"text":"Answer briefly."}]},
		"contents":[{"role":"user","parts":[{"text":"hi"}]},{"role":"model","parts":[{"text":"hello"}]},
			{"role":"user","parts":[{"text":"weather in Paris?"}]}],
		"tools":[{"functionDeclarations":[{"name":"get_weather","description":"The weather","parameters":{"type":"OBJECT","properties":{"city":{"type":"STRING"}}}}]}],
		"toolConfig":{"functionCallingConfig":{"mode":"ANY"}}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var answer struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					FunctionCall *struct {
						Name string         `json:"name"`
						Args map[string]any `json:"args"`
					} `json:"functionCall"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount int `json:"promptTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal([]byte(body), &answer); err != nil || len(answer.Candidates) != 1 ||
		len(answer.Candidates[0].Content.Parts) == 0 || answer.Candidates[0].Content.Parts[0].FunctionCall == nil ||
		answer.Candidates[0].Content.Parts[0].FunctionCall.Name != "get_weather" || answer.UsageMetadata.PromptTokenCount != 7 {
		t.Fatalf("answer %s (%v)", body, err)
	}
	mu.Lock()
	c := got.Conversation
	mu.Unlock()
	if c == nil || len(c.Tools) != 1 || c.ToolChoice == nil {
		t.Fatalf("the runtime was asked %+v", got)
	}
	pre, _ := json.Marshal(c.Preface)
	if !strings.Contains(string(pre), "Answer briefly.") || !strings.Contains(string(pre), "hello") {
		t.Errorf("preface %s lacks the system instruction or the earlier turns", pre)
	}

	// The function response turn: the model's call, then the user's
	// functionResponse, accepted by the API.
	resp, body = postJSON(t, srv, "/api/ai/playground", `{"stream":false,
		"contents":[{"role":"user","parts":[{"text":"weather in Paris?"}]},
			{"role":"model","parts":[{"functionCall":{"name":"get_weather","args":{"city":"Paris"}}}]},
			{"role":"user","parts":[{"functionResponse":{"name":"get_weather","response":{"forecast":"sunny"}}}]}],
		"tools":[{"functionDeclarations":[{"name":"get_weather","parameters":{"type":"OBJECT","properties":{"city":{"type":"STRING"}}}}]}],
		"toolConfig":{"functionCallingConfig":{"mode":"NONE"}}}`)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("a function response turn: %d %s", resp.StatusCode, body)
	}

	// The API's own refusal reaches the screen: a role it does not take.
	resp, body = postJSON(t, srv, "/api/ai/playground", `{"stream":false,"contents":[{"role":"system","parts":[{"text":"x"}]}]}`)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "role") {
		t.Errorf("a bad role: %d %s", resp.StatusCode, body)
	}
}

// TestPlaygroundRelaysTokenCounts (#1962): the Studio's token count is the
// API's countTokens of the conversation, system instruction and tools, and
// its tokenizer pane computeTokens' list, both from the real vertexai
// routes the SDK calls.
func TestPlaygroundRelaysTokenCounts(t *testing.T) {
	const model = "gemma-4-e2b-it-community"
	gen := &vertexai.CountingGenerator{ScriptedGenerator: &vertexai.ScriptedGenerator{ID: model, Options: true}}
	api := httptest.NewServer(vertexai.NewServer(gen).Handler())
	defer api.Close()
	srv := newPlaygroundServer(t, &Playground{Addr: func() string { return strings.TrimPrefix(api.URL, "http://") }, Model: model})

	resp, body := postJSON(t, srv, "/api/ai/playground/tokens?verb=countTokens", `{
		"systemInstruction":{"parts":[{"text":"be brief"}]},
		"contents":[{"role":"user","parts":[{"text":"one two three"}]}],"toolConfig":{"functionCallingConfig":{"mode":"AUTO"}}}`)
	var count struct {
		TotalTokens         int `json:"totalTokens"`
		PromptTokensDetails []struct {
			Modality string `json:"modality"`
		} `json:"promptTokensDetails"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &count) != nil || count.TotalTokens < 5 ||
		len(count.PromptTokensDetails) != 1 || count.PromptTokensDetails[0].Modality != "TEXT" {
		t.Errorf("countTokens: %d %s", resp.StatusCode, body)
	}
	resp, body = postJSON(t, srv, "/api/ai/playground/tokens?verb=computeTokens", `{"prompt":"one two"}`)
	var comp struct {
		TokensInfo []struct {
			TokenIDs []string `json:"tokenIds"`
			Tokens   []string `json:"tokens"`
		} `json:"tokensInfo"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &comp) != nil || len(comp.TokensInfo) != 1 ||
		strings.Join(comp.TokensInfo[0].TokenIDs, ",") != "1,2" {
		t.Errorf("computeTokens: %d %s", resp.StatusCode, body)
	}
	if resp, body := postJSON(t, srv, "/api/ai/playground/tokens?verb=generateContent", `{"prompt":"x"}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("another verb: %d %s", resp.StatusCode, body)
	}
	// A runtime without a tokenizer: the API's UNIMPLEMENTED, unchanged.
	plain := httptest.NewServer(vertexai.NewServer(&vertexai.EchoGenerator{ID: model}).Handler())
	defer plain.Close()
	srv2 := newPlaygroundServer(t, &Playground{Addr: func() string { return strings.TrimPrefix(plain.URL, "http://") }, Model: model})
	if resp, body := postJSON(t, srv2, "/api/ai/playground/tokens?verb=countTokens", `{"prompt":"x"}`); resp.StatusCode != http.StatusNotImplemented ||
		!strings.Contains(body, "tokenizer") {
		t.Errorf("no tokenizer: %d %s", resp.StatusCode, body)
	}
}
