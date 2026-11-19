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

	cloudbuild "cloud.google.com/go/cloudbuild/apiv1/v2"
	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestCloudRunBuildTriggerEditAndDisableThroughThePage, in the run shard,
// whose Run port serves Cloud Build. Cloud Build is its own product
// (#1854): its menu is Google's (Dashboard, History, Triggers,
// Repositories, Worker pools, Settings) and Cloud Run's old Build history
// address leads to History. Create trigger is Google's single page (Event,
// Source, Configuration, Substitution variables, Approval, Service
// account). From the Triggers list, a trigger's row menu has Run, Disable,
// Edit and Delete; its page's Edit replaces the inline build the official
// client's GetBuildTrigger then reads, and Disable makes it disabled.
func TestCloudRunBuildTriggerEditAndDisableThroughThePage(t *testing.T) {
	needService(t, "cloud-build-triggers")
	needService(t, "cloud-build-dashboard")
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envRun)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: the trigger is read back through the Cloud Build API itself", envRun)
	}
	ctx := context.Background()
	c, err := cloudbuild.NewClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	project := uniqueProject(t)
	name := fmt.Sprintf("browser-trig-%d", time.Now().UnixNano()%1e6)
	body, _ := json.Marshal(map[string]string{"name": name, "region": "global", "description": "first", "event": "Manual invocation",
		"config": "Cloud Build configuration file (yaml or json)", "location": "Inline",
		"inline": "steps:\n- name: alpine:3.20\n  entrypoint: sh\n  args: ['-c', 'echo one']\n", "substitutions": `{"_GREETING":"hi"}`})
	if code, out := consoleDo(t, http.MethodPost, "/api/resources/cloud-build-triggers?project="+project, string(body)); code != http.StatusOK {
		t.Fatalf("create trigger %s through the console API = %d: %s", name, code, out)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/cloud-build-triggers?project="+project+"&name="+name, "")
	})
	get := func() *cbpb.BuildTrigger {
		t.Helper()
		got, err := c.GetBuildTrigger(ctx, &cbpb.GetBuildTriggerRequest{ProjectId: project, TriggerId: name,
			Name: "projects/" + project + "/locations/global/triggers/" + name})
		if err != nil {
			t.Fatalf("GetBuildTrigger %s: %v", name, err)
		}
		return got
	}
	if got := get(); got.GetSubstitutions()["_GREETING"] != "hi" || len(got.GetBuild().GetSteps()) != 1 {
		t.Fatalf("GetBuildTrigger after Create trigger = %v; want the inline build and its substitution", got)
	}

	p := open(t)
	// The product's menu, in Google's order, and History's old address.
	p.navigate("/cloud-build/dashboard?project=" + project)
	p.waitFor(fmt.Sprintf(`document.querySelector("#view").textContent.includes(%q)`, name))
	var menu string
	p.eval(`[...document.querySelectorAll("#product-nav a")].map((a) => a.textContent.trim()).filter(Boolean).join("|")`, &menu)
	if !strings.Contains(menu, "Dashboard|History|Triggers|Repositories|Worker pools|Settings") {
		t.Errorf("Cloud Build's menu = %q; want Dashboard, History, Triggers, Repositories, Worker pools, Settings", menu)
	}
	p.navigate("/run/builds?project=" + project)
	p.waitFor(`location.pathname === "/cloud-build/builds"`)

	// Create trigger: Google's sections and choices, Pull request drawn
	// disabled.
	p.navigate("/cloud-build/triggers/create?project=" + project)
	p.waitFor(`document.querySelector("form.create-page") !== null`)
	var form string
	p.eval(`document.querySelector("form.create-page").textContent`, &form)
	for _, want := range []string{"Event", "Push to a branch", "Push new tag", "Pull request", "Manual invocation", "Pub/Sub message",
		"Webhook event", "Source", "Configuration", "Autodetected", "Dockerfile", "Buildpacks", "Substitution variables",
		"Require approval before build executes", "Service account"} {
		if !strings.Contains(form, want) {
			t.Errorf("Create trigger has no %q", want)
		}
	}
	var prDisabled bool
	p.eval(`[...document.querySelectorAll("form.create-page input[type=radio]")].some((r) => r.value === "Pull request" && r.disabled)`, &prDisabled)
	if !prDisabled {
		t.Error("Create trigger's Pull request is not drawn disabled")
	}

	// The row menu: Run, Disable, Edit, Delete.
	p.navigate("/cloud-build/triggers?project=" + project)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, `button[aria-label="Actions for `+name+`"]`))
	p.run(chromedp.Click(chromedp.CSS(fmt.Sprintf(`button[aria-label=%q]`, "Actions for "+name))))
	var items string
	p.eval(`[...document.querySelectorAll('.overflow-menu:not([hidden]) [role="menuitem"]')].map((b) => b.textContent.trim()).join("|")`, &items)
	if items != "Run|Disable|Edit|Delete" {
		t.Errorf("the trigger's row menu = %q, want Run|Disable|Edit|Delete", items)
	}
	p.run(chromedp.KeyEvent(kb.Escape))

	p.clickText("#view tbody a", name)
	p.waitFor(fmt.Sprintf(`location.pathname === %q`, "/cloud-build/triggers/"+name))
	p.pageAction("Edit", map[string]string{"description": "edited in the browser",
		"inline": "steps:\n- name: busybox:1.36\n  entrypoint: sh\n  args: ['-c', 'echo two']\n"}, "")
	got := get()
	if s := got.GetBuild().GetSteps(); got.GetDescription() != "edited in the browser" || len(s) != 1 || s[0].GetName() != "busybox:1.36" ||
		len(s[0].GetArgs()) != 2 || s[0].GetArgs()[1] != "echo two" {
		t.Errorf("GetBuildTrigger after Edit = %v; want the new description and step", got)
	}
	p.clickText("#view .page-actions button", "Disable")
	p.confirmByName(name)
	p.waitFor(`document.querySelector(".modal") === null`)
	for deadline := time.Now().Add(10 * time.Second); !get().GetDisabled() && time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
	}
	if !get().GetDisabled() {
		t.Error("GetBuildTrigger after Disable: the trigger is not disabled")
	}

	// Repositories' 2nd gen and 1st gen tabs, and Settings' Service
	// account permissions.
	p.navigate("/cloud-build/repositories?project=" + project)
	p.waitFor(`document.querySelector("#view").textContent.includes("2nd gen") && document.querySelector("#view").textContent.includes("1st gen")`)
	p.navigate("/cloud-build/settings?project=" + project)
	p.waitFor(`document.querySelector("#view").textContent.includes("Service account permissions") &&
		document.querySelector("#view").textContent.includes("Cloud Run Admin")`)
}

