//go:build browser

package browser

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// authzRule makes an external Application Load Balancer's forwarding rule
// (a backend service, URL map and target HTTP proxy) for an authorization
// policy to target, and answers its relative name.
func authzRule(t *testing.T, project string) string {
	t.Helper()
	sfx := time.Now().UnixNano() % 1e6
	bs, um, px, fr := fmt.Sprintf("azbs-%d", sfx), fmt.Sprintf("azmap-%d", sfx), fmt.Sprintf("azpx-%d", sfx), fmt.Sprintf("azfr-%d", sfx)
	lb3Compute(t, http.MethodPost, project, "global/backendServices", map[string]any{"name": bs, "protocol": "HTTP", "loadBalancingScheme": "EXTERNAL_MANAGED"})
	lb3Compute(t, http.MethodPost, project, "global/urlMaps", map[string]any{"name": um, "defaultService": "global/backendServices/" + bs})
	lb3Compute(t, http.MethodPost, project, "global/targetHttpProxies", map[string]any{"name": px, "urlMap": "global/urlMaps/" + um})
	lb3Compute(t, http.MethodPost, project, "global/forwardingRules", map[string]any{"name": fr, "target": "global/targetHttpProxies/" + px,
		"portRange": "80", "loadBalancingScheme": "EXTERNAL_MANAGED"})
	t.Cleanup(func() {
		for _, rel := range []string{"global/forwardingRules/" + fr, "global/targetHttpProxies/" + px, "global/urlMaps/" + um, "global/backendServices/" + bs} {
			req, _ := http.NewRequest(http.MethodDelete, "http://"+endpoint(t, envBDRCompute)+"/compute/v1/projects/"+project+"/"+rel, nil)
			if resp, err := http.DefaultClient.Do(req); err == nil {
				resp.Body.Close()
			}
		}
	})
	return "projects/" + project + "/global/forwardingRules/" + fr
}

// TestCustomAuthorizationPolicyThroughTheBrowser2160 (#2160): Create
// authorization policy offers the action (ALLOW, DENY, CUSTOM), the policy
// profile and the custom provider (Cloud IAP or an authorization
// extension); a CUSTOM policy with Cloud IAP is created from the page,
// listed with its provider, and its page's Edit form reads Cloud IAP back.
// The template's rule shows source resources (a service account and tag
// values). The keyboard subtest reaches the Custom provider select with
// Tab; the WCAG subtest audits the list, the create page and the policy's
// page.
func TestCustomAuthorizationPolicyThroughTheBrowser2160(t *testing.T) {
	needService(t, "netsecurity-authz")
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	rule := authzRule(t, project)
	name := fmt.Sprintf("iap-%d", time.Now().UnixNano()%1e6)
	p := open(t)
	p.navigate("/net-security/authzpolicies/create" + q)
	p.waitFor(`document.querySelector("#view #f-customProvider") !== null && document.querySelector("#view #f-action") !== null`)
	var opts string
	p.eval(`[...document.querySelectorAll("#view #f-action option")].map((o) => o.value).join(",") + "|" +
		[...document.querySelectorAll("#view #f-customProvider option")].map((o) => o.textContent.trim()).join(",")`, &opts)
	if opts != "ALLOW,DENY,CUSTOM|None,Cloud IAP,Authorization extension" {
		t.Errorf("the action and provider options = %q", opts)
	}
	var tpl string
	p.eval(`document.querySelector("#view #f-spec").value`, &tpl)
	if !strings.Contains(tpl, "iamServiceAccount") || !strings.Contains(tpl, "tagValueIdSet") {
		t.Errorf("the template = %q", tpl)
	}
	setField(p, "#view", "f-name", name)
	setField(p, "#view", "f-action", "CUSTOM")
	setField(p, "#view", "f-customProvider", "cloudIap")
	setField(p, "#view", "f-spec", `{"target": {"loadBalancingScheme": "EXTERNAL_MANAGED", "resources": ["`+rule+`"]}}`)
	p.run(chromedp.Click(chromedp.CSS(`#view form button[type="submit"].primary`)))
	p.waitFor(`location.pathname === "/net-security/authzpolicies" && ` + rowReady(name))
	var row string
	p.eval(`[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes(`+fmt.Sprintf("%q", name)+`)).textContent`, &row)
	if !strings.Contains(row, "CUSTOM") || !strings.Contains(row, "Cloud IAP") {
		t.Errorf("the row reads %q", row)
	}
	p.navigate("/net-security/authzpolicies/global/" + name + q + "&edit=1")
	p.waitFor(`document.querySelector("#view form #f-customProvider") !== null`)
	var prov string
	p.eval(`document.querySelector("#view form #f-customProvider").value`, &prov)
	if prov != "cloudIap" {
		t.Errorf("the Edit form's provider = %q", prov)
	}

	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		k.navigate("/net-security/authzpolicies/create" + q)
		k.waitFor(`document.querySelector("#view #f-customProvider") !== null`)
		setField(k, "#view", "f-action", "CUSTOM")
		k.tabTo(`a.id === "f-customProvider"`)
	})
	t.Run("wcag", func(t *testing.T) {
		pages := listAndCreate("Authorization policies", "/net-security/authzpolicies", project)
		pages = append(pages, a11yPage{"Authorization policy", "/net-security/authzpolicies/global/" + name + q, settled, ""})
		auditScreens(t, pages)
	})
}
