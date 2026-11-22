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
// API as Ready with its range and instances, and deleted from the row's
// menu.
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
	vmRowMenu(p, "browser-conn", "Delete", nil, byButton)
}

// TestServerlessVPCAccessMeetsWCAG2140 (#2140): Serverless VPC access and
// Create connector meet WCAG (contrast in both themes, keyboard, names,
// motion, reflow), as the compute shard's other screens do.
func TestServerlessVPCAccessMeetsWCAG2140(t *testing.T) {
	needService(t, "vpc-connectors")
	project := servedProject(t)
	auditScreens(t, listAndCreate("Serverless VPC access", "/networking/connectors/list", project))
}
