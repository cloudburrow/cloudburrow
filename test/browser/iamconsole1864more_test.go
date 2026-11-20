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
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
	ci "google.golang.org/api/cloudidentity/v1"
)

// typeConfirmWord types back the word the open confirmation asks for.
func (p *tab) typeConfirmWord() {
	p.t.Helper()
	word := p.confirmWord()
	p.run(chromedp.SendKeys(chromedp.CSS(`.modal #confirm-input`), word), chromedp.KeyEvent(kb.Enter))
}

// pickPermission checks perm in the open Add Permissions picker of the
// form under scope.
func pickPermission(p *tab, scope, perm string) {
	p.t.Helper()
	setField(p, scope, "f-permissions-filter", perm)
	box := fmt.Sprintf(`%s #f-permissions-picker input[data-value=%q]`, scope, perm)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, box))
	p.run(chromedp.Click(chromedp.CSS(box)))
}

// TestIAMCustomRolePermissionPicker (#1864): Create custom role's Add
// Permissions picks two permissions, Create makes the role holding them;
// Edit role unchecks one and Update saves it; the Roles list's Disable over
// the checked role sets its launch stage to DISABLED. The IAM API reads
// each back.
func TestIAMCustomRolePermissionPicker(t *testing.T) {
	needService(t, "iam-roles")
	ctx := context.Background()
	iamc, err := admin.NewIamClient(ctx, rmClientOptions(t)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = iamc.Close() })
	project := uniqueProject(t)
	name := "projects/" + project + "/roles/pickedRole"

	p := open(t)
	p.navigate("/iam-admin/roles?project=" + project)
	p.clickText("#view .page-header-actions button", "Create custom role")
	p.waitFor(`document.querySelector("#view #f-permissions-add") !== null`)
	setField(p, "#view", "f-title", "Picked role")
	setField(p, "#view", "f-roleId", "pickedRole")
	p.run(chromedp.Click(chromedp.CSS(`#view #f-permissions-add`)))
	p.waitFor(`!document.querySelector("#view #f-permissions-picker").hidden && document.querySelectorAll("#view #f-permissions-picker input[type=checkbox]").length > 0`)
	var opts []string
	p.eval(`[...document.querySelectorAll("#view #f-permissions-service option")].map((o) => o.textContent)`, &opts)
	if len(opts) < 10 || opts[0] != "All Services" || !slices.Contains(opts, "storage") {
		t.Errorf("the picker's services are %d, starting %v", len(opts), opts[:min(3, len(opts))])
	}
	pickPermission(p, "#view", "storage.objects.get")
	pickPermission(p, "#view", "storage.objects.list")
	p.run(chromedp.Click(chromedp.CSS(`#view #f-permissions-pick-add`)))
	p.waitFor(`document.querySelectorAll("#view .picklist-field > ul.picklist-rows input:checked").length === 2`)
	p.run(chromedp.Click(chromedp.CSS(`#view form button[type="submit"].primary`)))
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("pickedRole"))`)
	r, err := iamc.GetRole(ctx, &adminpb.GetRoleRequest{Name: name})
	if err != nil {
		t.Fatalf("GetRole: %v", err)
	}
	if got := strings.Join(r.GetIncludedPermissions(), ","); got != "storage.objects.get,storage.objects.list" {
		t.Errorf("the role holds %s", got)
	}

	// Edit role: uncheck storage.objects.list, then Update.
	p.navigate("/iam-admin/roles/pickedRole?project=" + project)
	p.clickText("#view .page-actions button", "Edit role")
	box := `.modal.is-open input[data-value="storage.objects.list"]`
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, box))
	var submit string
	p.eval(`document.querySelector('.modal.is-open button[type="submit"]').textContent`, &submit)
	if submit != "Update" {
		t.Errorf("Edit role ends in %q, want Update", submit)
	}
	p.run(chromedp.Click(chromedp.CSS(box)))
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	if r, err = iamc.GetRole(ctx, &adminpb.GetRoleRequest{Name: name}); err != nil || strings.Join(r.GetIncludedPermissions(), ",") != "storage.objects.get" {
		t.Errorf("after Update the role holds %v, %v", r.GetIncludedPermissions(), err)
	}

	// Disable over the checked role.
	p.navigate("/iam-admin/roles?project=" + project)
	p.waitFor(`document.querySelector("#view input.filter") !== null`)
	p.eval(`(() => { const f = document.querySelector("#view input.filter"); f.value = "pickedRole";
		f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, nil)
	sel := `[...document.querySelectorAll("#view tbody tr")].find((r) => r.textContent.includes("pickedRole"))?.querySelector('input[type="checkbox"]')`
	p.waitFor(sel + ` != null`)
	p.eval(`(() => { `+sel+`.click(); return true; })()`, nil)
	p.clickText("#view .action-bar button", "Disable")
	p.typeConfirmWord()
	p.waitFor(`document.querySelector(".modal") === null`)
	if r, err = iamc.GetRole(ctx, &adminpb.GetRoleRequest{Name: name}); err != nil || r.GetStage() != adminpb.Role_DISABLED {
		t.Errorf("after Disable the role's stage is %v, %v", r.GetStage(), err)
	}
}

