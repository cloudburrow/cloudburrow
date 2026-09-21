package vertexai

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testModel = "gemma-4-e2b-it-community"

func newTestServer(t *testing.T, gen Generator) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(NewServer(gen).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func vertexPath(model, verb string) string {
	return fmt.Sprintf("/%s/projects/p/locations/us-central1/publishers/google/models/%s:%s",
		APIVersion, model, verb)
}

func post(t *testing.T, srv *httptest.Server, path, body string) (*http.Response, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(b)
}

const helloBody = `{"contents":[{"role":"user","parts":[{"text":"hello world"}]}]}`

func TestGenerateContentReturnsTheVertexResponseShape(t *testing.T) {
	srv := newTestServer(t, &EchoGenerator{ID: testModel})

	resp, body := post(t, srv, vertexPath(testModel, "generateContent"), helloBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}

	var got GenerateContentResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}
	if len(got.Candidates) != 1 {
		t.Fatalf("candidates = %d, want 1: %s", len(got.Candidates), body)
	}
	c := got.Candidates[0]
	if c.Content.Role != "model" {
		t.Errorf("role = %q, want model", c.Content.Role)
	}
	if want := "HELLO WORLD"; c.Content.Parts[0].Text != want {
		t.Errorf("text = %q, want %q", c.Content.Parts[0].Text, want)
	}
	if c.FinishReason != FinishReasonStop {
		t.Errorf("finishReason = %q, want %q", c.FinishReason, FinishReasonStop)
	}
	// The model that ran must be named, so a substitution could not be silent.
	if got.ModelVersion != testModel {
		t.Errorf("modelVersion = %q, want %q", got.ModelVersion, testModel)
	}
	// Token accounting must be absent rather than invented.
	if strings.Contains(body, "usageMetadata") {
		t.Errorf("the response carries usageMetadata, which nothing measured: %s", body)
	}
}

// The Gemini-API path shape must work too, because the official SDK produces
// it when configured with an API key rather than a project.
func TestGeminiAPIPathIsServed(t *testing.T) {
	srv := newTestServer(t, &EchoGenerator{ID: testModel})
	path := fmt.Sprintf("/%s/models/%s:generateContent", GeminiAPIVersion, testModel)
	resp, body := post(t, srv, path, helloBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
}

func TestStreamGenerateContentEmitsOneEventPerChunk(t *testing.T) {
	gen := &ScriptedGenerator{ID: testModel, Chunks: []string{"one ", "two ", "three"}}
	srv := newTestServer(t, gen)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		srv.URL+vertexPath(testModel, "streamGenerateContent")+"?alt=sse", strings.NewReader(helloBody))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	var texts []string
	var finishes []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var ev GenerateContentResponse
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatalf("event is not a GenerateContentResponse: %v\n%s", err, data)
		}
		if len(ev.Candidates) != 1 {
			t.Fatalf("event has %d candidates, want 1: %s", len(ev.Candidates), data)
		}
		if txt := ev.Candidates[0].Content.Parts[0].Text; txt != "" {
			texts = append(texts, txt)
		}
		if fr := ev.Candidates[0].FinishReason; fr != "" {
			finishes = append(finishes, fr)
		}
	}

	if got, want := strings.Join(texts, ""), "one two three"; got != want {
		t.Errorf("streamed text = %q, want %q", got, want)
	}
	if len(texts) != 3 {
		t.Errorf("got %d text events, want one per chunk (3): %v", len(texts), texts)
	}
	// The finish reason must arrive exactly once, on the last event.
	if len(finishes) != 1 || finishes[0] != FinishReasonStop {
		t.Errorf("finish reasons = %v, want exactly one %q", finishes, FinishReasonStop)
	}
}

