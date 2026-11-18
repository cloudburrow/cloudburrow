package vertexai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func genPost(t *testing.T, srv *Server, body string) (int, GenerateContentResponse, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/v1beta1/projects/p/locations/us-central1/publishers/google/models/"+testModel+":generateContent", strings.NewReader(body))
	srv.Handler().ServeHTTP(rec, req)
	var resp GenerateContentResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, resp, rec.Body.String()
}

func TestOptionsReachAGeneratorThatHonoursThem(t *testing.T) {
	var mu sync.Mutex
	var got Request
	gen := &ScriptedGenerator{ID: testModel, Chunks: []string{"ok"}, Options: true,
		Seen: func(r Request) { mu.Lock(); got = r; mu.Unlock() }}
	code, _, body := genPost(t, NewServer(gen), `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],
		"generationConfig":{"maxOutputTokens":8,"temperature":0,"topP":0.5,"topK":3,"seed":7}}`)
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if got.MaxOutputTokens == nil || *got.MaxOutputTokens != 8 || got.Temperature == nil || *got.Temperature != 0 ||
		got.TopP == nil || *got.TopP != 0.5 || got.TopK == nil || *got.TopK != 3 || got.Seed == nil || *got.Seed != 7 {
		t.Errorf("request reached the generator as %+v", got)
	}
}

func TestOptionsStayRefusedWithoutARunner(t *testing.T) {
	code, _, body := genPost(t, NewServer(&ScriptedGenerator{ID: testModel, Chunks: []string{"ok"}}),
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"seed":7}}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "seed") {
		t.Errorf("status %d: %s", code, body)
	}
}

func TestOptionRanges(t *testing.T) {
	srv := NewServer(&ScriptedGenerator{ID: testModel, Chunks: []string{"ok"}, Options: true})
	for name, cfg := range map[string]string{
		"temperature": `{"temperature":2.5}`, "topP": `{"topP":1.5}`, "topK": `{"topK":0}`,
		"maxOutputTokens": `{"maxOutputTokens":0}`, "stopSequences": `{"stopSequences":["a","b","c","d","e","f"]}`,
	} {
		code, _, body := genPost(t, srv, `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":`+cfg+`}`)
		if code != http.StatusBadRequest || !strings.Contains(body, name) {
			t.Errorf("%s: status %d: %s", name, code, body)
		}
	}
}

func TestMaxTokensFinishReason(t *testing.T) {
	gen := &ScriptedGenerator{ID: testModel, Chunks: []string{"cut"}, Options: true, Finish: FinishReasonMaxTokens}
	_, resp, body := genPost(t, NewServer(gen), `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":1}}`)
	if len(resp.Candidates) != 1 || resp.Candidates[0].FinishReason != FinishReasonMaxTokens {
		t.Errorf("response %s, want finishReason MAX_TOKENS", body)
	}
}

func TestStopSequencesEndTheAnswer(t *testing.T) {
	// Without a runner too: stops are applied on the stream.
	gen := &ScriptedGenerator{ID: testModel, Chunks: []string{"one, tw", "o, three, four"}}
	_, resp, body := genPost(t, NewServer(gen), `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"stopSequences":["three", ", two"]}}`)
	if len(resp.Candidates) != 1 || resp.Candidates[0].Content.Parts[0].Text != "one" || resp.Candidates[0].FinishReason != FinishReasonStop {
		t.Errorf("response %s, want \"one\" and STOP", body)
	}
}

func TestApplyStopsPassesThroughAndSplitsSafely(t *testing.T) {
	in := make(chan Chunk)
	go func() {
		for _, c := range []string{"héllo ", "wor", "ld"} {
			in <- Chunk{Text: c}
		}
		in <- Chunk{Last: true, FinishReason: FinishReasonStop}
		close(in)
	}()
	ctx, cancel := context.WithCancel(context.Background())
	var texts []string
	var last Chunk
	for c := range applyStops(context.Background(), cancel, in, []string{"xyzzy"}) {
		if c.Last {
			last = c
			continue
		}
		texts = append(texts, c.Text)
	}
	if strings.Join(texts, "") != "héllo world" || last.FinishReason != FinishReasonStop {
		t.Errorf("got %q, %+v", texts, last)
	}
	if ctx.Err() == nil {
		t.Error("cancel was not called when the stream ended")
	}
}

func TestRunnerFlags(t *testing.T) {
	m, k, s := int32(8), int32(3), int32(-2)
	tt, p := float32(0.7), float32(0.95)
	got := runnerFlags(Request{MaxOutputTokens: &m, Temperature: &tt, TopP: &p, TopK: &k, Seed: &s})
	want := []string{"--max_output_tokens=8", "--sampler_temperature=0.7", "--sampler_top_p=0.95", "--sampler_top_k=3", "--sampler_seed=-2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("runnerFlags = %q, want %q", got, want)
	}
	if n := decodedTokens("x\ncloudburrow_generate: decoded_tokens=8\n"); n != 8 {
		t.Errorf("decodedTokens = %d", n)
	}
	if n := decodedTokens("nothing"); n != -1 {
		t.Errorf("decodedTokens = %d", n)
	}
}
