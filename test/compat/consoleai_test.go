//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestConsoleEmbeddingsPlaygroundMatchesTheOfficialSDK (#1305) needs the real
// model: an instance started from a -tags onnx build with
// -local-ai-embeddings. The console's Embeddings playground relays to the same
// :predict the genai SDK sends, so its vector for a text is the SDK's, and a
// Vertex model that is not served answers the API's own 404, naming the one
// that is, rather than a substitute.
func TestConsoleEmbeddingsPlaygroundMatchesTheOfficialSDK(t *testing.T) {
	h := New(t)
	cl := genaiClientAt(t, h, EnvEmbeddings)
	addr := consoleAddr(t, h)
	ctx, cancel := context.WithTimeout(h.Context(), 5*time.Minute)
	defer cancel()

	const text = "The quick brown fox jumps over the lazy dog."
	sdk := embed(t, ctx, cl, nil, text)[0]

	code, body := consoleDo(t, addr, http.MethodPost, "/api/ai/embeddings?method=predict&model="+embeddingModel,
		`{"instances":[{"content":"`+text+`"}],"parameters":{"outputDimensionality":768}}`)
	if code != http.StatusOK {
		t.Fatalf("console :predict = %d: %s", code, body)
	}
	var got struct {
		Predictions []struct {
			Embeddings struct{ Values []float32 } `json:"embeddings"`
		} `json:"predictions"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil || len(got.Predictions) != 1 {
		t.Fatalf("console :predict: %v: %s", err, body)
	}
	v := got.Predictions[0].Embeddings.Values
	if len(v) != len(sdk) || len(v) != 768 {
		t.Fatalf("console vector has %d values, the SDK's %d, want 768", len(v), len(sdk))
	}
	if c := cosine(v, sdk); c < 0.9999 {
		t.Errorf("console and SDK vectors differ: cosine %.6f", c)
	}

	for _, m := range []string{"text-embedding-005", "textembedding-gecko@003"} {
		code, body := consoleDo(t, addr, http.MethodPost, "/api/ai/embeddings?model="+m,
			`{"instances":[{"content":"x"}]}`)
		if code != http.StatusNotFound || !strings.Contains(body, embeddingModel) {
			t.Errorf("%s through the console = %d %s, want the API's 404 naming %s", m, code, body, embeddingModel)
		}
	}
}