// Streaming must actually stream. A handler that buffered everything and wrote
// it at the end would pass the test above, so arrival timing is asserted here.
func TestStreamArrivesBeforeGenerationFinishes(t *testing.T) {
	release := make(chan struct{})
	gen := &ScriptedGenerator{ID: testModel, Chunks: []string{"first", "second"}}

	// A generator whose second chunk waits for the test to release it.
	slow := &blockingGenerator{inner: gen, afterFirst: release}
	srv := newTestServer(t, slow)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		srv.URL+vertexPath(testModel, "streamGenerateContent")+"?alt=sse", strings.NewReader(helloBody))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// The first event must be readable while the generator is still blocked.
	sc := bufio.NewScanner(resp.Body)
	got := make(chan string, 1)
	go func() {
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				got <- data
				return
			}
		}
		close(got)
	}()

	select {
	case data, ok := <-got:
		if !ok {
			t.Fatal("the stream closed before any event arrived")
		}
		if !strings.Contains(data, "first") {
			t.Errorf("first event = %s, want the first chunk", data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event arrived while generation was still in progress: the response is buffered, not streamed")
	}
	close(release)
}

// blockingGenerator holds after the first chunk until released.
type blockingGenerator struct {
	inner      *ScriptedGenerator
	afterFirst <-chan struct{}
}

func (g *blockingGenerator) Model() string { return g.inner.ID }

func (g *blockingGenerator) Generate(ctx context.Context, req Request) (<-chan Chunk, func() error, error) {
	out := make(chan Chunk)
	go func() {
		defer close(out)
		for i, c := range g.inner.Chunks {
			if i == 1 {
				select {
				case <-g.afterFirst:
				case <-ctx.Done():
					return
				}
			}
			select {
			case out <- Chunk{Text: c}:
			case <-ctx.Done():
				return
			}
		}
		select {
		case out <- Chunk{Last: true, FinishReason: FinishReasonStop}:
		case <-ctx.Done():
		}
	}()
	return out, func() error { return nil }, nil
}

func TestCancellationStopsGeneration(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	gen := &ScriptedGenerator{ID: testModel, Chunks: []string{"never"}, Block: block}
	srv := newTestServer(t, gen)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		srv.URL+vertexPath(testModel, "generateContent"), strings.NewReader(helloBody))

	errc := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		errc <- err
	}()

	time.Sleep(150 * time.Millisecond)
	cancel()

	select {
	case err := <-errc:
		if err == nil {
			t.Error("the cancelled request returned a response")
		}
	case <-time.After(5 * time.Second):
		t.Error("the request did not end after cancellation")
	}
}

func TestGenerationFailureIsReportedNotSwallowed(t *testing.T) {
	gen := &ScriptedGenerator{ID: testModel, Err: fmt.Errorf("runtime exited 1")}
	srv := newTestServer(t, gen)

	resp, body := post(t, srv, vertexPath(testModel, "generateContent"), helloBody)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "runtime exited 1") {
		t.Errorf("the underlying failure was lost: %s", body)
	}
}

// A failure after the stream has begun cannot be a status code, so it must
// still reach the client as an event rather than a stream that simply ends.
func TestStreamFailureAfterFirstChunkIsVisible(t *testing.T) {
	gen := &ScriptedGenerator{ID: testModel, Chunks: []string{"partial"}, Err: fmt.Errorf("runtime died mid-generation")}
	srv := newTestServer(t, gen)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		srv.URL+vertexPath(testModel, "streamGenerateContent")+"?alt=sse", strings.NewReader(helloBody))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "runtime died mid-generation") {
		t.Errorf("the stream ended without reporting the failure: %s", b)
	}
}

func TestNoSilentModelSubstitution(t *testing.T) {
	srv := newTestServer(t, &EchoGenerator{ID: testModel})

	resp, body := post(t, srv, vertexPath("gemini-2.0-flash", "generateContent"), helloBody)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a Gemini request returned %d, want 404: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, testModel) {
		t.Errorf("the error does not name what is actually served: %s", body)
	}
}

