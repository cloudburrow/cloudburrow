package bigqueryfront

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTranslateResult: the local endpoint's answers become
// ml_translate_result as the page's sample shows it (snake_case, the empty
// repeated fields present), an error the row's status, and a 501 the
// query's.
func TestTranslateResult(t *testing.T) {
	var paths, bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		paths, bodies = append(paths, r.URL.Path), append(bodies, string(b))
		switch {
		case strings.Contains(string(b), "xx"):
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"error": {"code": 400, "message": "Target language is invalid.", "status": "INVALID_ARGUMENT"}}`)
		case strings.Contains(string(b), "nomodel"):
			w.WriteHeader(501)
			_, _ = io.WriteString(w, `{"error": {"code": 501, "message": "translateText: this instance has no local generation model", "status": "UNIMPLEMENTED"}}`)
		case strings.HasSuffix(r.URL.Path, ":translateText"):
			_, _ = io.WriteString(w, `{"translations": [{"translatedText": "苹果", "detectedLanguageCode": "en"}]}`)
		default:
			_, _ = io.WriteString(w, `{"languages": [{"languageCode": "en", "confidence": 0.9}]}`)
		}
	}))
	defer srv.Close()
	f := front{vertexai: srv.URL}
	ctx := context.Background()
	res, status, code := f.translate(ctx, "p", "us", "TRANSLATE_TEXT", "zh-CN", "apple")
	if code != 0 || status != "" || string(res) != `{"glossary_translations":[],"translation_memory_translations":[],"translations":[{"detected_language_code":"en","translated_text":"苹果"}]}` {
		t.Errorf("TRANSLATE_TEXT: %s %q %d", res, status, code)
	}
	if paths[0] != "/v3/projects/p/locations/us:translateText" || !strings.Contains(bodies[0], `"targetLanguageCode":"zh-CN"`) {
		t.Errorf("request: %s %s", paths[0], bodies[0])
	}
	res, status, code = f.translate(ctx, "p", "us", "DETECT_LANGUAGE", "", "apple")
	if code != 0 || status != "" || string(res) != `{"languages":[{"confidence":0.9,"language_code":"en"}]}` || paths[1] != "/v3/projects/p/locations/us:detectLanguage" {
		t.Errorf("DETECT_LANGUAGE: %s %q %d %s", res, status, code, paths[1])
	}
	if res, status, code = f.translate(ctx, "p", "us", "TRANSLATE_TEXT", "xx", "apple"); res != nil || status != "INVALID_ARGUMENT: Target language is invalid." || code != 0 {
		t.Errorf("an invalid target: %s %q %d", res, status, code)
	}
	if _, status, code = f.translate(ctx, "p", "us", "TRANSLATE_TEXT", "en", "nomodel"); code != 501 || !strings.Contains(status, "no local generation model") {
		t.Errorf("no model: %q %d", status, code)
	}
}
