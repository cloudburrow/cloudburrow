//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"
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

type consoleLocalModel struct {
	ID         string `json:"id"`
	Pinned     bool   `json:"pinned"`
	Bytes      int64  `json:"bytes"`
	State      string `json:"state"`
	Downloaded int64  `json:"downloaded"`
	Error      string `json:"error"`
	Loadable   bool   `json:"loadable"`
	Loaded     bool   `json:"loaded"`
}

func consoleLocalModelState(t *testing.T, addr, id string) consoleLocalModel {
	t.Helper()
	code, body := consoleDo(t, addr, http.MethodGet, "/api/ai/local-models", "")
	var list struct {
		Models []consoleLocalModel `json:"models"`
	}
	if err := json.Unmarshal([]byte(body), &list); code != http.StatusOK || err != nil {
		t.Fatalf("local models = %d: %s", code, body)
	}
	for _, m := range list.Models {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("%s is not listed: %s", id, body)
	return consoleLocalModel{}
}

// TestConsoleLocalModelsUnloadDeleteDownloadLoad (#1306) needs the real
// model, as the embeddings tests do, and downloads it again (~330 MB): the
// Local models page unloads the served embedding model, which the genai SDK
// then sees refused; deletes its local copy; downloads it from the pinned
// revision once the exact size is confirmed, every file verified; and loads
// it, after which the SDK embeds again. A model with no pin is never
// downloaded, and a loaded one cannot be deleted.
func TestConsoleLocalModelsUnloadDeleteDownloadLoad(t *testing.T) {
	h := New(t)
	cl := genaiClientAt(t, h, EnvEmbeddings)
	addr := consoleAddr(t, h)
	ctx, cancel := context.WithTimeout(h.Context(), 20*time.Minute)
	defer cancel()
	act := func(action, body string) (int, string) {
		return consoleDo(t, addr, http.MethodPost, "/api/ai/local-models/"+embeddingModel+"/"+action, body)
	}

	m := consoleLocalModelState(t, addr, embeddingModel)
	if !m.Pinned || m.State != "downloaded" || !m.Loaded || !m.Loadable {
		t.Fatalf("served model on the page: %+v", m)
	}
	// Whatever happens below, leave the model loaded for the next test.
	t.Cleanup(func() {
		if s := consoleLocalModelState(t, addr, embeddingModel); s.State == "downloaded" && !s.Loaded {
			_, _ = act("load", "{}")
		}
	})
	if code, body := consoleDo(t, addr, http.MethodPost, "/api/ai/local-models/gemma-4-e2b-it-community/download",
		`{"confirmBytes":1}`); code != http.StatusPreconditionFailed {
		t.Errorf("unpinned download = %d %s, want 412", code, body)
	}
	if code, body := act("delete", ""); code != http.StatusConflict {
		t.Errorf("delete while loaded = %d %s, want 409", code, body)
	}

	if code, body := act("unload", ""); code != http.StatusOK {
		t.Fatalf("unload = %d %s", code, body)
	}
	contents := []*genai.Content{genai.NewContentFromText("unloaded", genai.RoleUser)}
	if _, err := cl.Models.EmbedContent(ctx, embeddingModel, contents, nil); err == nil ||
		!strings.Contains(err.Error(), "unloaded") {
		t.Fatalf("EmbedContent after unload: %v, want the unload refusal", err)
	}

	if code, body := act("delete", ""); code != http.StatusOK {
		t.Fatalf("delete = %d %s", code, body)
	}
	if s := consoleLocalModelState(t, addr, embeddingModel); s.State != "absent" {
		t.Fatalf("after delete: %+v", s)
	}
	if code, body := act("download", fmt.Sprintf(`{"confirmBytes":%d}`, m.Bytes-1)); code != http.StatusBadRequest {
		t.Fatalf("download with the wrong size = %d %s, want 400", code, body)
	}
	if code, body := act("download", fmt.Sprintf(`{"confirmBytes":%d}`, m.Bytes)); code != http.StatusOK {
		t.Fatalf("download = %d %s", code, body)
	}
	for {
		s := consoleLocalModelState(t, addr, embeddingModel)
		if s.State == "downloaded" {
			break
		}
		if s.State != "downloading" {
			t.Fatalf("download ended %+v", s)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("download did not finish: %+v", s)
		case <-time.After(2 * time.Second):
		}
	}
	if code, body := act("load", ""); code != http.StatusOK {
		t.Fatalf("load = %d %s", code, body)
	}
	if v := embed(t, ctx, cl, nil, "loaded again")[0]; len(v) != 768 {
		t.Fatalf("after load: %d values, want 768", len(v))
	}
}