func TestAliasIsExplicitAndVisible(t *testing.T) {
	s := NewServer(&EchoGenerator{ID: testModel})
	s.Alias("gemini-2.0-flash")
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, body := post(t, srv, vertexPath("gemini-2.0-flash", "generateContent"), helloBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
	var got GenerateContentResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	// The alias resolves, and the response still reports what actually ran.
	if got.ModelVersion != testModel {
		t.Errorf("modelVersion = %q, want the model that ran (%q)", got.ModelVersion, testModel)
	}
}

func TestUnconfiguredRuntimeFailsPrecondition(t *testing.T) {
	srv := newTestServer(t, nil)
	resp, body := post(t, srv, vertexPath(testModel, "generateContent"), helloBody)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (FAILED_PRECONDITION): %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "make litert-lm") {
		t.Errorf("the error does not say how to fix it: %s", body)
	}
}

// Every unsupported option must be refused. Accepting one and generating
// anyway would report a setting that was never applied.
func TestUnsupportedOptionsAreRefused(t *testing.T) {
	srv := newTestServer(t, &EchoGenerator{ID: testModel})

	cases := map[string]string{
		"temperature":       `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"temperature":0.2}}`,
		"temperature zero":  `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"temperature":0}}`,
		"topP":              `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"topP":0.9}}`,
		"topK":              `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"topK":40}}`,
		"seed":              `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"seed":7}}`,
		"stopSequences":     `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"stopSequences":["x"]}}`,
		"maxOutputTokens":   `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":10}}`,
		"responseMimeType":  `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"responseMimeType":"application/json"}}`,
		"tools":             `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tools":[{"functionDeclarations":[]}]}`,
		"safetySettings":    `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"safetySettings":[{"category":"HARM_CATEGORY_HARASSMENT"}]}`,
		"systemInstruction": `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"systemInstruction":{"parts":[{"text":"be brief"}]}}`,
		"cachedContent":     `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"cachedContent":"x"}`,
		"inlineData":        `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"AA=="}}]}]}`,
		"unknown field":     `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"somethingNew":true}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			resp, got := post(t, srv, vertexPath(testModel, "generateContent"), body)
			if resp.StatusCode == http.StatusOK {
				t.Fatalf("%s was accepted and generation proceeded, so a caller would believe it applied: %s", name, got)
			}
			if resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusNotImplemented {
				t.Errorf("status = %d, want 400 or 501: %s", resp.StatusCode, got)
			}
		})
	}
}

func TestMultiTurnIsRefusedRatherThanFlattened(t *testing.T) {
	srv := newTestServer(t, &EchoGenerator{ID: testModel})
	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]},{"role":"model","parts":[{"text":"hello"}]},{"role":"user","parts":[{"text":"more"}]}]}`
	resp, got := post(t, srv, vertexPath(testModel, "generateContent"), body)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("a multi-turn conversation was flattened into one prompt: %s", got)
	}
	if !strings.Contains(got, "chat template") {
		t.Errorf("the reason is not explained: %s", got)
	}
}

func TestCountTokensIsRefusedRatherThanEstimated(t *testing.T) {
	srv := newTestServer(t, &EchoGenerator{ID: testModel})
	resp, body := post(t, srv, vertexPath(testModel, "countTokens"), helloBody)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("countTokens returned a count nothing measured: %s", body)
	}
	if !strings.Contains(body, "tokenizer") {
		t.Errorf("the reason is not explained: %s", body)
	}
}

func TestUnknownPathNamesWhatIsServed(t *testing.T) {
	srv := newTestServer(t, &EchoGenerator{ID: testModel})
	resp, body := post(t, srv, "/v1/models/x:generateContent", helloBody)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, APIVersion) {
		t.Errorf("the 404 does not say which version is served: %s", body)
	}
}

func TestOversizedRequestIsRejected(t *testing.T) {
	srv := newTestServer(t, &EchoGenerator{ID: testModel})
	huge := strings.Repeat("a", maxRequestBytes+1024)
	body := fmt.Sprintf(`{"contents":[{"role":"user","parts":[{"text":%q}]}]}`, huge)
	resp, got := post(t, srv, vertexPath(testModel, "generateContent"), body)
	if resp.StatusCode == http.StatusOK {
		t.Errorf("an oversized body was accepted: %d", len(body))
	}
	_ = got
}
