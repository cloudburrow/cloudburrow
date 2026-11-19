//go:build browser

package browser

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	admin "cloud.google.com/go/iam/admin/apiv1"
	"cloud.google.com/go/iam/admin/apiv1/adminpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"github.com/chromedp/chromedp"
)

// iamRolesOf is member's roles in the project's policy (version 3), each
// with ?title when conditional, sorted.
func iamRolesOf(t *testing.T, projects *resourcemanager.ProjectsClient, project, member string) []string {
	t.Helper()
	pol, err := projects.GetIamPolicy(context.Background(), &iampb.GetIamPolicyRequest{Resource: "projects/" + project,
		Options: &iampb.GetPolicyOptions{RequestedPolicyVersion: 3}})
	if err != nil {
		t.Fatalf("GetIamPolicy: %v", err)
	}
	var out []string
	for _, b := range pol.GetBindings() {
		if slices.Contains(b.GetMembers(), member) {
			r := b.GetRole()
			if c := b.GetCondition(); c != nil {
				r += "?" + c.GetTitle()
			}
			out = append(out, r)
		}
	}
	slices.Sort(out)
	return out
}

// pickRole chooses role in the n-th role row of the open IAM panel, through
// Select a role's search.
func pickRole(p *tab, n int, role string) {
	p.t.Helper()
	row := fmt.Sprintf(`.modal.is-open .iam-role-row[data-n="%d"]`, n)
	p.run(chromedp.Click(chromedp.CSS(row + " .iam-role-button")))
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, row+" .iam-role-pop:not([hidden]) .iam-role-search"))
	p.eval(fmt.Sprintf(`(() => { const s = document.querySelector(%q); s.value = %q;
		s.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, row+" .iam-role-search", role), nil)
	opt := fmt.Sprintf(`%s .iam-role-option[data-role=%q]`, row, role)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, opt))
	p.run(chromedp.Click(chromedp.CSS(opt)))
	p.waitFor(fmt.Sprintf(`document.querySelector(%q).title === %q`, row+" .iam-role-button", role))
}

// TestIAMPageGrantEditAndRemoveAccess (#1864): Google's IAM page. Grant
// access gives two principals two roles, one with an IAM condition, in one
// save; the table lists them by principal and View by roles by role; Edit
// principal drops one of a principal's roles; Remove access takes the
// other principal's roles away. Each is read back through the Projects API.
func TestIAMPageGrantEditAndRemoveAccess(t *testing.T) {
	needService(t, "iam")
	ctx := context.Background()
	projects, err := resourcemanager.NewProjectsClient(ctx, rmClientOptions(t)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = projects.Close() })
	project := uniqueProject(t)
	ada, bob := "user:ada@example.com", "user:bob@example.com"

	p := open(t)
	p.navigate("/iam-admin/iam?project=" + project)
	p.waitFor(`(document.querySelector("#iam-scope-title") || {}).textContent === 'Permissions for project "` + project + `"'`)
	p.waitFor(`document.querySelector("#iam-principals-table") !== null || document.querySelector("#iam-body .state") !== null`)
	var tabs []string
	p.eval(`[...document.querySelectorAll("#view .tab-strip .tab")].map((t) => t.textContent)`, &tabs)
	if strings.Join(tabs, ",") != "View by principals,View by roles,Deny" {
		t.Errorf("the IAM page's tabs are %v", tabs)
	}

	p.run(chromedp.Click(chromedp.CSS(`#iam-grant-access`)))
	p.waitFor(`document.querySelector(".modal.is-open.is-side #iam-new-principals") !== null`)
	setField(p, ".modal.is-open", "iam-new-principals", ada+", "+bob)
	pickRole(p, 0, "roles/storage.objectViewer")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open #iam-add-another-role`)))
	p.waitFor(`document.querySelector('.modal.is-open .iam-role-row[data-n="1"]') !== null`)
	pickRole(p, 1, "roles/viewer")
	p.clickText(`.modal.is-open .iam-role-row[data-n="1"] .link-button`, "Add IAM condition")
	setField(p, ".modal.is-open", "iam-cond-title-1", "Until 2099")
	setField(p, ".modal.is-open", "iam-cond-expr-1", `request.time < timestamp("2099-01-01T00:00:00Z")`)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open #iam-save`)))
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	for _, m := range []string{ada, bob} {
		if got := iamRolesOf(t, projects, project, m); strings.Join(got, ",") != "roles/storage.objectViewer,roles/viewer?Until 2099" {
			t.Errorf("after Grant access %s holds %v", m, got)
		}
	}
	p.waitFor(fmt.Sprintf(`document.querySelector('#iam-principals-table tr[data-member=%q]') !== null`, ada))
	var adaRow string
	p.eval(fmt.Sprintf(`document.querySelector('#iam-principals-table tr[data-member=%q]').textContent`, ada), &adaRow)
	if !strings.Contains(adaRow, "User") || !strings.Contains(adaRow, "Viewer") || !strings.Contains(adaRow, "Until 2099") {
		t.Errorf("Ada's row reads %q; want her type, roles and condition", adaRow)
	}

	// View by roles lists each role with its principals.
	p.run(chromedp.Click(chromedp.CSS(`#iam-tab-roles`)))
	p.waitFor(`document.querySelector('#iam-roles-table tr[data-role="roles/viewer"]') !== null`)
	var viewers string
	p.eval(`document.querySelector('#iam-roles-table tr[data-role="roles/viewer"]').textContent`, &viewers)
	if !strings.Contains(viewers, "2 principals") {
		t.Errorf("roles/viewer's row reads %q; want 2 principals", viewers)
	}
	p.run(chromedp.Click(chromedp.CSS(`#iam-tab-principals`)))

	// Edit principal: Ada keeps only roles/viewer.
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, fmt.Sprintf(`button[aria-label="Edit principal %s"]`, ada)))
	p.run(chromedp.Click(chromedp.CSS(fmt.Sprintf(`button[aria-label="Edit principal %s"]`, ada))))
	p.waitFor(`document.querySelector(".modal.is-open #iam-edit-principal") !== null`)
	var first string
	p.eval(`document.querySelector('.modal.is-open .iam-role-row[data-n="0"] .iam-role-button').title`, &first)
	if first != "roles/storage.objectViewer" {
		t.Fatalf("Edit principal's first role is %q", first)
	}
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open .iam-role-row[data-n="0"] button[aria-label="Delete role 1"]`)))
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open #iam-save`)))
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	if got := iamRolesOf(t, projects, project, ada); strings.Join(got, ",") != "roles/viewer?Until 2099" {
		t.Errorf("after Edit principal Ada holds %v; want only the conditional roles/viewer", got)
	}

	// Remove access: Bob's roles go.
	sel := fmt.Sprintf(`input[aria-label="Select %s"]`, bob)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, sel))
	p.run(chromedp.Click(chromedp.CSS(sel)))
	p.run(chromedp.Click(chromedp.CSS(`#iam-remove-access`)))
	p.waitFor(`document.querySelector(".modal.is-open #confirm-input") !== null`)
	setField(p, ".modal.is-open", "confirm-input", bob)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(fmt.Sprintf(`document.querySelector('#iam-principals-table tr[data-member=%q]') === null`, bob))
	if got := iamRolesOf(t, projects, project, bob); len(got) != 0 {
		t.Errorf("after Remove access Bob holds %v", got)
	}
}

