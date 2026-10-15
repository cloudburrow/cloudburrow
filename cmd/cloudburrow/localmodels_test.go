package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/embedding"
	"github.com/cloudburrow/cloudburrow/internal/localai"
	"github.com/cloudburrow/cloudburrow/internal/service/vertexai"
)

type closingEmbedder struct{ closed *bool }

func (closingEmbedder) Model() string { return localai.EmbeddingGemmaONNXID }
func (e closingEmbedder) Close() error {
	*e.closed = true
	return nil
}
func (closingEmbedder) Embed(context.Context, []embedding.Input, int, bool) ([]embedding.Output, error) {
	return nil, errors.New("unused")
}

// TestLocalModelsRuntimeUnloadsTheEmbedder (#1306): the Local models page's
// runtime reports the served embedder as loaded, Unload closes it and leaves
// :predict its 501 with the reason, and a generation model is never offered
// Load or Unload.
func TestLocalModelsRuntimeUnloadsTheEmbedder(t *testing.T) {
	srv := vertexai.NewServer(nil)
	closed := false
	srv.SetEmbedder(closingEmbedder{closed: &closed}, nil)
	g := localModelsFor(config.Config{StateDir: t.TempDir()}, srv)

	emb, _ := localai.Lookup(localai.EmbeddingGemmaONNXID)
	if s := g.State(emb); !s.Loaded || s.State != "absent" || !s.Pinned || s.Bytes != emb.Pin.TotalBytes() {
		t.Fatalf("embedding model %+v", s)
	}
	if ok, _ := (embedRuntime{srv: srv}).Loads(emb); ok != localai.ONNXRuntimeCompiled {
		t.Fatalf("Loads = %v in a build where ONNX Runtime compiled = %v", ok, localai.ONNXRuntimeCompiled)
	}
	if err := (embedRuntime{srv: srv}).Unload(emb); err != nil || !closed || srv.EmbedderLoaded() {
		t.Fatalf("unload: %v closed=%v loaded=%v", err, closed, srv.EmbedderLoaded())
	}
	gen, _ := localai.Lookup("gemma-4-e2b-it-community")
	if s := g.State(gen); s.Loadable || s.State != "not-pinned" || !strings.Contains(s.LoadNote, "-local-ai-model") {
		t.Fatalf("generation model %+v", s)
	}
	if s := localModelsFor(config.Config{StateDir: t.TempDir()}, nil).State(emb); s.Loadable || s.Loaded ||
		!strings.Contains(s.LoadNote, "-local-ai-embeddings") {
		t.Fatalf("no endpoint: %+v", s)
	}
}