// TestIAMGroupPageButtons (#1864): a group's page has Google's Add members
// (Member email, Google Groups role), Remove members over the checked
// members, View in Google Groups (not available locally) and Delete group;
// the official Cloud Identity client reads each change back.
func TestIAMGroupPageButtons(t *testing.T) {
	needService(t, "iam-groups")
	svc := cloudIdentityClient(t)
	project := uniqueProject(t)
	email := fmt.Sprintf("buttons-%s@cloudburrow.local", project[len(project)-6:])
	name := browserGroup(t, svc, email)
	members := func() []string {
		l, err := svc.Groups.Memberships.List(name).Do()
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, m := range l.Memberships {
			out = append(out, m.PreferredMemberKey.Id)
		}
		slices.Sort(out)
		return out
	}

	p := open(t)
	p.navigate("/iam-admin/groups/" + email + "?project=" + project)
	p.waitFor(`document.querySelector("#view .page-actions") !== null`)
	var page []string
	p.eval(`[...document.querySelectorAll("#view .page-actions button")].map((b) => b.textContent)`, &page)
	for _, want := range []string{"Edit group", "View in Google Groups", "Delete group"} {
		if !slices.Contains(page, want) {
			t.Errorf("the group page's buttons are %v; want %s", page, want)
		}
	}
	var disabled bool
	p.eval(`[...document.querySelectorAll("#view .page-actions button")].find((b) => b.textContent === "View in Google Groups").disabled`, &disabled)
	if !disabled {
		t.Error("View in Google Groups is not drawn disabled")
	}

	p.clickText("#view .section-actions button", "Add members")
	p.waitFor(`document.querySelector(".modal.is-open #f-members") !== null`)
	setField(p, ".modal.is-open", "f-members", "ada@example.com, bob@example.com")
	setField(p, ".modal.is-open", "f-role", "MANAGER")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	if got := strings.Join(members(), ","); got != "ada@example.com,bob@example.com" {
		t.Fatalf("after Add members the group holds %s", got)
	}

	p.waitFor(`document.querySelector('#view tbody input[aria-label="Select bob@example.com"]') !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#view tbody input[aria-label="Select bob@example.com"]`)))
	p.eval(`[...document.querySelectorAll("#view button")].find((b) => b.textContent === "Remove members" && !b.closest(".page-actions") && !b.disabled).click()`, nil)
	p.typeConfirmWord()
	p.waitFor(`document.querySelector(".modal") === null`)
	if got := strings.Join(members(), ","); got != "ada@example.com" {
		t.Errorf("after Remove members the group holds %s", got)
	}

	// Edit group's fields are Google's.
	p.navigate("/iam-admin/groups/" + email + "?project=" + project)
	p.clickText("#view .page-actions button", "Edit group")
	p.waitFor(`document.querySelector(".modal.is-open #f-displayName") !== null`)
	var labels []string
	p.eval(`[...document.querySelectorAll(".modal.is-open label")].map((l) => l.textContent.trim())`, &labels)
	if !slices.Contains(labels, "Group name") || !slices.Contains(labels, "Group description") {
		t.Errorf("Edit group's labels are %v", labels)
	}
	p.run(chromedp.KeyEvent(kb.Escape))
	p.waitFor(`document.querySelector(".modal") === null`)

	p.clickText("#view .page-actions button", "Delete group")
	p.typeConfirmWord()
	p.waitFor(`location.pathname === "/iam-admin/groups"`)
	if _, err := svc.Groups.Get(name).Do(); err == nil {
		t.Error("Delete group left the group")
	}
}