// TestIAMMenuFollowsGoogle (#1864): IAM & Admin's menu in Google's order;
// Policy Troubleshooter's page is its check form (#1879).
func TestIAMMenuFollowsGoogle(t *testing.T) {
	needService(t, "iam")
	project := uniqueProject(t)
	p := open(t)
	p.navigate("/iam-admin/iam?project=" + project)
	p.waitFor(`document.querySelectorAll("#product-nav a").length > 10`)
	var pages []string
	p.eval(`[...document.querySelectorAll("#product-nav a")].map((a) => a.textContent.trim()).filter(Boolean)`, &pages)
	want := []string{"IAM", "Identity & Organization", "Policy Troubleshooter", "Policy Analyzer", "Organization Policies",
		"Service Accounts", "Workload Identity Federation", "Workforce Identity Federation", "Labels", "Tags", "Settings",
		"Privacy & Security", "Identity-Aware Proxy", "Roles", "Audit Logs", "Manage resources", "Groups", "Essential Contacts", "Asset Inventory"}
	at := 0
	for _, w := range want {
		i := slices.Index(pages[at:], w)
		if i < 0 {
			t.Fatalf("IAM & Admin's menu %v lacks %q after position %d, in Google's order %v", pages, w, at, want)
		}
		at += i + 1
	}
	// Policy Troubleshooter is served since #1879: its page is the check
	// form, not "Not available locally".
	p.navigate("/iam-admin/troubleshooter?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view button")].some((b) => b.textContent.trim() === "Check access")`)
	// Tags in this menu is Resource Manager's tag keys page.
	p.navigate("/iam-admin/tags?project=" + project)
	p.waitFor(`(document.querySelector("#product-nav [aria-current=page]") || {}).textContent?.trim() === "Tags"`)
}

// TestIAMCreateServiceAccountInThreeSteps (#1864): Create service account
// as Google's three steps: the ID made from the name, a project role for
// the new account, and its users; then its page's tabs in Google's order.
func TestIAMCreateServiceAccountInThreeSteps(t *testing.T) {
	needService(t, "iam-service-accounts")
	ctx := context.Background()
	opts := rmClientOptions(t)
	projects, err := resourcemanager.NewProjectsClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	iamc, err := admin.NewIamClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = projects.Close(); _ = iamc.Close() })
	project := uniqueProject(t)

	p := open(t)
	p.navigate("/iam-admin/serviceaccounts/create?project=" + project)
	p.waitFor(`document.querySelector("#f-displayName") !== null`)
	var steps []string
	p.eval(`[...document.querySelectorAll(".create-step-title")].map((s) => s.textContent)`, &steps)
	if len(steps) != 3 || steps[0] != "Service account details" || !strings.HasPrefix(steps[2], "Grant users access") {
		t.Errorf("the create page's steps are %v", steps)
	}
	// Google's create page ends in Done
	// (docs.cloud.google.com/iam/docs/service-accounts-create, #1933).
	var done string
	p.eval(`document.querySelector('#view form button[type="submit"].primary').textContent`, &done)
	if done != "Done" {
		t.Errorf("Create service account ends in %q, want Done", done)
	}
	p.eval(`(() => { const f = document.querySelector("#f-displayName"); f.value = "Build Runner";
		f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, nil)
	var id string
	p.eval(`document.querySelector("#f-accountId").value`, &id)
	if id != "build-runner" {
		t.Errorf("the ID made from the name is %q; want build-runner", id)
	}
	p.clickText(".create-step-continue button", "Continue")
	setField(p, "#view", "f-roles", "roles/viewer")
	p.clickText(".create-step-continue button", "Continue")
	setField(p, "#view", "f-users", "user:ada@example.com")
	p.run(chromedp.Click(chromedp.CSS(`#view form button[type="submit"].primary`)))
	email := "build-runner@" + project + ".iam.gserviceaccount.com"
	// Done returns to the list, as Google's does.
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("build-runner@"))`)
	if got := iamRolesOf(t, projects, project, "serviceAccount:"+email); strings.Join(got, ",") != "roles/viewer" {
		t.Errorf("the new account holds %v on the project; want roles/viewer", got)
	}
	pol, err := iamc.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: "projects/" + project + "/serviceAccounts/" + email})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(pol.Members("roles/iam.serviceAccountUser"), "user:ada@example.com") {
		t.Errorf("the account's policy grants %v; want Ada as a Service Account User", pol.Roles())
	}
	p.navigate("/iam-admin/serviceaccounts/" + email + "?project=" + project)
	p.waitFor(`document.querySelectorAll("#view .tab-strip .tab").length >= 5`)
	var tabs []string
	p.eval(`[...document.querySelectorAll("#view .tab-strip .tab")].map((t) => t.textContent)`, &tabs)
	if !slices.Equal(tabs[:5], []string{"Details", "Permissions", "Keys", "Metrics", "Logs"}) {
		t.Errorf("the account's tabs are %v; want Details, Permissions, Keys, Metrics, Logs", tabs)
	}
}

// TestIAMCreateRoleFromSelection (#1864): two predefined roles checked on
// the Roles page make a custom role holding their permissions.
func TestIAMCreateRoleFromSelection(t *testing.T) {
	needService(t, "iam-roles")
	ctx := context.Background()
	iamc, err := admin.NewIamClient(ctx, rmClientOptions(t)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = iamc.Close() })
	project := uniqueProject(t)
	p := open(t)
	p.navigate("/iam-admin/roles?project=" + project)
	p.waitFor(`document.querySelectorAll("#view tbody tr").length > 0`)
	p.eval(`(() => { const f = document.querySelector("#view input.filter"); f.value = "objectViewer";
		f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, nil)
	check := func(row string) {
		sel := fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes(%q))?.querySelector('input[type="checkbox"]')`, row)
		p.waitFor(sel + ` != null`)
		p.eval(`(() => { const c = `+sel+`; c.click(); return true; })()`, nil)
	}
	check("predefined:storage.objectViewer")
	p.clickText("#view .action-bar button", "Create role from selection")
	p.waitFor(`document.querySelector(".modal.is-open #f-roles") !== null`)
	var roles string
	p.eval(`document.querySelector(".modal.is-open #f-roles").value`, &roles)
	if roles != "predefined:storage.objectViewer" {
		t.Errorf("the selection's roles are %q", roles)
	}
	setField(p, ".modal.is-open", "f-title", "Object readers")
	setField(p, ".modal.is-open", "f-roleId", "objectReaders")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	r, err := iamc.GetRole(ctx, &adminpb.GetRoleRequest{Name: "projects/" + project + "/roles/objectReaders"})
	if err != nil {
		t.Fatalf("GetRole: %v", err)
	}
	if !slices.Contains(r.GetIncludedPermissions(), "storage.objects.get") || r.GetTitle() != "Object readers" {
		t.Errorf("the role made from the selection is %q with %v", r.GetTitle(), r.GetIncludedPermissions())
	}
}

// TestIAMLabelsPage (#1864): IAM & Admin > Labels adds a label to the
// selected project, which the Projects API reads back.
func TestIAMLabelsPage(t *testing.T) {
	needService(t, "iam-labels")
	ctx := context.Background()
	projects, err := resourcemanager.NewProjectsClient(ctx, rmClientOptions(t)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = projects.Close() })
	project := uniqueProject(t)
	p := open(t)
	p.navigate("/iam-admin/labels?project=" + project)
	p.clickText("#view .page-header-actions button", "Add label")
	p.waitFor(`document.querySelector("#f-key") !== null`)
	setField(p, "body", "f-key", "env")
	setField(p, "body", "f-value", "dev")
	p.run(chromedp.Click(chromedp.CSS(`form button[type="submit"].primary`)))
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("env"))`)
	got, err := projects.GetProject(ctx, &rmpb.GetProjectRequest{Name: "projects/" + project})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetLabels()["env"] != "dev" {
		t.Errorf("the project's labels are %v; want env=dev", got.GetLabels())
	}
}

