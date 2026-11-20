//go:build browser

package browser

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	cloudquotas "cloud.google.com/go/cloudquotas/apiv1"
	qpb "cloud.google.com/go/cloudquotas/apiv1/cloudquotaspb"
	"google.golang.org/api/option"
	sv "google.golang.org/api/siteverification/v1"
)

// TestQuotasThroughThePage (#2030), in the storage shard: IAM & Admin >
// Quotas & System Limits lists the Cloud Quotas API's quotas with Google's
// defaults; Edit quotas on Service Account Count submits a preference the
// official cloudquotas client reads back granted, and the row shows the
// new value beside the default; a 50% decrease is refused with the safety
// check's reason. The API's own Quotas & System Limits tab shows its rows.
func TestQuotasThroughThePage(t *testing.T) {
	needService(t, "serviceusage")
	rm := endpoint(t, envResourceManager)
	c, err := cloudquotas.NewRESTClient(context.Background(), option.WithEndpoint("http://"+rm), option.WithoutAuthentication(), option.WithHTTPClient(&http.Client{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	project := uniqueProject(t)
	p := open(t)
	p.navigate("/iam-admin/quotas?project=" + project)
	row := `[...document.querySelectorAll("#quotas-table tbody tr")].find((r) => r.textContent.includes("Service Account Count"))`
	p.waitFor(row + ` !== undefined`)
	var text string
	p.eval(row+`.textContent`, &text)
	if !strings.Contains(text, "iam.googleapis.com") || !strings.Contains(text, "100") {
		t.Errorf("Service Account Count row = %q", text)
	}
	p.clickExpr(row + `.querySelector("button")`)
	p.waitFor(`document.querySelector("#quota-edit") !== null`)
	p.setField("#quota-edit #f-value", "95")
	p.setField("#quota-edit #f-justification", "browser")
	p.clickExpr(`document.querySelector('#quota-edit button[type="submit"]')`)
	p.waitFor(`document.querySelector("#quota-edit") === null && ` + row + `.textContent.includes("(default 100)")`)
	var pref *qpb.QuotaPreference
	parent := "projects/" + project + "/locations/global"
	it := c.ListQuotaPreferences(context.Background(), &qpb.ListQuotaPreferencesRequest{Parent: parent})
	for {
		q, err := it.Next()
		if err != nil {
			break
		}
		if q.GetQuotaId() == "SERVICE-ACCOUNT-COUNT-per-project" {
			pref = q
		}
	}
	if pref == nil || pref.GetQuotaConfig().GetGrantedValue().GetValue() != 95 || pref.GetJustification() != "browser" ||
		pref.GetQuotaConfig().GetRequestOrigin() != qpb.QuotaConfig_CLOUD_CONSOLE {
		t.Fatalf("the preference the page made = %v", pref)
	}
	// A decrease by more than 10% is refused by Google's safety check: the
	// call's 400 is the refusal the dialog shows.
	p.expected = []string{"status of 400 (Bad Request) " + "http://" + endpoint(t, envConsole) + "/api/studio/serviceusage?call=quota-set&project=" + project}
	p.clickExpr(row + `.querySelector("button")`)
	p.waitFor(`document.querySelector("#quota-edit") !== null`)
	p.setField("#quota-edit #f-value", "40")
	p.clickExpr(`document.querySelector('#quota-edit button[type="submit"]')`)
	p.waitFor(`(document.querySelector("#quota-edit .form-error:not([hidden])") || {}).textContent?.includes("QUOTA_DECREASE_PERCENTAGE_TOO_HIGH")`)

	p.navigate("/apis/api/privateca.googleapis.com/quotas?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#api-quotas-table tbody tr")].some((r) => r.textContent.includes("CAs per location per project"))`)
}

// TestDomainVerificationThroughThePage (#2030), in the storage shard: Add
// domain gets a FILE token for a site this test serves on loopback, Verify
// checks it, and the official siteverification client lists the site;
// Remove, confirmed, deletes it.
func TestDomainVerificationThroughThePage(t *testing.T) {
	needService(t, "serviceusage")
	rm := endpoint(t, envResourceManager)
	hc := &http.Client{}
	t.Cleanup(hc.CloseIdleConnections)
	c, err := sv.NewService(context.Background(), option.WithEndpoint("http://"+rm+"/siteVerification/v1/"), option.WithoutAuthentication(), option.WithHTTPClient(hc))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b, ok := files[r.URL.Path]; ok {
			_, _ = io.WriteString(w, b)
			return
		}
		http.NotFound(w, r)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	site := "http://" + ln.Addr().String() + "/"
	tok, err := c.WebResource.GetToken(&sv.SiteVerificationWebResourceGettokenRequest{VerificationMethod: "FILE",
		Site: &sv.SiteVerificationWebResourceGettokenRequestSite{Type: "SITE", Identifier: site}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	// The page acts as the instance's user, the same owner as this client:
	// the token is the same.
	files["/"+tok.Token] = "google-site-verification: " + tok.Token
	project := uniqueProject(t)
	p := open(t)
	p.navigate("/apis/credentials/domainverification?project=" + project)
	p.waitFor(`document.querySelector("#domains-table") !== null`)
	p.clickExpr(`document.querySelector("#domains-add")`)
	p.waitFor(`document.querySelector("#domain-add") !== null`)
	p.setField("#domain-add #f-identifier", site)
	p.clickExpr(`document.querySelector('#domain-add button[type="submit"]')`)
	p.waitFor(`document.querySelector("#domain-verify") !== null`)
	var note string
	p.eval(`document.querySelector("#domain-verify .form-help").textContent`, &note)
	if !strings.Contains(note, tok.Token) {
		t.Errorf("the token the page shows: %q", note)
	}
	p.clickExpr(`document.querySelector('#domain-verify button[type="submit"]')`)
	cell := fmt.Sprintf(`[...document.querySelectorAll("#domains-table tbody td")].find((d) => d.textContent === %q)`, site)
	p.waitFor(cell + ` !== undefined`)
	list, err := c.WebResource.List().Do()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range list.Items {
		found = found || r.Site.Identifier == site
	}
	if !found {
		t.Fatalf("the API does not list %s", site)
	}
	p.clickExpr(cell + `.closest("tr").querySelector("button")`)
	p.waitFor(`document.querySelector("#domain-remove") !== null`)
	p.clickExpr(`document.querySelector('#domain-remove button[type="submit"]')`)
	p.waitFor(cell + ` === undefined`)
	list, _ = c.WebResource.List().Do()
	for _, r := range list.Items {
		if r.Site.Identifier == site {
			t.Errorf("%s still verified after Remove", site)
		}
	}
}

// TestGoogleAuthPlatformThroughThePages (#2030), in the storage shard:
// Get started configures the app; Branding saves; Clients > Create client
// makes a Web client whose ID and secret the dialog shows once; the
// client's page shows the secret's last four characters only, Add secret
// adds a second, and Delete, confirmed, moves the client to Deleted
// credentials, where Restore brings it back. The Credentials page lists
// it under OAuth 2.0 Client IDs.
func TestGoogleAuthPlatformThroughThePages(t *testing.T) {
	needService(t, "serviceusage")
	project := uniqueProject(t)
	p := open(t)
	p.navigate("/apis/credentials/consent?project=" + project)
	p.waitFor(`document.querySelector("#auth-get-started") !== null`)
	p.setField("#auth-get-started #f-appName", "Browser app")
	p.setField("#auth-get-started #f-supportEmail", "dev@example.com")
	p.setField("#auth-get-started #f-developerEmails", "dev@example.com")
	p.clickExpr(`document.querySelector("#auth-get-started-save")`)
	p.waitFor(`document.querySelector("#auth-overview") !== null`)
	var overview string
	p.eval(`document.querySelector("#auth-overview").textContent`, &overview)
	if !strings.Contains(overview, "Browser app") || !strings.Contains(overview, "/o/oauth2/v2/auth") {
		t.Errorf("Overview = %q", overview)
	}
	p.navigate("/apis/credentials/consent/branding?project=" + project)
	p.waitFor(`document.querySelector("#auth-branding") !== null`)
	p.setField("#auth-branding #f-appName", "Browser app 2")
	p.clickExpr(`document.querySelector("#auth-branding-save")`)
	p.waitFor(`document.querySelector(".page-header") && document.querySelector(".page-header").textContent.includes("Browser app 2")`)

	p.navigate("/apis/credentials/consent/clients/create?project=" + project)
	p.waitFor(`document.querySelector("#auth-client-form") !== null`)
	p.setField("#auth-client-form #f-name", "Browser web client")
	p.setField("#auth-client-form #f-redirectUris", "http://127.0.0.1:8080/callback")
	p.clickExpr(`document.querySelector("#auth-client-save")`)
	p.waitFor(`document.querySelector("#oauth-created-secret") !== null`)
	var id, secret string
	p.eval(`document.querySelector("#oauth-created-id").value`, &id)
	p.eval(`document.querySelector("#oauth-created-secret").value`, &secret)
	if !strings.HasSuffix(id, ".apps.googleusercontent.com") || !strings.HasPrefix(secret, "GOCSPX-") {
		t.Fatalf("the created dialog shows a client ID of %d characters and a secret of %d", len(id), len(secret))
	}
	p.clickExpr(`document.querySelector("#oauth-created-close")`)
	p.waitFor(`document.querySelector("#auth-client-id") !== null && document.querySelector("#auth-secrets tbody tr") !== null`)
	var cells string
	p.eval(`document.querySelector("#auth-secrets").textContent`, &cells)
	if strings.Contains(cells, secret) || !strings.Contains(cells, "****"+secret[len(secret)-4:]) {
		t.Errorf("the secrets table shows more than the last four characters")
	}
	p.clickExpr(`document.querySelector("#auth-secret-add")`)
	p.waitFor(`document.querySelector("#oauth-created-secret") !== null`)
	p.clickExpr(`document.querySelector("#oauth-created-close")`)
	p.waitFor(`document.querySelectorAll("#auth-secrets tbody tr").length === 2 && document.querySelector("#auth-secret-add").disabled`)

	p.navigate("/apis/credentials?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view table td")].some((d) => d.textContent === "Browser web client")`)

	p.navigate("/apis/credentials/consent/clients/" + id + "?project=" + project)
	p.waitFor(`document.querySelector("#auth-client-delete") !== null`)
	p.clickExpr(`document.querySelector("#auth-client-delete")`)
	p.waitFor(`document.querySelector(".modal.is-open form") !== null`)
	p.clickExpr(`document.querySelector('.modal.is-open button[type="submit"]')`)
	p.waitFor(`location.pathname === "/apis/credentials/consent/clients"`)
	p.navigate("/apis/credentials/consent/clients/deleted?project=" + project)
	restore := `[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes("Browser web client"))`
	p.waitFor(restore + ` !== undefined`)
	p.clickExpr(restore + `.querySelector("button")`)
	p.waitFor(restore + ` === undefined`)
	p.navigate("/apis/credentials/consent/clients?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view tbody td")].some((d) => d.textContent === "Browser web client")`)
}

// TestAPIsConsole2030ScreensMeetWCAG (#2030): Quotas & System Limits, Domain
// verification, each Google Auth Platform tab, Create client and a client's
// page, and the Edit quotas and Add domain dialogs, audited like every
// other screen (names, contrast in both themes, focus and dialogs).
func TestAPIsConsole2030ScreensMeetWCAG(t *testing.T) {
	needService(t, "serviceusage")
	project := uniqueProject(t)
	code, body := consoleDo(t, http.MethodPost, "/api/studio/serviceusage?call=oauth-app-set&project="+project,
		`{"brand":{"appName":"A11y app","supportEmail":"dev@example.com","developerEmails":["dev@example.com"],"userType":"EXTERNAL"}}`)
	if code != http.StatusOK {
		t.Fatalf("oauth-app-set = %d: %s", code, body)
	}
	code, body = consoleDo(t, http.MethodPost, "/api/studio/serviceusage?call=oauth-client-create&project="+project,
		`{"client":{"name":"A11y client","type":"WEB","redirectUris":["http://127.0.0.1:8080/cb"]}}`)
	if code != http.StatusOK {
		t.Fatalf("oauth-client-create = %d", code)
	}
	i := strings.Index(body, `"clientId":"`) + len(`"clientId":"`)
	id := body[i : i+strings.Index(body[i:], `"`)]
	var pages []a11yPage
	for _, l := range []struct{ name, path string }{
		{"Quotas & System Limits", "/iam-admin/quotas"}, {"API quotas tab", "/apis/api/privateca.googleapis.com/quotas"},
		{"Domain verification", "/apis/credentials/domainverification"},
		{"Google Auth Platform overview", "/apis/credentials/consent"}, {"Branding", "/apis/credentials/consent/branding"},
		{"Audience", "/apis/credentials/consent/audience"}, {"Data Access", "/apis/credentials/consent/scopes"},
		{"Clients", "/apis/credentials/consent/clients"}, {"Create client", "/apis/credentials/consent/clients/create"},
		{"Deleted clients", "/apis/credentials/consent/clients/deleted"}, {"OAuth client", "/apis/credentials/consent/clients/" + id},
	} {
		pages = append(pages, screen(l.name, l.path, project))
	}
	pages = append(pages,
		a11yPage{"Edit quotas dialog", "/iam-admin/quotas?project=" + project, `document.querySelector("#quotas-table tbody tr button") !== null`,
			`document.querySelector("#quotas-table tbody tr button").click()`},
		a11yPage{"Add domain dialog", "/apis/credentials/domainverification?project=" + project, `document.querySelector("#domains-add") !== null`,
			`document.querySelector("#domains-add").click()`},
	)
	auditScreens(t, pages)
}
