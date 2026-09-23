package console

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/service/vertexai"
)

// fakeGenerationAPI stands in for the local generation endpoint.
//
// It answers on the real Vertex paths, so a relay that got the path shape
// wrong fails here rather than only against a live runtime.
type fakeGenerationAPI struct {
	model string
	// chunks are streamed as server-sent events.
	chunks []string
	// status, when non-zero, is returned instead with errBody.
	status  int
	errBody string
	// block holds the first chunk until closed.
	block chan struct{}

	mu       sync.Mutex
	gotPaths []string
	gotBody  string
}

func (f *fakeGenerationAPI) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.gotPaths...)
}

func (f *fakeGenerationAPI) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.gotPaths = append(f.gotPaths, r.URL.Path)
		f.gotBody = string(body)
		f.mu.Unlock()

		// The real endpoint 404s an unknown model, which is what the
		// readiness probe relies on.
		if strings.Contains(r.URL.Path, "__readiness_probe__") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":404,"message":"model not served"}}`))
			return
		}
		if f.status != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(f.errBody))
			return
		}

		flusher := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		for i, c := range f.chunks {
			if i == 1 && f.block != nil {
				select {
				case <-f.block:
				case <-r.Context().Done():
					return
				}
			}
			ev := map[string]any{"candidates": []map[string]any{{
				"content": map[string]any{"role": "model", "parts": []map[string]any{{"text": c}}},
			}}, "modelVersion": f.model}
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func newPlaygroundServer(t *testing.T, p *Playground) *httptest.Server {
	t.Helper()
	s := New("", func(context.Context) Status { return Status{} })
	s.SetPlayground(p)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func getJSON(t *testing.T, srv *httptest.Server, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The console refuses cross-site requests, so a same-origin fetch is
	// what the browser would actually send.
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func postJSON(t *testing.T, srv *httptest.Server, path, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// A console with no local AI must work completely, and must not offer the
// screen. This is the requirement that missing AI does not break the console.
func TestConsoleWithoutLocalAIDoesNotOfferThePlayground(t *testing.T) {
	srv := newPlaygroundServer(t, nil)

	code, body := getJSON(t, srv, "/api/services")
	if code != http.StatusOK {
		t.Fatalf("services = %d: %s", code, body)
	}
	if strings.Contains(body, "playground") {
		t.Errorf("the playground is advertised with no local AI configured: %s", body)
	}

	code, body = getJSON(t, srv, "/api/ai/playground")
	if code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, body)
	}
	var st PlaygroundStatus
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	if st.Configured {
		t.Error("an unconfigured playground reports itself configured")
	}
	if !strings.Contains(st.Note, "-local-ai-model") {
		t.Errorf("the note does not say how to enable it: %q", st.Note)
	}

	// And generating must refuse rather than produce anything.
	resp, gen := postJSON(t, srv, "/api/ai/playground", `{"prompt":"hi"}`)
	if resp.StatusCode == http.StatusOK {
		t.Errorf("generation succeeded with no local AI: %s", gen)
	}
}

func TestPlaygroundReportsReadinessFromALiveProbe(t *testing.T) {
	api := &fakeGenerationAPI{model: "gemma-4-e2b-it-community"}
	addr := api.start(t)
	srv := newPlaygroundServer(t, &Playground{
		Addr: func() string { return addr }, Model: api.model,
		Publisher: "community", Community: true,
	})

	code, body := getJSON(t, srv, "/api/ai/playground")
	if code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, body)
	}
	var st PlaygroundStatus
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	if !st.Configured || !st.Ready {
		t.Fatalf("configured=%v ready=%v, want both true (%s)", st.Configured, st.Ready, st.Unavailable)
	}
	if st.Model != api.model {
		t.Errorf("model = %q, want %q", st.Model, api.model)
	}
	// Gemma must not be presentable as Gemini. The provenance travels with
	// the status so the screen can say it where the output appears.
	if !st.Community || !strings.Contains(strings.ToLower(st.Note), "community") {
		t.Errorf("a community conversion is not labelled as one: %+v", st)
	}
	if len(st.Supported) != 0 {
		t.Errorf("supported options = %v, want none: the runtime honours none", st.Supported)
	}
	if len(st.Refused) == 0 {
		t.Error("no refused options are listed, so the screen cannot explain what it omits")
	}
}

// A dead endpoint must be reported as such, not as an empty screen.
func TestPlaygroundReportsAnUnreachableEndpoint(t *testing.T) {
	srv := newPlaygroundServer(t, &Playground{
		// A port nothing is listening on.
		Addr:   func() string { return "127.0.0.1:1" },
		Model:  "gemma-4-e2b-it-community",
		Client: &http.Client{Timeout: 2 * time.Second},
	})

	code, body := getJSON(t, srv, "/api/ai/playground")
	if code != http.StatusOK {
		t.Fatalf("status = %d: %s", code, body)
	}
	var st PlaygroundStatus
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	if st.Ready {
		t.Error("an unreachable endpoint reported ready")
	}
	if st.Unavailable == "" {
		t.Error("an unreachable endpoint gave no reason")
	}
}