// TestIAMCreateGroupWithMembers (#1864): Create group as Google's form:
// Group name, Group email, Group description, then a member and its Google
// Groups role, added with the group.
func TestIAMCreateGroupWithMembers(t *testing.T) {
	needService(t, "iam-groups")
	project := uniqueProject(t)
	email := fmt.Sprintf("team-%s@cloudburrow.local", project[len(project)-6:])
	p := open(t)
	p.navigate("/iam-admin/groups/create?project=" + project)
	p.waitFor(`document.querySelector("#f-displayName") !== null`)
	var labels []string
	p.eval(`[...document.querySelectorAll("#view form label")].map((l) => l.textContent.replace("*", "").trim()).filter(Boolean)`, &labels)
	if i, j := slices.Index(labels, "Group name"), slices.Index(labels, "Group email"); i < 0 || j < 0 || i > j {
		t.Errorf("Create group's labels are %v; want Group name before Group email", labels)
	}
	setField(p, "#view", "f-displayName", "Team")
	setField(p, "#view", "f-email", email)
	setField(p, "#view", "f-members", "ada@example.com")
	setField(p, "#view", "f-memberRole", "MANAGER")
	// Google's Create group ends in Submit
	// (docs.cloud.google.com/iam/docs/groups-in-cloud-console, #1933).
	var submit string
	p.eval(`document.querySelector('#view form button[type="submit"].primary').textContent`, &submit)
	if submit != "Submit" {
		t.Errorf("Create group ends in %q, want Submit", submit)
	}
	p.run(chromedp.Click(chromedp.CSS(`#view form button[type="submit"].primary`)))
	p.waitFor(fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, email))
	p.navigate("/iam-admin/groups/" + email + "?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("ada@example.com"))`)
	var row string
	p.eval(`[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes("ada@example.com")).textContent`, &row)
	if !strings.Contains(row, "MANAGER") {
		t.Errorf("Ada's member row reads %q; want MANAGER", row)
	}
}
