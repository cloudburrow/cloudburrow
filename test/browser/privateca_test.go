//go:build browser

package browser

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestPrivateCAThroughThePage (#1834), in the storage shard, whose instance
// serves Certificate Authority Service for the page. Reached from the
// drawer's Security and identity row: Create CA pool, then on the pool's
// page Create CA, which is STAGED; Enable on the CA's page; Request
// certificate from a CSR on the pool's page; and on the certificate's page
// Revoke; a subordinate CA signed by the root through Create CA; and
// Create template on the Certificate templates page. The console API reads each change back from the same server the
// SDK calls, and the certificate the page shows verifies against the CA's.
func TestPrivateCAThroughThePage(t *testing.T) {
	needService(t, "privateca")
	project := uniqueProject(t)
	pool := "projects/" + project + "/locations/us-east1/caPools/browser-pool"
	detail := func(path ...string) map[string]any {
		t.Helper()
		q := "/api/detail/privateca?project=" + project
		for _, s := range path {
			q += "&name=" + url.QueryEscape(s)
		}
		code, body := consoleDo(t, http.MethodGet, q, "")
		var d map[string]any
		if err := json.Unmarshal([]byte(body), &d); code != http.StatusOK || err != nil {
			t.Fatalf("read %v through the console API = %d (%v): %s", path, code, err, body)
		}
		return d
	}

	p := open(t)
	p.navigateByDrawer("Security and identity", "/security/cas", "Certificate Authority Service")
	p.navigate("/security/cas?project=" + project)
	p.clickText("#view button", "Create CA pool")
	p.submitForm(map[string]string{"caPoolId": "browser-pool", "location": "us-east1", "tier": "ENTERPRISE"}, "caPoolId")
	p.waitFor(fmt.Sprintf(`location.pathname === "/security/cas" &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, pool))

	p.clickText("#view tbody a", pool)
	p.waitFor(`location.pathname.endsWith("browser-pool")`)
	p.pageAction("Create CA", map[string]string{"certificateAuthorityId": "root", "commonName": "Browser Root",
		"algorithm": "EC_P256_SHA256", "lifetimeDays": "365"}, "")
	p.follow("root")
	p.waitFor(`location.pathname.endsWith("/certificateAuthorities/root")`)
	p.waitFor(`document.querySelector("#view").textContent.includes("STAGED")`)
	p.clickText("#view .page-actions button", "Enable")
	p.waitFor(`document.querySelector("#view").textContent.includes("ENABLED")`)
	ca := detail(pool, "certificateAuthorities", "root")
	if !strings.Contains(fmt.Sprint(ca["summary"]), "ENABLED") {
		t.Fatalf("the CA after Enable reads %v", ca["summary"])
	}

	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "web.example.com"},
		DNSNames: []string{"web.example.com"}}, key)
	csr := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
	p.navigate("/security/cas?project=" + project)
	p.follow(pool)
	p.waitFor(`location.pathname.endsWith("browser-pool")`)
	p.pageAction("Request certificate from a CSR", map[string]string{"certificateId": "web", "pemCsr": csr, "lifetimeDays": "7"}, "")
	p.clickText(`#view [role="tab"]`, "Certificates")
	p.follow("web")
	p.waitFor(`location.pathname.endsWith("/certificates/web")`)
	p.waitFor(`document.querySelector("#view").textContent.includes("CN=web.example.com")`)

	var pems struct{ Leaf, CA string }
	cert := detail(pool, "certificates", "web")
	for _, s := range cert["sections"].([]any) {
		if sec := s.(map[string]any); sec["id"] == "pem" {
			pems.Leaf, _ = sec["text"].(string)
		}
	}
	for _, s := range ca["sections"].([]any) {
		if sec := s.(map[string]any); sec["id"] == "pem" {
			pems.CA, _ = sec["text"].(string)
		}
	}
	blk, _ := pem.Decode([]byte(pems.Leaf))
	if blk == nil {
		t.Fatalf("the certificate's page shows no PEM: %v", cert)
	}
	leaf, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM([]byte(pems.CA))
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "web.example.com"}); err != nil {
		t.Errorf("the certificate issued from the page does not verify against its CA: %v", err)
	}

	// A subordinate signed and activated by the root, from Create CA.
	p.navigate("/security/cas?project=" + project)
	p.follow(pool)
	p.waitFor(`location.pathname.endsWith("browser-pool")`)
	p.pageAction("Create CA", map[string]string{"certificateAuthorityId": "sub", "type": "SUBORDINATE", "issuerCa": pool + "/certificateAuthorities/root",
		"commonName": "Browser Sub", "algorithm": "EC_P256_SHA256", "lifetimeDays": "90"}, "")
	if got := fmt.Sprint(detail(pool, "certificateAuthorities", "sub")["summary"]); !strings.Contains(got, "STAGED") {
		t.Errorf("the subordinate made on the page reads %s; want it activated, STAGED", got)
	}

	// The Certificate templates page.
	// The product's pages, reached from its drawer row: CA pools and
	// Certificate templates.
	p.navigateByDrawer("Security and identity", "/security/cas", "Certificate Authority Service")
	p.waitFor(`document.querySelectorAll("#product-nav a").length > 0`)
	p.clickText("#product-nav a", "Certificate templates")
	p.waitFor(`location.pathname === "/security/cas/templates"`)
	p.navigate("/security/cas/templates?project=" + project)
	p.clickText("#view button", "Create template")
	p.submitForm(map[string]string{"certificateTemplateId": "browser-tmpl", "location": "us-east1", "usages": "digital_signature,client_auth"}, "certificateTemplateId")
	tmpl := "projects/" + project + "/locations/us-east1/certificateTemplates/browser-tmpl"
	p.waitFor(fmt.Sprintf(`location.pathname === "/security/cas/templates" &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, tmpl))
	code, body := consoleDo(t, http.MethodGet, "/api/detail/privateca-templates?project="+project+"&name="+url.QueryEscape(tmpl), "")
	if code != http.StatusOK || !strings.Contains(body, "digital_signature,client_auth") {
		t.Errorf("the template made on the page reads %d: %s", code, body)
	}

	p.navigate("/security/cas?project=" + project)
	p.follow(pool)
	p.waitFor(`location.pathname.endsWith("browser-pool")`)
	p.clickText(`#view [role="tab"]`, "Certificates")
	p.follow("web")
	p.waitFor(`document.querySelector("#view").textContent.includes("CN=web.example.com")`)
	p.pageAction("Revoke", map[string]string{"reason": "SUPERSEDED"}, "web")
	p.waitFor(`document.querySelector("#view").textContent.includes("SUPERSEDED") && document.querySelector("#view").textContent.includes("Revoked")`)
	if got := fmt.Sprint(detail(pool, "certificates", "web")["summary"]); !strings.Contains(got, "Revoked") {
		t.Errorf("the certificate after Revoke reads %s", got)
	}
}

// follow opens the page a table's link with text leads to, at the address
// the page drew for it: a row link redrawn by the poll between being found
// and being clicked was clicked stale.
func (p *tab) follow(text string) {
	p.t.Helper()
	find := fmt.Sprintf(`[...document.querySelectorAll("#view tbody a")].find((a) => a.textContent.trim() === %q && a.getClientRects().length)`, text)
	p.waitFor(find + ` !== undefined`)
	var href string
	p.eval(find+`.getAttribute("href")`, &href)
	p.navigate(href)
}
