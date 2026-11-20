//go:build browser

package browser

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
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
		p.run(chromedp.Click(chromedp.CSS("#nav-toggle")))
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
	p.run(chromedp.Click(chromedp.CSS(`[data-cb-click]`)))
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
	p.run(chromedp.Click(chromedp.CSS(`[data-cb-click]`)))
}

// TestResourceHierarchyAndTagsThroughTheDrawer, in the storage shard. The
// Resource hierarchy and Tags screens (#1294, #1295) had providers but no
// route. Since #1865 they are IAM & Admin's, as Google's are: the drawer
// has no Resource Manager row, and IAM & Admin's menu holds Manage
// resources and Tags. The Resource hierarchy list still answers at its
// address: Create folder there makes a folder the v3 Folders API lists
// under the organization, and Google's Create tag key (Tag key, Tag key
// description) and Create tag value make a key and value the TagKeys and
// TagValues APIs read back.
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
	p.navigateByDrawer("IAM & Admin", "/iam-admin/iam", "IAM & Admin")
	var rows []string
	p.eval(`[...document.querySelectorAll("#nav-list a")].map((a) => a.textContent.trim())`, &rows)
	for _, r := range rows {
		if strings.Contains(r, "Resource Manager") {
			t.Errorf("the drawer still has a Resource Manager row (%q): its pages are IAM & Admin's", r)
		}
	}
	// IAM & Admin's pages hold Manage resources and Tags, and not the old
	// Projects or Resource hierarchy lists.
	var pages []string
	p.waitFor(`document.querySelectorAll("#product-nav a").length > 0`)
	p.eval(`[...document.querySelectorAll("#product-nav a")].map((a) => a.textContent.trim())`, &pages)
	for _, want := range []string{"Manage resources", "Tags"} {
		if !contains(pages, want) {
			t.Errorf("IAM & Admin's pages are %q; want %s among them", pages, want)
		}
	}
	for _, gone := range []string{"Projects", "Resource hierarchy"} {
		if contains(pages, gone) {
			t.Errorf("IAM & Admin's pages are %q; %s is Manage resources now", pages, gone)
		}
	}

	// Resource hierarchy: Create folder, at the list's address, which marks
	// Manage resources in the menu.
	p.navigate("/resource-hierarchy")
	p.waitFor(`(document.querySelector("#product-nav [aria-current=page]") || {}).textContent?.trim() === "Manage resources"`)
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
	p.waitFor(`location.pathname === "/iam-admin/tags"`)
	var tagLabels []string
	p.clickText("#view button", "Create tag key")
	p.waitFor(`document.querySelector("#f-shortName") !== null`)
	p.eval(`[...document.querySelectorAll("label[for^=f-] > span:first-child, #view form fieldset > legend")].map((s) => s.textContent.trim())`, &tagLabels)
	for _, want := range []string{"Tag key", "Tag key description", "For use with network firewall", "Tag values"} {
		if !contains(tagLabels, want) {
			t.Errorf("Create tag key's fields are %q; want Google's %q among them", tagLabels, want)
		}
	}
	p.navigate("/iam-admin/tags")
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

