//go:build browser

package browser

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	redisadmin "cloud.google.com/go/redis/apiv1"
	"cloud.google.com/go/redis/apiv1/redispb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// editPage takes the resource page's Edit, which is a page of its own
// (#1862, Google's Edit Redis instance page), sets values on it, saves with
// Save, and waits to be back on the resource's page.
func (p *tab) editPage(values map[string]string) {
	p.t.Helper()
	p.waitFor(`document.querySelector("#detail-edit") !== null`)
	p.run(chromedp.Click(chromedp.CSS("#detail-edit")))
	p.waitFor(`document.querySelector("form.edit-page") !== null && document.querySelector(".modal.is-open") === null`)
	for id, v := range values {
		p.eval(fmt.Sprintf(`(() => { const f = document.querySelector("form.edit-page #f-%s");
			if (f.type === "checkbox") { f.checked = %q === "true"; f.dispatchEvent(new Event("change", { bubbles: true })); return true; }
			f.value = %q; f.dispatchEvent(new Event("input", { bubbles: true })); f.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, id, v, v), nil)
	}
	p.run(chromedp.Click(chromedp.CSS("#edit-page-save")))
	p.waitForWithin(`document.querySelector("form.edit-page") === null ||
		[...document.querySelectorAll("form.edit-page .form-error")].some((e) => !e.hidden && e.textContent)`, 3*time.Minute)
	var refused string
	p.eval(`[...document.querySelectorAll("form.edit-page .form-error")].filter((e) => !e.hidden).map((e) => e.textContent).join(" ")`, &refused)
	if refused != "" {
		p.t.Fatalf("Save was refused: %s", refused)
	}
	p.waitFor(`!location.search.includes("edit=1") && document.querySelector("#detail-edit") !== null`)
}

// TestMemorystoreRedisEditPageThroughThePage (#1862), in the memorystore
// shard. Create a Redis instance's Advanced network options choose Private
// services access and an IP range, which CreateInstance's
// connect_mode and reserved_ip_range keep; the instance's page says
// "Private services access" and "No persistence", not the API's enums.
// Edit is a link to Google's Edit Redis instance page, not a dialog: it is
// headed "Edit Redis instance", with the instance's crumbs and a back
// arrow, ends in Save and Cancel, offers its choices in words, meets WCAG,
// and Save sends UpdateInstance and goes back to the instance's page.
func TestMemorystoreRedisEditPageThroughThePage(t *testing.T) {
	needService(t, "memorystore")
	addr := strings.TrimSpace(os.Getenv(envMemorystoreAdmin))
	project := strings.TrimSpace(os.Getenv(envMemorystoreProject))
	if addr == "" || project == "" {
		t.Skipf("%s and %s are not set: every change is read back through the admin API itself", envMemorystoreAdmin, envMemorystoreProject)
	}
	ctx := context.Background()
	c, err := redisadmin.NewCloudRedisClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	id := fmt.Sprintf("br-edit-%d", time.Now().UnixNano()%1e8)
	// A /29 of its own, outside the default network's subnets (10.128.0.0/9).
	ipRange := fmt.Sprintf("192.168.%d.%d/29", time.Now().UnixNano()%250, 8*(time.Now().UnixNano()/1000%30))
	name := "projects/" + project + "/locations/us-central1/instances/" + id
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(context.Background(), &redispb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = op.Wait(context.Background())
		}
	})

	p := open(t)
	q := "?project=" + project
	p.navigate("/memorystore" + q)
	p.waitFor(`[...document.querySelectorAll("#view button")].some((b) => b.textContent === "Create instance")`)
	p.clickText("#view button", "Create instance")
	p.run(chromedp.WaitVisible(chromedp.CSS(`form.create-page #f-id`)))
	var headings string
	p.eval(`[...document.querySelectorAll("form.create-page .form-section-title")].map((h) => h.textContent).join(",")`, &headings)
	if !strings.Contains(headings, "Advanced network options") {
		t.Errorf("Create a Redis instance's headings are %q; want Advanced network options", headings)
	}
	p.setField("form.create-page #f-id", id)
	p.clickText("form.create-page label", "Private services access")
	p.setField("form.create-page #f-reservedIpRange", ipRange)
	p.run(chromedp.Click(chromedp.CSS(`form.create-page button[type="submit"]`)))
	p.waitForWithin(fmt.Sprintf(`(document.querySelector("form.create-page") === null && document.querySelector("#view").textContent.includes(%q)) ||
		[...document.querySelectorAll("form.create-page .form-error")].some((e) => !e.hidden && e.textContent)`, id), 3*time.Minute)
	var refused string
	p.eval(`[...document.querySelectorAll("form.create-page .form-error")].filter((e) => !e.hidden).map((e) => e.textContent).join(" ")`, &refused)
	if refused != "" {
		t.Fatalf("Create instance was refused: %s", refused)
	}
	inst, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
	if err != nil || inst.GetConnectMode() != redispb.Instance_PRIVATE_SERVICE_ACCESS || inst.GetReservedIpRange() != ipRange {
		t.Fatalf("after Create instance, GetInstance = %v, %v; want private services access over %s", inst, err, ipRange)
	}

	page := "/memorystore/" + id + q
	p.navigate(page)
	p.waitFor(`document.querySelector("#detail-edit") !== null`)
	text := memorystoreTabsText(p)
	for _, want := range []string{"Private services access", ipRange, "No persistence"} {
		if !strings.Contains(text, want) {
			t.Errorf("the instance's page does not say %q", want)
		}
	}
	for _, enum := range []string{"PRIVATE_SERVICE_ACCESS", "DIRECT_PEERING"} {
		if strings.Contains(text, enum) {
			t.Errorf("the instance's page shows the API's %s", enum)
		}
	}

	// Edit is a link to the page, not a dialog.
	var link struct {
		Tag, Href string
	}
	p.eval(`(() => { const a = document.querySelector("#detail-edit"); return { Tag: a.tagName, Href: a.getAttribute("href") }; })()`, &link)
	if link.Tag != "A" || !strings.Contains(link.Href, "edit=1") {
		t.Errorf("Edit = %+v; want a link to the Edit Redis instance page", link)
	}
	p.run(chromedp.Click(chromedp.CSS("#detail-edit")))
	p.waitFor(`document.querySelector("form.edit-page") !== null`)
	var form struct {
		Title, Crumbs, Buttons, Persistence string
		Modal, Back                         bool
	}
	p.eval(`(() => ({
		Title: document.querySelector("#view h1").textContent,
		Crumbs: [...document.querySelectorAll("#view .breadcrumb > *")].map((e) => e.textContent).join(""),
		Buttons: [...document.querySelectorAll("form.edit-page .form-actions > *")].map((e) => e.textContent).join(","),
		Persistence: [...document.querySelectorAll("form.edit-page #f-persistence option")].map((o) => o.textContent).join(","),
		Modal: document.querySelector(".modal.is-open") !== null,
		Back: document.querySelector("#view .page-title-row a") !== null,
	}))()`, &form)
	if form.Title != "Edit Redis instance" || !strings.HasSuffix(form.Crumbs, id+"/Edit Redis instance") || form.Buttons != "Save,Cancel" || form.Modal || !form.Back {
		t.Errorf("the Edit page = %+v; want Edit Redis instance under the instance's crumbs, a back arrow, Save and Cancel, and no dialog", form)
	}
	if !strings.HasPrefix(form.Persistence, "No persistence,RDB snapshot every hour") {
		t.Errorf("Persistence offers %q; want its choices in words", form.Persistence)
	}

	// Cancel goes back without saving.
	p.clickText("form.edit-page .form-actions a", "Cancel")
	p.waitFor(`!location.search.includes("edit=1") && document.querySelector("#detail-edit") !== null`)

	p.navigate(page)
	p.editPage(map[string]string{"displayName": "renamed on the edit page"})
	got, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
	if err != nil || got.GetDisplayName() != "renamed on the edit page" {
		t.Errorf("after Save, GetInstance = %v, %v; want the new display name", got, err)
	}

	// WCAG 2.2 AA on the Edit page and on the instance's page.
	auditScreens(t, []a11yPage{
		{"Memorystore Redis instance", page, settled + ` && document.querySelector("#detail-edit") !== null`, ""},
		{"Edit Redis instance page", "/memorystore/" + id + q + "&edit=1", settled + ` && document.querySelector("form.edit-page") !== null`, ""},
	})
}