// TestIAMServiceAccountDetailsNameAndAuditTitle (#1864): a service account's
// Details tab has its Name box and Save, which the IAM API reads back; the
// Audit Logs table is titled "Data Access audit logs configuration" and
// Set Default Configuration ends in Save.
func TestIAMServiceAccountDetailsNameAndAuditTitle(t *testing.T) {
	needService(t, "iam-service-accounts")
	needService(t, "iam-audit")
	ctx := context.Background()
	iamc, err := admin.NewIamClient(ctx, rmClientOptions(t)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = iamc.Close() })
	project := uniqueProject(t)
	a, err := iamc.CreateServiceAccount(ctx, &adminpb.CreateServiceAccountRequest{Name: "projects/" + project, AccountId: "inline-name",
		ServiceAccount: &adminpb.ServiceAccount{DisplayName: "Before", Description: "kept"}})
	if err != nil {
		t.Fatal(err)
	}

	p := open(t)
	p.navigate("/iam-admin/serviceaccounts/" + a.GetEmail() + "?project=" + project)
	p.waitFor(`document.querySelector("#view form.inline-edit #f-displayName") !== null`)
	setField(p, "#view form.inline-edit", "f-displayName", "After")
	p.run(chromedp.Click(chromedp.CSS(`#view form.inline-edit button[type="submit"]`)))
	p.waitFor(`[...document.querySelectorAll("#view dd")].some((d) => d.textContent === "After")`)
	got, err := iamc.GetServiceAccount(ctx, &adminpb.GetServiceAccountRequest{Name: a.GetName()})
	if err != nil || got.GetDisplayName() != "After" || got.GetDescription() != "kept" {
		t.Errorf("after Save the account is %q, %q, %v", got.GetDisplayName(), got.GetDescription(), err)
	}

	p.navigate("/iam-admin/audit?project=" + project)
	p.waitFor(`(document.querySelector("#view .table-title") || {}).textContent === "Data Access audit logs configuration"`)
	p.clickText("#view button", "Set Default Configuration")
	p.waitFor(`document.querySelector("#action-panel") !== null`)
	var submit string
	p.eval(`document.querySelector("#action-panel-save").textContent`, &submit)
	if submit != "Save" {
		t.Errorf("Set Default Configuration ends in %q, want Save", submit)
	}
}

// TestIAMRolesGroupsAndAccountScreensMeetWCAG (#1864): Create custom role
// with its picker open, a custom role's page, Edit role, a group's page,
// Add members, a service account's Details with its Name box, and Audit
// Logs, audited as every other screen is.
func TestIAMRolesGroupsAndAccountScreensMeetWCAG(t *testing.T) {
	needService(t, "iam-roles")
	needService(t, "iam-groups")
	needService(t, "iam-service-accounts")
	needService(t, "iam-audit")
	ctx := context.Background()
	iamc, err := admin.NewIamClient(ctx, rmClientOptions(t)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = iamc.Close() })
	project := uniqueProject(t)
	if _, err := iamc.CreateRole(ctx, &adminpb.CreateRoleRequest{Parent: "projects/" + project, RoleId: "a11yRole",
		Role: &adminpb.Role{Title: "A11y", IncludedPermissions: []string{"storage.objects.get"}}}); err != nil {
		t.Fatal(err)
	}
	a, err := iamc.CreateServiceAccount(ctx, &adminpb.CreateServiceAccountRequest{Name: "projects/" + project, AccountId: "a11y-account",
		ServiceAccount: &adminpb.ServiceAccount{DisplayName: "A11y"}})
	if err != nil {
		t.Fatal(err)
	}
	svc := cloudIdentityClient(t)
	email := fmt.Sprintf("a11y-%s@cloudburrow.local", project[len(project)-6:])
	name := browserGroup(t, svc, email)
	if _, err := svc.Groups.Memberships.Create(name, &ci.Membership{PreferredMemberKey: &ci.EntityKey{Id: "ada@example.com"},
		Roles: []*ci.MembershipRole{{Name: "MEMBER"}}}).Do(); err != nil {
		t.Fatal(err)
	}
	q := "?project=" + project
	auditScreens(t, []a11yPage{
		{"Create custom role with Add Permissions open", "/iam-admin/roles/create" + q,
			`document.querySelector("#view #f-permissions-add") !== null`,
			`document.querySelector("#view #f-permissions-add").click()`},
		screen("a custom role", "/iam-admin/roles/a11yRole", project),
		{"Edit role dialog", "/iam-admin/roles/a11yRole" + q, `document.querySelector("#view .page-actions button") !== null`,
			`[...document.querySelectorAll("#view .page-actions button")].find((b) => b.textContent === "Edit role").click()`},
		screen("a group", "/iam-admin/groups/"+email, project),
		{"Add members dialog", "/iam-admin/groups/" + email + q, `document.querySelector("#view .section-actions button") !== null`,
			`[...document.querySelectorAll("#view .section-actions button")].find((b) => b.textContent === "Add members").click()`},
		{"a service account's Details", "/iam-admin/serviceaccounts/" + a.GetEmail() + q,
			`document.querySelector("#view form.inline-edit") !== null`, ""},
		screen("Audit Logs", "/iam-admin/audit", project),
	})
}
