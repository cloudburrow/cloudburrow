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
	"time"
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
	p.clickText("#view button", "Create pool")
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

// TestPrivateCAKMSKeyCMEKBucketAndRulesThroughThePage (#1834), in the sql
// shard, whose instance serves Certificate Authority Service with Cloud KMS
// and Cloud Storage. Create CA pool with a customer-managed encryption key,
// a requester-specified not-before time, a CEL expression and passthrough
// extensions; Create CA whose key is a Cloud KMS version, publishing to a
// bucket, its subject an RDN sequence; Request certificate from a config
// with an RDN sequence, the RDN_SEQUENCE subject mode and a not-before
// time; and Create template with a CEL expression and passthrough
// extensions. The console API reads each back from the server the SDK
// calls.
func TestPrivateCAKMSKeyCMEKBucketAndRulesThroughThePage(t *testing.T) {
	needService(t, "privateca")
	needService(t, "kms")
	needService(t, "storage")
	project := uniqueProject(t)
	ring := "projects/" + project + "/locations/us-central1/keyRings/pca"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/kms?project="+project, `{"keyRingId":"pca","location":"us-central1"}`); code != http.StatusOK {
		t.Fatalf("create a key ring through the console API = %d: %s", code, body)
	}
	for id, v := range map[string]map[string]string{
		"sign": {"cryptoKeyId": "sign", "purpose": "ASYMMETRIC_SIGN", "algorithm": "EC_SIGN_P256_SHA256"},
		"cmek": {"cryptoKeyId": "cmek", "purpose": "ENCRYPT_DECRYPT"},
	} {
		b, _ := json.Marshal(map[string]any{"Path": []string{ring}, "Action": "createkey", "Values": v})
		if code, body := consoleDo(t, http.MethodPost, "/api/actions/kms?project="+project, string(b)); code != http.StatusOK {
			t.Fatalf("create the %s key through the console API = %d: %s", id, code, body)
		}
	}
	version, cmek := ring+"/cryptoKeys/sign/cryptoKeyVersions/1", ring+"/cryptoKeys/cmek"
	bucket := project + "-pca"
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/storage?project="+project, `{"name":"`+bucket+`"}`); code != http.StatusOK {
		t.Fatalf("create a bucket through the console API = %d: %s", code, body)
	}
	t.Cleanup(func() { consoleDo(t, http.MethodDelete, "/api/resources/storage?project="+project+"&name="+bucket, "") })
	read := func(service string, path ...string) string {
		t.Helper()
		q := "/api/detail/" + service + "?project=" + project
		for _, s := range path {
			q += "&name=" + url.QueryEscape(s)
		}
		code, body := consoleDo(t, http.MethodGet, q, "")
		if code != http.StatusOK {
			t.Fatalf("read %v through the console API = %d: %s", path, code, body)
		}
		return body
	}
	const cel = `subject_alt_names.all(san, san.type == DNS && san.value.endsWith(".example.com"))`
	pool := "projects/" + project + "/locations/us-central1/caPools/kms-pool"

	p := open(t)
	p.navigate("/security/cas?project=" + project)
	p.clickText("#view button", "Create pool")
	p.waitFor(`document.querySelector("#f-allowRequesterNotBefore") !== null`)
	p.eval(`(() => { const f = document.querySelector("#f-allowRequesterNotBefore"); f.checked = true;
		f.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, nil)
	p.submitForm(map[string]string{"caPoolId": "kms-pool", "location": "us-central1", "tier": "ENTERPRISE", "encryptionKey": cmek,
		"celExpression": cel, "passthroughExtensions": "EXTENDED_KEY_USAGE,1.2.3.4"}, "caPoolId")
	p.waitFor(fmt.Sprintf(`location.pathname === "/security/cas" &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, pool))
	got := read("privateca", pool)
	for _, want := range []string{cmek, "EXTENDED_KEY_USAGE,1.2.3.4", "san.value.endsWith"} {
		if !strings.Contains(got, want) {
			t.Errorf("the pool made on the page does not show %q: %s", want, got)
		}
	}
	if !strings.Contains(got, `"Requester-specified not-before time","value":"Allowed"`) {
		t.Errorf("the pool made on the page does not allow a requester's not-before time: %s", got)
	}

	p.clickText("#view tbody a", pool)
	p.waitFor(`location.pathname.endsWith("kms-pool")`)
	p.pageAction("Create CA", map[string]string{"certificateAuthorityId": "root", "rdnSequence": "C=US\nO=CloudBurrow\nCN=KMS Root",
		"algorithm": "EC_P256_SHA256", "kmsKeyVersion": version, "gcsBucket": bucket, "lifetimeDays": "365"}, "")
	p.follow("root")
	p.waitFor(`location.pathname.endsWith("/certificateAuthorities/root")`)
	p.waitFor(`document.querySelector("#view").textContent.includes("STAGED")`)
	p.waitFor(fmt.Sprintf(`document.querySelector("#view").textContent.includes(%q)`, version))
	p.clickText("#view .page-actions button", "Enable")
	p.waitFor(`document.querySelector("#view").textContent.includes("ENABLED")`)
	ca := read("privateca", pool, "certificateAuthorities", "root")
	for _, want := range []string{"Cloud KMS key version", version, `"Publishing bucket","value":"` + bucket + `"`, "/" + bucket + "/", "C=US,O=CloudBurrow,CN=KMS Root"} {
		if !strings.Contains(ca, want) {
			t.Errorf("the CA made on the page does not show %q: %s", want, ca)
		}
	}

	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	pub := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	notBefore := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339)
	p.navigate("/security/cas?project=" + project)
	p.follow(pool)
	p.waitFor(`location.pathname.endsWith("kms-pool")`)
	p.pageAction("Request certificate from a config", map[string]string{"certificateId": "rdn", "rdnSequence": "O=CloudBurrow\nCN=rdn.example.com",
		"dnsNames": "rdn.example.com", "publicKey": pub, "subjectMode": "RDN_SEQUENCE", "requestedNotBefore": notBefore, "lifetimeDays": "7"}, "")
	cert := read("privateca", pool, "certificates", "rdn")
	if !strings.Contains(cert, `"Subject mode","value":"RDN_SEQUENCE"`) || !strings.Contains(cert, strings.TrimSuffix(notBefore, "Z")) {
		t.Errorf("the certificate requested on the page does not have the RDN_SEQUENCE subject mode and the not-before %s: %s", notBefore, cert)
	}

	p.navigate("/security/cas/templates?project=" + project)
	p.clickText("#view button", "Create template")
	p.submitForm(map[string]string{"certificateTemplateId": "cel-tmpl", "location": "us-central1", "usages": "digital_signature",
		"celExpression": cel, "passthroughExtensions": "BASE_KEY_USAGE"}, "certificateTemplateId")
	tmpl := "projects/" + project + "/locations/us-central1/certificateTemplates/cel-tmpl"
	p.waitFor(fmt.Sprintf(`location.pathname === "/security/cas/templates" &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, tmpl))
	if got := read("privateca-templates", tmpl); !strings.Contains(got, "san.value.endsWith") || !strings.Contains(got, `"Passthrough extensions","value":"BASE_KEY_USAGE"`) {
		t.Errorf("the template made on the page reads %s", got)
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
