//go:build browser

package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	run "cloud.google.com/go/run/apiv2"
	"cloud.google.com/go/run/apiv2/runpb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestCloudRunServiceTabsCopyAndRevisionMenuThroughTheBrowser (#1852), in
// the run shard. Google's service page is changed on its tabs: "Configure
// settings across the Containers, Networking, Security, and Scaling tabs
// ... Click View diff & redeploy, then Deploy changes". The Scaling tab's
// form opens a review of the one setting changed, and Deploy changes
// deploys it, read back through the official client with the image kept.
// A revision's ⋮ menu has Manage traffic and, on one serving nothing,
// Delete. The Services list's rows have checkboxes, Copy opens the service
// copy page for the one checked, and the filter's Deployment type property
// lists its values.
// Every new screen and dialog is held to WCAG 2.1 AA.
func TestCloudRunServiceTabsCopyAndRevisionMenuThroughTheBrowser(t *testing.T) {
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
	id := fmt.Sprintf("browser-tabs-%d", time.Now().UnixNano()%1e6)
	copyID := id + "-copy"
	const image = "ghcr.io/knative/helloworld-go:latest"
	body, _ := json.Marshal(map[string]string{"name": id, "image": image, "env": `{"TARGET":"tabs"}`, "maxInstances": "10"})
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/run?project="+project+"&name="+copyID, "")
		consoleDo(t, http.MethodDelete, "/api/resources/run?project="+project+"&name="+id, "")
	})
	code, resp := consoleDeploy(t, http.MethodPost, "/api/resources/run?project="+project, string(body))
	switch {
	case code == http.StatusOK:
	case code == http.StatusBadRequest && strings.Contains(resp, "did not answer in time") && strings.Contains(resp, `"operation"`):
		waitForRunService(t, project, id, image, 5*time.Minute)
	default:
		t.Fatalf("deploy a service through the console API = %d: %s", code, resp)
	}
	name := "projects/" + project + "/locations/us-central1/services/" + id

	p := open(t)
	// Google's tabs: Containers, Networking, Security and Scaling.
	p.navigate("/run/" + id + "?project=" + project + "&tab=scaling")
	p.waitFor(`document.querySelector("#view #inline-edit-scaling-save") !== null`)
	var tabs []string
	p.eval(`[...document.querySelectorAll("#view .tab-strip .tab")].map((n) => n.textContent)`, &tabs)
	if want := "Metrics|SLOs|Logs|Revisions|Source|Triggers|Containers|Networking|Security|Scaling|YAML"; !strings.HasPrefix(strings.Join(tabs, "|"), want) {
		t.Errorf("the service's tabs are %q; want %q first", strings.Join(tabs, "|"), want)
	}
	var save string
	p.eval(`document.querySelector("#view #inline-edit-scaling-save").textContent`, &save)
	if save != "View diff & redeploy" {
		t.Errorf("the Scaling tab ends in %q, want View diff & redeploy", save)
	}

	// View diff & redeploy: the one change, then Cancel sends nothing.
	setField(p, "#view .inline-edit", "f-maxInstances", "3")
	p.clickText("#view .inline-edit button", "View diff & redeploy")
	p.waitFor(`document.querySelector(".modal.is-open #review-diff") !== null`)
	var review struct {
		Title, Deploy string
		Rows          []string
	}
	p.eval(`(() => { const m = document.querySelector(".modal.is-open"); return {
		Title: m.querySelector("h2").textContent, Deploy: m.querySelector("#review-deploy").textContent,
		Rows: [...m.querySelectorAll("#review-diff tbody tr")].map((r) => [...r.children].map((c) => c.textContent).join("=")) }; })()`, &review)
	if review.Deploy != "Deploy changes" || len(review.Rows) != 1 || !strings.HasSuffix(review.Rows[0], "=10=3") {
		t.Fatalf("the review is %+v; want the maximum's change from 10 to 3, ending in Deploy changes", review)
	}
	p.clickText(".modal.is-open button", "Cancel")
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	if sent := p.sent(http.MethodPatch, "/api/resources/run"); len(sent) != 0 {
		t.Errorf("Cancel sent %v", sent)
	}
	// The review is a dialog as WCAG asks; its check ends with Escape,
	// which closes it as Cancel does.
	p.clickText("#view .inline-edit button", "View diff & redeploy")
	p.waitFor(`document.querySelector(".modal.is-open #review-diff") !== null`)
	for _, f := range p.dialogFailures(true) {
		t.Errorf("the review dialog: %s", f)
	}
	p.waitFor(`document.querySelector(".modal.is-open") === null`)

	// Deploy changes deploys a new revision with the change, and keeps the
	// other tabs' settings.
	p.clickText("#view .inline-edit button", "View diff & redeploy")
	p.clickText(".modal.is-open button", "Deploy changes")
	deadline := time.Now().Add(5 * time.Minute)
	var got *runpb.Service
	for time.Now().Before(deadline) {
		got, err = c.GetService(ctx, &runpb.GetServiceRequest{Name: name})
		if err == nil && got.GetTemplate().GetScaling().GetMaxInstanceCount() == 3 {
			break
		}
		time.Sleep(3 * time.Second)
	}
	if got.GetTemplate().GetScaling().GetMaxInstanceCount() != 3 {
		t.Fatalf("after Deploy changes the template's scaling is %v; want a maximum of 3", got.GetTemplate().GetScaling())
	}
	ctr := got.GetTemplate().GetContainers()[0]
	target := ""
	for _, e := range ctr.GetEnv() {
		if e.GetName() == "TARGET" {
			target = e.GetValue()
		}
	}
	if ctr.GetImage() != image || target != "tabs" {
		t.Errorf("the Scaling tab's deploy changed the container: image %q, TARGET %q", ctr.GetImage(), target)
	}
	waitForRunService(t, project, id, image, 5*time.Minute)

	// Each revision's ⋮ menu: Manage traffic, Manage revision tags, and
	// Delete on the one that serves nothing now.
	p.navigate("/run/" + id + "?project=" + project + "&tab=revisions")
	p.waitFor(`document.querySelectorAll("#view tbody tr").length >= 2`)
	menu := func(row int) string {
		var items []string
		p.eval(fmt.Sprintf(`(() => { document.querySelectorAll("#view tbody tr")[%d].querySelector(".overflow-trigger").click();
			return [...document.querySelectorAll(".overflow-item")].filter((b) => b.getClientRects().length).map((b) => b.textContent.trim()); })()`, row), &items)
		p.eval(`(() => { document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true })); document.body.click(); return true; })()`, nil)
		return strings.Join(items, "|")
	}
	if got := menu(0); got != "Manage traffic|Manage revision tags" {
		t.Errorf("the serving revision's menu is %q; want Manage traffic and Manage revision tags", got)
	}
	if got := menu(1); got != "Manage traffic|Manage revision tags|Delete" {
		t.Errorf("the superseded revision's menu is %q; want Manage traffic, Manage revision tags and Delete", got)
	}

	// The Services list: Filter services > Deployment type > Container.
	p.navigate("/run?project=" + project)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, id))
	p.run(chromedp.Click(chromedp.CSS(`#view input.filter`)))
	p.clickText("#view .filter-menu-item", "Deployment type")
	p.clickText("#view .filter-menu-item", "Container")
	var chip string
	p.waitFor(`document.querySelectorAll("#view .filter-chips .chip").length === 1`)
	p.eval(`document.querySelector("#view .filter-chips .chip").textContent`, &chip)
	if chip != "Deployment type: Container" {
		t.Errorf("the chip reads %q, want Deployment type: Container", chip)
	}
	p.clickText("#view .filter-chips button", "Clear filters")
	// Clearing gives the filter focus, which opens its property menu over
	// the rows; leaving it closes the menu.
	p.eval(`(() => { document.activeElement.blur(); return true; })()`, nil)
	p.waitFor(`document.querySelector("#view .filter-menu").hidden`)

	// Copy opens the service copy page for the checked service: the
	// Create service page holding its settings, under a new name.
	p.run(chromedp.Click(chromedp.CSS(fmt.Sprintf(`#view tbody input[type=checkbox][aria-label="Select %s"]`, id))))
	p.clickText("#view button", "Copy")
	p.waitFor(`location.pathname === "/run/create" && document.querySelector("#view #f-name") !== null`)
	p.waitFor(fmt.Sprintf(`document.querySelector("#view #f-name").value === %q`, id+"-copy"))
	var copyPage struct{ Image, Max, From string }
	p.eval(`(() => ({ Image: document.querySelector("#view #f-image").value, Max: document.querySelector("#view #f-maxInstances").value,
		From: new URLSearchParams(location.search).get("from") }))()`, &copyPage)
	if copyPage.Image != image || copyPage.Max != "3" || copyPage.From != id {
		t.Errorf("the service copy page holds %+v; want %s's image and its maximum of 3", copyPage, id)
	}
	setField(p, "#view", "f-name", copyID)
	p.clickText(`#view button[type="submit"]`, "Create")
	waitForRunService(t, project, copyID, image, 5*time.Minute)
	cp, err := c.GetService(ctx, &runpb.GetServiceRequest{Name: "projects/" + project + "/locations/us-central1/services/" + copyID})
	if err != nil {
		t.Fatalf("read the copy: %v", err)
	}
	if cp.GetTemplate().GetScaling().GetMaxInstanceCount() != 3 || cp.GetTemplate().GetContainers()[0].GetImage() != image {
		t.Errorf("the copy's template is %v; want the checked service's", cp.GetTemplate())
	}

	// WCAG 2.1 AA on the new tabs and dialogs.
	q := "?project=" + project
	tab := func(id string) string {
		return `document.querySelector("#view #inline-edit-` + id + `-save") !== null`
	}
	auditScreens(t, []a11yPage{
		{"Cloud Run Containers tab", "/run/" + copyID + q + "&tab=containers", tab("containers"), ""},
		{"Cloud Run Scaling tab", "/run/" + copyID + q + "&tab=scaling", tab("scaling"), ""},
		{"Cloud Run Networking tab", "/run/" + copyID + q + "&tab=networking", tab("networking"), ""},
		{"Cloud Run Security tab", "/run/" + copyID + q + "&tab=security", tab("security"), ""},
		{"Cloud Run View diff & redeploy dialog", "/run/" + copyID + q + "&tab=scaling", tab("scaling"),
			`document.querySelector("#view #inline-edit-scaling-save").click()`},
		{"Cloud Run service copy page", "/run/create" + q + "&from=" + copyID,
			settled + fmt.Sprintf(` && document.querySelector("#view #f-name") !== null && document.querySelector("#view #f-name").value === %q`, copyID+"-copy"), ""},
	})
}
