//go:build browser

package browser

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"github.com/chromedp/chromedp"
)

// ssmCreateRepositoryPage creates a repository through the web interface's
// Create repository page (#1856), from the instance's page p is on: the
// button opens ?form=create-repo, values fill its fields, and Create
// repository goes on to the new repository's page.
func ssmCreateRepositoryPage(p *tab, inst string, values map[string]string) {
	p.t.Helper()
	p.clickText("#view .section-actions button, #view .page-actions button", "Create repository")
	p.waitFor(`new URLSearchParams(location.search).get("form") === "create-repo" && document.querySelector("#action-page-form #f-id") !== null`)
	for id, v := range values {
		setField(p, "#action-page-form", "f-"+id, v)
	}
	p.run(chromedp.Click(chromedp.CSS(`#action-page-form button[type="submit"]`)))
	p.waitFor(fmt.Sprintf(`location.pathname === %q || [...document.querySelectorAll("#action-page-form .form-error")].some((e) => !e.hidden && e.textContent)`,
		"/secure-source-manager/"+inst+"/"+values["id"]))
	var refused string
	p.eval(`[...document.querySelectorAll("#action-page-form .form-error")].filter((e) => !e.hidden).map((e) => e.textContent).join(" ")`, &refused)
	if refused != "" {
		p.t.Fatalf("Create repository was refused: %s", refused)
	}
}

// TestCloudRunSourceManagerPrivateInstanceAndCreateRepositoryPage (#1856),
// in the run shard, whose Run port serves Secure Source Manager:
//   - Create instance's Private instance can be chosen, and reveals the CA
//     pool; Create makes the private instance GetInstance reads with its
//     private_config and service attachments, and its page shows them;
//   - Create repository is the web interface's page of its own (the
//     instance's page with ?form=create-repo, headed Create repository,
//     with a breadcrumb back to the instance and Initialize repository
//     drawn disabled): Cancel goes back to the instance, and Create
//     repository makes the repository GetRepository reads and opens its
//     page.
//
// The Create instance page with Private instance chosen, the private
// instance's page and the Create repository page pass the WCAG 2.1 AA
// audit.
func TestCloudRunSourceManagerPrivateInstanceAndCreateRepositoryPage(t *testing.T) {
	c := ssmClient(t)
	ctx := context.Background()
	project := uniqueProject(t)
	inst := fmt.Sprintf("browser-priv-%d", time.Now().UnixNano()%1e6)
	loc := "projects/" + project + "/locations/us-central1"
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(context.Background(), &ssmpb.DeleteInstanceRequest{Name: loc + "/instances/" + inst, Force: true}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	caPool := "projects/" + project + "/locations/us-central1/caPools/source"

	p := open(t)
	p.navigate("/secure-source-manager/create?project=" + project)
	p.waitFor(`document.querySelector("#view #f-private") !== null`)
	var state struct {
		Disabled, PoolHidden bool
	}
	p.eval(`(() => ({ Disabled: document.querySelector("#view #f-private").disabled,
		PoolHidden: document.querySelector("#view #f-caPool").closest(".form-row").hidden }))()`, &state)
	if state.Disabled || !state.PoolHidden {
		t.Errorf("Create instance draws Private instance %+v; want it choosable and the CA pool hidden until it is", state)
	}
	setField(p, "#view", "f-private", "true")
	p.eval(`document.querySelector("#view #f-caPool").closest(".form-row").hidden`, &state.PoolHidden)
	if state.PoolHidden {
		t.Error("Private instance does not reveal the CA pool")
	}
	setField(p, "#view", "f-name", inst)
	setField(p, "#view", "f-caPool", caPool)
	createPage := "/secure-source-manager/create?project=" + project
	p.run(chromedp.Click(chromedp.CSS(`#view button[type="submit"]`)))
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, inst))
	in, err := c.GetInstance(ctx, &ssmpb.GetInstanceRequest{Name: loc + "/instances/" + inst})
	if err != nil || !in.GetPrivateConfig().GetIsPrivate() || in.GetPrivateConfig().GetCaPool() != caPool ||
		in.GetPrivateConfig().GetHttpServiceAttachment() == "" || in.GetPrivateConfig().GetSshServiceAttachment() == "" {
		t.Fatalf("GetInstance after Create instance with Private instance = %v, %v", in, err)
	}

	instPage := "/secure-source-manager/" + inst + "?project=" + project
	p.navigate(instPage)
	p.waitFor(settled + ` && document.querySelector("#view").textContent.includes("HTTP service attachment")`)
	var text string
	p.eval(`document.querySelector("#view").textContent`, &text)
	for _, want := range []string{"Private instance", "Yes (Private Service Connect)", "CA pool", caPool,
		in.GetPrivateConfig().GetHttpServiceAttachment(), "SSH service attachment", in.GetPrivateConfig().GetSshServiceAttachment()} {
		if !strings.Contains(text, want) {
			t.Errorf("the private instance's page lacks %q", want)
		}
	}

	// Create repository is a page: Cancel goes back to the instance.
	p.clickText("#view .section-actions button, #view .page-actions button", "Create repository")
	p.waitFor(`new URLSearchParams(location.search).get("form") === "create-repo" && document.querySelector("#action-page-form") !== null`)
	var pg struct {
		Heading, Crumb, Submit string
		Modal, InitOff         bool
	}
	p.eval(`(() => ({ Heading: document.querySelector("#view h1, #view h2").textContent.trim(),
		Crumb: document.querySelector("#view .breadcrumb").textContent,
		Submit: document.querySelector("#action-page-form button[type=submit]").textContent.trim(),
		Modal: document.querySelector(".modal") !== null,
		InitOff: document.querySelector("#action-page-form #f-initialize").disabled }))()`, &pg)
	if pg.Heading != "Create repository" || !strings.Contains(pg.Crumb, inst) || pg.Submit != "Create repository" || pg.Modal || !pg.InitOff {
		t.Errorf("the Create repository page = %+v", pg)
	}
	p.clickText("#action-page-form a", "Cancel")
	p.waitFor(fmt.Sprintf(`location.pathname === %q && !new URLSearchParams(location.search).has("form")`, "/secure-source-manager/"+inst))

	ssmCreateRepositoryPage(p, inst, map[string]string{"id": "site", "description": "made on its page", "branch": "trunk"})
	r, err := c.GetRepository(ctx, &ssmpb.GetRepositoryRequest{Name: loc + "/repositories/site"})
	if err != nil || r.GetDescription() != "made on its page" || r.GetInitialConfig().GetDefaultBranch() != "trunk" {
		t.Fatalf("GetRepository after the Create repository page = %v, %v", r, err)
	}
	p.waitFor(fmt.Sprintf(`document.querySelector("#view").textContent.includes(%q)`, r.GetUris().GetGitHttps()))

	auditScreens(t, []a11yPage{
		{"create private instance", createPage, settled + ` && document.querySelector("#view #f-private") !== null`,
			`(() => { const f = document.querySelector("#view #f-private"); f.checked = true; f.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`},
		{"private instance", instPage, settled, ""},
		{"create repository page", "/secure-source-manager/" + inst + "?project=" + project + "&form=create-repo",
			settled + ` && document.querySelector("#action-page-form") !== null`, ""},
	})
}
