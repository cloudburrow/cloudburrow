//go:build browser

package browser

import (
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestADialogKeepsFocusWhenItsFieldsChange (Landing 170 run 46073901317,
// TestCloudRunFunctionsEventarcTriggersDeliverThroughTheBrowser: "key 3 of
// a Tab walk took focus out of the dialog"): a dialog's focus stays trapped
// while its fields change under the keyboard user, as an aria-modal dialog
// must (WCAG 2.1.2, 2.4.3). The focused field hidden (a ShowWhen turning
// it off) or removed drops focus to the document; the next Tab goes to the
// field after it, Shift+Tab to the one before, never to what lies outside
// the dialog, such as a snackbar's Dismiss, which stays out of the page's
// inert. Focus moved outside by script comes back into the dialog.
func TestADialogKeepsFocusWhenItsFieldsChange(t *testing.T) {
	p := open(t)
	p.navigate("/")
	p.waitFor(settled)
	p.eval(`(() => {
		notify("a snackbar outside the dialog", "error");
		const { dialog } = openModal({ labelledBy: "focus-probe-title" });
		dialog.append(el("div", { class: "modal-body" }, el("h2", { id: "focus-probe-title", text: "Focus probe" }),
			...["a", "b", "c", "d"].map((n) => el("input", { id: "probe-" + n, "aria-label": n })),
			el("div", { class: "modal-actions" }, el("button", { type: "button", id: "probe-done", text: "Done" }))));
		return true; })()`, nil)
	p.waitFor(`document.querySelector(".modal.is-open #probe-a") !== null`)
	active := func() string {
		var id string
		p.eval(`(() => { const m = document.querySelector(".modal.is-open"); const a = document.activeElement;
			return m && m.contains(a) && a !== m ? a.id : "outside:" + (a ? a.tagName + "#" + a.id + "." + a.className : "none"); })()`, &id)
		return id
	}
	tab := func(back bool) {
		if back {
			p.run(chromedp.KeyEvent(kb.Tab, chromedp.KeyModifiers(chromedp.ModifierShift)))
		} else {
			p.run(chromedp.KeyEvent(kb.Tab))
		}
	}

	p.eval(`document.getElementById("probe-b").focus(); document.getElementById("probe-b").hidden = true; true`, nil)
	tab(false)
	if got := active(); got != "probe-c" {
		t.Errorf("Tab after the focused field was hidden went to %s, want probe-c", got)
	}
	p.eval(`document.getElementById("probe-c").remove(); true`, nil)
	tab(true)
	if got := active(); got != "probe-a" {
		t.Errorf("Shift+Tab after the focused field was removed went to %s, want probe-a", got)
	}
	p.eval(`document.querySelector("#snackbars .snackbar-close").focus(); true`, nil)
	if got := active(); got == "" || got[0] == 'o' {
		t.Errorf("focus moved to the snackbar outside the dialog stayed there: %s", got)
	}
	// The trap still wraps at both ends.
	p.eval(`document.getElementById("probe-done").focus(); true`, nil)
	tab(false)
	if got := active(); got != "probe-a" {
		t.Errorf("Tab from the last control went to %s, want probe-a", got)
	}
	tab(true)
	if got := active(); got != "probe-done" {
		t.Errorf("Shift+Tab from the first control went to %s, want probe-done", got)
	}
}
