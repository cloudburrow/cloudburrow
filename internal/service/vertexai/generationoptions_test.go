package vertexai

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// #2107: candidateCount, thinkingConfig and cachedContent.

func optionsGen() *convGen {
	return &convGen{chunks: []Chunk{
		{Usage: &Usage{PromptTokens: 50, CandidateTokens: -1}},
		{Thought: "Let me think."},
		{Text: "hello"},
		{Last: true, FinishReason: FinishReasonStop, Usage: &Usage{PromptTokens: 50, CandidateTokens: 7, ThoughtTokens: 3}},
	}}
}

func decodeResp(t *testing.T, body string) GenerateContentResponse {
	t.Helper()
	var r GenerateContentResponse
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return r
}

func TestCandidateCountRunsIndependentGenerations(t *testing.T) {
	g := optionsGen()
	srv := newTestServer(t, g)
	resp, body := post(t, srv, vertexPath(testModel, "generateContent"),
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"candidateCount":3,"seed":7}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	r := decodeResp(t, body)
	if len(r.Candidates) != 3 {
		t.Fatalf("%d candidates, want 3: %s", len(r.Candidates), body)
	}
	for i, c := range r.Candidates {
		if c.Index != int32(i) || c.Content.Parts[len(c.Content.Parts)-1].Text != "hello" {
			t.Errorf("candidate %d: %+v", i, c)
		}
	}
	g.mu.Lock()
	var seeds []int32
	for _, req := range g.got {
		seeds = append(seeds, *req.Seed)
	}
	g.mu.Unlock()
	if fmt.Sprint(seeds) != "[7 8 9]" {
		t.Errorf("seeds %v, want [7 8 9]: each candidate is its own sample", seeds)
	}
	u := r.UsageMetadata
	// The prompt once, the candidates summed (7 decoded, 3 of them thoughts).
	if u.PromptTokenCount != 50 || u.CandidatesTokenCount != 12 || u.ThoughtsTokenCount != 9 || u.TotalTokenCount != 71 {
		t.Errorf("usage %+v", u)
	}

	// Without a seed every candidate gets its own.
	g.got = nil
	post(t, srv, vertexPath(testModel, "generateContent"),
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"candidateCount":2}}`)
	if len(g.got) != 2 || g.got[0].Seed == nil || g.got[1].Seed == nil || *g.got[0].Seed == *g.got[1].Seed {
		t.Errorf("unseeded candidates were not seeded apart: %+v", g.got)
	}
}

func TestCandidateCountIsRefusedWhereVertexRefusesIt(t *testing.T) {
	srv := newTestServer(t, optionsGen())
	for name, c := range map[string]struct{ verb, cfg string }{
		"streaming": {"streamGenerateContent", `{"candidateCount":2}`},
		"zero":      {"generateContent", `{"candidateCount":0}`},
		"nine":      {"generateContent", `{"candidateCount":9}`},
	} {
		resp, body := post(t, srv, vertexPath(testModel, c.verb),
			`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":`+c.cfg+`}`)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "candidateCount") {
			t.Errorf("%s: %d %s", name, resp.StatusCode, body)
		}
	}
	// A runtime that takes no seed cannot sample N candidates.
	echo := newTestServer(t, &EchoGenerator{ID: testModel})
	resp, body := post(t, echo, vertexPath(testModel, "generateContent"),
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"candidateCount":2}}`)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "candidateCount") {
		t.Errorf("echo runtime: %d %s", resp.StatusCode, body)
	}
}

func TestThinkingConfigReachesTheRuntime(t *testing.T) {
	g := optionsGen()
	srv := newTestServer(t, g)
	gen := func(cfg string) GenerateContentResponse {
		t.Helper()
		resp, body := post(t, srv, vertexPath(testModel, "generateContent"),
			`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":`+cfg+`}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %s", cfg, resp.StatusCode, body)
		}
		return decodeResp(t, body)
	}
	r := gen(`{"thinkingConfig":{"includeThoughts":true,"thinkingBudget":128}}`)
	if req := g.last(t); req.Thinking == nil || !*req.Thinking || req.ThinkingBudget != 128 {
		t.Errorf("runtime asked for thinking %v budget %d", req.Thinking, req.ThinkingBudget)
	}
	parts := r.Candidates[0].Content.Parts
	if len(parts) != 2 || parts[0].Thought == nil || !*parts[0].Thought || parts[0].Text != "Let me think." || parts[1].Text != "hello" {
		t.Errorf("parts %+v", parts)
	}
	if u := r.UsageMetadata; u.ThoughtsTokenCount != 3 || u.CandidatesTokenCount != 4 || u.TotalTokenCount != 57 {
		t.Errorf("usage %+v", u)
	}

	// Thoughts are counted but not returned without includeThoughts.
	r = gen(`{"thinkingConfig":{}}`)
	if req := g.last(t); req.Thinking == nil || !*req.Thinking || req.ThinkingBudget != -1 {
		t.Errorf("thinkingConfig {}: thinking %v budget %d", req.Thinking, req.ThinkingBudget)
	}
	if parts := r.Candidates[0].Content.Parts; len(parts) != 1 || parts[0].Thought != nil {
		t.Errorf("thoughts returned without includeThoughts: %+v", parts)
	}
	if r.UsageMetadata.ThoughtsTokenCount != 3 {
		t.Errorf("usage %+v", r.UsageMetadata)
	}

	gen(`{"thinkingConfig":{"thinkingBudget":0}}`)
	if req := g.last(t); req.Thinking == nil || *req.Thinking {
		t.Errorf("thinkingBudget 0 did not turn thinking off: %v", req.Thinking)
	}
	gen(`{}`)
	if req := g.last(t); req.Thinking != nil {
		t.Errorf("no thinkingConfig set thinking %v", *req.Thinking)
	}

	for _, bad := range []string{`{"thinkingConfig":{"thinkingLevel":"HIGH"}}`, `{"thinkingConfig":{"thinkingBudget":-2}}`} {
		resp, body := post(t, srv, vertexPath(testModel, "generateContent"),
			`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":`+bad+`}`)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d %s", bad, resp.StatusCode, body)
		}
	}

	// A model turn's thoughts come back with the history and are not sent on.
	resp, body := post(t, srv, vertexPath(testModel, "generateContent"),
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]},{"role":"model","parts":[{"text":"hm","thought":true},{"text":"hello"}]},{"role":"user","parts":[{"text":"more"}]}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("history with thoughts: %d %s", resp.StatusCode, body)
	}
	b, _ := json.Marshal(g.last(t).Conversation)
	if strings.Contains(string(b), `"hm"`) {
		t.Errorf("a thought was sent back to the model: %s", b)
	}
}

