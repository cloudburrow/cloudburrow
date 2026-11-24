//go:build browser

package browser

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestServerlessVPCAccessThroughTheBrowser2140 (#2140), in the compute
// shard: Serverless VPC access's Create connector, made on its page with a
// Custom IP range and the scaling settings, read back through the console
// API as Ready with its range and instances, edited from the row's menu
// (the maximum raised and the instance type changed, as Google's Edit
// allows), and deleted from the row's menu.
func TestServerlessVPCAccessThroughTheBrowser2140(t *testing.T) {
	needService(t, "vpc-connectors")
	project := uniqueProject(t)
	q := "?project=" + url.QueryEscape(project)
	p := open(t)
	p.navigate("/networking/connectors/list" + q)
	p.clickText("#view button", "Create connector")
	p.submitForm(map[string]string{"name": "browser-conn", "region": "us-central1", "network": "default", "subnet": "custom",
		"ipCidrRange": "10.8.0.0/28", "minInstances": "3", "maxInstances": "6", "machineType": "f1-micro"}, "name")
	p.waitFor(`location.pathname === "/networking/connectors/list" && ` + rowReady("browser-conn"))
	code, body := consoleDo(t, http.MethodGet, "/api/resources/vpc-connectors"+q, "")
	if code != http.StatusOK {
		t.Fatalf("list = %d: %s", code, body)
	}
	for _, want := range []string{`"IP address range":"10.8.0.0/28"`, `"Min instances":"3"`, `"Max instances":"6"`, `"Instance type":"f1-micro"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the connector made on the page reads %s; want %s", body, want)
		}
	}
	vmRowMenu(p, "browser-conn", "Edit", map[string]string{"maxInstances": "8", "machineType": "e2-standard-4"}, "")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("e2-standard-4"))`)
	_, body = consoleDo(t, http.MethodGet, "/api/resources/vpc-connectors"+q, "")
	for _, want := range []string{`"Min instances":"3"`, `"Max instances":"8"`, `"Instance type":"e2-standard-4"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the edited connector reads %s; want %s", body, want)
		}
	}
	vmRowMenu(p, "browser-conn", "Delete", nil, byButton)
}

// openEditDialog opens the first row's actions menu and chooses Edit.
const openEditDialog = `document.querySelector("#view tbody tr .overflow-trigger:not(:disabled)").click();
	[...document.querySelectorAll("#view .overflow-item")].find((b) => b.textContent.trim() === "Edit").click()`

// TestServerlessVPCAccessMeetsWCAG2140 (#2140): Serverless VPC access,
// Create connector and a connector's Edit dialog meet WCAG (contrast in
// both themes, keyboard, names, motion, reflow), as the compute shard's
// other screens do.
func TestServerlessVPCAccessMeetsWCAG2140(t *testing.T) {
	needService(t, "vpc-connectors")
	project := servedProject(t)
	pages := listAndCreate("Serverless VPC access", "/networking/connectors/list", project)
	edit := uniqueProject(t)
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/vpc-connectors?project="+url.QueryEscape(edit),
		`{"name":"a11y-conn","region":"us-central1","network":"default","subnet":"custom","ipCidrRange":"10.8.0.0/28"}`); code != http.StatusOK {
		t.Fatalf("create a connector through the console API = %d: %s", code, body)
	}
	pages = append(pages, a11yPage{"edit connector dialog", "/networking/connectors/list?project=" + url.QueryEscape(edit), rowReady("a11y-conn"), openEditDialog})
	auditScreens(t, pages)
}
