//go:build browser

package browser

import (
	"fmt"
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

// TestTerminalTabsKeepTheirOwnShellsAcrossAReload (#834): + opens a second
// tab with its own shell, a different command run in each tab is drawn only
// in that tab, and after a reload the drawer comes back with both tabs,
// each reattached to its shell with its own output replayed. The tab strip
// is moved through with the arrow keys and Alt+PageUp, and closing a tab
// leaves the other.
func TestTerminalTabsKeepTheirOwnShellsAcrossAReload(t *testing.T) {
	p := open(t)
	p.navigate("/")
	p.waitFor(`document.querySelector("#view h1") !== null`)
	p.run(chromedp.Click("#terminal-toggle", chromedp.ByQuery))
	p.waitFor(`document.querySelectorAll("#terminal-tabs [role=tab]").length === 1`)
	p.awaitTerminal()
	// Each typed line evaluates arithmetic, so only that tab's shell can
	// put its result on the screen.
	p.typeInTerminal("echo tab-one-$((40+1))")
	p.waitFor(paneHas(0, "tab-one-41"))

	p.run(chromedp.Click("#terminal-new-tab", chromedp.ByQuery))
	p.waitFor(`document.querySelectorAll("#terminal-tabs [role=tab]").length === 2 &&
		document.querySelectorAll("#terminal-tabs [role=tab]")[1].getAttribute("aria-selected") === "true"`)
	p.awaitTerminal()
	p.typeInTerminal("echo tab-two-$((40+2))")
	p.waitFor(paneHas(1, "tab-two-42"))

	p.run(chromedp.Reload())
	p.waitFor(`document.querySelector("#view h1") !== null`)
	p.run(chromedp.Click("#terminal-toggle", chromedp.ByQuery))
	p.waitFor(`document.querySelectorAll("#terminal-tabs [role=tab]").length === 2`)
	// The selected tab is kept too: the second.
	p.waitFor(`document.querySelectorAll("#terminal-tabs [role=tab]")[1].getAttribute("aria-selected") === "true"`)
	p.waitFor(`document.querySelector("#terminal-state").textContent === "Reattached"`)
	p.waitFor(paneHas(1, "tab-two-42"))
	if p.paneText(1, "tab-one-41") {
		t.Error("the second tab shows the first tab's output")
	}

	// ArrowLeft in the tab strip selects the first tab, reattached to its
	// own shell.
	p.run(chromedp.Focus(`#terminal-tabs [aria-selected="true"]`, chromedp.ByQuery), chromedp.KeyEvent(kb.ArrowLeft))
	p.waitFor(`document.querySelectorAll("#terminal-tabs [role=tab]")[0].getAttribute("aria-selected") === "true"`)
	p.waitFor(`document.querySelector("#terminal-state").textContent === "Reattached"`)
	p.waitFor(paneHas(0, "tab-one-41"))
	if p.paneText(0, "tab-two-42") {
		t.Error("the first tab shows the second tab's output")
	}
	// Alt+PageUp from inside the terminal moves to the previous tab, which
	// wraps to the second.
	p.run(chromedp.Focus(`#terminal-screen .terminal-pane:not([hidden]) .xterm-helper-textarea`, chromedp.ByQuery),
		chromedp.KeyEvent(kb.PageUp, chromedp.KeyModifiers(input.ModifierAlt)))
	p.waitFor(`document.querySelectorAll("#terminal-tabs [role=tab]")[1].getAttribute("aria-selected") === "true"`)

	// Closing the second tab ends it and leaves the first, still a shell.
	p.run(chromedp.Click(`#terminal-tabs [aria-selected="true"] .terminal-tab-close`, chromedp.ByQuery))
	p.waitFor(`document.querySelectorAll("#terminal-tabs [role=tab]").length === 1 &&
		document.querySelectorAll("#terminal-tabs [role=tab]")[0].getAttribute("aria-selected") === "true"`)
	p.typeInTerminal("echo still-$((40+3))")
	p.waitFor(paneHas(0, "still-43"))
}

// typeInTerminal types a line into the selected tab's terminal and presses
// Enter. Inserted as text, as an input method would: key events for
// printable characters would reach xterm.js twice, as a keydown and as the
// input that follows it.
func (p *tab) typeInTerminal(line string) {
	p.t.Helper()
	p.run(chromedp.Focus(`#terminal-screen .terminal-pane:not([hidden]) .xterm-helper-textarea`, chromedp.ByQuery),
		input.InsertText(line), chromedp.KeyEvent(kb.Enter))
}

// paneHas is an expression true when the i-th tab's screen shows s.
func paneHas(i int, s string) string {
	return fmt.Sprintf(`(() => { const p = document.querySelectorAll("#terminal-screen .terminal-pane")[%d];
		return !!p && p.querySelector(".xterm-rows").innerText.includes(%q); })()`, i, s)
}

func (p *tab) paneText(i int, s string) bool {
	p.t.Helper()
	var ok bool
	p.eval(paneHas(i, s), &ok)
	return ok
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
