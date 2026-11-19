//go:build browser

package browser

import (
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/vertexai"
)

// studioGen answers each generation with the script next picks, and counts
// tokens as CountingGenerator does: a runtime with a tokenizer and function
// calls, without a model.
type studioGen struct {
	*vertexai.CountingGenerator
	next func(vertexai.Request) *vertexai.ScriptedGenerator
}

func (g *studioGen) Generate(ctx context.Context, req vertexai.Request) (<-chan vertexai.Chunk, func() error, error) {
	if g.Seen != nil {
		g.Seen(req)
	}
	return g.next(req).Generate(ctx, req)
}

// studioConsole serves a console in process whose Studio relays to the real
// vertexai server over gen.
func studioConsole(t *testing.T, gen vertexai.Generator, model string) string {
	t.Helper()
	api := httptest.NewServer(vertexai.NewServer(gen).Handler())
	t.Cleanup(api.Close)
	addr := strings.TrimPrefix(api.URL, "http://")
	s := console.New("", func(context.Context) console.Status { return console.Status{} })
	s.SetPlayground(&console.Playground{Addr: func() string { return addr }, Model: model,
		Publisher: "litert-community", Community: true})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// TestAgentStudioChatToolsImagesTokensAndCode (#1962): Agent Studio's
// prompt page in headless Chromium, on a console served in process whose
// generation endpoint is the real vertexai server over a scripted runtime
// with a tokenizer (no model is downloaded, so it runs in any shard). The
// token count is countTokens' as the prompt is typed, and the tokenizer
// shows computeTokens' tokens and IDs; a chat keeps its turns and sends
// them, with the System instructions, as the request's contents; Function
// calling sends the declarations and mode, the model's functionCall is
// shown and answered with a functionResponse turn; an uploaded image is an
// inlineData part; the response's usageMetadata is shown; the Advanced
// penalties reach the runtime; Get code writes the request as Python and
// curl; and Clear conversation empties the chat.
func TestAgentStudioChatToolsImagesTokensAndCode(t *testing.T) {
	const model = "gemma-4-e2b-it-community"
	var mu sync.Mutex
	var got []vertexai.Request
	usage := &vertexai.Usage{PromptTokens: 9, CandidateTokens: 2}
	gen := &studioGen{
		CountingGenerator: &vertexai.CountingGenerator{ScriptedGenerator: &vertexai.ScriptedGenerator{ID: model, Options: true,
			Seen: func(r vertexai.Request) { mu.Lock(); got = append(got, r); mu.Unlock() }}},
		next: func(r vertexai.Request) *vertexai.ScriptedGenerator {
			b, _ := json.Marshal(r.Conversation)
			switch {
			case strings.Contains(string(b), "sunny"):
				return &vertexai.ScriptedGenerator{ID: model, Chunks: []string{"It is ", "sunny."}, Usage: usage}
			case strings.Contains(r.Prompt, "weather"):
				return &vertexai.ScriptedGenerator{ID: model, Usage: usage,
					Calls: []vertexai.FunctionCall{{Name: "get_weather", Args: map[string]any{"city": "Paris"}}}}
			}
			return &vertexai.ScriptedGenerator{ID: model, Chunks: []string{"Hi", "!"}, Usage: usage}
		},
	}
	p := openAt(t, studioConsole(t, gen, model))
	p.navigate("/agent-platform/studio")
	p.waitFor(`document.querySelector("#pg-prompt") !== null && document.querySelector("#pg-tokens") !== null`)
	p.waitFor(`performance.getEntriesByType("resource").some((e) => e.name.includes("/api/resources/projects"))`)
	p.forgive()
	last := func() vertexai.Request {
		mu.Lock()
		defer mu.Unlock()
		if len(got) == 0 {
			t.Fatal("no generation reached the runtime")
		}
		return got[len(got)-1]
	}
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

	// The token count, as typed: "Answer briefly." and "hello there".
	set("#pg-system", "Answer briefly.")
	set("#pg-prompt", "hello there")
	p.waitFor(`/^[1-9][0-9]* tokens$/.test(document.querySelector("#pg-tokens").textContent)`)
	click("#pg-tokens")
	p.waitFor(`[...document.querySelectorAll("#pg-tokenizer-out .pg-token")].map((s) => s.textContent).join("|") === "hello|there"`)
	p.eval(`[...document.querySelectorAll("#pg-tokenizer button")].find((b) => b.textContent === "Token ID").click(), true`, nil)
	p.waitFor(`[...document.querySelectorAll("#pg-tokenizer-out .pg-token")].map((s) => s.textContent).join("|") === "1|2"`)
	click("#pg-tokens")

	// The first turn, with the Advanced penalties.
	set("#pg-opt-presencePenalty", "0.5")
	set("#pg-opt-frequencyPenalty", "-0.5")
	click("#pg-submit")
	p.waitFor(`document.querySelector("#pg-output").textContent === "Hi!"`)
	p.waitFor(`document.querySelectorAll("#pg-chat .pg-turn").length === 2`)
	if r := last(); r.PresencePenalty == nil || *r.PresencePenalty != 0.5 || r.FrequencyPenalty == nil || *r.FrequencyPenalty != -0.5 {
		t.Errorf("the penalties reached the runtime as %v, %v", r.PresencePenalty, r.FrequencyPenalty)
	}
	if u := text("#pg-usage"); !strings.Contains(u, "prompt 9") || !strings.Contains(u, "response 2") {
		t.Errorf("usageMetadata shown as %q", u)
	}
	if v := text("#pg-prompt"); v != "" {
		t.Errorf("the prompt box kept %q after the turn", v)
	}

	// Function calling: the declarations and mode, the model's call.
	click("#pg-fc")
	set("#pg-fc-decls", `[{"name":"get_weather","description":"The weather in a city","parameters":{"type":"OBJECT","properties":{"city":{"type":"STRING"}},"required":["city"]}}]`)
	set("#pg-fc-mode", "ANY")
	set("#pg-prompt", "weather in Paris?")
	click("#pg-submit")
	p.waitFor(`document.querySelector("#pg-fnresp-0") !== null`)
	r := last()
	pre, _ := json.Marshal(r.Conversation.Preface)
	if !strings.Contains(string(pre), "Answer briefly.") || !strings.Contains(string(pre), "hello there") || !strings.Contains(string(pre), "Hi!") {
		t.Errorf("the second turn's preface %s lacks the system instruction or the first turn", pre)
	}
	if len(r.Conversation.Tools) != 1 || r.Conversation.ToolChoice == nil {
		t.Errorf("the declarations reached the runtime as %s, %+v", r.Conversation.Tools, r.Conversation.ToolChoice)
	}
	if c := text("#pg-chat"); !strings.Contains(c, "Function call: get_weather") {
		t.Errorf("the chat does not show the call: %q", c)
	}

	// The function response turn.
	set("#pg-fnresp-0", `{"forecast":"sunny"}`)
	click("#pg-fnresp-send")
	p.waitFor(`document.querySelector("#pg-output").textContent === "It is sunny."`)
	if c := text("#pg-chat"); !strings.Contains(c, "Function response: get_weather") {
		t.Errorf("the chat does not show the response: %q", c)
	}

	// An uploaded image: an inlineData part, for the vision encoder.
	click("#pg-fc")
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(1, 1, color.RGBA{R: 255, A: 255})
	file := filepath.Join(t.TempDir(), "red.png")
	f, err := os.Create(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	p.run(chromedp.SetUploadFiles(chromedp.CSS(`#pg-upload`), []string{file}))
	p.waitFor(`document.querySelector("#pg-attachments").textContent.includes("red.png")`)
	set("#pg-prompt", "what is this?")
	click("#pg-submit")
	p.waitFor(`document.querySelectorAll("#pg-chat .pg-turn").length === 8`)
	if r := last(); !r.Vision {
		t.Error("the image did not reach the runtime's vision encoder")
	}
	if c := text("#pg-chat"); !strings.Contains(c, "[image image/png: red.png]") {
		t.Errorf("the chat does not show the image: %q", c)
	}

	// Get code: the request as Python and curl, at the local endpoint.
	set("#pg-prompt", "and now?")
	click("#pg-get-code")
	p.waitFor(`document.querySelector("#pg-code-text").textContent.includes("generate_content")`)
	if code := text("#pg-code-text"); !strings.Contains(code, `model="`+model+`"`) || !strings.Contains(code, "and now?") ||
		!strings.Contains(code, "system_instruction") || !strings.Contains(code, "http://127.0.0.1:") {
		t.Errorf("Python code:\n%s", code)
	}
	set("#pg-code-lang", "curl")
	p.waitFor(`document.querySelector("#pg-code-text").textContent.startsWith("curl")`)
	if code := text("#pg-code-text"); !strings.Contains(code, model+":generateContent") || !strings.Contains(code, `"functionResponse"`) {
		t.Errorf("curl:\n%s", code)
	}

	// Clear conversation.
	p.eval(`[...document.querySelectorAll("#view button")].find((b) => b.textContent === "Clear conversation").click(), true`, nil)
	p.waitFor(`document.querySelectorAll("#pg-chat .pg-turn").length === 0`)
}

// jsq quotes s as a JavaScript string.
func jsq(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestAgentStudioPromptPageMeetsWCAG (#1962): the Studio's prompt page,
// configured, with its tokenizer, Get code, Advanced, Function calling and
// Insert Media opened, audited as every screen is (names, contrast in both
// themes, keyboard, reduced motion, reflow at 320 and 640 px), on a console
// served in process.
func TestAgentStudioPromptPageMeetsWCAG(t *testing.T) {
	const model = "gemma-4-e2b-it-community"
	gen := &vertexai.CountingGenerator{ScriptedGenerator: &vertexai.ScriptedGenerator{ID: model, Chunks: []string{"ok"}, Options: true}}
	addr := studioConsole(t, gen, model)
	opener := func(t *testing.T) *tab {
		p := openAt(t, addr)
		// The in-process console serves no providers: its project list is 404.
		p.expected = []string{"/api/resources/projects", "status of 404"}
		return p
	}
	ready := `document.querySelector("#view h1") !== null && document.querySelector("#pg-tokens") !== null && /tokens?$/.test(document.querySelector("#pg-tokens").textContent)`
	pages := []a11yPage{
		{"Studio prompt", "/agent-platform/studio", ready, ""},
		{"Studio tokenizer", "/agent-platform/studio", ready,
			`document.querySelector("#pg-prompt").value = "hello there"; document.querySelector("#pg-tokens").click()`},
		{"Studio Get code", "/agent-platform/studio", ready, `document.querySelector("#pg-get-code").click()`},
		{"Studio settings opened", "/agent-platform/studio", ready,
			`document.querySelector("#pg-advanced").click(); document.querySelector("#pg-output-format").click();
			 document.querySelector("#pg-media").click(); document.querySelector("#pg-fc").click()`},
	}
	auditScreensAt(t, opener, pages)
}
