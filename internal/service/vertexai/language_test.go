package vertexai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func languageCall(t *testing.T, gen Generator, verb, body string) (int, map[string]any) {
	t.Helper()
	srv := httptest.NewServer(NewServer(gen).Handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/documents:"+verb, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// seqGen answers each generation with the next of its answers.
type seqGen struct {
	answers []string
	prompts []string
}

func (*seqGen) Model() string        { return "gemma-test" }
func (*seqGen) HonoursOptions() bool { return true }
func (g *seqGen) Generate(_ context.Context, req Request) (<-chan Chunk, func() error, error) {
	g.prompts = append(g.prompts, req.Prompt)
	ch := make(chan Chunk, 2)
	ch <- Chunk{Text: g.answers[0]}
	g.answers = g.answers[1:]
	ch <- Chunk{Last: true, FinishReason: FinishReasonStop}
	close(ch)
	return ch, func() error { return nil }, nil
}

func scripted(answer string, seen *[]Request) *ScriptedGenerator {
	return &ScriptedGenerator{ID: "gemma-test", Chunks: []string{answer}, Options: true, Seen: func(r Request) { *seen = append(*seen, r) }}
}

func doc(text string) string {
	b, _ := json.Marshal(map[string]any{"document": map[string]any{"type": "PLAIN_TEXT", "content": text}})
	return string(b)
}

// TestAnalyzeSentiment: sentences are split and offset here, each is rated
// under a JSON schema, greedily, and the document's sentiment is their mean
// score and summed magnitude.
func TestAnalyzeSentiment(t *testing.T) {
	var seen []Request
	gen := scripted(`{"sentences": [{"score": 0.83, "magnitude": 0.9}, {"score": -0.41, "magnitude": 0.44}]}`, &seen)
	text := "I love this café! The ending was weak."
	code, out := languageCall(t, gen, "analyzeSentiment", `{"document": {"type": "PLAIN_TEXT", "content": "`+text+`"}, "encodingType": "UTF16"}`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	b, _ := json.Marshal(out)
	want := `{"documentSentiment":{"magnitude":1.3,"score":0.2},"language":"en","sentences":[` +
		`{"sentiment":{"magnitude":0.9,"score":0.8},"text":{"beginOffset":0,"content":"I love this café!"}},` +
		`{"sentiment":{"magnitude":0.4,"score":-0.4},"text":{"beginOffset":18,"content":"The ending was weak."}}]}`
	if string(b) != want {
		t.Errorf("got  %s\nwant %s", b, want)
	}
	if len(seen) != 1 || *seen[0].Temperature != 0 || *seen[0].TopK != 1 || !strings.Contains(seen[0].JSONSchema, `"minItems":2`) {
		t.Errorf("request: %+v", seen)
	}
	// With encodingType NONE, beginOffset is -1.
	_, out = languageCall(t, gen, "analyzeSentiment", doc(text))
	if o := out["sentences"].([]any)[1].(map[string]any)["text"].(map[string]any)["beginOffset"]; o != -1.0 {
		t.Errorf("NONE: %v", o)
	}
	// A model that rates the wrong number of sentences is an error, not a guess.
	if code, _ := languageCall(t, scripted(`{"sentences": [{"score": 0.1, "magnitude": 0.1}]}`, &seen), "analyzeSentiment", doc(text)); code != 500 {
		t.Errorf("short answer: %d", code)
	}
}

// TestAnalyzeEntities: only mentions the text contains are kept, with
// their offsets; salience is normalised and sorts the entities.
func TestAnalyzeEntities(t *testing.T) {
	var seen []Request
	gen := scripted(`{"entities": [
		{"name": "Sundar Pichai", "type": "PERSON", "salience": 0.3, "mentions": [{"text": "Sundar Pichai", "type": "PROPER"}, {"text": "CEO", "type": "COMMON"}]},
		{"name": "Google", "type": "ORGANIZATION", "salience": 0.6, "mentions": [{"text": "Google", "type": "PROPER"}]},
		{"name": "Mars", "type": "LOCATION", "salience": 0.1, "mentions": [{"text": "Mars", "type": "PROPER"}]}]}`, &seen)
	code, out := languageCall(t, gen, "analyzeEntities", `{"document": {"type": "PLAIN_TEXT", "content": "Sundar Pichai is the CEO of Google. Google is in California."}, "encodingType": "UTF8"}`)
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	b, _ := json.Marshal(out)
	want := `{"entities":[` +
		`{"mentions":[{"text":{"beginOffset":28,"content":"Google"},"type":"PROPER"},{"text":{"beginOffset":36,"content":"Google"},"type":"PROPER"}],"metadata":{},"name":"Google","salience":0.66666669,"type":"ORGANIZATION"},` +
		`{"mentions":[{"text":{"beginOffset":0,"content":"Sundar Pichai"},"type":"PROPER"},{"text":{"beginOffset":21,"content":"CEO"},"type":"COMMON"}],"metadata":{},"name":"Sundar Pichai","salience":0.33333334,"type":"PERSON"}],` +
		`"language":"en"}`
	if string(b) != want {
		t.Errorf("got  %s\nwant %s", b, want)
	}
	if !strings.Contains(seen[0].JSONSchema, `"WORK_OF_ART"`) {
		t.Errorf("schema: %s", seen[0].JSONSchema)
	}
	// Entity sentiment: the same entities, then each mention rated in a
	// second pass; the entity's sentiment is the mean score and summed
	// magnitude.
	seq := &seqGen{answers: []string{
		`{"entities": [{"name": "food", "type": "OTHER", "salience": 1, "mentions": [{"text": "food", "type": "COMMON"}]}]}`,
		`{"mentions": [{"score": 0.9, "magnitude": 0.9}]}`}}
	_, out = languageCall(t, seq, "analyzeEntitySentiment", doc("The food was great."))
	b, _ = json.Marshal(out["entities"])
	if want := `[{"mentions":[{"sentiment":{"magnitude":0.9,"score":0.9},"text":{"beginOffset":-1,"content":"food"},"type":"COMMON"}],"metadata":{},"name":"food","salience":1,"sentiment":{"magnitude":0.9,"score":0.9},"type":"OTHER"}]`; string(b) != want {
		t.Errorf("entity sentiment: %s", b)
	}
	if len(seq.prompts) != 2 || !strings.Contains(seq.prompts[1], `1. "food" (the entity "food") in: The food was great.`) {
		t.Errorf("prompts: %q", seq.prompts)
	}
	// Entity sentiment serves en, ja and es only.
	if code, out := languageCall(t, gen, "analyzeEntitySentiment", doc("Das Essen war wirklich sehr gut und günstig.")); code != 400 {
		t.Errorf("de: %d %v", code, out)
	}
}

// TestClassifyText: names are constrained to the V1 taxonomy, a parent is
// dropped for its descendant, and the V1 model's twenty-token minimum and
// English-only rule are enforced.
func TestClassifyText(t *testing.T) {
	if len(nlV1Categories) != 620 || nlV1Categories[0] != "/Adult" {
		t.Fatalf("taxonomy: %d %q", len(nlV1Categories), nlV1Categories[0])
	}
	var seen []Request
	gen := scripted(`{"categories": [{"name": "/Arts & Entertainment", "confidence": 0.3}, {"name": "/Arts & Entertainment/TV & Video/TV Shows & Programs", "confidence": 0.52}]}`, &seen)
	text := "That actor on TV makes movies in Hollywood and also stars in a variety of popular new TV shows."
	code, out := languageCall(t, gen, "classifyText", doc(text))
	b, _ := json.Marshal(out)
	if code != 200 || string(b) != `{"categories":[{"confidence":0.51999998,"name":"/Arts \u0026 Entertainment/TV \u0026 Video/TV Shows \u0026 Programs"}]}` {
		t.Errorf("%d %s", code, b)
	}
	if !strings.Contains(seen[0].JSONSchema, "TV Shows \\u0026 Programs") || !strings.Contains(seen[0].JSONSchema, "/Adult") {
		t.Errorf("schema lacks the taxonomy: %.300s", seen[0].JSONSchema)
	}
	if nlTokens(text) != 20 {
		t.Errorf("tokens: %d", nlTokens(text))
	}
	for body, want := range map[string]int{
		doc("Too short to classify."): 400,
		doc("Ce film est vraiment magnifique et les acteurs jouent très bien, avec une histoire touchante du début à la fin du film."): 400,
		`{"document": {"type": "PLAIN_TEXT", "content": "` + text + `"}, "classificationModelOptions": {"v2Model": {}}}`:               501,
	} {
		if code, out := languageCall(t, gen, "classifyText", body); code != want {
			t.Errorf("%s: %d %v", body, code, out)
		}
	}
}

// TestLanguageRefusals: what the local model cannot answer is 501.
func TestLanguageRefusals(t *testing.T) {
	for _, c := range []struct {
		gen  Generator
		verb string
		body string
		want int
	}{
		{nil, "analyzeSentiment", doc("Good."), 501},
		{&ScriptedGenerator{ID: "g"}, "analyzeSentiment", doc("Good."), 501}, // no constrained decoding
		{nil, "analyzeSyntax", doc("Good."), 501},
		{nil, "moderateText", doc("Good."), 501},
		{nil, "analyzeSentiment", `{"document": {"type": "HTML", "content": "<p>Good.</p>"}}`, 501},
		{nil, "analyzeSentiment", `{"document": {"type": "PLAIN_TEXT", "gcsContentUri": "gs://b/o"}}`, 501},
		{nil, "analyzeSentiment", `{"document": {"type": "PLAIN_TEXT", "content": " "}}`, 400},
		{nil, "analyzeSentiment", `{}`, 400},
		{nil, "analyzeSentiment", `{"document": {"type": "PLAIN_TEXT", "content": "x"}, "encodingType": "UTF7"}`, 400},
		{nil, "analyzeEntities", `{"document": {"type": "PLAIN_TEXT", "content": "Mirëdita, si jeni sot?", "language": "sq"}}`, 400},
		{nil, "summarize", doc("x"), 404},
	} {
		if code, out := languageCall(t, c.gen, c.verb, c.body); code != c.want {
			t.Errorf("%s %s: %d %v", c.verb, c.body, code, out)
		}
	}
}

func TestSplitSentences(t *testing.T) {
	got := splitSentences("Hello there. \"Is it?\" she asked.\n\nNo heading\nこんにちは。元気？Mr. Smith arrived")
	var texts []string
	for _, s := range got {
		texts = append(texts, s.text)
	}
	want := []string{"Hello there.", "\"Is it?\"", "she asked.", "No heading\nこんにちは。", "元気？", "Mr.", "Smith arrived"}
	if strings.Join(texts, "|") != strings.Join(want, "|") {
		t.Errorf("%q", texts)
	}
	if got[1].start != 13 {
		t.Errorf("offset: %d", got[1].start)
	}
}
