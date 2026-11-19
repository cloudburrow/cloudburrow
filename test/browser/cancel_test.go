//go:build browser

package browser

import (
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestCancelDiscardsWithoutAskingEscapeAsks: a create dialog with typed
// input used to answer Cancel with a second question, "Discard your
// changes? Keep editing / Discard", although Cancel is that decision. The
// maintainer called it out on 2026-09-27. Cancel now closes at once; Escape,
// which can be pressed by accident, still asks before throwing input away.
// The dialog is the Resource hierarchy's Create folder: Create queue is a
// page of its own since #1842, and Create project the New project page since
// #1932.
func TestCancelDiscardsWithoutAskingEscapeAsks(t *testing.T) {
	needService(t, "hierarchy")
	p := open(t)
	project := uniqueProject(t)
	p.navigate("/resource-hierarchy?project=" + project)
	create := `#view .page-header-actions button.header-button`
	p.waitFor(`[...document.querySelectorAll("` + create + `")].some((b) => b.textContent === "Create folder")`)

	openTyped := func() {
		// Only the open dialog: a cancelled one fades out for a moment,
		// inert, before it is removed.
		p.waitFor(`document.querySelector(".modal") === null`)
		p.clickText(create, "Create folder")
		p.waitFor(`document.querySelector(".modal.is-open #f-displayName") !== null`)
		p.run(chromedp.SendKeys(chromedp.CSS(`.modal.is-open #f-displayName`), "Typed folder"))
	}

	openTyped()
	p.clickText(".modal.is-open .modal-actions button", "Cancel")
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	var asked bool
	p.eval(`[...document.querySelectorAll(".discard-prompt")].some((d) => !d.hidden)`, &asked)
	if asked {
		t.Error("Cancel on a form with typed input asked to discard; it should close at once")
	}

	openTyped()
	p.run(chromedp.KeyEvent(kb.Escape))
	p.waitFor(`document.querySelector(".modal.is-open .discard-prompt:not([hidden])") !== null`)
	var stillOpen bool
	p.eval(`document.querySelector(".modal.is-open") !== null && document.querySelector(".modal.is-open #f-displayName").value === "Typed folder"`, &stillOpen)
	if !stillOpen {
		t.Error("Escape on a form with typed input closed it or lost the input instead of asking")
	}
	if got := p.posts("/api/resources/hierarchy"); len(got) != 0 {
		t.Errorf("cancelling sent %v", got)
	}
}
