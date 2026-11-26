//go:build browser

package browser

import (
	"net/url"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

const drainFieldShown = `(() => { const f = document.querySelector("#f-drainingTimeoutSec"); return !!f && f.getClientRects().length > 0; })()`

// TestLoadBalancingConnectionDrainingThroughTheBrowser2226 (#2226): a load
// balancer's Edit page has Backend configuration's Connection draining
// timeout (0 by default, as load-balancing/docs/enabling-connection-
// draining's console steps name it); Review and finalize lists the change
// to 60 and Update saves it on the backend service, read back through the
// console API. The keyboard subtest reaches the field with Tab and Enter.
func TestLoadBalancingConnectionDrainingThroughTheBrowser2226(t *testing.T) {
	needService(t, "netservices-lb")
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	lb := lb2160Fixture(t, project)
	if got := editFieldDefault(t, "netservices-lb", project, "drainingTimeoutSec", "global", lb); got != "0" {
		t.Fatalf("Connection draining timeout reads %q; want 0", got)
	}
	p := open(t)
	p.navigate("/net-services/loadbalancing/list/loadBalancers/global/" + lb + q + "&edit=1")
	p.waitFor(`document.querySelector("#view form.create-page .create-step-continue button") !== null`)
	p.clickText("#view form.create-page .create-step-continue button", "Continue")
	p.waitFor(drainFieldShown)
	var label string
	p.eval(`(document.querySelector('label[for="f-drainingTimeoutSec"]') || {}).textContent || ""`, &label)
	if !strings.Contains(label, "Connection draining timeout") {
		t.Errorf("the field's label is %q", label)
	}
	p.setField("#view form.create-page #f-drainingTimeoutSec", "60")
	p.run(chromedp.Click(chromedp.CSS(`#view form.create-page button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal.is-open #review-diff") !== null`)
	var diff string
	p.eval(`document.querySelector("#review-diff tbody").textContent`, &diff)
	if !strings.Contains(diff, "Connection draining timeout") || !strings.Contains(diff, "60") {
		t.Errorf("Review and finalize lists %q; want the draining timeout's change to 60", diff)
	}
	p.run(chromedp.Click(chromedp.CSS(`#review-deploy`)))
	p.waitFor(`!new URLSearchParams(location.search).has("edit") && document.querySelector("#view form.create-page") === null`)
	if got := editFieldDefault(t, "netservices-lb", project, "drainingTimeoutSec", "global", lb); got != "60" {
		t.Errorf("Connection draining timeout after Update reads %q; want 60", got)
	}

	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		k.navigate("/net-services/loadbalancing/list/loadBalancers/global/" + lb + q + "&edit=1")
		k.waitFor(`document.querySelector("#view form.create-page .create-step-continue button") !== null`)
		k.tabTo(`a.closest(".create-step-continue") !== null && a.textContent.trim() === "Continue"`)
		k.run(chromedp.KeyEvent(kb.Enter))
		k.waitFor(drainFieldShown)
		k.tabTo(`a.id === "f-drainingTimeoutSec"`)
		k.run(chromedp.KeyEvent("5"))
		var v string
		k.eval(`document.querySelector("#f-drainingTimeoutSec").value`, &v)
		if !strings.HasSuffix(v, "5") {
			t.Errorf("typing into Connection draining timeout gave %q", v)
		}
	})
}

// TestLoadBalancingConnectionDrainingMeetsWCAG2226 (#2226): the Edit
// page's Backend configuration step, with Connection draining timeout
// shown, meets WCAG (names, contrast in both themes, keyboard, motion,
// reflow).
func TestLoadBalancingConnectionDrainingMeetsWCAG2226(t *testing.T) {
	needService(t, "netservices-lb")
	project := servedProject(t)
	q := "?project=" + url.QueryEscape(project)
	lb := lb2160Fixture(t, project)
	auditScreens(t, []a11yPage{{"Load balancer Edit page, Backend configuration", "/net-services/loadbalancing/list/loadBalancers/global/" + lb + q + "&edit=1",
		`document.querySelector("#view form.create-page .create-step-continue button") !== null`,
		`(() => { const b = document.querySelector("#view form.create-page .create-step-continue button"); if (b) b.click(); return true; })()`}})
}
