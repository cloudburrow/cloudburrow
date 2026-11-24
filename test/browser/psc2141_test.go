//go:build browser

package browser

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestCloudSQLPSCEndpointThroughTheBrowser (#2141), on an instance
// serving Cloud SQL and Compute Engine: a Cloud SQL instance with Private
// Service Connect shows its service attachment on Connections; Private
// Service Connect's Connected endpoints (#2140's tab), Connect endpoint
// (Published service, Target service, Endpoint name, Network, Subnetwork,
// IP address, Enable global access) ending in Add endpoint, adds an
// endpoint to it, listed Accepted. Each screen passes the WCAG 2.1 AA audit,
// keyboard included.
func TestCloudSQLPSCEndpointThroughTheBrowser(t *testing.T) {
	needService(t, "cloudsql-instances")
	needService(t, "netservices-psc-endpoints")
	project := servedProject(t)
	q := "?project=" + url.QueryEscape(project)
	svc := sqlAdminSDK(t)
	name := fmt.Sprintf("browser-psc-%d", time.Now().UnixNano()%1e6)
	op, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "POSTGRES_17", Region: "us-central1",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840", IpConfiguration: &sqladmin.IpConfiguration{Ipv4Enabled: false,
			ForceSendFields: []string{"Ipv4Enabled"}, PscConfig: &sqladmin.PscConfig{PscEnabled: true, AllowedConsumerProjects: []string{project}}}}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if d, err := svc.Instances.Delete(project, name).Do(); err == nil {
			waitSQL(t, svc, project, d, 5*time.Minute)
		}
	})
	waitSQL(t, svc, project, op, 10*time.Minute)
	in, err := svc.Instances.Get(project, name).Do()
	if err != nil || in.PscServiceAttachmentLink == "" {
		t.Fatalf("the instance reads %v, %v; want a service attachment", in, err)
	}

	p := open(t)
	p.navigate("/cloudsql-instances/" + name + q + "&tab=connections")
	p.waitFor(`document.querySelector("#view").textContent.includes("Service attachment")`)
	var text string
	p.eval(`document.querySelector("#view").textContent`, &text)
	if !strings.Contains(text, in.PscServiceAttachmentLink) {
		t.Errorf("Connections does not show the service attachment %s", in.PscServiceAttachmentLink)
	}

	ep := "browser-ep-" + name[len("browser-psc-"):]
	list := "/net-services/psc/list/consumers" + q
	p.navigate(list)
	p.clickText("#view button", "Connect endpoint")
	p.waitFor(`document.querySelector('#view input[name="f-target-choice"][value="published"]') !== null`)
	p.eval(`(() => { document.querySelector('#view input[name="f-target-choice"][value="published"]').click(); return true; })()`, nil)
	p.waitFor(`(() => { const f = document.querySelector("#f-targetService"); return !!f && f.getClientRects().length > 0; })()`)
	var labels string
	p.eval(`[...document.querySelectorAll("#view label")].map((l) => l.textContent.trim()).join("|")`, &labels)
	for _, w := range []string{"Published service", "Target service", "Endpoint name", "Network", "Subnetwork", "IP address", "Enable global access"} {
		if !strings.Contains(labels, w) {
			t.Errorf("Connect endpoint lacks %q", w)
		}
	}
	var submit string
	p.eval(`document.querySelector("#f-targetService").form.querySelector("button[type=submit]").textContent.trim()`, &submit)
	if submit != "Add endpoint" {
		t.Errorf("Connect endpoint ends in %q; want Add endpoint", submit)
	}
	p.submitForm(map[string]string{"targetService": in.PscServiceAttachmentLink, "name": ep}, "targetService")
	row := fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes(%q))`, ep)
	p.waitFor(`location.pathname === "/net-services/psc/list/consumers" && ` + row + ` !== undefined`)
	t.Cleanup(func() {
		consoleDo(t, http.MethodPost, "/api/actions/netservices-psc-endpoints"+q,
			fmt.Sprintf(`{"Path":["us-central1",%q],"Action":"delete","Values":{}}`, ep))
	})
	p.eval(row+`.textContent`, &text)
	if !strings.Contains(text, "Accepted") {
		t.Errorf("the endpoint's row reads %q; want Accepted", text)
	}

	auditScreens(t, []a11yPage{
		screen("private service connect endpoints", "/net-services/psc/list/consumers", project),
		screen("connect endpoint", "/net-services/psc/list/consumers/create", project),
	})
}
