//go:build localai

package localai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"
)

// generate is one GenerateContent through the official SDK: the text and
// the finish reason.
func generate(t *testing.T, cl *genai.Client, model, prompt string, cfg *genai.GenerateContentConfig) (string, genai.FinishReason) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	resp, err := cl.Models.GenerateContent(ctx, model, genai.Text(prompt), cfg)
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	if len(resp.Candidates) != 1 || resp.Candidates[0].Content == nil {
		t.Fatalf("no candidate: %+v", resp)
	}
	var b strings.Builder
	for _, p := range resp.Candidates[0].Content.Parts {
		b.WriteString(p.Text)
	}
	return b.String(), resp.Candidates[0].FinishReason
}

const essay = "Write a 300-word essay about the sea."

// TestRealMaxOutputTokensCapsTheAnswer: LiteRT-LM a789b7d0 and later apply
// the limit (#1797). Before it the same request wrote 746 words.
func TestRealMaxOutputTokensCapsTheAnswer(t *testing.T) {
	url, model := endpoint(t)
	cl := client(t, url)
	text, finish := generate(t, cl, model, essay, &genai.GenerateContentConfig{MaxOutputTokens: 8})
	t.Logf("8 tokens (recorded): %q, %s", text, finish)
	if n := len(strings.Fields(text)); n == 0 || n > 10 {
		t.Errorf("maxOutputTokens 8 gave %d words: %q", n, text)
	}
	if finish != genai.FinishReasonMaxTokens {
		t.Errorf("finishReason = %q, want MAX_TOKENS", finish)
	}
	long, _ := generate(t, cl, model, essay, &genai.GenerateContentConfig{MaxOutputTokens: 64})
	if a, b := len(strings.Fields(text)), len(strings.Fields(long)); b <= a || b > 64 {
		t.Errorf("maxOutputTokens 64 gave %d words against %d at 8: %q", b, a, long)
	}
}

// TestRealSeedIsRepeatable: a fixed seed at temperature 1 decodes the same
// answer twice; the sampler is seeded from it, not from the clock.
func TestRealSeedIsRepeatable(t *testing.T) {
	url, model := endpoint(t)
	cl := client(t, url)
	const prompt = "Invent a name for a new colour. Answer with the name only."
	cfg := func(seed int32) *genai.GenerateContentConfig {
		return &genai.GenerateContentConfig{Temperature: genai.Ptr[float32](1), TopK: genai.Ptr[float32](40),
			TopP: genai.Ptr[float32](1), Seed: genai.Ptr(seed), MaxOutputTokens: 24}
	}
	a, _ := generate(t, cl, model, prompt, cfg(7))
	b, _ := generate(t, cl, model, prompt, cfg(7))
	if a != b {
		t.Errorf("seed 7 gave %q, then %q", a, b)
	}
	seen := map[string]bool{a: true}
	for _, s := range []int32{1, 2, 3, 4} {
		o, _ := generate(t, cl, model, prompt, cfg(s))
		seen[o] = true
	}
	t.Logf("five seeds, %d distinct answers (recorded): %q", len(seen), keys(seen))
	if len(seen) < 2 {
		t.Errorf("five seeds at temperature 1 gave one answer: the sampler ignores the seed or the temperature")
	}
}

// TestRealTemperatureZeroIsDeterministic: temperature 0 is greedy, so two
// requests with different seeds agree.
func TestRealTemperatureZeroIsDeterministic(t *testing.T) {
	url, model := endpoint(t)
	cl := client(t, url)
	const prompt = "Invent a name for a new colour. Answer with the name only."
	a, _ := generate(t, cl, model, prompt, &genai.GenerateContentConfig{Temperature: genai.Ptr[float32](0), Seed: genai.Ptr[int32](1), MaxOutputTokens: 24})
	b, _ := generate(t, cl, model, prompt, &genai.GenerateContentConfig{Temperature: genai.Ptr[float32](0), Seed: genai.Ptr[int32](99), MaxOutputTokens: 24})
	if a != b {
		t.Errorf("temperature 0 gave %q, then %q", a, b)
	}
	t.Logf("temperature 0 (recorded): %q", a)
}

// TestRealStopSequenceEndsTheAnswer: the answer ends before the sequence.
func TestRealStopSequenceEndsTheAnswer(t *testing.T) {
	url, model := endpoint(t)
	cl := client(t, url)
	text, finish := generate(t, cl, model, "Count from 1 to 10, separated by commas. Answer with the numbers only.",
		&genai.GenerateContentConfig{Temperature: genai.Ptr[float32](0), StopSequences: []string{"5"}})
	t.Logf("stop at 5 (recorded): %q", text)
	if strings.Contains(text, "5") || !strings.Contains(text, "4") || finish != genai.FinishReasonStop {
		t.Errorf("stop sequence 5: %q, %s", text, finish)
	}
}

// TestRealResponseSchemaConstrainsTheAnswer: responseSchema is applied as a
// constraint with the chat template (#1779), so the answer is the JSON asked
// for and nothing else.
func TestRealResponseSchemaConstrainsTheAnswer(t *testing.T) {
	url, model := endpoint(t)
	cl := client(t, url)
	text, _ := generate(t, cl, model, "Is Paris the capital of France?", &genai.GenerateContentConfig{
		ResponseMIMEType: "application/json", ResponseSchema: &genai.Schema{Type: genai.TypeBoolean}})
	if s := strings.TrimSpace(text); s != "true" && s != "false" {
		t.Errorf("BOOLEAN schema answer %q", text)
	}
	text, _ = generate(t, cl, model, "Give the capital of France and its population in millions.", &genai.GenerateContentConfig{
		ResponseMIMEType: "application/json", ResponseSchema: &genai.Schema{Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{"city": {Type: genai.TypeString}, "millions": {Type: genai.TypeNumber}},
			Required:   []string{"city", "millions"}}})
	var v struct {
		City     *string  `json:"city"`
		Millions *float64 `json:"millions"`
	}
	if err := json.Unmarshal([]byte(text), &v); err != nil || v.City == nil || v.Millions == nil {
		t.Errorf("OBJECT schema answer %q: %v", text, err)
	}
	t.Logf("object (recorded): %s", text)
	text, _ = generate(t, cl, model, "Pick a number between 1 and 9.", &genai.GenerateContentConfig{
		ResponseMIMEType: "application/json", ResponseSchema: &genai.Schema{Type: genai.TypeInteger}})
	var n int64
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &n); err != nil {
		t.Errorf("INTEGER schema answer %q: %v", text, err)
	}
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
