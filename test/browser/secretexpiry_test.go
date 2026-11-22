//go:build browser

package browser

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestSecretManagerExpiryThroughTheForms (#1225), in the run shard: Create
// secret, on its own page, offers an expiration date and time under Set
// expiration date (#1850), and no TTL, as Google's console (#2142); a
// secret created expiring in 48 hours shows its expiry on its page, and
// Edit secret, prefilled with that date, clears it, after which the page
// says Never.
func TestSecretManagerExpiryThroughTheForms(t *testing.T) {
	needService(t, "secrets")
	p := open(t)
	project := uniqueProject(t)
	id := "browser-expiry"
	name := "projects/" + project + "/secrets/" + id
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/secrets?project="+project+"&name="+id, "")
	})
	p.navigate("/secrets/create?project=" + project)
	// Google's console sets an expiration date and time only; a TTL is
	// gcloud's and REST's (#2142).
	p.waitFor(`document.querySelector("#view #f-expireTime") !== null`)
	var ttl bool
	p.eval(`document.querySelector("#view #f-ttl") !== null`, &ttl)
	if ttl {
		t.Error("Create secret offers a TTL; Google's page sets an expiration date only")
	}
	setField(p, "#view", "f-secretId", id)
	setField(p, "#view", "f-payload", "value")
	setField(p, "#view", "f-setExpiration", "true")
	var picked struct{ Local string }
	p.eval(`(() => {
		const f = document.querySelector("#view #f-expireTime");
		const d = new Date(Date.now() + 48 * 3600 * 1000); d.setSeconds(0, 0);
		const pad = (n) => String(n).padStart(2, "0");
		const local = d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate()) + "T" + pad(d.getHours()) + ":" + pad(d.getMinutes());
		f.value = local; f.dispatchEvent(new Event("input", { bubbles: true })); f.dispatchEvent(new Event("change", { bubbles: true }));
		return { Local: local };
	})()`, &picked)
	p.run(chromedp.Click(chromedp.CSS(`#view button[type="submit"]`)))
	p.waitFor(`location.pathname === "/secrets"`)

	got := editDefaults(t, "secrets", project, name)
	exp, err := time.Parse(time.RFC3339, got["expireTime"])
	if err != nil || time.Until(exp) < 47*time.Hour || time.Until(exp) > 49*time.Hour {
		t.Fatalf("an expiration date 48h ahead set in the browser reads expire time %q (%v)", got["expireTime"], err)
	}
	if got["Expiration time"] != got["expireTime"] {
		t.Errorf("the page shows expire time %q, the edit form %q", got["Expiration time"], got["expireTime"])
	}

	p.clickText("#view tbody a", name)
	// Edit secret is a page ending in Update secret (#1850).
	p.clickText("#view .page-actions button", "Edit secret")
	p.waitFor(`document.querySelector("#view form.create-page #f-expireTime") !== null`)
	var prefilled string
	p.eval(`document.querySelector("#view #f-expireTime").value`, &prefilled)
	if prefilled != picked.Local {
		t.Errorf("Edit secret is prefilled with expire time %q, want %q", prefilled, picked.Local)
	}
	setField(p, "#view", "f-expireTime", "")
	p.clickText(`#view form.create-page button[type="submit"]`, "Update secret")
	p.waitFor(`!location.search.includes("edit=1") && document.querySelector("#view form.create-page") === null`)
	if got := editDefaults(t, "secrets", project, name); got["Expiration time"] != "Never" || got["expireTime"] != "" {
		t.Errorf("after clearing in Edit secret the secret reads expire time %q, form %q", got["Expiration time"], got["expireTime"])
	}
}

// TestSecretManagerListFilterThroughTheBox (#1228): the Secrets list has a filter
// box in Secret Manager's syntax; applying a filter lists only the matching
// secrets and keeps it in the URL, a regular expression is refused under
// the box with the API's message, and Clear lists every secret again.
func TestSecretManagerListFilterThroughTheBox(t *testing.T) {
	needService(t, "secrets")
	p := open(t)
	project := uniqueProject(t)
	for _, s := range []struct{ id, env string }{{"filter-prod", "prod"}, {"filter-dev", "dev"}} {
		body := `{"secretId":"` + s.id + `","payload":"x","labels":"{\"env\":\"` + s.env + `\"}"}`
		if code, resp := consoleDo(t, http.MethodPost, "/api/resources/secrets?project="+project, body); code != http.StatusOK {
			t.Fatalf("create %s through the console API = %d: %s", s.id, code, resp)
		}
		id := s.id
		t.Cleanup(func() {
			consoleDo(t, http.MethodDelete, "/api/resources/secrets?project="+project+"&name="+id, "")
		})
	}
	rows := func() string {
		var names []string
		p.eval(`[...document.querySelectorAll("#view tbody a")].map((a) => a.textContent)`, &names)
		return strings.Join(names, " ")
	}
	p.navigate("/secrets?project=" + project)
	p.waitFor(`document.querySelectorAll("#view tbody a").length === 2`)
	setField(p, "#view", "server-filter", "labels.env=prod")
	p.run(chromedp.Click(chromedp.CSS(`#server-filter-apply`)))
	p.waitFor(`document.querySelectorAll("#view tbody a").length === 1`)
	if got := rows(); !strings.HasSuffix(got, "/secrets/filter-prod") {
		t.Errorf("labels.env=prod lists %q", got)
	}
	var search string
	p.eval(`location.search`, &search)
	if q, _ := url.ParseQuery(strings.TrimPrefix(search, "?")); q.Get("filter") != "labels.env=prod" {
		t.Errorf("the filter is not in the URL: %q", search)
	}

	setField(p, "#view", "server-filter", `name ~ "filter-.*"`)
	p.run(chromedp.Click(chromedp.CSS(`#server-filter-apply`)))
	p.waitFor(`document.querySelector("#server-filter-error") !== null`)
	var refusal string
	p.eval(`document.querySelector("#server-filter-error").textContent`, &refusal)
	if !strings.Contains(refusal, "~") {
		t.Errorf("a regular expression was refused with %q", refusal)
	}
	p.forgive()

	p.run(chromedp.Click(chromedp.CSS(`#server-filter-clear`)))
	p.waitFor(`document.querySelectorAll("#view tbody a").length === 2`)
}
