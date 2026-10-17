//go:build browser

package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// TestPlaygroundStreamsShowsProvenanceAndRecordsCancelled (#1576): the AI
// Playground page in headless Chromium, on a console served in process
// whose generation endpoint is a simulated runtime streaming Vertex's
// streamGenerateContent events 150 ms apart (no model is downloaded, so it
// needs no instance and runs in any shard). The page shows the model, its
// publisher and the community-conversion notice on the screen with the
// output; the output grows chunk by chunk as the events arrive, not in one
// late write; the run's history entry is OK; and a run cancelled while the
// runtime is still generating is recorded as Cancelled, not Failed.
func TestPlaygroundStreamsShowsProvenanceAndRecordsCancelled(t *testing.T) {
	const model = "gemma-4-e2b-it-community"
	chunks := []string{"one ", "two ", "three ", "four ", "five"}
	var calls atomic.Int32
	cancelled := make(chan struct{}, 1)
	runtime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "__readiness_probe__") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":404,"message":"model not served"}}`))
			return
		}
		if !strings.HasSuffix(r.URL.Path, model+":streamGenerateContent") {
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
			return
		}
		// Read to the end, as a server does, so this handler's context
		// ends when the console drops the request.
		_, _ = io.Copy(io.Discard, r.Body)
		n := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		send := func(text string) {
			b, _ := json.Marshal(map[string]any{"candidates": []map[string]any{{
				"content": map[string]any{"role": "model", "parts": []map[string]any{{"text": text}}}}}, "modelVersion": model})
			fmt.Fprintf(w, "data: %s\n\n", b)
			w.(http.Flusher).Flush()
		}
		for i, c := range chunks {
			if n > 1 && i == 1 {
				// The second run generates until it is cancelled.
				<-r.Context().Done()
				cancelled <- struct{}{}
				return
			}
			send(c)
			select {
			case <-time.After(150 * time.Millisecond):
			case <-r.Context().Done():
				if n > 1 {
					cancelled <- struct{}{}
				}
				return
			}
		}
	}))
	t.Cleanup(runtime.Close)
	addr := strings.TrimPrefix(runtime.URL, "http://")

	s := console.New("", func(context.Context) console.Status { return console.Status{} })
	s.SetPlayground(&console.Playground{Addr: func() string { return addr }, Model: model,
		Publisher: "litert-community", Community: true})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	p := openAt(t, strings.TrimPrefix(srv.URL, "http://"))
	p.navigate("/ai/playground")
	p.waitFor(`document.querySelector("#pg-output") !== null && document.querySelector("#pg-prompt") !== null`)
	// The shell's project picker asks for /api/resources/projects, which a
	// console with no providers answers 404: expected here.
	p.waitFor(`performance.getEntriesByType("resource").some((e) => e.name.includes("/api/resources/projects"))`)
	p.forgive()

	// Provenance, on the screen that shows the output.
	var provenance struct {
		Model, Publisher, Note string
		SameView               bool
	}
	p.eval(`(() => {
		const dd = (label) => { const dt = [...document.querySelectorAll("#view dt")].find((d) => d.textContent === label);
			return dt && dt.nextElementSibling ? dt.nextElementSibling.textContent : ""; };
		const note = [...document.querySelectorAll("#view p.unavailable")].find((n) => n.textContent.includes("COMMUNITY"));
		return { Model: dd("Model"), Publisher: dd("Publisher"), Note: note ? note.textContent : "",
			SameView: !!note && document.querySelector("#view").contains(document.querySelector("#pg-output")) };
	})()`, &provenance)
	if provenance.Model != model || provenance.Publisher != "litert-community" ||
		!strings.Contains(provenance.Note, "not a Gemini result") || !provenance.SameView {
		t.Errorf("provenance beside the output = %+v", provenance)
	}

	// Streaming: every length the output takes is recorded as it changes.
	p.eval(`(() => {
		const out = document.querySelector("#pg-output");
		window.__lengths = [];
		new MutationObserver(() => {
			const n = out.textContent.length;
			if (window.__lengths[window.__lengths.length - 1] !== n) window.__lengths.push(n);
		}).observe(out, { childList: true, characterData: true, subtree: true });
		document.querySelector("#pg-stream").checked = true;
		document.querySelector("#pg-stream").dispatchEvent(new Event("change"));
		document.querySelector("#pg-prompt").value = "count to five";
		[...document.querySelectorAll("#view button")].find((b) => b.textContent === "Run").click();
		return true;
	})()`, nil)
	want := strings.Join(chunks, "")
	p.waitFor(fmt.Sprintf(`document.querySelector("#pg-output").textContent === %q`, want))
	var lengths []int
	p.eval(`window.__lengths.filter((n) => n > 0)`, &lengths)
	if len(lengths) < len(chunks) {
		t.Errorf("the output took lengths %v: %d separate steps, want one per streamed chunk (%d)", lengths, len(lengths), len(chunks))
	}
	historyState := `(() => { const s = document.querySelector("#view tbody tr .status"); return s ? s.textContent : ""; })()`
	p.waitFor(historyState + ` === "OK"`)

	// Cancelled mid-generation: recorded as Cancelled, and the runtime's
	// request is ended.
	p.eval(`(() => { document.querySelector("#pg-prompt").value = "count forever";
		[...document.querySelectorAll("#view button")].find((b) => b.textContent === "Run").click(); return true; })()`, nil)
	p.waitFor(`document.querySelector("#pg-output").textContent === "one "`)
	p.eval(`[...document.querySelectorAll("#view button")].find((b) => b.textContent === "Cancel").click(), true`, nil)
	p.waitFor(historyState + ` === "Cancelled"`)
	select {
	case <-cancelled:
	case <-time.After(10 * time.Second):
		t.Error("cancelling on the page did not end the runtime's request")
	}
	var rows int
	p.eval(`document.querySelectorAll("#view tbody tr .status").length`, &rows)
	if rows < 2 {
		t.Errorf("history has %d entries, want the OK run and the Cancelled one", rows)
	}
}
