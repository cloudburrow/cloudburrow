//go:build browser

package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	ak "google.golang.org/api/apikeys/v2"
	"google.golang.org/api/option"
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

// TestAPIKeysCreateACopyAndServiceAccount (#2145), in the storage shard: a
// key's page has Create a copy, which makes a new key with its
// restrictions under the name given, read back with the official API Keys
// client; Create API key has Authenticate API calls through a service
// account, which reveals Select a service account. The Create API key page
// and a key's page pass the WCAG audit.
func TestAPIKeysCreateACopyAndServiceAccount(t *testing.T) {
	needService(t, "serviceusage")
	hc := &http.Client{}
	t.Cleanup(hc.CloseIdleConnections)
	c, err := ak.NewService(context.Background(), option.WithEndpoint("http://"+endpoint(t, envAPIKeys)+"/"),
		option.WithoutAuthentication(), option.WithHTTPClient(hc))
	if err != nil {
		t.Fatal(err)
	}
	project := uniqueProject(t)
	parent := "projects/" + project + "/locations/global"
	op, err := c.Projects.Locations.Keys.Create(parent, &ak.V2Key{DisplayName: "Source key", Restrictions: &ak.V2Restrictions{
		ServerKeyRestrictions: &ak.V2ServerKeyRestrictions{AllowedIps: []string{"198.51.100.0/24"}},
		ApiTargets:            []*ak.V2ApiTarget{{Service: "serviceusage.googleapis.com"}}}}).Do()
	if err != nil || !op.Done {
		t.Fatalf("create the source key with the official client = %v, %v", op, err)
	}
	var made ak.V2Key
	if err := json.Unmarshal(op.Response, &made); err != nil {
		t.Fatal(err)
	}
	id := made.Name[strings.LastIndex(made.Name, "/")+1:]
	t.Cleanup(func() {
		if all, _ := c.Projects.Locations.Keys.List(parent).Do(); all != nil {
			for _, k := range all.Keys {
				_, _ = c.Projects.Locations.Keys.Delete(k.Name).Do()
			}
		}
	})

	p := open(t)
	p.navigate("/apis/credentials/key/" + id + "?project=" + project)
	p.waitFor(`document.querySelector("#api-key-copy-button") !== null`)
	var ips string
	p.eval(`document.querySelector("#api-key-ips-items").textContent`, &ips)
	if !strings.Contains(ips, "198.51.100.0/24") {
		t.Errorf("the key's page lists the IP addresses %q", ips)
	}
	p.clickExpr(`document.querySelector("#api-key-copy-button")`)
	p.waitFor(`location.pathname.startsWith("/apis/credentials/apikey/copy/") && document.querySelector("#api-key-form") !== null`)
	p.setField("#api-key-name", "Copied key")
	p.clickExpr(`document.querySelector("#api-key-create")`)
	p.waitFor(`document.querySelector("#api-key-string") !== null`)
	list, err := c.Projects.Locations.Keys.List(parent).Do()
	if err != nil || len(list.Keys) != 2 {
		t.Fatalf("keys after Create a copy = %v, %v", list, err)
	}
	for _, k := range list.Keys {
		if k.Name == made.Name {
			continue
		}
		if k.DisplayName != "Copied key" || k.Restrictions == nil || k.Restrictions.ServerKeyRestrictions == nil ||
			strings.Join(k.Restrictions.ServerKeyRestrictions.AllowedIps, ",") != "198.51.100.0/24" || len(k.Restrictions.ApiTargets) != 1 {
			t.Errorf("the copy: %+v", k)
		}
	}
	p.clickExpr(`document.querySelector("#api-key-close")`)

	p.navigate("/apis/credentials/apikey/create?project=" + project)
	p.waitFor(`document.querySelector("#api-key-bind-sa") !== null`)
	p.clickExpr(`document.querySelector("#api-key-bind-sa")`)
	p.waitFor(`!document.querySelector("#api-key-sa-pick").hidden && document.querySelector("#api-key-service-account") !== null`)

	auditScreens(t, []a11yPage{
		screen("Create API key", "/apis/credentials/apikey/create", project),
		screen("Edit API key", "/apis/credentials/key/"+id, project),
	})
}
