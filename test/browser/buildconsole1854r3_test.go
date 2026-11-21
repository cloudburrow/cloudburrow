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
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestCloudRunBuildConsoleRound3ThroughThePages (#1854), in the run shard,
// whose Run port serves Cloud Build:
//   - a private pool's row has a trash icon, and its name opens Edit
//     private pool;
//   - History's column selector adds Trigger description and Artifacts,
//     off until chosen, and Reject rejects every checked build, which
//     GetBuild reads REJECTED with the dialog's message;
//   - a build's page has Build dependencies;
//   - the Dashboard's Build history is a bar per build, a card can be
//     pinned to the top, and Filter triggers narrows the cards;
//   - Settings has the Service account drop-down.
//
// Each of those pages passes the WCAG 2.1 AA audit.
func TestCloudRunBuildConsoleRound3ThroughThePages(t *testing.T) {
	needService(t, "cloud-build-worker-pools")
	needService(t, "cloud-build-triggers")
	needService(t, "cloud-build-dashboard")
	needService(t, "cloud-build-settings")
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envRun)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: what the pages do is read back through the Cloud Build API itself", envRun)
	}
	ctx := context.Background()
	c, err := cloudbuild.NewClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	project := uniqueProject(t)
	suffix := time.Now().UnixNano() % 1e6
	q := "?project=" + project

	// The pool's row: a trash icon named for it, no menu Delete.
	pool := fmt.Sprintf("browser-r3-%d", suffix)
	if code, body := consoleDo(t, http.MethodPost, "/api/resources/cloud-build-worker-pools"+q,
		`{"name":"`+pool+`","region":"us-central1","series":"e2","machineType-e2":"e2-standard-4"}`); code != http.StatusOK {
		t.Fatalf("create pool %s through the console API = %d: %s", pool, code, body)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/cloud-build-worker-pools"+q+"&name="+pool, "")
	})
	p := open(t)
	p.navigate("/cloud-build/worker-pools" + q)
	trash := fmt.Sprintf(`#view tbody button.row-delete-icon[aria-label="Delete %s"]`, pool)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, trash))
	var href string
	p.eval(fmt.Sprintf(`[...document.querySelectorAll("#view tbody a")].find((a) => a.textContent === %q).getAttribute("href")`, pool), &href)
	if !strings.Contains(href, "edit=1") {
		t.Errorf("the pool's name links to %q; want Edit private pool", href)
	}

	// A gated trigger with a description, run twice.
	name := fmt.Sprintf("browser-r3-%d", suffix)
	body, _ := json.Marshal(map[string]string{"name": name, "region": "global", "event": "Manual invocation",
		"description": "ships the web tier", "config": "Cloud Build configuration file (yaml or json)", "location": "Inline",
		"approval": "true", "inline": "steps:\n- name: alpine:3.20\n  entrypoint: sh\n  args: ['-c', 'echo gated']\n"})
	if code, out := consoleDo(t, http.MethodPost, "/api/resources/cloud-build-triggers"+q, string(body)); code != http.StatusOK {
		t.Fatalf("create trigger %s through the console API = %d: %s", name, code, out)
	}
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/cloud-build-triggers"+q+"&name="+name, "")
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

	// History: the optional columns are off until chosen.
	p.navigate("/cloud-build/builds" + q)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, fmt.Sprintf(`input[type=checkbox][aria-label="Select %s"]`, ids[0])))
	heads := func() string {
		var h string
		p.eval(`[...document.querySelectorAll("#view thead th")].map((h) => h.textContent.trim()).join(",")`, &h)
		return h
	}
	if h := heads(); strings.Contains(h, "Trigger description") || strings.Contains(h, "Artifacts") {
		t.Errorf("History's columns are %q; Trigger description and Artifacts are the column selector's", h)
	}
	p.run(chromedp.Click(chromedp.CSS(`#view button.view-options`)))
	p.waitFor(`document.querySelector("#col-Trigger-description") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#col-Trigger-description`)))
	p.waitFor(`[...document.querySelectorAll("#view thead th")].some((h) => h.textContent.trim() === "Trigger description")`)
	var cell bool
	p.eval(`[...document.querySelectorAll("#view tbody td")].some((d) => d.textContent === "ships the web tier")`, &cell)
	if !cell {
		t.Error("History's Trigger description column does not show the trigger's description")
	}

	// Reject at the top of the page, over the checked builds.
	for _, id := range ids {
		p.run(chromedp.Click(chromedp.CSS(fmt.Sprintf(`input[type=checkbox][aria-label="Select %s"]`, id))))
	}
	p.clickText("#view button", "Reject")
	p.submitForm(map[string]string{"comment": "rejected together"}, "comment")
	for _, id := range ids {
		var b *cbpb.Build
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
			if b, err = c.GetBuild(ctx, &cbpb.GetBuildRequest{ProjectId: project, Id: id}); err == nil &&
				b.GetApproval().GetState() == cbpb.BuildApproval_REJECTED {
				break
			}
		}
		if b.GetApproval().GetState() != cbpb.BuildApproval_REJECTED || b.GetApproval().GetResult().GetComment() != "rejected together" {
			t.Errorf("build %s after History's Reject = %v, %v", id, b.GetApproval(), err)
		}
	}

	// A build's page has Build dependencies, between Execution details
	// and Build artifacts.
	buildPage := "/cloud-build/builds/" + ids[0] + q
	p.navigate(buildPage)
	p.waitFor(`document.querySelector("#tab-dependencies") !== null`)
	var tabs string
	p.eval(`[...document.querySelectorAll("#view [role=tab]")].map((t) => t.textContent).join(",")`, &tabs)
	if !strings.Contains(tabs, "Execution details,Build dependencies,Build artifacts") {
		t.Errorf("the build's tabs are %q", tabs)
	}

	// The Dashboard: bars, a pin, Filter triggers.
	p.navigate("/cloud-build/dashboard" + q)
	card := fmt.Sprintf(`[...document.querySelectorAll(".summary-cards .listing-summary")].find((c) => c.querySelector("h2").textContent.includes(%q))`, name)
	p.waitFor(card + ` !== undefined`)
	var bars int
	p.eval(card+`.querySelectorAll(".bar-chart-bar").length`, &bars)
	if bars != 2 {
		t.Errorf("the card's Build history has %d bars; want one per build", bars)
	}
	var label string
	p.eval(card+`.querySelector(".bar-chart-bar").getAttribute("aria-label")`, &label)
	if !strings.Contains(label, ", ") {
		t.Errorf("a bar says %q; want its outcome and time", label)
	}
	p.eval(card+`.querySelector("button.card-pin").click()`, nil)
	p.waitFor(card + `.querySelector("button.card-pin").getAttribute("aria-pressed") === "true"`)
	p.navigate("/cloud-build/dashboard" + q)
	p.waitFor(card + ` !== undefined`)
	var pinned bool
	p.eval(card+`.querySelector("button.card-pin").getAttribute("aria-pressed") === "true"`, &pinned)
	if !pinned {
		t.Error("the pin was not kept across a reload")
	}
	p.setField("#card-filter", "no-such-trigger-anywhere")
	p.waitFor(card + ` === undefined`)
	p.setField("#card-filter", "web tier")
	p.waitFor(card + ` !== undefined`)

	// Settings: the Service account drop-down, the default first.
	p.navigate("/cloud-build/settings" + q)
	p.waitFor(`document.querySelector("#scope-serviceAccount option") !== null`)
	var first string
	p.eval(`document.querySelector("#scope-serviceAccount option").textContent`, &first)
	if !strings.Contains(first, "(default)") {
		t.Errorf("the Service account drop-down begins with %q; want the default build account", first)
	}

	auditScreens(t, []a11yPage{
		{"Worker pools", "/cloud-build/worker-pools" + q, settled, ""},
		{"Edit private pool", "/cloud-build/worker-pools/" + pool + q + "&edit=1", settled, ""},
		{"History", "/cloud-build/builds" + q, settled, ""},
		{"Build dependencies", buildPage + "&tab=dependencies", settled, ""},
		{"Dashboard", "/cloud-build/dashboard" + q, settled, ""},
		{"Settings", "/cloud-build/settings" + q, settled, ""},
	})
}
