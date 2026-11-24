//go:build browser

package browser

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

func browserCAPEM(t *testing.T) string {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "browser-root"}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// TestTrustConfigsAndClientAuthenticationThroughTheBrowser2160 (#2160):
// Certificate Manager's Trust Configs tab, whose Create makes a trust
// config from a pasted PEM trust anchor (Google's Create Trust Config
// page: Name, Location, the Trust store section), listed with its count
// and opened on its Trust Config details page with Edit; then Network
// Security's Authentication Configuration › Client Authentication, whose
// Create makes a server TLS policy that rejects invalid certificates
// against it, listed with Google's mode names. The keyboard subtest
// reaches the Trust Configs tab with Tab and Enter alone.
func TestTrustConfigsAndClientAuthenticationThroughTheBrowser2160(t *testing.T) {
	needService(t, "ccm-trustconfigs")
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	p := open(t)
	name := fmt.Sprintf("tc-%d", time.Now().UnixNano()%1e6)
	p.navigate("/security/ccm/list/trustConfigs" + q)
	p.waitFor(settled + ` && document.querySelector("#view nav.page-tabs a[aria-current]")?.textContent === "Trust Configs"`)
	p.clickText("#view button", "Create")
	p.waitFor(`document.querySelector("#f-name") !== null && document.querySelector("#f-trustAnchors") !== null`)
	var sections string
	p.eval(`[...document.querySelectorAll("#view form h2, #view form h3, #view form legend")].map((h) => h.textContent.trim()).join("|")`, &sections)
	if !containsAll(sections, "Trust store", "Allowlisted certificates") {
		t.Errorf("Create Trust Config's sections = %s", sections)
	}
	setField(p, "#view", "f-name", name)
	setField(p, "#view", "f-trustAnchors", browserCAPEM(t))
	p.run(chromedp.Click(chromedp.CSS(`#view form button[type="submit"].primary`)))
	p.waitFor(`location.pathname === "/security/ccm/list/trustConfigs" && ` + rowReady(name))
	var anchors string
	p.eval(fmt.Sprintf(`(() => { const r = [...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes(%q));
		const i = [...document.querySelectorAll("#view thead th")].findIndex((h) => h.textContent.trim() === "Trust anchors");
		return r && i >= 0 ? r.children[i].textContent.trim() : ""; })()`, name), &anchors)
	if anchors != "1" {
		t.Errorf("the row's Trust anchors = %q", anchors)
	}
	p.navigate("/security/ccm/list/trustConfigs/global/" + name + q)
	p.waitFor(`document.querySelector("#view h1, #view h2") !== null && document.body.textContent.includes("Trust Config details")`)
	var hasEdit bool
	p.eval(`[...document.querySelectorAll("#view button, #view a")].some((b) => b.textContent.trim() === "Edit")`, &hasEdit)
	if !hasEdit {
		t.Error("the trust config's page has no Edit")
	}

	auth := fmt.Sprintf("auth-%d", time.Now().UnixNano()%1e6)
	p.navigate("/net-security/authconfig" + q)
	p.waitFor(settled + ` && document.querySelector("#view nav.page-tabs a[aria-current]")?.textContent === "Client Authentication"`)
	p.clickText("#view button", "Create")
	p.waitFor(`document.querySelector("#f-name") !== null && document.querySelector("#f-trustConfig") !== null`)
	var modes string
	p.eval(`[...document.querySelectorAll("#view form label")].map((l) => l.textContent.trim()).filter((t) => t.startsWith("Reject") || t.startsWith("Allow")).join("|")`, &modes)
	if !containsAll(modes, "Reject invalid", "Allow invalid or missing client certificate") {
		t.Errorf("Client Validation Mode's options = %s", modes)
	}
	setField(p, "#view", "f-name", auth)
	setField(p, "#view", "f-trustConfig", name)
	p.run(chromedp.Click(chromedp.CSS(`#view form button[type="submit"].primary`)))
	p.waitFor(`location.pathname === "/net-security/authconfig" && ` + rowReady(auth))
	var row string
	p.eval(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes(%q))?.textContent ?? ""`, auth), &row)
	if !containsAll(row, "Load balancing", "Reject invalid", name) {
		t.Errorf("the Client Authentication row = %q", row)
	}

	t.Run("keyboard", func(t *testing.T) {
		k := open(t)
		k.navigate("/security/ccm/list/certificates" + q)
		k.waitFor(settled + ` && document.querySelector("#view nav.page-tabs") !== null`)
		k.tabTo(`a.closest("nav.page-tabs") !== null && a.textContent === "Trust Configs"`)
		k.run(chromedp.KeyEvent(kb.Enter))
		k.waitFor(`location.pathname === "/security/ccm/list/trustConfigs"`)
	})
}

// TestMutualTLSPagesMeetWCAG2160 (#2160): the Trust Configs tab and
// Create Trust Config, and Client Authentication and its Create, meet
// WCAG (contrast in both themes, keyboard, names, motion, reflow).
func TestMutualTLSPagesMeetWCAG2160(t *testing.T) {
	needService(t, "ccm-trustconfigs")
	project := servedProject(t)
	var pages []a11yPage
	pages = append(pages, listAndCreate("Trust configs", "/security/ccm/list/trustConfigs", project)...)
	pages = append(pages, listAndCreate("Client Authentication", "/net-security/authconfig", project)...)
	auditScreens(t, pages)
}

// containsAll is whether s holds every want.
func containsAll(s string, want ...string) bool {
	for _, w := range want {
		if !strings.Contains(s, w) {
			return false
		}
	}
	return true
}