func TestThoughtsStreamAsThoughtParts(t *testing.T) {
	srv := newTestServer(t, optionsGen())
	resp, body := post(t, srv, vertexPath(testModel, "streamGenerateContent")+"?alt=sse",
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"includeThoughts":true}}}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"text":"Let me think.","thought":true`) {
		t.Errorf("%d %s", resp.StatusCode, body)
	}
}

func cacheReq(t *testing.T, srv string, method, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, srv+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestCachedContentLifecycleAndUse(t *testing.T) {
	old := minCacheTokens
	minCacheTokens = 10
	t.Cleanup(func() { minCacheTokens = old })
	g := optionsGen()
	s := NewServer(g)
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	s.cacheStore().now = func() time.Time { return now }
	file := t.TempDir() + "/caches.json"
	s.SetCachedContentState(file)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	const parent = "/v1beta1/projects/p/locations/us-central1/cachedContents"
	model := "projects/p/locations/us-central1/publishers/google/models/" + testModel
	code, body := cacheReq(t, srv.URL, http.MethodPost, parent, `{"model":"`+model+`","displayName":"doc",
		"systemInstruction":{"parts":[{"text":"Answer from the document."}]},
		"contents":[{"role":"user","parts":[{"text":"The document says the sky is green."}]}],"ttl":"600s"}`)
	if code != http.StatusOK {
		t.Fatalf("create: %d %s", code, body)
	}
	var cc struct {
		Name, DisplayName, Model, ExpireTime, CreateTime string
		UsageMetadata                                    struct{ TotalTokenCount, TextCount int }
	}
	if err := json.Unmarshal([]byte(body), &cc); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cc.Name, "projects/p/locations/us-central1/cachedContents/") || cc.DisplayName != "doc" || cc.Model != model ||
		cc.ExpireTime != "2026-11-21T12:10:00Z" || cc.UsageMetadata.TotalTokenCount != 42 || cc.UsageMetadata.TextCount == 0 {
		t.Errorf("created %s", body)
	}

	// Generation with the cache: its system instruction and contents
	// prefix the request, and its tokens are reported.
	code, body = cacheReq(t, srv.URL, http.MethodPost, vertexPath(testModel, "generateContent"),
		`{"cachedContent":"`+cc.Name+`","contents":[{"role":"user","parts":[{"text":"What colour is the sky?"}]}]}`)
	if code != http.StatusOK {
		t.Fatalf("generate: %d %s", code, body)
	}
	conv, _ := json.Marshal(g.last(t).Conversation)
	for _, want := range []string{"Answer from the document.", "the sky is green", "What colour is the sky?"} {
		if !strings.Contains(string(conv), want) {
			t.Errorf("the runtime's conversation lacks %q: %s", want, conv)
		}
	}
	if u := decodeResp(t, body).UsageMetadata; u.CachedContentTokenCount != 42 || u.PromptTokenCount != 50 || len(u.CacheTokensDetails) != 1 {
		t.Errorf("usage %+v", u)
	}
	// A request that sets what the cache holds is refused.
	code, body = cacheReq(t, srv.URL, http.MethodPost, vertexPath(testModel, "generateContent"),
		`{"cachedContent":"`+cc.Name+`","systemInstruction":{"parts":[{"text":"x"}]},"contents":[{"role":"user","parts":[{"text":"q"}]}]}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "system_instruction, tools or tool_config") {
		t.Errorf("system instruction with a cache: %d %s", code, body)
	}

	// Get, list, update ttl and expireTime.
	if code, body = cacheReq(t, srv.URL, http.MethodGet, "/v1beta1/"+cc.Name, ""); code != http.StatusOK || !strings.Contains(body, cc.Name) {
		t.Errorf("get: %d %s", code, body)
	}
	if code, body = cacheReq(t, srv.URL, http.MethodGet, parent+"?pageSize=10", ""); code != http.StatusOK || !strings.Contains(body, cc.Name) {
		t.Errorf("list: %d %s", code, body)
	}
	if code, body = cacheReq(t, srv.URL, http.MethodPatch, "/v1/"+cc.Name, `{"ttl":"7200s"}`); code != http.StatusOK || !strings.Contains(body, "2026-11-21T14:00:00Z") {
		t.Errorf("update ttl: %d %s", code, body)
	}
	if code, body = cacheReq(t, srv.URL, http.MethodPatch, "/v1beta1/"+cc.Name, `{"expireTime":"2026-11-22T00:00:00Z"}`); code != http.StatusOK || !strings.Contains(body, "2026-11-22T00:00:00Z") {
		t.Errorf("update expireTime: %d %s", code, body)
	}
	if code, body = cacheReq(t, srv.URL, http.MethodPatch, "/v1beta1/"+cc.Name, `{"displayName":"other"}`); code != http.StatusBadRequest {
		t.Errorf("update displayName: %d %s", code, body)
	}
	if code, body = cacheReq(t, srv.URL, http.MethodPatch, "/v1beta1/"+cc.Name, `{"ttl":"30s"}`); code != http.StatusBadRequest {
		t.Errorf("ttl under a minute: %d %s", code, body)
	}

	// The state survives a restart.
	s2 := NewServer(g)
	s2.cacheStore().now = func() time.Time { return now }
	s2.SetCachedContentState(file)
	if _, err := s2.getCache(cc.Name); err != nil {
		t.Errorf("after a restart: %v", err)
	}

	// Expired caches are gone.
	now = now.Add(13 * time.Hour)
	if code, body = cacheReq(t, srv.URL, http.MethodGet, "/v1beta1/"+cc.Name, ""); code != http.StatusNotFound {
		t.Errorf("expired get: %d %s", code, body)
	}
	now = now.Add(-13 * time.Hour)
	s.cacheStore().mu.Lock()
	s.cacheStore().caches = map[string]*cacheEntry{}
	s.cacheStore().mu.Unlock()

	// Delete.
	code, body = cacheReq(t, srv.URL, http.MethodPost, parent, `{"model":"`+model+`","contents":[{"role":"user","parts":[{"text":"doc"}]}]}`)
	if code != http.StatusOK {
		t.Fatalf("create 2: %d %s", code, body)
	}
	_ = json.Unmarshal([]byte(body), &cc)
	if cc.ExpireTime != "2026-11-21T13:00:00Z" {
		t.Errorf("default expiration %s, want an hour", cc.ExpireTime)
	}
	if code, body = cacheReq(t, srv.URL, http.MethodDelete, "/v1beta1/"+cc.Name, ""); code != http.StatusOK {
		t.Errorf("delete: %d %s", code, body)
	}
	if code, _ = cacheReq(t, srv.URL, http.MethodGet, "/v1beta1/"+cc.Name, ""); code != http.StatusNotFound {
		t.Errorf("after delete: %d", code)
	}

	// Too small a cache, and another model, are refused.
	minCacheTokens = 100
	if code, body = cacheReq(t, srv.URL, http.MethodPost, parent, `{"model":"`+model+`","contents":[{"role":"user","parts":[{"text":"doc"}]}]}`); code != http.StatusBadRequest || !strings.Contains(body, "minimum token count") {
		t.Errorf("small cache: %d %s", code, body)
	}
	if code, body = cacheReq(t, srv.URL, http.MethodPost, parent, `{"model":"projects/p/locations/us-central1/publishers/google/models/gemini-9","contents":[{"role":"user","parts":[{"text":"doc"}]}]}`); code != http.StatusNotFound {
		t.Errorf("unserved model: %d %s", code, body)
	}
	if code, body = cacheReq(t, srv.URL, http.MethodPost, vertexPath(testModel, "generateContent"),
		`{"cachedContent":"projects/p/locations/us-central1/cachedContents/123","contents":[{"role":"user","parts":[{"text":"q"}]}]}`); code != http.StatusNotFound {
		t.Errorf("unknown cache: %d %s", code, body)
	}
}

