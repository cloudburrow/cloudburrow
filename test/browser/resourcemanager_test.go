//go:build browser

package browser

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	su "google.golang.org/api/serviceusage/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// The Resource Manager v3 API and the Service Usage API, which every
// instance serves, as the compat harness names them; the storage shard's
// browser step exports both.
const (
	envResourceManager = "CLOUDBURROW_TEST_RESOURCEMANAGER"
	envServiceUsage    = "CLOUDBURROW_TEST_SERVICEUSAGE"
)

func rmClientOptions(t *testing.T) []option.ClientOption {
	t.Helper()
	return []option.ClientOption{option.WithEndpoint(endpoint(t, envResourceManager)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
}

// navigateByDrawer goes to path the way someone would: the drawer, opened
// where it starts closed, then More products and the section's category,
// then the product's row. It fails when the drawer has no such row, which is
// what an unrouted screen looks like.
func (p *tab) navigateByDrawer(section, path, label string) {
	p.t.Helper()
	p.run(reducedMotion())
	p.navigate("/")
	p.waitFor(`document.querySelectorAll("#nav-list a").length > 0`)
	var state string
	p.eval(`document.documentElement.dataset.nav || ""`, &state)
	if state == "closed" {
		p.run(chromedp.Click("#nav-toggle", chromedp.ByQuery))
		p.waitFor(`document.documentElement.dataset.nav === "open"`)
	}
	expand := func(text string) {
		p.t.Helper()
		find := fmt.Sprintf(`[...document.querySelectorAll("#nav-list button.nav-group")].find((b) => b.textContent.trim() === %q)`, text)
		p.waitFor(find + ` !== undefined`)
		var open string
		p.eval(find+`.getAttribute("aria-expanded")`, &open)
		if open != "true" {
			p.clickText("#nav-list button.nav-group", text)
			p.waitFor(find + `.getAttribute("aria-expanded") === "true"`)
		}
	}
	expand("More products")
	expand(section)
	link := fmt.Sprintf(`[...document.querySelectorAll("#nav-list a")].find((a) => new URL(a.href).pathname === %q)`, path)
	var found struct {
		Present bool
		Text    string
	}
	p.eval(fmt.Sprintf(`(() => { const a = %s; return { Present: !!a, Text: a ? a.textContent.trim() : "" }; })()`, link), &found)
	if !found.Present {
		p.t.Fatalf("the drawer has no row for %s: the screen is not reachable from the console", path)
	}
	if !strings.Contains(found.Text, label) {
		p.t.Errorf("the drawer's row for %s reads %q; want %q", path, found.Text, label)
	}
	p.eval(fmt.Sprintf(`(() => { const a = %s; a.scrollIntoView({ block: "center" });
		document.querySelectorAll("[data-cb-click]").forEach((n) => n.removeAttribute("data-cb-click"));
		a.setAttribute("data-cb-click", ""); return true; })()`, link), nil)
	p.run(chromedp.Click(`[data-cb-click]`, chromedp.ByQuery))
	p.waitFor(fmt.Sprintf(`location.pathname === %q && document.querySelector("#view h1") !== null`, path))
}

// submitForm sets a create or action form's fields, by their names, and
// submits it, whether the form opened on its own page or in a dialog. Dialogs
// that closed stay in the page while they animate out, holding fields of the
// same IDs, so it waits until the one left is the open one.
func (p *tab) submitForm(values map[string]string, first string) {
	p.t.Helper()
	p.waitFor(fmt.Sprintf(`(() => { const m = document.querySelectorAll(".modal");
		const f = m.length === 0 ? document.querySelector("#f-%[1]s")
			: m.length === 1 && m[0].classList.contains("is-open") ? m[0].querySelector("#f-%[1]s") : null;
		return !!f && f.getClientRects().length > 0; })()`, first))
	var inDialog bool
	p.eval(`document.querySelector(".modal.is-open") !== null`, &inDialog)
	scope := ""
	if inDialog {
		scope = ".modal.is-open "
	}
	for id, v := range values {
		p.setField(scope+"#f-"+id, v)
	}
	p.eval(fmt.Sprintf(`(() => { document.querySelectorAll("[data-cb-click]").forEach((n) => n.removeAttribute("data-cb-click"));
		document.querySelector(%q).form.querySelector('button[type="submit"]').setAttribute("data-cb-click", ""); return true; })()`, scope+"#f-"+first), nil)
	p.run(chromedp.Click(`[data-cb-click]`, chromedp.ByQuery))
}

// TestResourceHierarchyAndTagsThroughTheDrawer, in the storage shard. The
// Resource hierarchy and Tags screens (#1294, #1295) had providers but no
// route. Reached from the drawer's Resource Manager row and the product's
// page list: Create folder there makes a folder the v3 Folders API lists
// under the organization, and Create tag key and Create tag value make a key
// and value the TagKeys and TagValues APIs read back.
func TestResourceHierarchyAndTagsThroughTheDrawer(t *testing.T) {
	needService(t, "hierarchy")
	needService(t, "tags")
	opts := rmClientOptions(t)
	ctx := context.Background()
	folders, err := resourcemanager.NewFoldersClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	orgs, err := resourcemanager.NewOrganizationsClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := resourcemanager.NewTagKeysClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	values, err := resourcemanager.NewTagValuesClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = folders.Close(); _ = orgs.Close(); _ = keys.Close(); _ = values.Close() })
	org, err := orgs.SearchOrganizations(ctx, &rmpb.SearchOrganizationsRequest{}).Next()
	if err != nil {
		t.Fatalf("SearchOrganizations: %v", err)
	}

	p := open(t)
	p.navigateByDrawer("Management tools", "/projects", "Resource Manager")
	// The product's pages, Projects, Resource hierarchy and Tags.
	var pages []string
	p.waitFor(`document.querySelectorAll("#product-nav a").length > 0`)
	p.eval(`[...document.querySelectorAll("#product-nav a")].map((a) => a.textContent.trim())`, &pages)
	for _, want := range []string{"Projects", "Resource hierarchy", "Tags"} {
		if !contains(pages, want) {
			t.Errorf("Resource Manager's pages are %q; want %s among them", pages, want)
		}
	}

	// Resource hierarchy: Create folder.
	p.clickText("#product-nav a", "Resource hierarchy")
	p.waitFor(`location.pathname === "/resource-hierarchy"`)
	folder := fmt.Sprintf("Browser %d", time.Now().UnixNano()%1e8)
	p.clickText("#view button", "Create folder")
	p.submitForm(map[string]string{"displayName": folder}, "displayName")
	p.waitFor(fmt.Sprintf(`location.pathname === "/resource-hierarchy" &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, folder))
	var made *rmpb.Folder
	it := folders.ListFolders(ctx, &rmpb.ListFoldersRequest{Parent: org.GetName()})
	for {
		f, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListFolders under %s: %v", org.GetName(), err)
		}
		if f.GetDisplayName() == folder {
			made = f
		}
	}
	if made == nil {
		t.Fatalf("the v3 Folders API lists no folder %q under %s after Create folder", folder, org.GetName())
	}
	t.Cleanup(func() {
		if op, err := folders.DeleteFolder(context.Background(), &rmpb.DeleteFolderRequest{Name: made.GetName()}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	if made.GetState() != rmpb.Folder_ACTIVE {
		t.Errorf("the folder made in the browser is %v, want ACTIVE", made.GetState())
	}

	// Tags: Create tag key, then Create tag value on its page.
	p.clickText("#product-nav a", "Tags")
	p.waitFor(`location.pathname === "/tags"`)
	short := fmt.Sprintf("browser-env-%d", time.Now().UnixNano()%1e8)
	p.clickText("#view button", "Create tag key")
	p.submitForm(map[string]string{"shortName": short, "description": "made in the browser"}, "shortName")
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, short))
	key, err := keys.GetNamespacedTagKey(ctx, &rmpb.GetNamespacedTagKeyRequest{Name: tagNamespace(org.GetName()) + "/" + short})
	if err != nil {
		t.Fatalf("GetNamespacedTagKey after Create tag key: %v", err)
	}
	t.Cleanup(func() {
		vs := values.ListTagValues(context.Background(), &rmpb.ListTagValuesRequest{Parent: key.GetName()})
		for v, err := vs.Next(); err == nil; v, err = vs.Next() {
			if op, err := values.DeleteTagValue(context.Background(), &rmpb.DeleteTagValueRequest{Name: v.GetName()}); err == nil {
				_, _ = op.Wait(context.Background())
			}
		}
		if op, err := keys.DeleteTagKey(context.Background(), &rmpb.DeleteTagKeyRequest{Name: key.GetName()}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	if key.GetDescription() != "made in the browser" || key.GetParent() != org.GetName() {
		t.Errorf("the tag key made in the browser is %v; want its description, under %s", key, org.GetName())
	}

	// The key's page, by its address: the list redraws after the create,
	// replacing the link under a click.
	p.navigate("/tags/" + strings.TrimPrefix(key.GetName(), "tagKeys/"))
	p.waitFor(`document.querySelector("#tab-values") !== null`)
	p.clickText("#view .page-actions button", "Create tag value")
	p.submitForm(map[string]string{"shortName": "prod"}, "shortName")
	p.waitFor(`document.querySelector(".modal") === null`)
	vs := values.ListTagValues(ctx, &rmpb.ListTagValuesRequest{Parent: key.GetName()})
	v, err := vs.Next()
	if err != nil || v.GetShortName() != "prod" {
		t.Fatalf("ListTagValues of %s after Create tag value = %v, %v; want prod", key.GetName(), v, err)
	}
	if sent := p.posts("/api/actions/tags"); len(sent) != 1 {
		t.Errorf("Create tag value sent %d requests, want 1: %v", len(sent), sent)
	}
}

// tagNamespace is the namespace a key under parent is named in: the
// organization's number.
func tagNamespace(parent string) string {
	return strings.TrimPrefix(parent, "organizations/")
}

// TestServiceUsageDisableAndEnableThroughThePage, in the storage shard. The
// APIs & Services screen (#1352) had a provider but no route. Reached from
// the drawer, a project's Pub/Sub API is Disabled from its row, only once
// its name is typed back, and Service Usage's services.get answers DISABLED;
// Enable, which asks nothing, makes it ENABLED again.
func TestServiceUsageDisableAndEnableThroughThePage(t *testing.T) {
	needService(t, "serviceusage")
	c, err := su.NewService(context.Background(), option.WithEndpoint("http://"+endpoint(t, envServiceUsage)+"/"),
		option.WithoutAuthentication(), option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	project := uniqueProject(t)
	const api = "pubsub.googleapis.com"
	state := func() string {
		t.Helper()
		s, err := c.Services.Get("projects/" + project + "/services/" + api).Do()
		if err != nil {
			t.Fatalf("services.get %s in %s: %v", api, project, err)
		}
		return s.State
	}
	if got := state(); got != "ENABLED" {
		t.Fatalf("%s in a new project is %s, want ENABLED", api, got)
	}
	t.Cleanup(func() {
		_, _ = c.Services.Enable("projects/"+project+"/services/"+api, &su.EnableServiceRequest{}).Do()
	})

	p := open(t)
	p.navigateByDrawer("Management tools", "/apis", "APIs & Services")
	p.navigate("/apis?project=" + project)
	menu := func(item string) {
		trigger := fmt.Sprintf(`button[aria-label=%q]`, "Actions for "+api)
		p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, trigger))
		p.run(chromedp.Click(trigger, chromedp.ByQuery))
		p.clickText(`[role="menuitem"]`, item)
	}
	menu("Disable")
	p.confirmByName("wrong")
	p.waitFor(`!document.querySelector(".modal .form-error").hidden`)
	if sent := p.posts("/api/actions/serviceusage"); len(sent) != 0 {
		t.Fatalf("Disable confirmed with the wrong name was sent: %v", sent)
	}
	p.eval(`(() => { document.querySelector(".modal #confirm-input").value = ""; return true; })()`, nil)
	p.confirmByName(api)
	p.waitFor(`document.querySelector(".modal") === null`)
	if got := state(); got != "DISABLED" {
		t.Fatalf("after Disable in the browser, services.get says %s, want DISABLED", got)
	}

	p.navigate("/apis?project=" + project)
	menu("Enable")
	deadline := time.Now().Add(15 * time.Second)
	for state() != "ENABLED" {
		if time.Now().After(deadline) {
			t.Fatalf("after Enable in the browser, services.get still says %s", state())
		}
		time.Sleep(200 * time.Millisecond)
	}
	if sent := p.posts("/api/actions/serviceusage"); len(sent) != 2 {
		t.Errorf("Disable and Enable sent %d actions, want 2: %v", len(sent), sent)
	}
}
