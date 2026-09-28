//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/chromedp/chromedp"
)

// TestTasksEditQueueThroughTheForm (#784), in the storage shard: a queue's
// page has Edit queue, whose dialog is prefilled from the queue with the
// burst size shown disabled. Cancel after a change closes it at once, with no
// question asked and nothing sent. A max attempts of -2 is sent and refused on
// the form with UpdateQueue's own message; 7 is saved with one PATCH, the
// dialog closes, and the console API's page for the queue reads 7.
func TestTasksEditQueueThroughTheForm(t *testing.T) {
	needService(t, "tasks")
	p := open(t)
	project := uniqueProject(t)
	name := "projects/" + project + "/locations/us-central1/queues/browser-edit"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/tasks?project="+project, `{"name":"browser-edit","location":"us-central1"}`); code != http.StatusOK {
		t.Fatalf("create a queue through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/tasks?project="+project+"&name="+url.QueryEscape(name), "")
	})

	p.navigate("/tasks/queues?project=" + project)
	p.clickText("#view tbody a", name)
	openEdit := func() {
		p.clickText("#view .page-actions button", "Edit queue")
		p.waitFor(`document.querySelector(".modal.is-open #f-maxAttempts") !== null`)
	}
	setAttempts := func(v string) {
		p.eval(fmt.Sprintf(`(() => { const f = document.querySelector(".modal #f-maxAttempts");
			f.value = %q; f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, v), nil)
	}
	openEdit()

	var form struct {
		Attempts, Rate, Burst string
		BurstDisabled         bool
	}
	p.eval(`(() => { const q = (s) => document.querySelector(".modal " + s);
		return { Attempts: q("#f-maxAttempts").value, Rate: q("#f-maxDispatchesPerSecond").value,
		         Burst: q("#f-maxBurstSize").value, BurstDisabled: q("#f-maxBurstSize").disabled }; })()`, &form)
	if form.Attempts != "100" || form.Rate != "500" || form.Burst == "" || !form.BurstDisabled {
		t.Errorf("the edit dialog is prefilled with %+v; want max attempts 100, rate 500 and the burst size disabled", form)
	}

	// Cancel discards a change without asking (#783's rule).
	setAttempts("9")
	p.clickText(".modal .modal-actions button", "Cancel")
	p.waitFor(`document.querySelector(".modal") === null`)
	if sent := p.sent(http.MethodPatch, "/api/resources/tasks"); len(sent) != 0 {
		t.Fatalf("Cancel sent the change: %v", sent)
	}

	openEdit()
	setAttempts("-2")
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`!document.querySelector(".modal .form-error").hidden`)
	var refusal string
	p.eval(`document.querySelector(".modal .form-error").textContent`, &refusal)
	if want := "InvalidArgument: retry_config.max_attempts -2 must be -1 (unlimited) or greater"; refusal != want {
		t.Errorf("max attempts -2 was refused on the form with %q, want %q", refusal, want)
	}
	// The refusal is on the page, not a browser error: it is forgiven.
	p.forgive()

	setAttempts("7")
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	if sent := p.sent(http.MethodPatch, "/api/resources/tasks"); len(sent) != 2 {
		t.Errorf("the refused and the saved edit sent %d PATCHes, want 2: %v", len(sent), sent)
	}

	code, body := consoleDo(t, http.MethodGet, "/api/detail/tasks?project="+project+"&name="+url.QueryEscape(name), "")
	var detail struct {
		Edit struct {
			Fields []struct{ Name, Default string }
		}
	}
	if err := json.Unmarshal([]byte(body), &detail); code != http.StatusOK || err != nil {
		t.Fatalf("read the queue through the console API = %d (%v): %s", code, err, body)
	}
	for _, f := range detail.Edit.Fields {
		if f.Name == "maxAttempts" && f.Default != "7" {
			t.Errorf("the console API reads max attempts %q after the browser saved 7", f.Default)
		}
	}
}
