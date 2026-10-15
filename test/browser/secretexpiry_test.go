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

// TestSecretExpiryThroughTheForms (#1225): Create secret, on its own page,
// offers an expire time and a TTL; a secret created with a TTL shows its
// expiry on its page, and Edit secret, prefilled with that expire time,
// clears it, after which the page says Never.
func TestSecretExpiryThroughTheForms(t *testing.T) {
	needService(t, "secrets")
	p := open(t)
	project := uniqueProject(t)
	id := "browser-expiry"
	name := "projects/" + project + "/secrets/" + id
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/secrets?project="+project+"&name="+id, "")
	})
	p.navigate("/secrets/create?project=" + project)
	p.waitFor(`document.querySelector("#view #f-ttl") !== null && document.querySelector("#view #f-expireTime") !== null`)
	setField(p, "#view", "f-secretId", id)
	setField(p, "#view", "f-payload", "value")
	setField(p, "#view", "f-ttl", "48h")
	p.run(chromedp.Click(`#view button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`location.pathname === "/secrets"`)

	got := editDefaults(t, "secrets", project, name)
	exp, err := time.Parse(time.RFC3339, got["expireTime"])
	if err != nil || time.Until(exp) < 47*time.Hour || time.Until(exp) > 49*time.Hour {
		t.Fatalf("a 48h TTL created in the browser reads expire time %q (%v)", got["expireTime"], err)
	}
	if got["Expire time"] != got["expireTime"] {
		t.Errorf("the page shows expire time %q, the edit form %q", got["Expire time"], got["expireTime"])
	}

	p.clickText("#view tbody a", name)
	p.clickText("#view .page-actions button", "Edit secret")
	p.waitFor(`document.querySelector(".modal.is-open #f-expireTime") !== null`)
	var prefilled string
	p.eval(`document.querySelector(".modal #f-expireTime").value`, &prefilled)
	if prefilled != got["expireTime"] {
		t.Errorf("Edit secret is prefilled with expire time %q, want %q", prefilled, got["expireTime"])
	}
	setField(p, ".modal", "f-expireTime", "")
	p.run(chromedp.Click(`.modal button[type="submit"]`, chromedp.ByQuery))
	p.waitFor(`document.querySelector(".modal") === null`)
	if got := editDefaults(t, "secrets", project, name); got["Expire time"] != "Never" || got["expireTime"] != "" {
		t.Errorf("after clearing in Edit secret the secret reads expire time %q, form %q", got["Expire time"], got["expireTime"])
	}
}

// TestSecretListFilterThroughTheBox (#1228): the Secrets list has a filter
// box in Secret Manager's syntax; applying a filter lists only the matching
// secrets and keeps it in the URL, a regular expression is refused under
// the box with the API's message, and Clear lists every secret again.
func TestSecretListFilterThroughTheBox(t *testing.T) {
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
	p.run(chromedp.Click(`#server-filter-apply`, chromedp.ByQuery))
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
	p.run(chromedp.Click(`#server-filter-apply`, chromedp.ByQuery))
	p.waitFor(`document.querySelector("#server-filter-error") !== null`)
	var refusal string
	p.eval(`document.querySelector("#server-filter-error").textContent`, &refusal)
	if !strings.Contains(refusal, "~") {
		t.Errorf("a regular expression was refused with %q", refusal)
	}
	p.forgive()

	p.run(chromedp.Click(`#server-filter-clear`, chromedp.ByQuery))
	p.waitFor(`document.querySelectorAll("#view tbody a").length === 2`)
}
