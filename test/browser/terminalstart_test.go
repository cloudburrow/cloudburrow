//go:build browser

package browser

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// slowStart is a console.Terminal whose pod is never ready on its own: each
// Prepare reports the next of its scripted steps only when the test sends
// on step, so the page is looked at in each state of a first start. The
// first Prepare ends with failure; the next succeeds, and Open gives a shell
// that prints a prompt.
type slowStart struct {
	step    chan struct{}
	mu      sync.Mutex
	calls   int
	first   []console.TerminalProgress
	second  []console.TerminalProgress
	failure error
}

func (s *slowStart) Prepare(ctx context.Context, progress func(console.TerminalProgress)) error {
	s.mu.Lock()
	s.calls++
	reports, fail := s.first, s.failure
	if s.calls > 1 {
		reports, fail = s.second, nil
	}
	s.mu.Unlock()
	for _, r := range reports {
		progress(r)
		select {
		case <-s.step:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fail
}

func (s *slowStart) Open(string, uint16, uint16) (console.TerminalSession, error) {
	r, w := io.Pipe()
	go func() { _, _ = io.WriteString(w, "slow-start-prompt$ ") }()
	return &promptShell{out: r, in: w}, nil
}

// promptShell is a shell that prints its prompt and discards what is typed.
type promptShell struct {
	out *io.PipeReader
	in  *io.PipeWriter
}

func (p *promptShell) Read(b []byte) (int, error)               { return p.out.Read(b) }
func (p *promptShell) Write(b []byte) (int, error)              { return len(b), nil }
func (p *promptShell) Close() error                             { return p.in.Close() }
func (p *promptShell) Resize(uint16, uint16) error              { return nil }
func (p *promptShell) SetProject(context.Context, string) error { return nil }

// TestTerminalFirstStartShowsOneProgressPanelThenConnects (#1838): while the
// terminal image is pulled on a first start, the drawer shows one starting
// panel, as Cloud Shell's "Provisioning your Cloud Shell machine" does: a
// spinner, a progress bar from the bytes the node reports, "800 MB of 1.0
// GB", the elapsed time, a time left only once the rate gives one, and
// "First start only". The status is shown once, not in the header and the
// panel both, and the cluster's raw sentence is not shown. A failure clears
// the panel for the reason and Retry; Retry starts again and, once the pod
// is ready, the panel goes and the shell is connected without a click. The
// panel follows the light and the dark theme. The terminal is a scripted
// fake on a console served in process, so the test needs no cluster and
// runs in any shard.
func TestTerminalFirstStartShowsOneProgressPanelThenConnects(t *testing.T) {
	const total = 1033e6
	raw := "Pulling the terminal image (the Cloud SDK with kubectl; first use only): fetched 100 MB of 1033 MB, as the node reports it; 20s so far"
	term := &slowStart{
		step: make(chan struct{}),
		first: []console.TerminalProgress{
			{Message: "Creating the terminal pod"},
			{Message: raw, Step: "pull", Fetched: 100e6, Total: total, Elapsed: 20 * time.Second},
			{Message: raw, Step: "pull", Fetched: 200e6, Total: total, Elapsed: 40 * time.Second},
			{Message: raw, Step: "pull", Fetched: 800e6, Total: total, Elapsed: 60 * time.Second},
		},
		failure: errors.New("the terminal image cannot be pulled (ErrImagePull)"),
		second: []console.TerminalProgress{
			{Message: "unpacking", Step: "unpack", Fetched: total, Total: total, Elapsed: 70 * time.Second},
		},
	}
	s := console.New("", func(context.Context) console.Status { return console.Status{} })
	s.SetTerminal(term)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	// Prepare holds each step until the test sends one; a test that fails
	// part-way lets it go when the page closes the socket.
	p := openAt(t, strings.TrimPrefix(srv.URL, "http://"))
	p.navigate("/")
	p.waitFor(`document.querySelector("#view h1") !== null`)
	p.waitFor(`performance.getEntriesByType("resource").some((e) => e.name.includes("/api/resources/projects"))`)
	// A console with no providers answers the project list 404: expected.
	p.forgive()
	next := func() {
		t.Helper()
		select {
		case term.step <- struct{}{}:
		case <-time.After(30 * time.Second):
			t.Fatal("the terminal's Prepare did not wait for the next step")
		}
	}

	p.run(chromedp.Click(chromedp.CSS("#terminal-toggle")))
	p.waitFor(`!document.querySelector("#terminal-starting").hidden &&
		document.querySelector("#terminal-starting-line").textContent === "Creating the terminal pod"`)
	// Not a first-start step yet: no "this takes a few minutes".
	p.waitFor(`document.querySelector("#terminal-starting-note").hidden`)

	next()
	p.waitFor(`document.querySelector("#terminal-starting-line").textContent.startsWith("100 MB of 1.0 GB · ")`)
	var first struct {
		Line, Now, Note, State, Drawer string
		NoteShown, NoticeShown         bool
	}
	p.eval(`(() => {
		const bar = document.querySelector("#terminal-starting-bar");
		const note = document.querySelector("#terminal-starting-note");
		return { Line: document.querySelector("#terminal-starting-line").textContent,
			Now: bar.getAttribute("aria-valuenow") || "", Note: note.textContent, NoteShown: !note.hidden,
			State: document.querySelector("#terminal-state").textContent,
			NoticeShown: !document.querySelector("#terminal-notice").hidden,
			Drawer: document.querySelector("#terminal-drawer").innerText };
	})()`, &first)
	if first.Now != "9" {
		t.Errorf("the progress bar is at %q; want 9 (100 MB of 1033 MB)", first.Now)
	}
	if !first.NoteShown || first.Note != "First start only — this takes a few minutes." {
		t.Errorf("the first-start note is %q (shown %v)", first.Note, first.NoteShown)
	}
	// One sample is no rate: no time left yet.
	if strings.Contains(first.Line, "left") {
		t.Errorf("the line %q estimates the time left from a single reading", first.Line)
	}
	if !strings.Contains(first.Line, "s elapsed") {
		t.Errorf("the line %q does not say how long the pull has taken", first.Line)
	}
	// Said once: not in the header too, nor in a notice under the panel.
	if first.State != "Starting…" || first.NoticeShown {
		t.Errorf("the header says %q and the notice is shown %v; want Starting… and no notice", first.State, first.NoticeShown)
	}
	if n := strings.Count(first.Drawer, "100 MB of 1.0 GB"); n != 1 {
		t.Errorf("the drawer shows the progress %d times:\n%s", n, first.Drawer)
	}
	if strings.Contains(first.Drawer, "as the node reports it") {
		t.Errorf("the drawer shows the cluster's raw sentence:\n%s", first.Drawer)
	}

	// Twenty seconds at 5 MB/s, with 833 MB to go: about three minutes.
	next()
	p.waitFor(`document.querySelector("#terminal-starting-line").textContent.startsWith("200 MB of 1.0 GB · ") &&
		document.querySelector("#terminal-starting-line").textContent.endsWith(" · about 3 min left")`)
	next()
	p.waitFor(`document.querySelector("#terminal-starting-line").textContent.startsWith("800 MB of 1.0 GB · ") &&
		document.querySelector("#terminal-starting-line").textContent.endsWith(" · less than a minute left") &&
		document.querySelector("#terminal-starting-bar").getAttribute("aria-valuenow") === "77"`)

	// The panel is the console's surface in both themes.
	var themes struct{ Light, Dark, LightSurface, DarkSurface string }
	p.eval(`(() => {
		const root = document.documentElement, panel = document.querySelector("#terminal-starting");
		const read = () => [getComputedStyle(panel).backgroundColor,
			getComputedStyle(document.querySelector("#terminal-drawer")).backgroundColor];
		const before = root.getAttribute("data-theme");
		root.setAttribute("data-theme", "light"); const light = read();
		root.setAttribute("data-theme", "dark"); const dark = read();
		if (before === null) root.removeAttribute("data-theme"); else root.setAttribute("data-theme", before);
		return { Light: light[0], LightSurface: light[1], Dark: dark[0], DarkSurface: dark[1] };
	})()`, &themes)
	if themes.Light != themes.LightSurface || themes.Dark != themes.DarkSurface || themes.Light == themes.Dark {
		t.Errorf("the panel's background is %s light and %s dark; want the drawer's surface, %s and %s",
			themes.Light, themes.Dark, themes.LightSurface, themes.DarkSurface)
	}

	// A failure: the panel goes, the reason and Retry stay.
	next()
	p.waitFor(`document.querySelector("#terminal-starting").hidden &&
		!document.querySelector("#terminal-notice").hidden &&
		document.querySelector("#terminal-notice-text").textContent.includes("ErrImagePull") &&
		!document.querySelector("#terminal-retry").hidden &&
		document.querySelector("#terminal-retry").textContent === "Retry"`)

	// Retry starts again; the unpacking step has no count, so its bar is
	// indeterminate.
	p.run(chromedp.Click(chromedp.CSS("#terminal-retry")))
	p.waitFor(`!document.querySelector("#terminal-starting").hidden &&
		document.querySelector("#terminal-notice").hidden &&
		document.querySelector("#terminal-starting-line").textContent.startsWith("Unpacking the image · ") &&
		document.querySelector("#terminal-starting-bar").classList.contains("is-indeterminate") &&
		!document.querySelector("#terminal-starting-bar").hasAttribute("aria-valuenow")`)

	// Ready: the panel clears and the shell connects without a click.
	next()
	p.waitFor(`document.querySelector("#terminal-starting").hidden &&
		document.querySelector("#terminal-notice").hidden &&
		document.querySelector("#terminal-state").textContent === "Connected"`)
	p.waitFor(`document.querySelector("#terminal-screen .xterm-rows").innerText.includes("slow-start-prompt$")`)
}
