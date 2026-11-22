//go:build browser

package browser

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/service/vertexai"
)

// gen2121 is a runtime with a tokenizer that answers with thoughts when
// thinking is on (#2121): Number of responses, Thinking and Context caching
// are tested against the real vertexai server without a model.
type gen2121 struct {
	*vertexai.CountingGenerator
	mu  sync.Mutex
	got []vertexai.Request
}

func (g *gen2121) Generate(ctx context.Context, req vertexai.Request) (<-chan vertexai.Chunk, func() error, error) {
	g.mu.Lock()
	g.got = append(g.got, req)
	g.mu.Unlock()
	if req.Thinking == nil || !*req.Thinking {
		return g.CountingGenerator.Generate(ctx, req)
	}
	ch := make(chan vertexai.Chunk, 3)
	ch <- vertexai.Chunk{Thought: "Weighing it."}
	ch <- vertexai.Chunk{Text: "Blue."}
	ch <- vertexai.Chunk{Last: true, FinishReason: vertexai.FinishReasonStop,
		Usage: &vertexai.Usage{PromptTokens: 5, CandidateTokens: 4, ThoughtTokens: 2}}
	close(ch)
	return ch, func() error { return nil }, nil
}

func (g *gen2121) requests() []vertexai.Request {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]vertexai.Request(nil), g.got...)
}

func newGen2121(model string) *gen2121 {
	return &gen2121{CountingGenerator: &vertexai.CountingGenerator{ScriptedGenerator: &vertexai.ScriptedGenerator{
		ID: model, Chunks: []string{"Hi", "!"}, Options: true, Usage: &vertexai.Usage{PromptTokens: 9, CandidateTokens: 2}}}}
}

