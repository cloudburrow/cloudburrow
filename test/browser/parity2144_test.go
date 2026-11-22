//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestPrivateCAGoogleTabsThroughThePage (#2144), in the storage shard: the
// Certificate Authority Service page's tabs, as Google names them; Create
// CA on the CA manager tab, starting with Select CA pool and Enabled at
// once by Initialized state; Request a certificate on the Private
// certificate manager tab by Manually enter details, whose generated
// private key the certificate's page offers as Download private key, with
// Download certificate and Download certificate chain. The console API
// reads each back from the server the SDK calls.
func TestPrivateCAGoogleTabsThroughThePage(t *testing.T) {
	needService(t, "privateca")
	project := uniqueProject(t)
	pool := "projects/" + project + "/locations/us-west1/caPools/tabs-pool"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/privateca?project="+project,
		`{"caPoolId":"tabs-pool","location":"us-west1","tier":"ENTERPRISE","allowCsr":"true","allowConfig":"true"}`); code != http.StatusOK {
		t.Fatalf("create a CA pool through the console API = %d: %s", code, body)
	}

	p := open(t)
	p.navigate("/security/cas?project=" + project)
	var tabs string
	p.waitFor(`document.querySelectorAll("nav.page-tabs a").length === 4`)
	p.eval(`[...document.querySelectorAll("nav.page-tabs a")].map((a) => a.textContent.trim()).join("|")`, &tabs)
	if tabs != "CA pool manager|CA manager|Private certificate manager|Template manager" {
		t.Errorf("the Certificate Authority Service page's tabs are %s", tabs)
	}

	// CA manager > Create CA.
	p.clickText("nav.page-tabs a", "CA manager")
	p.waitFor(`location.pathname === "/security/cas/certificateAuthorities"`)
	p.clickText("#view button", "Create CA")
	p.waitFor(`document.querySelector("#f-caPool") !== null && [...document.querySelector("#f-caPool").options].length > 0`)
	var steps string
	p.eval(`[...document.querySelectorAll("#view .create-step-title")].map((h) => h.textContent.trim()).join("|")`, &steps)
	for _, want := range []string{"Select CA pool", "Select CA type", "Configure CA subject name", "Configure CA key size and algorithm", "Configure CA artifacts"} {
		if !strings.Contains(steps, want) {
			t.Errorf("Create CA has no %q step: %s", want, steps)
		}
	}
	p.submitForm(map[string]string{"caPool": pool, "certificateAuthorityId": "tabs-root", "initialState": "ENABLED",
		"commonName": "Tabs Root", "organizationalUnit": "Ops", "locality": "Ottawa", "algorithm": "EC_P256_SHA256", "lifetimeDays": "365"}, "certificateAuthorityId")
	p.waitFor(`location.pathname.includes("certificateAuthorities") && document.querySelector("#view").textContent.includes("ENABLED")`)
	ca := detail2144(t, "privateca", project, pool, "certificateAuthorities", "tabs-root")
	if !strings.Contains(ca, "OU=Ops") || !strings.Contains(ca, "L=Ottawa") {
		t.Errorf("the CA made on the CA manager tab reads %s", ca)
	}
	p.navigate("/security/cas/certificateAuthorities?project=" + project)
	p.waitFor(listedRow("/security/cas/certificateAuthorities", "tabs-root"))

	// Private certificate manager > Request a certificate, Manually enter details.
	p.clickText("nav.page-tabs a", "Private certificate manager")
	p.waitFor(`location.pathname === "/security/cas/certificates"`)
	p.clickText("#view button", "Request a certificate")
	p.waitFor(`document.querySelector("#f-caPool") !== null && document.querySelector("#f-caPool").value !== ""`)
	p.setField("#f-location", "us-west1")
	p.setField("#f-caPool", pool)
	p.setField("#f-method", "manual")
	p.clickText("#view button", "Add item")
	p.waitFor(`document.querySelector("#view .group-row input") !== null`)
	p.eval(`(() => { const i = document.querySelector("#view .group-row input");
		i.value = "tabs.example.com"; i.dispatchEvent(new Event("input", { bubbles: true })); i.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, nil)
	p.submitForm(map[string]string{"certificateId": "tabs-cert", "lifetimeDays": "7", "usage": "server_auth", "keyAlgorithm": "EC_P256"}, "certificateId")
	p.waitFor(listedRow("/security/cas/certificates", "tabs-cert"))
	p.follow("tabs-cert")
	p.waitFor(`location.pathname.endsWith("/certificates/tabs-cert") && document.querySelector("#view").textContent.includes("tabs.example.com")`)
	cert := detail2144(t, "privateca", project, pool, "certificates", "tabs-cert")
	if !strings.Contains(cert, "BEGIN PRIVATE KEY") || !strings.Contains(cert, `"download":"tabs-cert.key"`) {
		t.Errorf("the certificate's page offers no generated private key: %s", cert)
	}
	for _, tab := range []string{"Certificate", "Certificate chain", "Private key"} {
		p.clickText(`#view [role="tab"]`, tab)
		want := "Download " + strings.ToLower(tab[:1]) + tab[1:]
		p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view button[data-download]")].some((b) => b.textContent.trim() === %q && b.getClientRects().length)`, want))
	}

	var pages []a11yPage
	for _, l := range []struct{ name, path string }{
		{"CA pool manager", "/security/cas"}, {"CA manager", "/security/cas/certificateAuthorities"},
		{"Private certificate manager", "/security/cas/certificates"}, {"Template manager", "/security/cas/templates"},
	} {
		pages = append(pages, listAndCreate(l.name, l.path, project)...)
	}
	auditScreens(t, pages)
}

// detail2144 is a resource's page as the console API answers it.
func detail2144(t *testing.T, service, project string, path ...string) string {
	t.Helper()
	q := "/api/detail/" + service + "?project=" + project
	for _, s := range path {
		q += "&name=" + url.QueryEscape(s)
	}
	code, body := consoleDo(t, http.MethodGet, q, "")
	if code != http.StatusOK {
		t.Fatalf("read %v through the console API = %d: %s", path, code, body)
	}
	var v any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("decode %v: %v", path, err)
	}
	return body
}
