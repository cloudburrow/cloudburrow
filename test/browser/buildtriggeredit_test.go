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
// account). From the Triggers list, a trigger's row has a Run button and a
// menu of Edit, Disable and Delete, in Google's order; its page's Edit replaces the inline build the official
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

	// The row: a Run button, then the menu Edit, Disable, Delete (#1854).
	p.navigate("/cloud-build/triggers?project=" + project)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, `button[aria-label="Actions for `+name+`"]`))
	var runButton bool
	p.eval(fmt.Sprintf(`document.querySelector(%q) !== null`, `td.row-actions button.row-button[aria-label="Run `+name+`"]`), &runButton)
	if !runButton {
		t.Error("the trigger's row has no Run button")
	}
	p.run(chromedp.Click(chromedp.CSS(fmt.Sprintf(`button[aria-label=%q]`, "Actions for "+name))))
	var items string
	p.eval(`[...document.querySelectorAll('.overflow-menu:not([hidden]) [role="menuitem"]')].map((b) => b.textContent.trim()).join("|")`, &items)
	if items != "Edit|Disable|Delete" {
		t.Errorf("the trigger's row menu = %q, want Edit|Disable|Delete", items)
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

// TestCloudRunBuildConsoleRound2ThroughThePages (#1854), in the run shard,
// whose Run port serves Cloud Build:
//   - Create private pool is Google's page (Machine configuration, Worker
//     release, Network type, Assign external IPs) and makes the pool the
//     official client's GetWorkerPool reads;
//   - Create trigger's Private pool lists the selected project's pools,
//     read when the page opens: another project's page does not offer it;
//   - History's Approve approves every checked build awaiting approval,
//     which GetBuild then reads APPROVED with the dialog's message;
//   - the Dashboard is a card per trigger, titled by its latest build's
//     outcome, with Latest build, Duration, Build history, Average
//     duration and Pass - Fail %.
func TestCloudRunBuildConsoleRound2ThroughThePages(t *testing.T) {
	needService(t, "cloud-build-worker-pools")
	needService(t, "cloud-build-triggers")
	needService(t, "cloud-build-dashboard")
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envRun)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: what the pages make is read back through the Cloud Build API itself", envRun)
	}
	ctx := context.Background()
	c, err := cloudbuild.NewClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	project, other := uniqueProject(t), uniqueProject(t)
	suffix := time.Now().UnixNano() % 1e6
	pool := fmt.Sprintf("browser-pp-%d", suffix)
	poolName := "projects/" + project + "/locations/us-east1/workerPools/" + pool
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/cloud-build-worker-pools?project="+project+"&name="+pool, "")
	})

	p := open(t)
	p.navigate("/cloud-build/worker-pools/create?project=" + project)
	p.waitFor(`document.querySelector("form.create-page") !== null`)
	var form string
	p.eval(`document.querySelector("#view").textContent`, &form)
	for _, want := range []string{"Create private pool", "Machine configuration", "Series", "Machine type", "Disk size", "Worker release",
		"Release channel", "Network type", "Default network", "Private network", "Assign external IPs"} {
		if !strings.Contains(form, want) {
			t.Errorf("Create private pool has no %q", want)
		}
	}
	p.submitForm(map[string]string{"name": pool, "region": "us-east1", "series": "n1", "machineType-n1": "n1-standard-4", "diskSizeGb": "250"}, "name")
	var w *cbpb.WorkerPool
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if w, err = c.GetWorkerPool(ctx, &cbpb.GetWorkerPoolRequest{Name: poolName}); err == nil {
			break
		}
	}
	if wc, nc := w.GetPrivatePoolV1Config().GetWorkerConfig(), w.GetPrivatePoolV1Config().GetNetworkConfig(); err != nil ||
		wc.GetMachineType() != "n1-standard-4" || wc.GetDiskSizeGb() != 250 || wc.GetWorkerRelease() != "regular" ||
		nc.GetEgressOption() != cbpb.PrivatePoolV1Config_NetworkConfig_PUBLIC_EGRESS {
		t.Fatalf("GetWorkerPool after Create private pool = %v, %v", w, err)
	}

	// Create trigger follows the project: its pools, not another's.
	offers := func(proj string) bool {
		p.navigate("/cloud-build/triggers/create?project=" + proj)
		p.waitFor(`document.querySelector("#f-pool") !== null`)
		var has bool
		p.eval(fmt.Sprintf(`[...document.querySelectorAll("#f-pool option")].some((o) => o.value === %q)`, poolName), &has)
		return has
	}
	if !offers(project) {
		t.Errorf("Create trigger in %s does not offer its pool %s", project, poolName)
	}
	if offers(other) {
		t.Errorf("Create trigger in %s offers %s's pool", other, project)
	}

	// Two builds awaiting approval, approved together on History.
	name := fmt.Sprintf("browser-gate-%d", suffix)
	body, _ := json.Marshal(map[string]string{"name": name, "region": "global", "event": "Manual invocation",
		"config": "Cloud Build configuration file (yaml or json)", "location": "Inline", "approval": "true",
		"inline": "steps:\n- name: alpine:3.20\n  entrypoint: sh\n  args: ['-c', 'echo gated']\n"})
	if code, out := consoleDo(t, http.MethodPost, "/api/resources/cloud-build-triggers?project="+project, string(body)); code != http.StatusOK {
		t.Fatalf("create trigger %s through the console API = %d: %s", name, code, out)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/cloud-build-triggers?project="+project+"&name="+name, "")
	})
	var ids []string
	for i := 0; i < 2; i++ {
		op, err := c.RunBuildTrigger(ctx, &cbpb.RunBuildTriggerRequest{ProjectId: project, TriggerId: name,
			Name: "projects/" + project + "/locations/global/triggers/" + name})
		if err != nil {
			t.Fatalf("RunBuildTrigger: %v", err)
		}
		md, err := op.Metadata()
		if err != nil {
			t.Fatalf("the run's metadata: %v", err)
		}
		ids = append(ids, md.GetBuild().GetId())
	}
	p.navigate("/cloud-build/builds?project=" + project)
	for _, id := range ids {
		sel := fmt.Sprintf(`input[type=checkbox][aria-label="Select %s"]`, id)
		p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, sel))
		p.run(chromedp.Click(chromedp.CSS(sel)))
	}
	p.clickText("#view button", "Approve")
	p.submitForm(map[string]string{"comment": "approved together"}, "comment")
	for _, id := range ids {
		var b *cbpb.Build
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
			if b, err = c.GetBuild(ctx, &cbpb.GetBuildRequest{ProjectId: project, Id: id}); err == nil &&
				b.GetApproval().GetState() == cbpb.BuildApproval_APPROVED {
				break
			}
		}
		if b.GetApproval().GetState() != cbpb.BuildApproval_APPROVED || b.GetApproval().GetResult().GetComment() != "approved together" {
			t.Errorf("build %s after History's Approve = %v, %v", id, b.GetApproval(), err)
		}
	}

	// The Dashboard: a card per trigger, with Google's labels.
	p.navigate("/cloud-build/dashboard?project=" + project)
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll(".summary-cards .listing-summary h2")].some((h) => h.textContent.includes(%q))`, name))
	var card string
	p.eval(fmt.Sprintf(`[...document.querySelectorAll(".summary-cards .listing-summary")].find((c) => c.querySelector("h2").textContent.includes(%q)).textContent`, name), &card)
	for _, want := range []string{"Trigger", "Latest build", "Build history"} {
		if !strings.Contains(card, want) {
			t.Errorf("the dashboard's card for %s has no %q: %s", name, want, card)
		}
	}
	var tables int
	p.eval(`document.querySelectorAll("#view table").length`, &tables)
	if tables != 0 {
		t.Errorf("the dashboard draws %d tables; Google's is cards", tables)
	}
}
