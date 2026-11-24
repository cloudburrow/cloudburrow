//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestIAPTCPThroughTheBrowser2160 (#2160): Identity-Aware Proxy's tabs are
// HTTPS resources and SSH and TCP resources; on the second, Create
// destination group (Name, Region, IP address, FQDNs) makes a group,
// listed with its destinations, whose page opens from its name and whose
// Edit destination group saves new ranges. The keyboard subtest reaches
// the SSH and TCP resources tab with Tab and opens it with Enter; the WCAG
// subtest audits the tab and its create page.
func TestIAPTCPThroughTheBrowser2160(t *testing.T) {
	needService(t, "security-iap-tcp")
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	p := open(t)
	for i, path := range []string{"/security/iap", "/security/iap/tcp"} {
		p.navigate(path + q)
		p.waitFor(settled + ` && document.querySelector("#view nav.page-tabs") !== null`)
		var strip string
		p.eval(`[...document.querySelectorAll("#view nav.page-tabs a")].map((a) => a.textContent + (a.getAttribute("aria-current") ? "*" : "")).join(",")`, &strip)
		names := []string{"HTTPS resources", "SSH and TCP resources"}
		names[i] += "*"
		if strip != strings.Join(names, ",") {
			t.Errorf("%s's tabs = %s", path, strip)
		}
	}
	name := fmt.Sprintf("lab-%c", 'a'+rune(time.Now().UnixNano()%26))
	p.navigate("/security/iap/tcp" + q)
	p.clickText("#view button", "Create destination group")
	p.waitFor(`document.querySelector("#f-name") !== null && document.querySelector("#f-cidrs") !== null && document.querySelector("#f-fqdns") !== null`)
	setField(p, "#view", "f-name", name)
	setField(p, "#view", "f-cidrs", "10.1.2.0/24")
	setField(p, "#view", "f-fqdns", "*.corp.example")
	p.run(chromedp.Click(chromedp.CSS(`#view form button[type="submit"].primary`)))
	p.waitFor(`location.pathname === "/security/iap/tcp" && ` + rowReady(name))
	code, body := consoleDo(t, http.MethodGet, "/api/resources/security-iap-tcp"+q, "")
	if code != http.StatusOK || !strings.Contains(body, `"Destinations":"10.1.2.0/24, *.corp.example"`) {
		t.Errorf("the group made on the page reads %d %s", code, body)
	}
	p.navigate("/security/iap/tcp/locations/us-central1/" + name + q)
	p.waitFor(`document.querySelector("#view h1") !== null && document.querySelector("#view h1").textContent.includes("Destination group details") && document.querySelector("#view").textContent.includes(` + jsString(name) + `)`)
	p.clickText("#view button", "Edit")
	p.waitFor(`document.querySelector(".modal.is-open #f-cidrs") !== null`)
	setField(p, ".modal.is-open", "f-cidrs", "10.9.0.0/16")
	p.clickText(".modal.is-open button", "Save")
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	if code, body := consoleDo(t, http.MethodGet, "/api/resources/security-iap-tcp"+q, ""); code != http.StatusOK || !strings.Contains(body, "10.9.0.0/16") {
		t.Errorf("after Edit destination group: %d %s", code, body)
	}

	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		k.navigate("/security/iap" + q)
		k.waitFor(settled + ` && document.querySelector("#view nav.page-tabs") !== null`)
		k.tabTo(`a.closest("nav.page-tabs") !== null && a.textContent === "SSH and TCP resources"`)
		k.run(chromedp.KeyEvent(kb.Enter))
		k.waitFor(`location.pathname === "/security/iap/tcp"`)
	})
	t.Run("wcag", func(t *testing.T) {
		auditScreens(t, listAndCreate("IAP SSH and TCP resources", "/security/iap/tcp", project))
	})
}

// lbLocalAddress is a load balancer's frontend's local address, from the
// console's Frontends tab.
func lbLocalAddress(t *testing.T, project, lb string) string {
	t.Helper()
	code, body := consoleDo(t, http.MethodGet, "/api/resources/netservices-lb-frontends?project="+url.QueryEscape(project), "")
	var l struct {
		Items []struct {
			Name   string
			Fields map[string]string
		}
	}
	if err := json.Unmarshal([]byte(body), &l); code != http.StatusOK || err != nil {
		t.Fatalf("the frontends: %d %s", code, body)
	}
	for _, it := range l.Items {
		if strings.HasPrefix(it.Name, lb) && it.Fields["Local address"] != "" {
			return it.Fields["Local address"]
		}
	}
	t.Fatalf("no frontend of %s with a local address: %s", lb, body)
	return ""
}