// TestCloudRunBuildWorkerPoolEditThroughThePage (#1491), in the run shard.
// A pool had no page and was changed only with gcloud: from the Worker
// pools list, a pool's page opens, and Edit worker pool changes the display
// name, machine type and disk the official client's GetWorkerPool then
// reads. The Triggers page names the default build service account.
func TestCloudRunBuildWorkerPoolEditThroughThePage(t *testing.T) {
	needService(t, "cloud-build-worker-pools")
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envRun)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: the pool is read back through the Cloud Build API itself", envRun)
	}
	ctx := context.Background()
	c, err := cloudbuild.NewClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	project := uniqueProject(t)
	name := fmt.Sprintf("browser-pool-%d", time.Now().UnixNano()%1e6)
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/cloud-build-worker-pools?project="+project,
		`{"name":"`+name+`","region":"us-central1","machineType":"e2-standard-2"}`); code != http.StatusOK {
		t.Fatalf("create pool %s through the console API = %d: %s", name, code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/cloud-build-worker-pools?project="+project+"&name="+name, "")
	})

	p := open(t)
	p.navigate("/cloud-build/worker-pools?project=" + project)
	p.clickText("#view tbody a", name)
	p.waitFor(fmt.Sprintf(`location.pathname === %q`, "/cloud-build/worker-pools/"+name))
	p.pageAction("Edit worker pool", map[string]string{"displayName": "Browser builders", "machineType": "e2-highcpu-8", "diskSizeGb": "200"}, "")
	w, err := c.GetWorkerPool(ctx, &cbpb.GetWorkerPoolRequest{Name: "projects/" + project + "/locations/us-central1/workerPools/" + name})
	if wc := w.GetPrivatePoolV1Config().GetWorkerConfig(); err != nil || w.GetDisplayName() != "Browser builders" ||
		wc.GetMachineType() != "e2-highcpu-8" || wc.GetDiskSizeGb() != 200 {
		t.Errorf("GetWorkerPool after Edit worker pool = %v, %v; want the form's values", w, err)
	}

	// The default build service account is Create trigger's Service
	// account choice (#1854).
	p.navigate("/cloud-build/triggers/create?project=" + project)
	p.waitFor(`[...document.querySelectorAll("form.create-page select option")].some((o) => o.textContent.includes("gserviceaccount.com"))`)
}
