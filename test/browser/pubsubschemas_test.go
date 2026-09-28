//go:build browser

package browser

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestPubSubSchemaCreateShowsTheValidationError (#788), in the storage
// shard, whose instance serves Pub/Sub. On the Schemas screen, Create schema
// asks for an ID and a definition; Cancel after typing both closes it at once
// with nothing sent (#783). A definition that is not Avro is sent once and
// refused on the open form with ValidateSchema's own message; the same form,
// given a valid definition, creates the schema, which the console API then
// lists. Its row's Delete asks for the schema's name back: the short name is
// refused with nothing sent, and the full name sends one DELETE.
func TestPubSubSchemaCreateShowsTheValidationError(t *testing.T) {
	needService(t, "pubsub-schemas")
	p := open(t)
	project := uniqueProject(t)
	schema := "projects/" + project + "/schemas/browser-schema"
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/pubsub-schemas?project="+project+"&name="+url.QueryEscape(schema), "")
	})
	const created = "/api/resources/pubsub-schemas"
	setDefinition := func(v string) {
		p.eval(fmt.Sprintf(`(() => { const f = document.querySelector(".modal #f-definition");
			f.value = %q; f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, v), nil)
	}
	openCreate := func() {
		p.waitFor(`document.querySelector(".modal") === null`)
		p.waitFor(`[...document.querySelectorAll("#view .state button.primary")].some((b) => b.textContent === "Create schema")`)
		p.clickText(`#view .state button.primary`, "Create schema")
		p.run(chromedp.WaitVisible(`.modal #f-definition`, chromedp.ByQuery))
	}

	p.navigate("/pubsub/schemas?project=" + project)

	// Cancel discards without asking.
	openCreate()
	p.run(chromedp.SendKeys(`.modal #f-name`, "discarded-schema", chromedp.ByQuery))
	setDefinition(`{"type":"record"`)
	p.clickText(".modal .modal-actions button", "Cancel")
	p.waitFor(`document.querySelector(".modal") === null`)
	if posts := p.posts(created); len(posts) != 0 {
		t.Fatalf("Cancel sent the schema: %v", posts)
	}

	// An invalid definition: ValidateSchema's refusal, on the open form.
	openCreate()
	p.run(chromedp.SendKeys(`.modal #f-name`, "browser-schema", chromedp.ByQuery))
	setDefinition(`{"type":"record"`)
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal .form-error") !== null && !document.querySelector(".modal .form-error").hidden`)
	var refusal string
	p.eval(`document.querySelector(".modal .form-error").textContent`, &refusal)
	if want := "InvalidArgument: Could not parse schema definition"; refusal != want {
		t.Errorf("an invalid definition was refused with %q, want ValidateSchema's %q", refusal, want)
	}
	if posts := p.posts(created); len(posts) != 1 {
		t.Fatalf("the invalid definition was posted %d times, want once: %v", len(posts), posts)
	}
	// The refusal is on the page, not a browser error: it is forgiven.
	p.forgive()
	if code, body := consoleDo(t, http.MethodGet, "/api/resources/pubsub-schemas?project="+project, ""); code != http.StatusOK ||
		strings.Contains(body, `"`+schema+`"`) {
		t.Fatalf("a definition ValidateSchema refused was created: %d %s", code, body)
	}

	// The same form, corrected: the schema is created and listed.
	setDefinition(`{"type":"record","name":"Burrow","fields":[{"name":"name","type":"string"}]}`)
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	row := fmt.Sprintf(`tr[aria-label=%q]`, "Inspect "+schema)
	p.waitFor(fmt.Sprintf(`document.querySelector(".modal") === null && document.querySelector(%q) !== null`, row))
	if posts := p.posts(created); len(posts) != 2 {
		t.Errorf("the create was posted %d times in all, want twice: %v", len(posts), posts)
	}
	if code, body := consoleDo(t, http.MethodGet, "/api/resources/pubsub-schemas?project="+project, ""); code != http.StatusOK ||
		!strings.Contains(body, `"`+schema+`"`) || !strings.Contains(body, `"AVRO"`) {
		t.Fatalf("the console API does not list the Avro schema created in the browser: %d %s", code, body)
	}

	// Delete from its row, confirmed by the schema's name.
	p.run(chromedp.Click(fmt.Sprintf(`button[aria-label=%q]`, "Actions for "+schema), chromedp.ByQuery))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Delete")
	p.waitFor(`document.querySelector(".modal #confirm-input") !== null && document.activeElement === document.querySelector(".modal #confirm-input")`)
	p.run(chromedp.SendKeys(`.modal #confirm-input`, "browser-schema", chromedp.ByQuery), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`!document.querySelector(".modal .form-error").hidden`)
	if sent := p.sent(http.MethodDelete, created); len(sent) != 0 {
		t.Fatalf("a delete confirmed with the short name was sent: %v", sent)
	}
	p.eval(`(() => { const f = document.querySelector(".modal #confirm-input"); f.value = ""; return true; })()`, nil)
	p.run(chromedp.SendKeys(`.modal #confirm-input`, schema, chromedp.ByQuery), chromedp.KeyEvent(kb.Enter))
	p.waitFor(fmt.Sprintf(`document.querySelector(".modal") === null && document.querySelector(%q) === null`, row))
	if sent := p.sent(http.MethodDelete, created); len(sent) != 1 {
		t.Errorf("the delete was sent %d times, want once: %v", len(sent), sent)
	}
	if code, body := consoleDo(t, http.MethodGet, "/api/resources/pubsub-schemas?project="+project, ""); code != http.StatusOK ||
		strings.Contains(body, `"`+schema+`"`) {
		t.Errorf("the console API still lists the schema deleted in the browser: %d %s", code, body)
	}
}