// TestIAPBrowserSignIn2160 (#2160): a browser that opens an IAP-secured
// load balancer is sent to the sign-in, here this instance's own consent
// page (Google's would be accounts.google.com); Allow signs it in and
// brings it back to the page it asked for, ?gcp-iap-mode=IDENTITY, which
// answers the signed-in account; CLEAR_LOGIN_COOKIE signs it out, so the
// next visit goes to the sign-in again. The keyboard subtest signs in with
// Tab and Enter alone. The sign-in page is Google Auth Platform's consent
// page, which its own tests audit.
func TestIAPBrowserSignIn2160(t *testing.T) {
	needService(t, "security-iap")
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	lb := lb2160Fixture(t, project)
	bs := lb + "-backend-service"
	on := open(t)
	on.navigate("/security/iap" + q)
	on.waitFor(rowReady(bs))
	on.clickText("#view tbody tr button", "Turn on IAP")
	on.waitFor(`document.querySelector(".modal.is-open") !== null`)
	on.clickText(".modal.is-open button", "Turn On")
	on.waitFor(`document.querySelector(".modal.is-open") === null`)
	if code, body := consoleDo(t, http.MethodGet, "/api/resources/security-iap"+q, ""); code != http.StatusOK || !strings.Contains(body, `"IAP":"On"`) {
		t.Fatalf("after Turn on IAP: %d %s", code, body)
	}
	app := "http://" + lbLocalAddress(t, project, lb)
	// IAP's refusals of a browser without its session, as Google answers
	// them, are the page's own 403s.
	forbidden := []string{"status of 403 (Forbidden) " + app + "/", "/favicon.ico"}
	signIn := func(p *tab, keyboard bool) {
		p.t.Helper()
		p.run(chromedp.Navigate(app + "/?gcp-iap-mode=IDENTITY"))
		p.waitFor(`document.querySelector("#oauth-allow") !== null || document.body.innerText.includes("accounts.google.com:")`)
		var signedIn bool
		p.eval(`document.body.innerText.includes("accounts.google.com:")`, &signedIn)
		if signedIn {
			return
		}
		var title string
		p.eval(`document.querySelector("#oauth-title").textContent`, &title)
		if !strings.Contains(title, "Google Cloud") {
			p.t.Errorf("the sign-in page's title = %q", title)
		}
		if keyboard {
			p.tabTo(`a.id === "oauth-allow"`)
			p.run(chromedp.KeyEvent(kb.Enter))
		} else {
			p.run(chromedp.Click(chromedp.CSS("#oauth-allow")))
		}
		p.waitFor(`document.body.innerText.includes("accounts.google.com:")`)
		var loc string
		p.eval(`location.href`, &loc)
		if !strings.HasPrefix(loc, app+"/?gcp-iap-mode=IDENTITY") {
			p.t.Errorf("after the sign-in the browser is at %s", loc)
		}
	}
	p := open(t)
	p.expected = forbidden
	signIn(p, false)
	var id map[string]string
	var text string
	p.eval(`document.body.innerText`, &text)
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &id); err != nil || !strings.HasPrefix(id["email"], "accounts.google.com:") || !strings.HasPrefix(id["sub"], "accounts.google.com:") {
		t.Errorf("IDENTITY = %q", text)
	}
	// Signed in: the app's own page is no longer IAP's sign-in.
	p.run(chromedp.Navigate(app + "/"))
	p.waitFor(`document.readyState === "complete"`)
	var here string
	p.eval(`location.origin`, &here)
	if here != app {
		t.Errorf("a signed-in visit went to %s", here)
	}
	p.run(chromedp.Navigate(app + "/?gcp-iap-mode=CLEAR_LOGIN_COOKIE"))
	p.waitFor(`document.readyState === "complete" && document.querySelector("#oauth-allow") !== null`)

	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		k.expected = forbidden
		k.run(chromedp.Navigate(app + "/?gcp-iap-mode=CLEAR_LOGIN_COOKIE"))
		signIn(k, true)
	})
}
