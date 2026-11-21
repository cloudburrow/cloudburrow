//go:build browser

package browser

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	run "cloud.google.com/go/run/apiv2"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestCloudRunGooglePagesThroughTheBrowser (#1852), in the run shard. Cloud
// Run's menu is Google's: Services, Jobs, Worker pools, Domain mappings.
// Deploy container opens the Create service page: Google's sections
// (Configure, Authentication, Billing, Service scaling, Ingress, and
// Containers, Volumes, Networking, Security with its tabs), ending in
// Create. A service created with Allow public access reads back through the
// official client with allUsers holding roles/run.invoker; the Services list
// has Google's columns and says so; the service's page has Google's tabs in
// Google's order, Manage traffic on Revisions and the v1 YAML on YAML; and
// the Domain mappings page's Add mapping maps a domain to it.
func TestCloudRunGooglePagesThroughTheBrowser(t *testing.T) {
	needService(t, "run")
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envRun)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: the service is read back through the Cloud Run API itself", envRun)
	}
	ctx := context.Background()
	c, err := run.NewServicesClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	project := uniqueProject(t)
	id := fmt.Sprintf("browser-google-%d", time.Now().UnixNano()%1e6)
	domain := id + ".example.com"
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/run-domains?project="+project+"&name="+domain, "")
		consoleDo(t, http.MethodDelete, "/api/resources/run?project="+project+"&name="+id, "")
	})

	p := open(t)
	p.navigate("/run/create?project=" + project)
	p.waitFor(`document.querySelector("#view #f-authentication") !== null`)
	var form struct {
		Title, Button, Region, Auth string
		Steps, Tabs, Menu           []string
	}
	p.eval(`(() => ({
		Title: document.querySelector("#view h1").textContent,
		Button: document.querySelector("#view button[type=submit]").textContent,
		Region: document.querySelector("#view #f-region").value,
		Auth: document.querySelector("#view #f-authentication").value,
		Steps: [...document.querySelectorAll("#view .create-step-title")].map((n) => n.textContent),
		Tabs: [...document.querySelectorAll("#view .form-tabs [role=tab]")].map((n) => n.textContent),
		Menu: [...document.querySelectorAll("#product-nav a")].map((a) => a.textContent.trim()) }))()`, &form)
	steps := "Configure|Authentication|Billing|Service scaling|Ingress|Containers, Volumes, Networking, Security"
	tabs := "Container(s)|Variables & Secrets|Volumes|Networking|Security"
	if form.Title != "Create service" || form.Button != "Create" || form.Region != "us-central1" ||
		form.Auth != "Require authentication" || strings.Join(form.Steps, "|") != steps || strings.Join(form.Tabs, "|") != tabs {
		t.Errorf("the Create service page reads %+v; want steps %q and tabs %q", form, steps, tabs)
	}
	menu := strings.Join(form.Menu, "|")
	if !strings.Contains(menu, "Services|Jobs|Worker pools|Domain mappings") {
		t.Errorf("Cloud Run's menu is %q; want Google's Services, Jobs, Worker pools, Domain mappings", menu)
	}
	// A tab shows its own fields only, and every field stays in the form.
	p.clickText("#view .form-tabs [role=tab]", "Variables & Secrets")
	var shown struct{ Env, Port bool }
	p.eval(`(() => ({ Env: document.querySelector("#view #f-env").getClientRects().length > 0,
		Port: document.querySelector("#view #f-port").getClientRects().length > 0 }))()`, &shown)
	if !shown.Env || shown.Port {
		t.Errorf("on Variables & Secrets, environment variables shown %v and the container port %v; want only the first", shown.Env, shown.Port)
	}

	p.run(chromedp.SendKeys(chromedp.CSS(`#view #f-name`), id))
	p.clickText("#view .is-radio label", "Allow public access")
	p.clickText(`#view button[type="submit"]`, "Create")
	p.waitFor(`location.pathname === "/run"`)

	name := "projects/" + project + "/locations/us-central1/services/" + id
	pol, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
	if err != nil {
		t.Fatalf("read the service's policy: %v", err)
	}
	public := false
	for _, b := range pol.GetBindings() {
		public = public || (b.GetRole() == "roles/run.invoker" && slices.Contains(b.GetMembers(), "allUsers"))
	}
	if !public {
		t.Errorf("Allow public access left the policy %v; want allUsers in roles/run.invoker", pol.GetBindings())
	}

	// Google's columns, with the service's values.
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, id))
	var heads, row string
	p.eval(`[...document.querySelectorAll("#view thead th")].map((h) => h.textContent.trim()).filter(Boolean).join("|")`, &heads)
	p.eval(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes(%q)).textContent`, id), &row)
	if !strings.Contains(heads, "Name|Deployment type|Region|Authentication|Ingress|Last deployed|Deployed by") {
		t.Errorf("the Services list's headers are %q", heads)
	}
	for _, v := range []string{"Container", "us-central1", "Allow public access"} {
		if !strings.Contains(row, v) {
			t.Errorf("the service's row %q lacks %q", row, v)
		}
	}

	// Google's tabs, in Google's order, with Manage traffic on Revisions.
	p.navigate("/run/" + id + "?project=" + project + "&tab=revisions")
	p.waitFor(sectionButton("Manage traffic"))
	var page struct {
		Title string
		Tabs  []string
	}
	p.eval(`(() => ({ Title: document.querySelector("#view h1").textContent,
		Tabs: [...document.querySelectorAll("#view .tab-strip .tab")].map((n) => n.textContent) }))()`, &page)
	want := "Metrics|SLOs|Logs|Revisions|Source|Triggers|Containers|Networking|Security|Scaling|YAML"
	if page.Title != "Service details" || !strings.HasPrefix(strings.Join(page.Tabs, "|"), want) {
		t.Errorf("the service page is %+v; want Service details with tabs %q first", page, want)
	}
	p.clickText("#view .tab-strip .tab", "YAML")
	p.waitFor(`[...document.querySelectorAll("#view pre")].some((n) => n.textContent.includes("serving.knative.dev/v1"))`)
	p.clickText("#view .tab-strip .tab", "Security")
	p.waitFor(`[...document.querySelectorAll("#view dd")].some((n) => n.textContent === "Allow public access")`)

	// The Domain mappings page: Add mapping, a service then the domain.
	p.navigate("/run/domains?project=" + project)
	p.clickText("#view button", "Add mapping")
	p.waitFor(`document.querySelector(".modal.is-open #f-service") !== null`)
	setField(p, ".modal.is-open", "f-service", id)
	setField(p, ".modal.is-open", "f-domain", domain)
	setField(p, ".modal.is-open", "f-certificateMode", "NONE")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q) && r.textContent.includes(%q))`, domain, id))
	if code, body := consoleDo(t, http.MethodGet, "/api/resources/run-domains?project="+project, ""); code != http.StatusOK || !strings.Contains(body, domain) {
		t.Errorf("the Domain mappings listing = %d: %.300s; want %s", code, body, domain)
	}
}
