//go:build browser

package browser

import (
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestTerminalRunsACommandAndKeepsTheSession (#781): the top bar's Activate
// terminal button opens the drawer, a command typed into it runs in the
// cluster shell and its output is drawn, minimising hides the screen, and
// closing and reopening returns to the same shell with its output
// replayed.
func TestTerminalRunsACommandAndKeepsTheSession(t *testing.T) {
	p := open(t)
	p.navigate("/")
	p.waitFor(`document.querySelector("#view h1") !== null`)

	p.run(chromedp.Click("#terminal-toggle", chromedp.ByQuery))
	p.waitFor(`!document.querySelector("#terminal-drawer").hidden`)
	p.awaitTerminal()

	// The typed line reads "echo cb-$((40+2))", so only the shell's
	// evaluation of it can put cb-42 on the screen.
	// Inserted as text, as an input method would, then Enter: key events
	// for printable characters would reach xterm.js twice, as a keydown and
	// as the input that follows it.
	p.run(chromedp.Focus(".xterm-helper-textarea", chromedp.ByQuery),
		input.InsertText("echo cb-$((40+2))"), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector("#terminal-screen .xterm-rows").innerText.includes("cb-42")`)

	p.run(chromedp.Click("#terminal-minimise", chromedp.ByQuery))
	p.waitFor(`getComputedStyle(document.querySelector("#terminal-drawer .terminal-body")).display === "none"`)
	p.run(chromedp.Click("#terminal-minimise", chromedp.ByQuery))
	p.waitFor(`getComputedStyle(document.querySelector("#terminal-drawer .terminal-body")).display !== "none"`)

	p.run(chromedp.Click("#terminal-close", chromedp.ByQuery))
	p.waitFor(`document.querySelector("#terminal-drawer").hidden &&
		document.querySelector("#terminal-toggle").getAttribute("aria-expanded") === "false"`)
	p.run(chromedp.Click("#terminal-toggle", chromedp.ByQuery))
	p.waitFor(`document.querySelector("#terminal-state").textContent === "Reattached"`)
	p.waitFor(`document.querySelector("#terminal-screen .xterm-rows").innerText.includes("cb-42")`)
}

// awaitTerminal waits for the drawer to be connected to a shell, failing
// with the drawer's own reason if it says the terminal is unavailable. The
// first open on a cluster pulls the terminal image, hence the long wait.
func (p *tab) awaitTerminal() {
	p.t.Helper()
	deadline := time.Now().Add(150 * time.Second)
	for {
		var s struct{ State, Notice string }
		p.eval(`({ State: document.querySelector("#terminal-state").textContent,
		           Notice: document.querySelector("#terminal-notice").hidden ? "" :
		                   document.querySelector("#terminal-notice-text").textContent })`, &s)
		switch {
		case s.State == "Connected" || s.State == "Reattached":
			return
		case strings.HasPrefix(s.Notice, "The terminal is unavailable"):
			p.t.Fatalf("the drawer says: %s", s.Notice)
		case time.Now().After(deadline):
			p.t.Fatalf("the terminal did not connect; the drawer shows %+v", s)
		}
		time.Sleep(time.Second)
	}
}
