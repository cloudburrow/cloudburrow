package bigqueryfront

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestUnderstandResult: the local endpoint's answers become
// ml_understand_text_result as BigQuery's tutorial shows it (snake_case,
// float32 digits kept), an error the row's status, and a 501 the query's.
func TestUnderstandResult(t *testing.T) {
	var paths, bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		paths, bodies = append(paths, r.URL.Path), append(bodies, string(b))
		switch {
		case strings.HasSuffix(r.URL.Path, ":analyzeSyntax"):
			w.WriteHeader(501)
			_, _ = io.WriteString(w, `{"error": {"code": 501, "message": "analyzeSyntax: no local tagger", "status": "UNIMPLEMENTED"}}`)
		case strings.Contains(string(b), "short"):
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"error": {"code": 400, "message": "Invalid text content: too few tokens (words) to process.", "status": "INVALID_ARGUMENT"}}`)
		case strings.HasSuffix(r.URL.Path, ":classifyText"):
			_, _ = io.WriteString(w, `{"categories": [{"name": "/Arts & Entertainment/TV & Video/TV Shows & Programs", "confidence": 0.51999998}]}`)
		default:
			_, _ = io.WriteString(w, `{"documentSentiment": {"magnitude": 0.8, "score": -0.7}, "language": "en", "sentences": [{"text": {"content": "Bad.", "beginOffset": -1}, "sentiment": {"magnitude": 0.8, "score": -0.7}}]}`)
		}
	}))
	defer srv.Close()
	f := front{vertexai: srv.URL}
	ctx := context.Background()
	res, status, code := f.understand(ctx, "p", "CLASSIFY_TEXT", "", "a long text")
	if code != 0 || status != "" || string(res) != `{"categories":[{"confidence":0.51999998,"name":"/Arts & Entertainment/TV & Video/TV Shows & Programs"}]}` {
		t.Errorf("CLASSIFY_TEXT: %s %q %d", res, status, code)
	}
	if paths[0] != "/v1/documents:classifyText" || bodies[0] != `{"document":{"content":"a long text","type":"PLAIN_TEXT"}}` {
		t.Errorf("request: %s %s", paths[0], bodies[0])
	}
	res, _, _ = f.understand(ctx, "p", "ANALYZE_SENTIMENT", "UTF8", "Bad.")
	if string(res) != `{"document_sentiment":{"magnitude":0.8,"score":-0.7},"language":"en","sentences":[{"sentiment":{"magnitude":0.8,"score":-0.7},"text":{"begin_offset":-1,"content":"Bad."}}]}` {
		t.Errorf("ANALYZE_SENTIMENT: %s", res)
	}
	if !strings.Contains(bodies[1], `"encodingType":"UTF8"`) || paths[1] != "/v1/documents:analyzeSentiment" {
		t.Errorf("request: %s %s", paths[1], bodies[1])
	}
	if res, status, code = f.understand(ctx, "p", "CLASSIFY_TEXT", "", "short"); res != nil || code != 0 ||
		status != "INVALID_ARGUMENT: Invalid text content: too few tokens (words) to process." {
		t.Errorf("an error: %s %q %d", res, status, code)
	}
	if _, status, code = f.understand(ctx, "p", "ANALYZE_SYNTAX", "", "x"); code != 501 || !strings.Contains(status, "no local tagger") {
		t.Errorf("syntax: %q %d", status, code)
	}
}