// TestServiceUsageDisableAndEnableThroughThePage, in the storage shard.
// APIs & Services (#1352, #1866) as Google's console has it: reached from
// the drawer, Enabled APIs & services shows the Traffic, Errors and Median
// latency charts over the enabled APIs with their Requests, Errors and
// Latency. A project's Pub/Sub API is opened from the table to its Metrics
// tab, Disable API (confirmed) makes Service Usage's services.get answer
// DISABLED and drops it from the table, and the Library's search finds it,
// whose Enable makes it ENABLED again and offers Manage.
func TestServiceUsageDisableAndEnableThroughThePage(t *testing.T) {
	needService(t, "serviceusage")
	hc := &http.Client{Transport: &http.Transport{}}
	t.Cleanup(hc.CloseIdleConnections)
	c, err := su.NewService(context.Background(), option.WithEndpoint("http://"+endpoint(t, envServiceUsage)+"/"),
		option.WithoutAuthentication(), option.WithHTTPClient(hc))
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
	// Google's menu, in Google's order.
	var menu []string
	p.waitFor(`document.querySelectorAll("#product-nav a").length >= 6`)
	p.eval(`[...document.querySelectorAll("#product-nav a")].map((a) => a.textContent.trim())`, &menu)
	want := []string{"Enabled APIs & services", "Library", "Credentials", "OAuth consent screen", "Page usage agreements", "Domain verification", "Operations"}
	if strings.Join(menu, "|") != strings.Join(want, "|") {
		t.Errorf("APIs & Services menu = %q, want %q", menu, want)
	}

	p.navigate("/apis?project=" + project)
	row := fmt.Sprintf(`#apis-table tr[data-api=%q]`, api)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, row))
	p.waitFor(`document.querySelector("#apis-dashboard-charts") !== null`)
	var heads, charts []string
	p.eval(`[...document.querySelectorAll("#apis-table th")].map((n) => n.textContent)`, &heads)
	if strings.Join(heads, "|") != "Name|Requests|Errors|Latency, median|Latency, 95%" {
		t.Errorf("Enabled APIs & services columns = %q", heads)
	}
	p.eval(`[...document.querySelectorAll("#apis-dashboard-charts .chart-card h3")].map((n) => n.textContent)`, &charts)
	if strings.Join(charts, "|") != "Traffic|Errors|Median latency" {
		t.Errorf("dashboard charts = %q, want Traffic, Errors, Median latency", charts)
	}

	// The API's page, from its row: Metrics, then Disable API.
	p.clickExpr(fmt.Sprintf(`document.querySelector(%q)`, row+" a"))
	p.waitFor(`document.querySelector("#api-tab-metrics.is-selected") !== null`)
	var tabs []string
	p.eval(`[...document.querySelectorAll('[role="tab"]')].map((n) => n.textContent)`, &tabs)
	if strings.Join(tabs, "|") != "Overview|Metrics|Quotas & System Limits|Credentials" {
		t.Errorf("API page tabs = %q", tabs)
	}
	p.clickExpr(`document.querySelector("#api-disable-button")`)
	p.waitFor(`document.querySelector(".modal #api-disable") !== null`)
	p.clickExpr(`document.querySelector('.modal #api-disable button[type="submit"]')`)
	p.waitFor(`document.querySelector(".modal") === null`)
	if got := state(); got != "DISABLED" {
		t.Fatalf("after Disable API in the browser, services.get says %s, want DISABLED", got)
	}
	p.waitFor(`location.pathname === "/apis" && document.querySelector("#apis-table") !== null`)
	var listed bool
	p.eval(fmt.Sprintf(`document.querySelector(%q) !== null`, row), &listed)
	if listed {
		t.Errorf("%s is still in the enabled APIs after Disable API", api)
	}

	// The Library finds it by search, and its Enable enables it.
	p.navigate("/apis/library?project=" + project)
	p.waitFor(`document.querySelector("#library-search") !== null`)
	p.setField("#library-search", "Cloud Pub/Sub API")
	card := fmt.Sprintf(`#library-results [data-api=%q]`, api)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null && document.querySelectorAll("#library-results [data-api]").length === 1`, card))
	p.clickExpr(fmt.Sprintf(`document.querySelector(%q)`, card))
	p.clickExpr(`document.querySelector("#library-enable")`)
	deadline := time.Now().Add(15 * time.Second)
	for state() != "ENABLED" {
		if time.Now().After(deadline) {
			t.Fatalf("after Enable in the Library, services.get still says %s", state())
		}
		time.Sleep(200 * time.Millisecond)
	}
	p.waitFor(`document.querySelector("#api-tab-metrics.is-selected") !== null`)
	p.navigate("/apis/library/" + api + "?project=" + project)
	p.waitFor(`document.querySelector("#library-manage") !== null`)
	// Disable API and Enable were one Studio call each, through the console.
	var changes []string
	for _, u := range p.posts("/api/studio/serviceusage") {
		if strings.Contains(u, "call=disable") || strings.Contains(u, "call=enable") {
			changes = append(changes, u)
		}
	}
	if len(changes) != 2 {
		t.Errorf("Disable API and Enable sent %d changes, want 2: %v", len(changes), changes)
	}
}

// TestAPIsCredentialsAndUnservedPages (#1866), in the storage shard.
// Credentials has Create credentials, whose Service account opens IAM's
// form and whose API key and OAuth client ID say they are not available
// locally, and lists API Keys and OAuth 2.0 Client IDs as not served; the
// OAuth consent screen, Page usage agreements and Domain verification say
// why a local instance has none; a library entry this instance does not
// serve has Enable disabled with the --services value that would serve it.
func TestAPIsCredentialsAndUnservedPages(t *testing.T) {
	needService(t, "serviceusage")
	project := uniqueProject(t)
	p := open(t)
	p.navigate("/apis/credentials?project=" + project)
	p.waitFor(`document.querySelector("#credentials-create") !== null`)
	p.clickExpr(`document.querySelector("#credentials-create")`)
	var items []string
	p.eval(`[...document.querySelectorAll("#credentials-create-menu [role=menuitem]")].map((n) => n.textContent + (n.disabled ? " (disabled)" : ""))`, &items)
	// API key is served since #1915 (TestAPIKeysThroughTheCredentialsPage).
	if len(items) != 3 || items[0] != "API key" ||
		!strings.HasPrefix(items[1], "OAuth client ID") || !strings.HasPrefix(items[2], "Service account") {
		t.Errorf("Create credentials = %q", items)
	}
	for _, id := range []string{"#credentials-oauth"} {
		var text string
		p.eval(fmt.Sprintf(`document.querySelector(%q).textContent`, id), &text)
		if !strings.Contains(text, "Not available locally") {
			t.Errorf("%s = %q, want not available locally", id, text)
		}
	}
	for _, path := range []string{"/apis/credentials/consent", "/apis/credentials/agreements", "/apis/credentials/domainverification"} {
		p.navigate(path + "?project=" + project)
		p.waitFor(`[...document.querySelectorAll(".state h2")].some((n) => n.textContent === "Not available locally")`)
	}
	// Spanner is not in the storage shard's --services.
	p.navigate("/apis/library/spanner.googleapis.com?project=" + project)
	p.waitFor(`document.querySelector("#library-unavailable") !== null`)
	var disabled bool
	var why string
	p.eval(`document.querySelector("#library-enable").disabled`, &disabled)
	p.eval(`document.querySelector("#library-unavailable").textContent`, &why)
	if !disabled || !strings.Contains(why, "--services spanner") {
		t.Errorf("Spanner's Library page: Enable disabled %v, reason %q", disabled, why)
	}
}

// TestResourceHierarchyFolderPermissionsThroughTheTab (#1552), in the storage
// shard. A folder's page has a Permissions tab, as Google's console offers
// IAM on a folder picked in the resource selector: Grant access there is the
// v3 Folders API's SetIamPolicy, which its GetIamPolicy reads back, and
// Remove principal, confirmed by typing the principal back, empties it.
func TestResourceHierarchyFolderPermissionsThroughTheTab(t *testing.T) {
	needService(t, "hierarchy")
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
	t.Cleanup(func() { _ = folders.Close(); _ = orgs.Close() })
	org, err := orgs.SearchOrganizations(ctx, &rmpb.SearchOrganizationsRequest{}).Next()
	if err != nil {
		t.Fatalf("SearchOrganizations: %v", err)
	}
	op, err := folders.CreateFolder(ctx, &rmpb.CreateFolderRequest{Folder: &rmpb.Folder{
		Parent: org.GetName(), DisplayName: fmt.Sprintf("Perms %d", time.Now().UnixNano()%1e8)}})
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	f, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	t.Cleanup(func() {
		if op, err := folders.DeleteFolder(context.Background(), &rmpb.DeleteFolderRequest{Name: f.GetName()}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	members := func() map[string]string {
		pol, err := folders.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: f.GetName()})
		if err != nil {
			t.Fatalf("GetIamPolicy of %s: %v", f.GetName(), err)
		}
		out := map[string]string{}
		for _, b := range pol.GetBindings() {
			for _, m := range b.GetMembers() {
				out[m] = b.GetRole()
			}
		}
		return out
	}
	const role = "roles/resourcemanager.folderViewer"
	ada := "user:ada@example.com"

	p := open(t)
	p.navigate("/resource-hierarchy/" + strings.TrimPrefix(f.GetName(), "folders/"))
	p.waitFor(`[...document.querySelectorAll("#view .tab-strip .tab")].some((t) => t.textContent === "Permissions")`)
	p.clickText("#view .tab-strip .tab", "Permissions")
	p.waitFor(`document.querySelector("#grant-access") !== null`)
	p.clickText("#view #grant-access", "Grant access")
	p.waitFor(`document.querySelector(".modal.is-open #f-members") !== null`)
	p.eval(fmt.Sprintf(`(() => { const q = (s) => document.querySelector(".modal.is-open " + s);
		q("#f-members").value = %q; q("#f-members").dispatchEvent(new Event("input", { bubbles: true }));
		q("#f-role").value = %q; q("#f-role").dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, ada, role), nil)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	p.waitFor(`document.querySelectorAll("#permissions-table tbody tr").length === 1`)
	if got := members(); got[ada] != role {
		t.Errorf("after Grant access the Folders API reads %v; want %s holding %s", got, ada, role)
	}

	p.run(chromedp.Click(chromedp.CSS(fmt.Sprintf(`button[aria-label=%q]`, "Remove "+ada+" from "+role))))
	p.waitFor(`document.querySelector(".modal.is-open #confirm-input") !== null`)
	p.eval(fmt.Sprintf(`(() => { const f = document.querySelector(".modal.is-open #confirm-input");
		f.value = %q; f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, ada), nil)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	p.waitFor(`document.querySelector("#permissions-table") === null`)
	if got := members(); len(got) != 0 {
		t.Errorf("after Remove principal the Folders API reads %v", got)
	}
	if sent := p.posts("/api/permissions/hierarchy"); len(sent) != 2 {
		t.Errorf("the grant and the removal sent %d requests, want 2: %v", len(sent), sent)
	}
}

// TestResourceHierarchyOrganizationPageAndPermissions (#1554), in the storage
// shard. The Resource hierarchy list links the organization to its own page,
// as Google's console opens the organization picked in the resource
// selector: it names what GetOrganization answers, lists its Organization
// policies, and has a Permissions tab, where Grant access is the v3
// Organizations API's SetIamPolicy, which its GetIamPolicy reads back, and
// Remove principal, confirmed by typing the principal back, takes it away.
func TestResourceHierarchyOrganizationPageAndPermissions(t *testing.T) {
	needService(t, "hierarchy")
	opts := rmClientOptions(t)
	ctx := context.Background()
	orgs, err := resourcemanager.NewOrganizationsClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = orgs.Close() })
	org, err := orgs.SearchOrganizations(ctx, &rmpb.SearchOrganizationsRequest{}).Next()
	if err != nil {
		t.Fatalf("SearchOrganizations: %v", err)
	}
	const role = "roles/resourcemanager.organizationViewer"
	member := fmt.Sprintf("user:org-%d@example.com", time.Now().UnixNano()%1e8)
	holds := func() bool {
		pol, err := orgs.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: org.GetName()})
		if err != nil {
			t.Fatalf("GetIamPolicy of %s: %v", org.GetName(), err)
		}
		for _, b := range pol.GetBindings() {
			if b.GetRole() == role && slices.Contains(b.GetMembers(), member) {
				return true
			}
		}
		return false
	}

	p := open(t)
	p.navigate("/resource-hierarchy")
	p.waitFor(`[...document.querySelectorAll("#view .listing-summary a")].some((a) => a.getAttribute("href").startsWith("/resource-hierarchy/organization"))`)
	p.eval(`[...document.querySelectorAll("#view .listing-summary a")].find((a) => a.getAttribute("href").startsWith("/resource-hierarchy/organization")).click(), true`, nil)
	p.waitFor(`location.pathname === "/resource-hierarchy/organization"`)
	p.waitFor(fmt.Sprintf(`(document.querySelector("#view").textContent || "").includes(%q)`, org.GetName()))
	p.waitFor(`[...document.querySelectorAll("#view .tab-strip .tab")].some((t) => t.textContent === "Organization policies")`)
	p.waitFor(`[...document.querySelectorAll("#view .tab-strip .tab")].some((t) => t.textContent === "Permissions")`)
	p.clickText("#view .tab-strip .tab", "Permissions")
	p.waitFor(`document.querySelector("#grant-access") !== null`)
	p.clickText("#view #grant-access", "Grant access")
	p.waitFor(`document.querySelector(".modal.is-open #f-members") !== null`)
	p.eval(fmt.Sprintf(`(() => { const q = (s) => document.querySelector(".modal.is-open " + s);
		q("#f-members").value = %q; q("#f-members").dispatchEvent(new Event("input", { bubbles: true }));
		q("#f-role").value = %q; q("#f-role").dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, member, role), nil)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	remove := fmt.Sprintf(`button[aria-label=%q]`, "Remove "+member+" from "+role)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, remove))
	if !holds() {
		t.Errorf("after Grant access the Organizations API does not read %s holding %s", member, role)
	}

	p.run(chromedp.Click(chromedp.CSS(remove)))
	p.waitFor(`document.querySelector(".modal.is-open #confirm-input") !== null`)
	p.eval(fmt.Sprintf(`(() => { const f = document.querySelector(".modal.is-open #confirm-input");
		f.value = %q; f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, member), nil)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) === null`, remove))
	if holds() {
		t.Errorf("after Remove principal the Organizations API still reads %s holding %s", member, role)
	}
	if sent := p.posts("/api/permissions/hierarchy"); len(sent) != 2 {
		t.Errorf("the grant and the removal sent %d requests, want 2: %v", len(sent), sent)
	}
}
