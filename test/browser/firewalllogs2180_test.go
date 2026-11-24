//go:build browser

package browser

import (
	"net/http"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestFirewallRuleLogsThroughTheBrowser (#2180), in a shard whose
// instance serves the compute service. Create firewall rule on Firewall
// policies with Logs On shows the Metadata choice (hidden while Logs is
// Off); an egress deny made with Exclude metadata lists Logs On, and the
// rule's Edit form keeps Exclude metadata. Its WCAG subtests check the form's
// controls are named and its contrast in both themes, and the keyboard one
// reaches the Metadata choice by Tab once Logs is On.
func TestFirewallRuleLogsThroughTheBrowser(t *testing.T) {
	needService(t, "vpc-networks")
	project := uniqueProject(t)
	form := "/networking/firewalls/list?project=" + project
	openForm := func(p *tab) {
		p.navigate(form)
		p.waitFor(settled + ` && [...document.querySelectorAll("#view button")].some((b) => b.textContent.trim() === "Create firewall rule")`)
		p.clickText("#view button", "Create firewall rule")
		p.waitFor(`document.querySelector("#f-logs") !== null`)
	}
	visible := `(() => { const f = document.querySelector("#f-logsMetadata"); const row = f && f.closest(".form-row");
		return !!row && !row.hidden && row.getClientRects().length > 0; })()`

	p := open(t)
	openForm(p)
	var shown bool
	p.eval(visible, &shown)
	if shown {
		t.Error("Metadata is shown while Logs is Off")
	}
	p.setField("#f-logs", "On")
	p.waitFor(visible)
	p.submitForm(map[string]string{"name": "browser-logged", "network": "default", "direction": "Egress", "action": "Deny",
		"logs": "On", "logsMetadata": "Exclude metadata", "destinationRanges": "10.9.0.0/16",
		"protocolsMode": "specified", "udp": "true", "udpPorts": "53"}, "name")
	p.waitFor(`location.pathname === "/networking/firewalls/list" && ` + listedRow("/networking/firewalls/list", "browser-logged"))
	code, body := consoleDo(t, http.MethodGet, "/api/resources/vpc-firewalls?project="+project, "")
	if code != http.StatusOK || !strings.Contains(body, `"Logs":"On"`) {
		t.Errorf("the rule made on the page lists %d: %s", code, body)
	}
	code, body = consoleDo(t, http.MethodGet, "/api/detail/vpc-firewalls?project="+project+"&name=browser-logged", "")
	if code != http.StatusOK || !strings.Contains(body, `"name":"logsMetadata"`) || !strings.Contains(body, `"default":"Exclude metadata"`) {
		t.Errorf("the rule's Edit form does not keep Exclude metadata: %d %s", code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/vpc-firewalls?project="+project+"&name=browser-logged", "")
	})

	t.Run("names", func(t *testing.T) {
		p := open(t)
		openForm(p)
		p.setField("#f-logs", "On")
		p.waitFor(visible)
		for _, u := range p.unnamedControls() {
			t.Errorf("an unnamed %s", u)
		}
	})
	for _, theme := range []string{"light", "dark"} {
		t.Run("contrast/"+theme, func(t *testing.T) {
			p := open(t)
			p.run(reducedMotion())
			p.navigate("/")
			p.waitFor(settled)
			p.setTheme(theme)
			openForm(p)
			p.setField("#f-logs", "On")
			p.waitFor(visible)
			for _, fl := range p.contrastFailures() {
				t.Errorf("%s theme: %s", theme, fl)
			}
		})
	}
	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		k.run(reducedMotion())
		openForm(k)
		// Tab reaches the group's checked radio (Off); an arrow key moves
		// the choice to On, as a radio group does.
		k.tabTo(`a.type === "radio" && a.closest("fieldset") && a.closest("fieldset").querySelector("#f-logs") !== null`)
		k.run(chromedp.KeyEvent(kb.ArrowLeft))
		k.waitFor(visible)
		k.tabTo(`a.type === "radio" && a.closest("fieldset") && a.closest("fieldset").querySelector("#f-logsMetadata") !== null`)
	})
}
