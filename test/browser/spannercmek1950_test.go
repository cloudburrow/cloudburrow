//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestSpannerCMEKThroughThePage (#1950), in a shard whose instance serves
// Spanner and Cloud KMS: an instance's Create backup has Google's
// Encryption choice (Use database encryption, Google-managed encryption
// key, Cloud KMS key), the key field shown only for Cloud KMS key; a
// backup made there with a key of the instance's Cloud KMS opens on a page
// whose properties name Cloud KMS key and the key's first version; and a
// database created with that key opens on a page whose summary says Cloud
// KMS key and whose properties name the key and its version in use (#1972).
func TestSpannerCMEKThroughThePage(t *testing.T) {
	needService(t, "spanner")
	needService(t, "kms")
	p := open(t)
	project := uniqueProject(t)
	q := "?project=" + project
	ring := "projects/" + project + "/locations/us-central1/keyRings/browser-spanner"
	key := ring + "/cryptoKeys/k"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/kms"+q, `{"keyRingId":"browser-spanner","location":"us-central1"}`); code != http.StatusOK {
		t.Fatalf("create a key ring through the console API = %d: %s", code, body)
	}
	create, _ := json.Marshal(map[string]any{"Path": []string{ring}, "Action": "createkey", "Values": map[string]string{"cryptoKeyId": "k"}})
	if code, body := consoleDo(t, http.MethodPost, "/api/actions/kms"+q, string(create)); code != http.StatusOK {
		t.Fatalf("create a key through the console API = %d: %s", code, body)
	}
	inst := fmt.Sprintf("browser-cmek-%d", time.Now().UnixNano()%1e8)
	const db = "orders"
	body, _ := json.Marshal(map[string]string{"instance": inst, "database": db,
		"ddl": "CREATE TABLE Orders (Id INT64 NOT NULL) PRIMARY KEY (Id)"})
	if code, out := consoleDo(t, http.MethodPost, "/api/resources/spanner"+q, string(body)); code != http.StatusOK {
		t.Fatalf("create a Spanner database through the console API = %d: %s", code, out)
	}
	t.Cleanup(func() {
		b, _ := json.Marshal(map[string]any{"Path": []string{inst, "Backups", "keyed"}, "Action": "deletebackup"})
		consoleDo(t, http.MethodPost, "/api/actions/spanner"+q, string(b))
		c, _ := json.Marshal(map[string]any{"Path": []string{inst}, "Action": "dropinstance"})
		consoleDo(t, http.MethodPost, "/api/actions/spanner"+q, string(c))
	})

	p.navigate("/spanner" + q)
	p.clickText("#view tbody a", inst)
	p.waitFor(`document.querySelector(".modal") === null`)
	p.waitFor(`document.querySelector("#tab-backups") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#tab-backups`)))
	p.clickText("#view #section-actions-backups button", "Create backup")
	p.waitFor(`document.querySelector(".modal.is-open #f-backup") !== null`)
	var form struct {
		Options  []string
		KeyShown bool
	}
	p.eval(`(() => { const k = document.querySelector(".modal.is-open #f-kmsKey");
		return { Options: [...document.querySelectorAll(".modal.is-open input[type=radio][name='f-encryption-choice']")].map((r) => r.value),
			KeyShown: !!k && !k.closest("[hidden]") } })()`, &form)
	if len(form.Options) != 3 || form.Options[0] != "Use database encryption" || form.Options[2] != "Cloud KMS key" || form.KeyShown {
		t.Fatalf("Create backup's encryption = %+v; want Google's three choices and no key field until Cloud KMS key", form)
	}
	p.run(chromedp.SendKeys(chromedp.CSS(`.modal.is-open #f-backup`), "keyed"))
	p.eval(`(() => { const r = [...document.querySelectorAll(".modal.is-open input[type=radio][name='f-encryption-choice']")].find((r) => r.value === "Cloud KMS key");
		r.click(); return true })()`, nil)
	p.waitFor(`(() => { const k = document.querySelector(".modal.is-open #f-kmsKey"); return !!k && !k.closest("[hidden]") })()`)
	p.run(chromedp.SendKeys(chromedp.CSS(`.modal.is-open #f-kmsKey`), key))
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)

	p.waitFor(`document.querySelector("#tab-backups") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#tab-backups`)))
	p.waitFor(`[...document.querySelectorAll("#view tbody a")].some((a) => a.textContent === "keyed")`)
	p.eval(`(() => { const a = [...document.querySelectorAll("#view tbody a")].find((a) => a.textContent === "keyed"); a.click(); return true })()`, nil)
	p.waitFor(`[...document.querySelectorAll("#view dd, #view td")].some((e) => e.textContent.includes("/backups/keyed"))`)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view dd, #view td")].some((e) => e.textContent.trim() === %q)`, key+"/cryptoKeyVersions/1"))
	p.waitFor(`[...document.querySelectorAll("#view dd, #view td")].some((e) => e.textContent.trim() === "Cloud KMS key")`)

	// The database page's encryption fields (#1972).
	const keyedDB = "keyeddb"
	mk, _ := json.Marshal(map[string]any{"Path": []string{inst}, "Action": "createdatabase",
		"Values": map[string]string{"database": keyedDB, "encryption": "Cloud KMS key", "kmsKey": key}})
	if code, out := consoleDo(t, http.MethodPost, "/api/actions/spanner"+q, string(mk)); code != http.StatusOK {
		t.Fatalf("create a customer-managed database through the console API = %d: %s", code, out)
	}
	p.navigate("/spanner" + q)
	p.clickText("#view tbody a", inst)
	p.clickText("#view tbody a", keyedDB)
	p.waitFor(`document.querySelector(".modal") === null`)
	p.waitFor(`[...document.querySelectorAll("#view dd, #view td")].some((e) => e.textContent.trim() === "Cloud KMS key")`)
	p.eval(`(() => { const t = document.querySelector("#tab-properties"); if (t) t.click(); return true })()`, nil)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view dd, #view td")].some((e) => e.textContent.trim() === %q)`, key))
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view dd, #view td")].some((e) => e.textContent.trim() === %q)`, key+"/cryptoKeyVersions/1"))
}