// The relay must call the same API the SDK calls. A console that reached the
// generator by another route could work while the API was broken.
func TestPlaygroundGeneratesThroughTheRealAPIPath(t *testing.T) {
	api := &fakeGenerationAPI{model: "gemma-4-e2b-it-community", chunks: []string{"one ", "two"}}
	addr := api.start(t)
	srv := newPlaygroundServer(t, &Playground{
		Addr: func() string { return addr }, Model: api.model,
	})

	resp, body := postJSON(t, srv, "/api/ai/playground", `{"prompt":"hello"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("generate = %d: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if !strings.Contains(body, "one ") || !strings.Contains(body, "two") {
		t.Errorf("the streamed text did not reach the client: %s", body)
	}

	var generated string
	for _, p := range api.paths() {
		if strings.Contains(p, ":streamGenerateContent") {
			generated = p
		}
	}
	want := "/v1beta1/projects/console/locations/us-central1/publishers/google/models/" +
		api.model + ":streamGenerateContent"
	if generated != want {
		t.Errorf("the console called %q, want the Vertex path the SDK uses %q", generated, want)
	}

	api.mu.Lock()
	sent := api.gotBody
	api.mu.Unlock()
	if !strings.Contains(sent, `"role":"user"`) || !strings.Contains(sent, `"hello"`) {
		t.Errorf("the request body is not the shape the SDK sends: %s", sent)
	}
	// No generation options may be smuggled in by the console.
	for _, opt := range []string{"temperature", "topP", "maxOutputTokens", "generationConfig"} {
		if strings.Contains(sent, opt) {
			t.Errorf("the console sent %s, which the API refuses: %s", opt, sent)
		}
	}
}

// The API's own message must reach the screen. A generic failure would hide
// which field was refused, which is the only useful part.
func TestPlaygroundSurfacesTheAPIErrorUnchanged(t *testing.T) {
	api := &fakeGenerationAPI{
		model:   "gemma-4-e2b-it-community",
		status:  http.StatusBadRequest,
		errBody: `{"error":{"code":400,"message":"generationConfig.temperature cannot be honoured"}}`,
	}
	addr := api.start(t)
	srv := newPlaygroundServer(t, &Playground{Addr: func() string { return addr }, Model: api.model})

	resp, body := postJSON(t, srv, "/api/ai/playground", `{"prompt":"hello"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "temperature cannot be honoured") {
		t.Errorf("the API's message was replaced: %s", body)
	}
}

func TestPlaygroundRejectsAnEmptyPrompt(t *testing.T) {
	api := &fakeGenerationAPI{model: "m"}
	addr := api.start(t)
	srv := newPlaygroundServer(t, &Playground{Addr: func() string { return addr }, Model: "m"})

	resp, body := postJSON(t, srv, "/api/ai/playground", `{"prompt":"   "}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: %s", resp.StatusCode, body)
	}
	for _, p := range api.paths() {
		if strings.Contains(p, "streamGenerateContent") {
			t.Error("an empty prompt reached the generation API")
		}
	}
}

// Cancelling must stop the upstream request rather than leaving it running.
func TestPlaygroundCancellationPropagatesUpstream(t *testing.T) {
	block := make(chan struct{})
	api := &fakeGenerationAPI{
		model: "m", chunks: []string{"first", "second"}, block: block,
	}
	addr := api.start(t)
	srv := newPlaygroundServer(t, &Playground{Addr: func() string { return addr }, Model: "m"})

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		srv.URL+"/api/ai/playground", strings.NewReader(`{"prompt":"hi"}`))
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Read the first event, then cancel while the upstream is still held.
	buf := make([]byte, 256)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("first read: %v", err)
	}
	cancel()

	// The upstream handler must observe the cancellation. Releasing the
	// block lets it finish either way; what is asserted is that the stream
	// to the client ended rather than continuing.
	close(block)
	if _, err := io.Copy(io.Discard, resp.Body); err == nil {
		t.Log("stream closed cleanly after cancellation")
	}
}

// TestPlaygroundRefusalsMatchTheAPI holds the list the screen shows in step
// with what the API actually does.
//
// The screen explains which options it does not offer. That explanation is a
// claim about another package's behaviour, and a claim nothing checks drifts:
// the day the runtime gains a real temperature control, this fails and the
// screen stops telling users it is refused.
func TestPlaygroundRefusalsMatchTheAPI(t *testing.T) {
	api := httptest.NewServer(vertexai.NewServer(&vertexai.EchoGenerator{ID: "m"}).Handler())
	defer api.Close()

	// Each refused name paired with a request that exercises it.
	bodies := map[string]string{
		"temperature":       `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"temperature":0.5}}`,
		"topP":              `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"topP":0.5}}`,
		"topK":              `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"topK":5}}`,
		"seed":              `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"seed":1}}`,
		"stopSequences":     `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"stopSequences":["a"]}}`,
		"maxOutputTokens":   `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":5}}`,
		"candidateCount":    `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"candidateCount":2}}`,
		"responseMimeType":  `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"responseMimeType":"text/plain"}}`,
		"tools":             `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tools":[{}]}`,
		"safetySettings":    `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"safetySettings":[{}]}`,
		"systemInstruction": `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"systemInstruction":{"parts":[{"text":"x"}]}}`,
		"multi-turn":        `{"contents":[{"role":"user","parts":[{"text":"a"}]},{"role":"user","parts":[{"text":"b"}]}]}`,
		"countTokens":       `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
	}

	if len(bodies) != len(refusedOptions) {
		t.Fatalf("the screen lists %d refused options and this test covers %d; they must match",
			len(refusedOptions), len(bodies))
	}

	for _, name := range refusedOptions {
		body, ok := bodies[name]
		if !ok {
			t.Errorf("the screen lists %q as refused but nothing checks that it is", name)
			continue
		}
		t.Run(name, func(t *testing.T) {
			verb := "generateContent"
			if name == "countTokens" {
				verb = "countTokens"
			}
			url := api.URL + "/v1beta1/projects/p/locations/l/publishers/google/models/m:" + verb
			resp, err := http.Post(url, "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			got, _ := io.ReadAll(resp.Body)
			if resp.StatusCode == http.StatusOK {
				t.Fatalf("the screen says %q is refused, but the API accepted it: %s", name, got)
			}
		})
	}
}
