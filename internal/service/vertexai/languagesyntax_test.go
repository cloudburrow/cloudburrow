package vertexai

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/syntaxnet"
)

func TestLemma(t *testing.T) {
	for _, c := range []struct{ w, tag, want string }{
		{"Google", "NNP", "Google"}, {"phones", "NNS", "phone"}, {"stories", "NNS", "story"}, {"boxes", "NNS", "box"},
		{"unveiled", "VBD", "unveil"}, {"stopped", "VBD", "stop"}, {"baked", "VBN", "bake"}, {"running", "VBG", "run"},
		{"making", "VBG", "make"}, {"was", "VBD", "be"}, {"'s", "VBZ", "be"}, {"'s", "POS", "'s"}, {"n't", "RB", "not"},
		{"said", "VBD", "say"}, {"loves", "VBZ", "love"}, {"children", "NNS", "child"}, {"The", "DT", "The"}, {".", ".", "."},
	} {
		if got := lemma(c.w, c.tag); got != c.want {
			t.Errorf("lemma(%q, %s) = %q, want %q", c.w, c.tag, got, c.want)
		}
	}
}

func TestPartOfSpeech(t *testing.T) {
	p := partOfSpeech(syntaxnet.Token{Tag: "NNP", Category: "NOUN"}, "Google")
	if p["tag"] != "NOUN" || p["number"] != "SINGULAR" || p["proper"] != "PROPER" || p["tense"] != "TENSE_UNKNOWN" {
		t.Errorf("NNP: %v", p)
	}
	p = partOfSpeech(syntaxnet.Token{Tag: "PRP", Category: "PRON"}, "They")
	if p["person"] != "THIRD" || p["number"] != "PLURAL" || p["case"] != "NOMINATIVE" {
		t.Errorf("PRP: %v", p)
	}
	if p := partOfSpeech(syntaxnet.Token{Tag: ".", Category: "."}, "."); p["tag"] != "PUNCT" {
		t.Errorf("punct: %v", p)
	}
}

// TestAnalyzeSyntax parses analyzing-syntax's sample text with the model
// in PARSEY_DIR and checks the page's sample tokens.
func TestAnalyzeSyntax(t *testing.T) {
	dir := os.Getenv("PARSEY_DIR")
	if dir == "" {
		t.Skip("PARSEY_DIR (a directory of Parsey McParseface's files) is not set")
	}
	syntaxnet.SetSource(func(context.Context) (string, error) { return dir, nil })
	t.Cleanup(func() { syntaxnet.SetSource(nil) })
	text := "Google, headquartered in Mountain View, unveiled the new Android phone at the Consumer Electronic Show.  " +
		"Sundar Pichai said in his keynote that users love their new Android phones."
	b, _ := json.Marshal(map[string]any{"document": map[string]any{"type": "PLAIN_TEXT", "content": text}, "encodingType": "UTF8"})
	code, out := languageCall(t, nil, "analyzeSyntax", string(b))
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	toks, _ := out["tokens"].([]any)
	ss, _ := out["sentences"].([]any)
	if len(toks) != 32 || len(ss) != 2 || out["language"] != "en" {
		t.Fatalf("%d tokens, %d sentences, %v", len(toks), len(ss), out["language"])
	}
	j := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	if got := j(toks[0]); got != `{"dependencyEdge":{"headTokenIndex":7,"label":"NSUBJ"},"lemma":"Google","partOfSpeech":{"aspect":"ASPECT_UNKNOWN",`+
		`"case":"CASE_UNKNOWN","form":"FORM_UNKNOWN","gender":"GENDER_UNKNOWN","mood":"MOOD_UNKNOWN","number":"SINGULAR","person":"PERSON_UNKNOWN",`+
		`"proper":"PROPER","reciprocity":"RECIPROCITY_UNKNOWN","tag":"NOUN","tense":"TENSE_UNKNOWN","voice":"VOICE_UNKNOWN"},"text":{"beginOffset":0,"content":"Google"}}` {
		t.Errorf("token 0: %s", got)
	}
	last := toks[len(toks)-1].(map[string]any)
	if got := j(last["dependencyEdge"]) + j(last["text"]) + j(last["lemma"]); got != `{"headTokenIndex":20,"label":"P"}{"beginOffset":179,"content":"."}"."` {
		t.Errorf("last token: %s", got)
	}
	if got := j(ss[1]); got != `{"text":{"beginOffset":105,"content":"Sundar Pichai said in his keynote that users love their new Android phones."}}` {
		t.Errorf("sentence 2: %s", got)
	}
	var edges []string
	for _, tk := range toks {
		m := tk.(map[string]any)
		edges = append(edges, m["text"].(map[string]any)["content"].(string)+":"+m["dependencyEdge"].(map[string]any)["label"].(string))
	}
	t.Log(strings.Join(edges, " "))
	b, _ = json.Marshal(map[string]any{"document": map[string]any{"type": "PLAIN_TEXT", "content": "Bonjour à tous.", "language": "fr"}})
	if code, out := languageCall(t, nil, "analyzeSyntax", string(b)); code != 501 {
		t.Errorf("French: %d %v", code, out)
	}
}