// TestResponseLogprobsReportsTheChosenTokens: responseLogprobs and logprobs
// reach the runtime; each step comes back in logprobsResult (chosen and top
// candidates) with avgLogprobs the chosen tokens' mean, unary and streamed,
// each streamed chunk carrying its own tokens' steps; a stop sequence drops
// the steps of the tokens it cuts; logprobs outside 1 to 20, or without
// responseLogprobs, is refused as Vertex refuses it.
func TestResponseLogprobsReportsTheChosenTokens(t *testing.T) {
	step := func(tok string, id int32, lp float32) Chunk {
		return Chunk{Logprobs: []LogprobStep{{
			Chosen: LogprobsCandidate{Token: tok, TokenID: id, LogProbability: lp},
			Top: []LogprobsCandidate{
				{Token: tok, TokenID: id, LogProbability: lp},
				{Token: "x", TokenID: 99, LogProbability: -3},
			},
		}}}
	}
	g := &convGen{chunks: []Chunk{
		{Usage: &Usage{PromptTokens: 5, CandidateTokens: -1}},
		step("Hel", 10, -0.5),
		{Text: "Hel"},
		step("lo", 11, -1.5),
		step(" you", 12, -1),
		{Text: "lo you"},
		{Last: true, FinishReason: FinishReasonStop, Usage: &Usage{PromptTokens: 5, CandidateTokens: 3}},
	}}
	srv := newTestServer(t, g)
	gen := func(cfg string) (*http.Response, string) {
		return post(t, srv, vertexPath(testModel, "generateContent"),
			`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":`+cfg+`}`)
	}
	resp, body := gen(`{"responseLogprobs":true,"logprobs":2}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	c := decodeResp(t, body).Candidates[0]
	if c.LogprobsResult == nil || len(c.LogprobsResult.ChosenCandidates) != 3 || c.AvgLogprobs == nil || *c.AvgLogprobs != -1 ||
		c.LogprobsResult.ChosenCandidates[1] != (LogprobsCandidate{Token: "lo", TokenID: 11, LogProbability: -1.5}) ||
		len(c.LogprobsResult.TopCandidates) != 3 || len(c.LogprobsResult.TopCandidates[2].Candidates) != 2 ||
		c.LogprobsResult.TopCandidates[2].Candidates[1].TokenID != 99 {
		t.Errorf("candidate %s", body)
	}
	g.mu.Lock()
	if !g.got[0].ResponseLogprobs || g.got[0].TopLogprobs != 2 {
		t.Errorf("request %+v: responseLogprobs and logprobs did not reach the runtime", g.got[0])
	}
	g.mu.Unlock()
	_, body = post(t, srv, vertexPath(testModel, "streamGenerateContent")+"?alt=sse",
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"responseLogprobs":true}}`)
	if strings.Count(body, `"chosenCandidates"`) != 2 || !strings.Contains(body, `"avgLogprobs":-1.25`) {
		t.Errorf("stream: %s", body)
	}
	// "lo you" stops at " you": the step of " you" goes with it.
	_, body = gen(`{"responseLogprobs":true,"stopSequences":[" you"]}`)
	if c := decodeResp(t, body).Candidates[0]; c.Content.Parts[0].Text != "Hello" || c.LogprobsResult == nil || len(c.LogprobsResult.ChosenCandidates) != 2 {
		t.Errorf("stop sequence: %s", body)
	}
	for name, cfg := range map[string]string{
		"logprobs 0":          `{"responseLogprobs":true,"logprobs":0}`,
		"logprobs 21":         `{"responseLogprobs":true,"logprobs":21}`,
		"no responseLogprobs": `{"logprobs":3}`,
	} {
		resp, body := gen(cfg)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "logprobs") {
			t.Errorf("%s: status %d: %s", name, resp.StatusCode, body)
		}
	}
}
