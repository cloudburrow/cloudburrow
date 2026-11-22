//go:build browser

package browser

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/service/vertexai"
)

// gen2122 answers "Hi!" in two chunks and, with responseLogprobs, reports
// each token's step: the chosen token and, with logprobs N, N candidates
// (#2122). It records each request the runtime is asked.
type gen2122 struct {
	*vertexai.CountingGenerator
	mu  sync.Mutex
	got []vertexai.Request
}

func (g *gen2122) Generate(ctx context.Context, req vertexai.Request) (<-chan vertexai.Chunk, func() error, error) {
	g.mu.Lock()
	g.got = append(g.got, req)
	g.mu.Unlock()
	if !req.ResponseLogprobs {
		return g.CountingGenerator.Generate(ctx, req)
	}
	step := func(tok string, id int32, lp float32) vertexai.LogprobStep {
		s := vertexai.LogprobStep{Chosen: vertexai.LogprobsCandidate{Token: tok, TokenID: id, LogProbability: lp}}
		alts := []vertexai.LogprobsCandidate{s.Chosen, {Token: " Hello", TokenID: 900, LogProbability: -1.5}, {Token: "Hey", TokenID: 901, LogProbability: -2.25}}
		for i := 0; i < int(req.TopLogprobs) && i < len(alts); i++ {
			s.Top = append(s.Top, alts[i])
		}
		return s
	}
	// As the runtime emits them: each step ahead of the text its token
	// spells, which the API pairs with it (a stop sequence may cut it).
	ch := make(chan vertexai.Chunk, 5)
	ch <- vertexai.Chunk{Logprobs: []vertexai.LogprobStep{step("Hi", 7, -0.25)}}
	ch <- vertexai.Chunk{Text: "Hi"}
	ch <- vertexai.Chunk{Logprobs: []vertexai.LogprobStep{step("!", 8, -0.75)}}
	ch <- vertexai.Chunk{Text: "!"}
	ch <- vertexai.Chunk{Last: true, FinishReason: vertexai.FinishReasonStop,
		Usage: &vertexai.Usage{PromptTokens: 5, CandidateTokens: 2}}
	close(ch)
	return ch, func() error { return nil }, nil
}

func (g *gen2122) requests() []vertexai.Request {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]vertexai.Request(nil), g.got...)
}

func newGen2122(model string) *gen2122 {
	return &gen2122{CountingGenerator: &vertexai.CountingGenerator{ScriptedGenerator: &vertexai.ScriptedGenerator{
		ID: model, Chunks: []string{"Hi", "!"}, Options: true, Usage: &vertexai.Usage{PromptTokens: 9, CandidateTokens: 2}}}}
}

