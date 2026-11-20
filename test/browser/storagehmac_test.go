//go:build browser

package browser

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/storage"
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
// browser's storage, and after a reload it is still nowhere. Interoperability
// lists the service account under "Access keys for service accounts"
// (#2047), and its page lists the key with its state as a button: Active,
// clicked, asks and ends in Deactivate; Inactive, clicked, activates at
// once; Delete, on an inactive key only, asks for the access ID's first 10
// characters. The official client reads each change. The tab passes the
// WCAG 2.1 AA audit.
func TestStorageHMACKeySecretShownOnce(t *testing.T) {
	needService(t, "storage-settings")
	p := open(t)
	project := uniqueProject(t)
	sa := "browser-hmac@" + project + ".iam.gserviceaccount.com"
	gcs, _ := storageSDK(t)
	ctx := context.Background()
	t.Cleanup(func() {
		it := gcs.ListHMACKeys(context.Background(), project)
		for {
			k, err := it.Next()
			if err != nil {
				return
			}
			h := gcs.HMACKeyHandle(project, k.AccessID)
			if k.State == storage.Active {
				_, _ = h.Update(context.Background(), storage.HMACKeyAttrsToUpdate{State: storage.Inactive})
			}
			_ = h.Delete(context.Background())
		}
	})
	state := func(id string) storage.HMACState {
		t.Helper()
		k, err := gcs.HMACKeyHandle(project, id).Get(ctx)
		if err != nil {
			t.Fatalf("the official client's Get(%s): %v", id, err)
		}
		return k.State
	}
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
	if pages != "Buckets | Observability | Storage Intelligence | Batch operations | Inventory reports | Insights datasets | Transfer data | Settings" {
		t.Errorf("the Cloud Storage product's pages read %q", pages)
	}

	// The keys are on Interoperability, as on Google's Settings page (#1843).
	p.run(chromedp.Click(chromedp.CSS(`#tab-interoperability`)))
	p.waitFor(`document.querySelector("#tab-interoperability.is-selected") !== null`)

	// Cancel discards what was typed without asking (#783).
	p.clickText("#view button", "Create a key for a service account")
	p.waitFor(`document.querySelector(".modal.is-open #f-serviceAccountEmail") !== null`)
	p.run(chromedp.SendKeys(chromedp.CSS(`.modal #f-serviceAccountEmail`), sa))
	p.clickText(".modal .modal-actions button", "Cancel")
	p.waitFor(`document.querySelector(".modal") === null`)
	if n := creates(); n != 0 {
		t.Fatalf("Cancel sent the create: %d requests", n)
	}

	p.clickText("#view button", "Create a key for a service account")
	p.waitFor(`document.querySelector(".modal.is-open #f-serviceAccountEmail") !== null`)
	p.run(chromedp.SendKeys(chromedp.CSS(`.modal #f-serviceAccountEmail`), sa))
	p.clickText(".modal.is-open button", "Create key")
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

	// The access ID, from the dialog's properties.
	accessID := ""
	if parts := strings.Split(shown.Props, "|"); len(parts) > 1 && parts[0] == "Access ID" {
		accessID = parts[1]
	}
	if len(accessID) != 61 {
		t.Fatalf("the create dialog names no access ID: %q", shown.Props)
	}
	// Access keys for service accounts lists the service account (#2047).
	saRow := fmt.Sprintf(`tr[aria-label=%q]`, "Inspect "+sa)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, saRow))
	if holds(shown.Secret) {
		t.Errorf("after the dialog closed the page or the browser's storage still holds the secret")
	}

	// Reloaded, the service account is listed and the secret is nowhere.
	p.navigate("/storage/settings?project=" + project + "&tab=interoperability")
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, saRow))
	if holds(shown.Secret) {
		t.Errorf("after a reload the page holds the secret")
	}
	var heading string
	p.eval(`document.querySelector("#view .table-title")?.textContent || ""`, &heading)
	if heading != "Access keys for service accounts" {
		t.Errorf("the Interoperability table is headed %q", heading)
	}

	// The service account's page lists its key, its state a button.
	p.clickText("#view tbody a", sa)
	status := func(label string) string { return fmt.Sprintf(`button[aria-label=%q]`, label+" "+accessID) }
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, status("Active")))
	if holds(shown.Secret) {
		t.Errorf("the service account's page holds the secret")
	}
	if p.has(status("Delete")) {
		t.Error("an ACTIVE key offers Delete")
	}
	// To Inactive: asked first, ending in Deactivate.
	p.run(chromedp.Click(chromedp.CSS(status("Active"))))
	p.waitFor(`document.querySelector(".modal.is-open form[data-confirm=confirm]") !== null`)
	p.clickText(".modal.is-open button", "Deactivate")
	p.waitFor(`document.querySelector(".modal") === null`)
	eventually(t, "the key to be INACTIVE", func() bool { return state(accessID) == storage.Inactive })
	// To Active: no additional steps.
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, status("Inactive")))
	p.run(chromedp.Click(chromedp.CSS(status("Inactive"))))
	eventually(t, "the key to be ACTIVE", func() bool { return state(accessID) == storage.Active })
	if p.has(".modal.is-open") {
		t.Error("activating a key asked first")
	}
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, status("Active")))
	p.run(chromedp.Click(chromedp.CSS(status("Active"))))
	p.clickText(".modal.is-open button", "Deactivate")
	eventually(t, "the key to be INACTIVE", func() bool { return state(accessID) == storage.Inactive })
	// Delete: the access ID's first 10 characters typed.
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, status("Delete")))
	p.run(chromedp.Click(chromedp.CSS(status("Delete"))))
	p.waitFor(`document.querySelector(".modal.is-open #confirm-input") !== null`)
	var word string
	p.eval(`document.querySelector(".modal.is-open label[for=confirm-input] strong").textContent`, &word)
	if word != accessID[:10] {
		t.Errorf("Delete asks for %q typed; want the access ID's first 10 characters, %s", word, accessID[:10])
	}
	p.confirmByName(accessID[:10])
	eventually(t, "the key to be deleted", func() bool { return state(accessID) == storage.Deleted })

	// A key of the account's again, made with the official client, for the
	// audit of its page.
	if _, err := gcs.CreateHMACKey(ctx, project, sa); err != nil {
		t.Fatalf("CreateHMACKey: %v", err)
	}
	auditScreens(t, []a11yPage{
		{"Interoperability tab", "/storage/settings?project=" + project + "&tab=interoperability", settled + " && " +
			fmt.Sprintf(`document.querySelector(%q) !== null`, saRow), ""},
		{"service account HMAC keys", settingsAccountPage(sa, project), settled + ` && document.querySelector("#view tbody tr button") !== null`, ""},
	})
}
