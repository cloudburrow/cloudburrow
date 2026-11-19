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

// TestPubSubSchemaCreateShowsTheValidationError (#788, #1847), in the
// storage shard, whose instance serves Pub/Sub. On the Schemas screen,
// Create schema opens Google's page, one page with no numbered steps;
// Cancel after typing goes back to the list with nothing sent (#783). On
// the page, Validate definition and Test message check the form with
// ValidateSchema and ValidateMessage, creating nothing: a definition that is
// not Avro is refused under the buttons, and a valid one passes; Test
// message's window refuses a message the definition does not fit and
// accepts one it does. Create with an invalid definition is sent once and
// refused on the page with ValidateSchema's own message; corrected, it
// creates the schema, which the console API then lists. Its row's Delete
// asks for the schema's name back: the short name is refused with nothing
// sent, and the full name sends one DELETE.
func TestPubSubSchemaCreateShowsTheValidationError(t *testing.T) {
	needService(t, "pubsub-schemas")
	p := open(t)
	project := uniqueProject(t)
	schema := "projects/" + project + "/schemas/browser-schema"
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/pubsub-schemas?project="+project+"&name="+url.QueryEscape(schema), "")
	})
	const created = "/api/resources/pubsub-schemas"
	const checked = "/api/resources/pubsub-schemas/check"
	// The create's posts, apart from the checks', whose path it prefixes.
	creates := func() []string {
		var out []string
		for _, u := range p.posts(created) {
			if !strings.Contains(u, created+"/check") {
				out = append(out, u)
			}
		}
		return out
	}
	setDefinition := func(v string) {
		p.eval(fmt.Sprintf(`(() => { const f = document.querySelector("#view #f-definition");
			f.value = %q; f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, v), nil)
	}
	openCreate := func() {
		p.waitFor(`[...document.querySelectorAll("#view .state button.primary")].some((b) => b.textContent === "Create schema")`)
		p.clickText(`#view .state button.primary`, "Create schema")
		p.waitFor(`location.pathname === "/pubsub/schemas/create" && document.querySelector("#view form.create-page #f-definition") !== null`)
	}

	p.navigate("/pubsub/schemas?project=" + project)

	// Cancel goes back with nothing sent.
	openCreate()
	var steps int
	p.eval(`document.querySelectorAll("#view .create-step").length`, &steps)
	if steps != 0 {
		t.Errorf("Create schema has %d numbered steps; Google's is one page", steps)
	}
	p.run(chromedp.SendKeys(chromedp.CSS(`#view #f-name`), "discarded-schema"))
	setDefinition(`{"type":"record"`)
	p.clickText("#view .form-actions a", "Cancel")
	p.waitFor(`location.pathname === "/pubsub/schemas"`)
	if posts := creates(); len(posts) != 0 {
		t.Fatalf("Cancel sent the schema: %v", posts)
	}

	openCreate()
	p.run(chromedp.SendKeys(chromedp.CSS(`#view #f-name`), "browser-schema"))
	verdict := func() (string, bool) {
		var v struct {
			Text  string
			Error bool
		}
		p.eval(`(() => { const r = document.querySelector("#view #create-check-result");
			return { Text: r.textContent, Error: r.classList.contains("is-error") }; })()`, &v)
		return v.Text, v.Error
	}
	waitVerdict := func() {
		p.waitFor(`!document.querySelector("#view #create-check-result").hidden &&
			!document.querySelector("#view #create-check-validate-definition").disabled`)
	}

	// Validate definition: refused, then passed; nothing is created.
	setDefinition(`{"type":"record"`)
	p.run(chromedp.Click(chromedp.CSS(`#view #create-check-validate-definition`)))
	waitVerdict()
	if text, isErr := verdict(); !isErr || !strings.Contains(text, "Could not parse schema definition") {
		t.Errorf("Validate definition of a broken definition said %q (error %v), want ValidateSchema's refusal", text, isErr)
	}
	p.forgive()
	good := `{"type":"record","name":"Burrow","fields":[{"name":"name","type":"string"}]}`
	setDefinition(good)
	p.run(chromedp.Click(chromedp.CSS(`#view #create-check-validate-definition`)))
	p.waitFor(`!document.querySelector("#view #create-check-result").classList.contains("is-error") &&
		!document.querySelector("#view #create-check-result").hidden`)
	if text, _ := verdict(); !strings.Contains(text, "valid") {
		t.Errorf("Validate definition of a valid definition said %q", text)
	}

	// Test message: a window with Message encoding and Message body, and Test.
	p.run(chromedp.Click(chromedp.CSS(`#view #create-check-test-message`)))
	p.waitFor(`document.querySelector(".modal.is-open #f-testMessage") !== null`)
	var window struct {
		Title, Submit string
		Encodings     []string
	}
	p.eval(`({ Title: document.querySelector(".modal.is-open h2").textContent,
		Submit: document.querySelector(".modal.is-open button[type=submit]").textContent,
		Encodings: [...document.querySelectorAll(".modal.is-open fieldset label")].map((l) => l.textContent) })`, &window)
	if window.Title != "Test message" || window.Submit != "Test" || strings.Join(window.Encodings, ",") != "JSON,Binary" {
		t.Errorf("the Test message window reads %+v", window)
	}
	setMessage := func(v string) {
		p.eval(fmt.Sprintf(`(() => { const f = document.querySelector(".modal.is-open #f-testMessage");
			f.value = %q; f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, v), nil)
	}
	testResult := func() (string, bool) {
		p.waitFor(`!document.querySelector(".modal.is-open .create-check-result").hidden &&
			!document.querySelector(".modal.is-open button[type=submit]").disabled`)
		var v struct {
			Text  string
			Error bool
		}
		p.eval(`(() => { const r = document.querySelector(".modal.is-open .create-check-result");
			return { Text: r.textContent, Error: r.classList.contains("is-error") }; })()`, &v)
		return v.Text, v.Error
	}
	setMessage(`{"nope": 1}`)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	if text, isErr := testResult(); !isErr || text == "" {
		t.Errorf("a message the schema does not fit was answered %q (error %v)", text, isErr)
	}
	p.forgive()
	setMessage(`{"name": "burrow"}`)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`!document.querySelector(".modal.is-open .create-check-result").classList.contains("is-error")`)
	if text, _ := testResult(); !strings.Contains(text, "valid") {
		t.Errorf("a message the schema fits was answered %q", text)
	}
	p.clickText(".modal.is-open .modal-actions button", "Close")
	p.waitFor(`document.querySelector(".modal") === null`)
	if posts := p.posts(checked); len(posts) != 4 {
		t.Errorf("the checks were posted %d times, want 4: %v", len(posts), posts)
	}
	if posts := creates(); len(posts) != 0 {
		t.Fatalf("a check created the schema: %v", posts)
	}

	// Create with an invalid definition: ValidateSchema's refusal, on the page.
	setDefinition(`{"type":"record"`)
	p.run(chromedp.Click(chromedp.CSS(`#view form.create-page button[type="submit"]`)))
	p.waitFor(`!document.querySelector("#view form.create-page > .form-error").hidden`)
	var refusal string
	p.eval(`document.querySelector("#view form.create-page > .form-error").textContent`, &refusal)
	if want := "InvalidArgument: Could not parse schema definition"; refusal != want {
		t.Errorf("an invalid definition was refused with %q, want ValidateSchema's %q", refusal, want)
	}
	if posts := creates(); len(posts) != 1 {
		t.Fatalf("the invalid definition was posted %d times, want once: %v", len(posts), posts)
	}
	// The refusal is on the page, not a browser error: it is forgiven.
	p.forgive()
	if code, body := consoleDo(t, http.MethodGet, "/api/resources/pubsub-schemas?project="+project, ""); code != http.StatusOK ||
		strings.Contains(body, `"`+schema+`"`) {
		t.Fatalf("a definition ValidateSchema refused was created: %d %s", code, body)
	}

	// The same page, corrected: the schema is created and listed.
	setDefinition(good)
	p.run(chromedp.Click(chromedp.CSS(`#view form.create-page button[type="submit"]`)))
	row := fmt.Sprintf(`tr[aria-label=%q]`, "Inspect "+schema)
	p.waitFor(fmt.Sprintf(`location.pathname === "/pubsub/schemas" && document.querySelector(%q) !== null`, row))
	if posts := creates(); len(posts) != 2 {
		t.Errorf("the create was posted %d times in all, want twice: %v", len(posts), posts)
	}
	if code, body := consoleDo(t, http.MethodGet, "/api/resources/pubsub-schemas?project="+project, ""); code != http.StatusOK ||
		!strings.Contains(body, `"`+schema+`"`) || !strings.Contains(body, `"Schema type":"Avro"`) {
		t.Fatalf("the console API does not list the Avro schema created in the browser: %d %s", code, body)
	}

	// Delete from its row, confirmed by the schema's name.
	p.run(chromedp.Click(chromedp.CSS(fmt.Sprintf(`button[aria-label=%q]`, "Actions for "+schema))))
	p.clickText(`.overflow-menu:not([hidden]) [role="menuitem"]`, "Delete")
	p.waitFor(`document.querySelector(".modal #confirm-input") !== null && document.activeElement === document.querySelector(".modal #confirm-input")`)
	p.run(chromedp.SendKeys(chromedp.CSS(`.modal #confirm-input`), "browser-schema"), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`!document.querySelector(".modal .form-error").hidden`)
	if sent := p.sent(http.MethodDelete, created); len(sent) != 0 {
		t.Fatalf("a delete confirmed with the short name was sent: %v", sent)
	}
	p.eval(`(() => { const f = document.querySelector(".modal #confirm-input"); f.value = ""; return true; })()`, nil)
	p.run(chromedp.SendKeys(chromedp.CSS(`.modal #confirm-input`), schema), chromedp.KeyEvent(kb.Enter))
	p.waitFor(fmt.Sprintf(`document.querySelector(".modal") === null && document.querySelector(%q) === null`, row))
	if sent := p.sent(http.MethodDelete, created); len(sent) != 1 {
		t.Errorf("the delete was sent %d times, want once: %v", len(sent), sent)
	}
	if code, body := consoleDo(t, http.MethodGet, "/api/resources/pubsub-schemas?project="+project, ""); code != http.StatusOK ||
		strings.Contains(body, `"`+schema+`"`) {
		t.Errorf("the console API still lists the schema deleted in the browser: %d %s", code, body)
	}
}