// TestAgentStudioLogProbabilities (#2122): Agent Studio's Advanced settings
// in headless Chromium. Top candidates is disabled until Log probabilities
// is on; on, with Top candidates 2, the request carries responseLogprobs and
// logprobs 2 to the runtime, and the response shows avgLogprobs and a row per
// chosen token with its log probability, probability and the step's two top
// candidates, unary and streamed (the stream's chunks gathered). Off again,
// neither field is sent and no scores are shown.
func TestAgentStudioLogProbabilities(t *testing.T) {
	const model = "gemma-4-e2b-it-community"
	gen := newGen2122(model)
	p := openAt(t, studioConsole(t, gen, model))
	p.navigate("/agent-platform/studio")
	p.waitFor(`document.querySelector("#pg-prompt") !== null && document.querySelector("#pg-logprobs") !== null`)
	p.forgive()
	set := func(id, v string) {
		p.eval(`((id, v) => { const e = document.querySelector(id); e.value = v; e.dispatchEvent(new Event("input", { bubbles: true }));
			e.dispatchEvent(new Event("change", { bubbles: true })); return true; })(`+jsq(id)+`, `+jsq(v)+`)`, nil)
	}
	click := func(id string) { p.eval(`document.querySelector(`+jsq(id)+`).click(), true`, nil) }
	text := func(q string) string {
		var s string
		p.eval(`(document.querySelector(`+jsq(q)+`) || {}).textContent || ""`, &s)
		return s
	}
	disabled := func() bool {
		var d bool
		p.eval(`document.querySelector("#pg-logprobs-top").disabled`, &d)
		return d
	}

	click("#pg-advanced")
	if !disabled() {
		t.Errorf("Top candidates is enabled without Log probabilities")
	}
	click("#pg-logprobs")
	if disabled() {
		t.Fatalf("Top candidates is still disabled with Log probabilities on")
	}
	set("#pg-logprobs-top", "2")

	for _, streamed := range []bool{false, true} {
		var on bool
		p.eval(`document.querySelector("#pg-stream").checked`, &on)
		if on != streamed {
			click("#pg-stream")
		}
		set("#pg-prompt", "greet me")
		click("#pg-submit")
		p.waitFor(`document.querySelectorAll("#pg-logprobs-rows-0 tr").length === 2`)
		if got := text("#pg-output"); got != "Hi!" {
			t.Errorf("streamed=%v: output %q", streamed, got)
		}
		if got := text("#pg-avg-logprobs-0"); got != "Average log probability (avgLogprobs): -0.5000" {
			t.Errorf("streamed=%v: %q", streamed, got)
		}
		var rows []string
		p.eval(`[...document.querySelectorAll("#pg-logprobs-rows-0 tr")].map((r) => [...r.cells].map((c) => c.textContent).join(" | "))`, &rows)
		want := []string{
			`1 | "Hi" | -0.2500 | 77.88% | "Hi" -0.2500, " Hello" -1.5000`,
			`2 | "!" | -0.7500 | 47.24% | "!" -0.7500, " Hello" -1.5000`,
		}
		if strings.Join(rows, "\n") != strings.Join(want, "\n") {
			t.Errorf("streamed=%v: rows\n%s\nwant\n%s", streamed, strings.Join(rows, "\n"), strings.Join(want, "\n"))
		}
		reqs := gen.requests()
		if r := reqs[len(reqs)-1]; !r.ResponseLogprobs || r.TopLogprobs != 2 {
			t.Errorf("streamed=%v: the runtime was asked responseLogprobs %v, logprobs %d", streamed, r.ResponseLogprobs, r.TopLogprobs)
		}
		p.waitFor(`document.querySelector("#pg-submit").disabled === false`)
		click("#pg-clear")
	}

	// Off: Top candidates empties and disables, and nothing is asked.
	click("#pg-logprobs")
	var v string
	p.eval(`document.querySelector("#pg-logprobs-top").value`, &v)
	if !disabled() || v != "" {
		t.Errorf("Top candidates after the switch is off: disabled %v, value %q", disabled(), v)
	}
	set("#pg-prompt", "again")
	click("#pg-submit")
	p.waitFor(`document.querySelector("#pg-output").textContent === "Hi!" && document.querySelector("#pg-submit").disabled === false`)
	var hidden bool
	p.eval(`document.querySelector("#pg-logprobs-out").hidden`, &hidden)
	if !hidden {
		t.Errorf("scores are shown for a request without responseLogprobs")
	}
	reqs := gen.requests()
	if r := reqs[len(reqs)-1]; r.ResponseLogprobs || r.TopLogprobs != 0 {
		t.Errorf("the switch off still asked responseLogprobs %v, logprobs %d", r.ResponseLogprobs, r.TopLogprobs)
	}
}

// TestAgentStudioLogProbabilitiesMeetWCAG (#2122): the Studio with Advanced
// open, Log probabilities on with Top candidates 2, and a response's scores
// drawn, audited as every screen is (names, contrast in both themes,
// keyboard, reduced motion, reflow at 320 and 640 px).
func TestAgentStudioLogProbabilitiesMeetWCAG(t *testing.T) {
	const model = "gemma-4-e2b-it-community"
	addr := studioConsole(t, newGen2122(model), model)
	opener := func(t *testing.T) *tab {
		p := openAt(t, addr)
		p.expected = []string{"/api/resources/projects", "status of 404"}
		return p
	}
	ready := `document.querySelector("#view h1") !== null && document.querySelector("#pg-logprobs") !== null`
	auditScreensAt(t, opener, []a11yPage{
		{"Studio log probabilities", "/agent-platform/studio", ready,
			`document.querySelector("#pg-advanced").click(); document.querySelector("#pg-logprobs").click();
			 const k = document.querySelector("#pg-logprobs-top"); k.value = "2"; k.dispatchEvent(new Event("input"));
			 document.querySelector("#pg-prompt").value = "hi"; document.querySelector("#pg-submit").click()`},
	})
}
