//go:build browser

package browser

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/vertexai"
)

// TestPlaygroundGenerationOptions (#1797, #1779): the AI Playground's
// Generation options, filled in on the page in headless Chromium, reach the
// generator as the request's options through the real vertexai server, whose
// scripted generator applies them as cloudburrow_generate does; a field left
// empty is absent, not zero.
func TestPlaygroundGenerationOptions(t *testing.T) {
	const model = "gemma-4-e2b-it-community"
	var mu sync.Mutex
	var got []vertexai.Request
	gen := &vertexai.ScriptedGenerator{ID: model, Chunks: []string{"true"}, Options: true,
		Seen: func(r vertexai.Request) { mu.Lock(); got = append(got, r); mu.Unlock() }}
	api := httptest.NewServer(vertexai.NewServer(gen).Handler())
	t.Cleanup(api.Close)
	addr := strings.TrimPrefix(api.URL, "http://")

	s := console.New("", func(context.Context) console.Status { return console.Status{} })
	s.SetPlayground(&console.Playground{Addr: func() string { return addr }, Model: model})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	p := openAt(t, strings.TrimPrefix(srv.URL, "http://"))
	p.navigate("/ai/playground")
	p.waitFor(`document.querySelector("#pg-opt-temperature") !== null && document.querySelector("#pg-prompt") !== null`)
	p.waitFor(`performance.getEntriesByType("resource").some((e) => e.name.includes("/api/resources/projects"))`)
	p.forgive()

	p.eval(`(() => {
		const set = (id, v) => { document.querySelector(id).value = v; };
		set("#pg-opt-temperature", "0");
		set("#pg-opt-seed", "7");
		set("#pg-opt-maxOutputTokens", "12");
		set("#pg-opt-stopSequences", "END, STOP");
		document.querySelector("#pg-opt-json").checked = true;
		set("#pg-opt-responseSchema", '{"type":"BOOLEAN"}');
		document.querySelector("#pg-prompt").value = "is it?";
		[...document.querySelectorAll("#view button")].find((b) => b.textContent === "Run").click();
		return true;
	})()`, nil)
	p.waitFor(`document.querySelector("#pg-output").textContent === "true"`)

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("%d generations, want 1", len(got))
	}
	r := got[0]
	if r.Temperature == nil || *r.Temperature != 0 || r.Seed == nil || *r.Seed != 7 ||
		r.MaxOutputTokens == nil || *r.MaxOutputTokens != 12 || r.JSONSchema != `{"type":"boolean"}` {
		t.Errorf("the page's options reached the generator as %+v", r)
	}
	if r.TopP != nil || r.TopK != nil {
		t.Errorf("empty fields were sent: topP %v, topK %v", r.TopP, r.TopK)
	}
}