// TestAgentStudioResponsesThinkingAndContextCaching (#2121): Agent Studio's
// Model settings in headless Chromium. Number of responses 3 sends
// candidateCount 3 (unary, as Vertex serves it) and shows three responses,
// each its own sample; Thinking's Manual budget and Include thoughts send
// thinkingConfig, and the thoughts are shown apart from the answer with their
// token count; Context caching creates a cache from the prompt, lists it with
// its tokens and expiry, updates its TTL, sends it as cachedContent (the
// usage shows the cached tokens) and deletes it.
func TestAgentStudioResponsesThinkingAndContextCaching(t *testing.T) {
	const model = "gemma-4-e2b-it-community"
	gen := newGen2121(model)
	p := openAt(t, studioConsole(t, gen, model))
	p.navigate("/agent-platform/studio")
	p.waitFor(`document.querySelector("#pg-prompt") !== null && document.querySelector("#pg-opt-candidateCount") !== null && document.querySelector("#pg-cache-rows tr") !== null`)
	p.forgive()
	set := func(id, v string) {
		p.eval(`((id, v) => { const e = document.querySelector(id); e.value = v; e.dispatchEvent(new Event("input", { bubbles: true }));
			e.dispatchEvent(new Event("change", { bubbles: true })); return true; })(`+jsq(id)+`, `+jsq(v)+`)`, nil)
	}
	click := func(id string) { p.eval(`document.querySelector(`+jsq(id)+`).click(), true`, nil) }
	text := func(id string) string {
		var s string
		p.eval(`(document.querySelector(`+jsq(id)+`) || {}).textContent || ""`, &s)
		return s
	}

	// Number of responses.
	set("#pg-opt-candidateCount", "3")
	set("#pg-prompt", "name a colour")
	click("#pg-submit")
	p.waitFor(`document.querySelectorAll("#pg-candidates .pg-candidate").length === 3`)
	for i, h := range []string{"Response 1", "Response 2", "Response 3"} {
		if got := text("#pg-candidate-h-" + string(rune('0'+i))); got != h {
			t.Errorf("candidate %d heading %q", i, got)
		}
		if got := text("#pg-candidate-" + string(rune('0'+i))); got != "Hi!" {
			t.Errorf("candidate %d text %q", i, got)
		}
	}
	reqs := gen.requests()
	if len(reqs) != 3 || reqs[0].Seed == nil || reqs[1].Seed == nil || *reqs[0].Seed == *reqs[1].Seed {
		t.Errorf("three responses reached the runtime as %d requests: %+v", len(reqs), reqs)
	}
	p.waitFor(`document.querySelectorAll("#pg-chat .pg-turn").length === 2`)
	click("#pg-clear")
	set("#pg-opt-candidateCount", "")

	// Thinking: a manual budget and the thoughts.
	click("#pg-thinking")
	set("#pg-thinking-mode", "manual")
	set("#pg-thinking-budget", "256")
	click("#pg-include-thoughts")
	set("#pg-prompt", "which colour?")
	click("#pg-submit")
	p.waitFor(`document.querySelector("#pg-output").textContent === "Blue."`)
	if got := text("#pg-thoughts"); got != "Weighing it." {
		t.Errorf("thoughts shown as %q", got)
	}
	if u := text("#pg-usage"); !strings.Contains(u, "thoughts 2") {
		t.Errorf("usage %q", u)
	}
	reqs = gen.requests()
	if r := reqs[len(reqs)-1]; r.Thinking == nil || !*r.Thinking || r.ThinkingBudget != 256 {
		t.Errorf("thinking reached the runtime as %v, %d", r.Thinking, r.ThinkingBudget)
	}
	set("#pg-thinking-mode", "off")
	click("#pg-clear")

	// Context caching: 2,100 words is over the 2,048-token minimum.
	set("#pg-prompt", strings.Repeat("ledger ", 2100))
	set("#pg-cache-name", "brief")
	set("#pg-cache-ttl", "10")
	click("#pg-cache-create")
	p.waitFor(`document.querySelector("#pg-cache-status").textContent.startsWith("Created brief")`)
	p.waitFor(`document.querySelectorAll("#pg-cache-rows tr[data-cache]").length === 1`)
	var name string
	p.eval(`document.querySelector("#pg-cache-rows tr[data-cache]").dataset.cache`, &name)
	if !strings.HasPrefix(name, "projects/console/locations/us-central1/cachedContents/") {
		t.Fatalf("cache %q", name)
	}
	id := name[strings.LastIndex(name, "/")+1:]
	var row, tokens string
	p.eval(`document.querySelector("#pg-cache-rows tr[data-cache]").textContent`, &row)
	p.eval(`document.querySelector("#pg-cache-rows tr[data-cache] td:nth-child(2)").textContent`, &tokens)
	if !strings.Contains(row, "brief") || len(tokens) != 4 || tokens < "2100" {
		t.Errorf("row %q, tokens %q", row, tokens)
	}
	set("#pg-cache-ttl-"+id, "30")
	p.eval(`document.querySelector("#pg-cache-rows tr[data-cache] button").click(), true`, nil)
	p.waitFor(`document.querySelector("#pg-cache-status").textContent === "Updated brief"`)

	set("#pg-cache-use", name)
	set("#pg-prompt", "and the total?")
	click("#pg-submit")
	p.waitFor(`document.querySelector("#pg-output").textContent === "Hi!"`)
	if u := text("#pg-usage"); !strings.Contains(u, "cached "+tokens+" ") {
		t.Errorf("usage with the cache %q", u)
	}
	reqs = gen.requests()
	b, _ := json.Marshal(reqs[len(reqs)-1].Conversation)
	if !strings.Contains(string(b), "ledger") || !strings.Contains(string(b), "and the total?") {
		t.Errorf("the cache's contents did not reach the runtime: %.200s", b)
	}

	p.eval(`[...document.querySelectorAll("#pg-cache-rows tr[data-cache] button")].find((b) => b.textContent === "Delete").click(), true`, nil)
	p.waitFor(`document.querySelector("#pg-cache-status").textContent === "Deleted brief"`)
	p.waitFor(`document.querySelectorAll("#pg-cache-rows tr[data-cache]").length === 0 && document.querySelector("#pg-cache-use").value === ""`)
}

// TestAgentStudioResponsesThinkingAndCachingMeetWCAG (#2121): the Studio
// with Thinking opened (Manual, so the budget shows), three responses drawn
// and the Context caching card, audited as every screen is (names, contrast
// in both themes, keyboard, reduced motion, reflow at 320 and 640 px).
func TestAgentStudioResponsesThinkingAndCachingMeetWCAG(t *testing.T) {
	const model = "gemma-4-e2b-it-community"
	addr := studioConsole(t, newGen2121(model), model)
	opener := func(t *testing.T) *tab {
		p := openAt(t, addr)
		p.expected = []string{"/api/resources/projects", "status of 404"}
		return p
	}
	ready := `document.querySelector("#view h1") !== null && document.querySelector("#pg-cache-rows tr") !== null`
	auditScreensAt(t, opener, []a11yPage{
		{"Studio thinking opened", "/agent-platform/studio", ready,
			`document.querySelector("#pg-thinking").click(); const m = document.querySelector("#pg-thinking-mode");
			 m.value = "manual"; m.dispatchEvent(new Event("change"))`},
		{"Studio three responses", "/agent-platform/studio", ready,
			`document.querySelector("#pg-opt-candidateCount").value = "3"; document.querySelector("#pg-prompt").value = "hi";
			 document.querySelector("#pg-submit").click()`},
	})
}
