//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestDeveloperConnectAccountConnectorsThroughThePage (#2105): Developer
// Connect > Account connectors, as Google's "Configure account connectors"
// walks it. Create connector (Region, Name, Pre-configured OAuth clients,
// the Google provider, Scopes) lists the connector; its Connector details
// page offers Connect your account, a link to its oauth start URI; that
// URI, followed over HTTP from the test (the instance's own consent page,
// Allow, and the redirect page), connects the instance's user, who the
// page's Users tab then lists; Edit changing the scopes removes the user;
// the row's Delete removes the connector. The console API reads each
// result back; every screen meets WCAG 2.1 AA and is walked by keyboard.
func TestDeveloperConnectAccountConnectorsThroughThePage(t *testing.T) {
	const svc = "developer-connect-account-connectors"
	needService(t, svc)
	project := servedProject(t)
	id := fmt.Sprintf("docs-%d", time.Now().UnixNano()%1e9)
	q := "?project=" + url.QueryEscape(project)
	type detailDoc struct {
		Summary  []struct{ Label, Value string }
		Sections []struct {
			ID      string
			Listing struct {
				Items []struct{ Name string }
			}
		}
	}
	detail := func() detailDoc {
		t.Helper()
		code, body := consoleDo(t, http.MethodGet, "/api/detail/"+svc+q+"&name="+url.QueryEscape(id), "")
		if code != http.StatusOK {
			t.Fatalf("read the connector through the console API = %d: %s", code, body)
		}
		var d detailDoc
		if err := json.Unmarshal([]byte(body), &d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	summary := func(d detailDoc, label string) string {
		for _, s := range d.Summary {
			if s.Label == label {
				return s.Value
			}
		}
		return ""
	}
	users := func(d detailDoc) []string {
		var out []string
		for _, s := range d.Sections {
			if s.ID == "users" {
				for _, it := range s.Listing.Items {
					out = append(out, it.Name)
				}
			}
		}
		return out
	}
	t.Cleanup(func() { consoleDo(t, http.MethodDelete, "/api/resources/"+svc+q+"&name="+url.QueryEscape(id), "") })

	p := open(t)
	listPath := "/developer-connect/account-connectors" + q
	p.navigate(listPath)
	p.clickText("#view button", "Create connector")
	var createPath string
	p.waitFor(`document.querySelector("#f-name") !== null`)
	p.eval(`location.pathname + location.search`, &createPath)
	p.submitForm(map[string]string{"name": id, "region": "us-central1", "scopes": "openid"}, "name")
	p.waitFor(fmt.Sprintf(`location.pathname === "/developer-connect/account-connectors" &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, id))
	p.follow(id)
	var connectorPath string
	p.eval(`location.pathname + location.search`, &connectorPath)
	if p.findAction("Connect your account") == "" {
		t.Fatal("the connector's page offers no Connect your account")
	}
	d := detail()
	start := summary(d, "OAuth start URI")
	if summary(d, "OAuth provider") != "Google" || summary(d, "Scopes") != "openid" || start == "" {
		t.Fatalf("the connector's page = %+v", d.Summary)
	}

	// Connect your account, over HTTP: every URL opened is the instance's.
	local := func(raw string) *url.URL {
		t.Helper()
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "http" || !(u.Hostname() == "localhost" || net.ParseIP(u.Hostname()).IsLoopback()) {
			t.Fatalf("%q is not this instance's", raw)
		}
		return u
	}
	noFollow := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(raw string) (*http.Response, string) {
		t.Helper()
		resp, err := noFollow.Get(local(raw).String())
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(b)
	}
	resp, _ := get(start)
	auth, err := resp.Location()
	if resp.StatusCode != http.StatusFound || err != nil {
		t.Fatalf("the oauth start URI = %d", resp.StatusCode)
	}
	resp, page := get(auth.String())
	m := regexp.MustCompile(`name="nonce" value="([^"]+)"`).FindStringSubmatch(page)
	if resp.StatusCode != http.StatusOK || m == nil {
		t.Fatalf("the consent page = %d:\n%s", resp.StatusCode, page)
	}
	u := local(auth.String())
	resp, err = noFollow.PostForm(u.Scheme+"://"+u.Host+u.Path, url.Values{"nonce": {m[1]}, "decision": {"allow"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	back, err := resp.Location()
	if err != nil {
		t.Fatalf("Allow = %d, no redirect", resp.StatusCode)
	}
	if resp, page := get(back.String()); resp.StatusCode != http.StatusOK || !strings.Contains(page, "Account connected") {
		t.Fatalf("the redirect page = %d:\n%s", resp.StatusCode, page)
	}
	connected := users(detail())
	if len(connected) != 1 || !strings.Contains(connected[0], "@") {
		t.Fatalf("Users after connecting = %v", connected)
	}
	// The Users list is the page's one section, drawn without a tab.
	p.navigate(connectorPath)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, connected[0]))

	// Edit, by keyboard alone: Tab to it, Enter opens its form.
	p.navigate(connectorPath)
	p.waitFor(settled)
	p.tabTo(`a.textContent.trim() === "Edit"`)
	p.run(chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector(".modal.is-open #f-scopes") !== null`)
	p.navigate(connectorPath)
	p.pageAction("Edit", map[string]string{"scopes": "openid\nemail"}, "")
	if d := detail(); summary(d, "Scopes") != "openid, email" || len(users(d)) != 0 {
		t.Errorf("after Edit: scopes %q, users %v; want the new scopes and no user", summary(d, "Scopes"), users(d))
	}

	auditScreens(t, []a11yPage{
		{"account connectors", listPath, rowReady(id), ""},
		{"create account connector", createPath, settled + ` && document.querySelector("#view form input") !== null`, ""},
		{"connector details", connectorPath, settled, ""},
	})

	p = open(t)
	p.navigate(listPath)
	p.openRowDelete(id, "Delete")
	p.confirmByName(id)
	p.waitFor(fmt.Sprintf(`![...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, id))
}
