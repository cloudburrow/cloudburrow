package vertexai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// echoTranslator answers every prompt with its last line upper-cased, and
// records the prompt.
type echoTranslator struct{ prompts *[]string }

func (echoTranslator) Model() string { return "gemma-test" }
func (e echoTranslator) Generate(_ context.Context, req Request) (<-chan Chunk, func() error, error) {
	*e.prompts = append(*e.prompts, req.Prompt)
	lines := strings.Split(req.Prompt, "\n")
	ch := make(chan Chunk, 2)
	ch <- Chunk{Text: " " + strings.ToUpper(lines[len(lines)-1]) + "\n"}
	ch <- Chunk{Last: true, FinishReason: FinishReasonStop}
	close(ch)
	return ch, func() error { return nil }, nil
}

func translationCall(t *testing.T, gen Generator, verb, body string) (int, map[string]any) {
	t.Helper()
	srv := httptest.NewServer(NewServer(gen).Handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v3/projects/p/locations/us-central1:"+verb, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// TestDetectLanguage: detectLanguage needs no generation model.
func TestDetectLanguage(t *testing.T) {
	code, out := translationCall(t, nil, "detectLanguage", `{"content": "Bonjour tout le monde, comment allez-vous ?", "mimeType": "text/plain"}`)
	langs, _ := out["languages"].([]any)
	if code != 200 || len(langs) != 1 {
		t.Fatalf("%d %v", code, out)
	}
	l := langs[0].(map[string]any)
	if l["languageCode"] != "fr" || l["confidence"].(float64) < 0.9 {
		t.Errorf("%v", l)
	}
	code, out = translationCall(t, nil, "detectLanguage", `{"content": "你好，世界。今天天气很好。"}`)
	if code != 200 || out["languages"].([]any)[0].(map[string]any)["languageCode"] != "zh-CN" {
		t.Errorf("Chinese: %d %v", code, out)
	}
	for body, want := range map[string]int{`{}`: 400, `{"content": "x", "model": "m"}`: 501, `{"content": "x", "mimeType": "text/html"}`: 501,
		`{"content": "x", "mimeType": "image/png"}`: 400, `nope`: 400} {
		if code, out := translationCall(t, nil, "detectLanguage", body); code != want {
			t.Errorf("%s: %d %v", body, code, out)
		}
	}
	if code, _ := translationCall(t, nil, "romanizeText", `{}`); code != 404 {
		t.Errorf("romanizeText: %d", code)
	}
}

// TestTranslateText: the prompt is TranslateGemma's, the detected source is
// answered, and a text already in the target language is not sent.
func TestTranslateText(t *testing.T) {
	var prompts []string
	gen := echoTranslator{&prompts}
	code, out := translationCall(t, gen, "translateText", `{"contents": ["Guten Morgen, wie geht es dir?", "Hello, how are you today?"], "targetLanguageCode": "en"}`)
	tr, _ := out["translations"].([]any)
	if code != 200 || len(tr) != 2 {
		t.Fatalf("%d %v", code, out)
	}
	first, second := tr[0].(map[string]any), tr[1].(map[string]any)
	if first["detectedLanguageCode"] != "de" || first["translatedText"] != "GUTEN MORGEN, WIE GEHT ES DIR?" {
		t.Errorf("%v", first)
	}
	if second["detectedLanguageCode"] != "en" || second["translatedText"] != "Hello, how are you today?" {
		t.Errorf("%v", second)
	}
	if len(prompts) != 1 || !strings.HasPrefix(prompts[0], "You are a professional German (de) to English (en) translator.") {
		t.Errorf("prompts: %q", prompts)
	}
	code, out = translationCall(t, gen, "translateText", `{"contents": ["apple"], "sourceLanguageCode": "en", "targetLanguageCode": "zh-CN"}`)
	if tr := out["translations"].([]any)[0].(map[string]any); code != 200 || tr["detectedLanguageCode"] != nil || tr["translatedText"] != "APPLE" {
		t.Errorf("with a source: %d %v", code, out)
	}
	for body, want := range map[string]int{
		`{"contents": ["x"]}`: 400,
		`{"contents": ["x"], "targetLanguageCode": "xx-nope"}`:                                 400,
		`{"contents": ["x"], "targetLanguageCode": "fr", "sourceLanguageCode": "qq"}`:          400,
		`{"targetLanguageCode": "fr"}`:                                                         400,
		`{"contents": ["x"], "targetLanguageCode": "fr", "mimeType": "text/html"}`:             501,
		`{"contents": ["x"], "targetLanguageCode": "fr", "glossaryConfig": {"glossary": "g"}}`: 501,
		`{"contents": ["x"], "targetLanguageCode": "fr", "model": "general/nmt"}`:              501,
	} {
		if code, out := translationCall(t, gen, "translateText", body); code != want {
			t.Errorf("%s: %d %v", body, code, out)
		}
	}
	// With no local generation model, translateText is UNIMPLEMENTED.
	if code, out := translationCall(t, nil, "translateText", `{"contents": ["Guten Morgen"], "targetLanguageCode": "en"}`); code != 501 {
		t.Errorf("no model: %d %v", code, out)
	}
}

func TestSameLanguage(t *testing.T) {
	for _, p := range [][2]string{{"zh", "zh-CN"}, {"iw", "he"}, {"EN", "en"}, {"tl", "fil"}} {
		if !sameLanguage(p[0], p[1]) {
			t.Errorf("%v", p)
		}
	}
	if sameLanguage("pt-BR", "pt-PT") || sameLanguage("zh-TW", "zh-CN") {
		t.Error("distinct variants")
	}
}
