//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// TestStorageHMACKeySecretShownOnce (#792), in the storage shard. The Cloud
// Storage Settings page names the project's service account, and it is a
// page of the Cloud Storage product beside the bucket browser. Cancel on
// Create key closes the form at once, with no discard prompt, and sends
// nothing. Create key then shows the new key's secret once, in a dialog with
// a Copy button and the note that it will not be shown again, which a click
// on the backdrop does not close. Once Done closes it the secret is nowhere
// in the page (the table, the Activity panel, the notifications) or the
// browser's storage, and after a reload it is still nowhere while the key
// is listed ACTIVE, its menu offering Deactivate and no Delete.
func TestStorageHMACKeySecretShownOnce(t *testing.T) {
	needService(t, "storage-settings")
	p := open(t)
	project := uniqueProject(t)
	sa := "browser-hmac@" + project + ".iam.gserviceaccount.com"
	t.Cleanup(func() {
		_, body := consoleDo(t, http.MethodGet, "/api/resources/storage-settings?project="+project, "")
		var l struct {
			Items []struct{ Name, Status string }
		}
		_ = json.Unmarshal([]byte(body), &l)
		for _, k := range l.Items {
			act := func(a string) {
				consoleDo(t, http.MethodPost, "/api/actions/storage-settings?project="+project,
					fmt.Sprintf(`{"Name":%q,"Action":%q}`, k.Name, a))
			}
			if k.Status == "ACTIVE" {
				act("deactivate")
			}
			act("delete")
		}
	})
	creates := func() int { return len(p.sent(http.MethodPost, "/api/resources/storage-settings")) }
	// holds reports whether the page or the browser's storage holds s.
	holds := func(s string) bool {
		var found bool
		p.eval(fmt.Sprintf(`(() => { const s = %q;
			const stores = [localStorage, sessionStorage].flatMap((st) =>
				Object.keys(st).map((k) => k + "=" + st.getItem(k)));
			return document.documentElement.outerHTML.includes(s) || stores.some((v) => v.includes(s)) ||
				location.href.includes(s); })()`, s), &found)
		return found
	}

	p.navigate("/storage/settings?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view .listing-summary dd")].some((n) => n.textContent.endsWith("@gs-project-accounts.iam.gserviceaccount.com"))`)
	var pages string
	p.eval(`[...document.querySelectorAll("#product-nav a")].map((a) => a.textContent).join(" | ")`, &pages)
	if pages != "Cloud Storage | Settings" {
		t.Errorf("the Cloud Storage product's pages read %q", pages)
	}

	// Cancel discards what was typed without asking (#783).
	p.clickText("#view button", "Create key")
	p.waitFor(`document.querySelector(".modal.is-open #f-serviceAccountEmail") !== null`)
	p.run(chromedp.SendKeys(`.modal #f-serviceAccountEmail`, sa, chromedp.ByQuery))
	p.clickText(".modal .modal-actions button", "Cancel")
	p.waitFor(`document.querySelector(".modal") === null`)
	if n := creates(); n != 0 {
		t.Fatalf("Cancel sent the create: %d requests", n)
	}

	p.clickText("#view button", "Create key")
	p.waitFor(`document.querySelector(".modal.is-open #f-serviceAccountEmail") !== null`)
	p.run(chromedp.SendKeys(`.modal #f-serviceAccountEmail`, sa, chromedp.ByQuery))
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal.is-open #one-time-value") !== null`)
	var shown struct {
		Secret, Note, Title string
		Copy                bool
		Props               string
	}
	p.eval(`({
		secret: document.querySelector("#one-time-value").textContent,
		note: document.querySelector("#one-time .one-time-note").textContent,
		title: document.querySelector("#one-time-title").textContent,
		copy: [...document.querySelectorAll("#one-time .modal-actions button")].some((b) => b.textContent === "Copy"),
		props: [...document.querySelectorAll("#one-time dt, #one-time dd")].map((n) => n.textContent).join("|"),
	})`, &shown)
	if len(shown.Secret) != 40 || !shown.Copy || shown.Title != "HMAC key created" ||
		!strings.Contains(shown.Note, "won't see this secret again") || !strings.Contains(shown.Props, "Service account|"+sa) {
		t.Fatalf("the create dialog shows %+v; want a 40-character secret, a Copy button and the warning", shown)
	}
	if n := creates(); n != 1 {
		t.Errorf("the create was sent %d times", n)
	}

	// A stray click on the backdrop keeps it open; Done closes it.
	p.eval(`(() => { document.querySelector(".modal.is-open").click(); return true; })()`, nil)
	p.waitFor(`document.querySelector(".modal.is-open #one-time-value") !== null`)
	p.clickText("#one-time .modal-actions button", "Done")
	p.waitFor(`document.querySelector(".modal") === null`)

	var accessID string
	p.waitFor(`document.querySelector("#view tbody tr td") !== null`)
	p.eval(`[...document.querySelectorAll("#view tbody tr")].map((r) => r.getAttribute("aria-label") || "").find((l) => l.startsWith("Inspect GOOG")) || ""`, &accessID)
	accessID = strings.TrimPrefix(accessID, "Inspect ")
	if len(accessID) != 61 {
		t.Fatalf("the table lists no key after the create (row %q)", accessID)
	}
	if holds(shown.Secret) {
		t.Errorf("after the dialog closed the page or the browser's storage still holds the secret")
	}

	// Reloaded, the key is listed and the secret is still nowhere.
	p.navigate("/storage/settings?project=" + project)
	row := fmt.Sprintf(`tr[aria-label=%q]`, "Inspect "+accessID)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, row))
	if holds(shown.Secret) {
		t.Errorf("after a reload the page holds the secret")
	}
	var status string
	p.eval(fmt.Sprintf(`document.querySelector(%q).textContent`, row), &status)
	if !strings.Contains(status, "ACTIVE") {
		t.Errorf("the key's row reads %q; want ACTIVE", status)
	}
	p.run(chromedp.Click(fmt.Sprintf(`button[aria-label=%q]`, "Actions for "+accessID), chromedp.ByQuery))
	var items []string
	p.eval(`[...document.querySelectorAll('.overflow-menu:not([hidden]) [role="menuitem"]')].map((b) => b.textContent)`, &items)
	if !contains(items, "Deactivate") || contains(items, "Delete") {
		t.Errorf("an ACTIVE key's menu offers %v; want Deactivate and no Delete", items)
	}
}
