//go:build browser

package browser

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	"github.com/chromedp/chromedp"
)

// TestIAMSelectARoleByProduct (#2013): Grant access's Select a role groups
// the roles by product, as Google's role picker does: Basic first, a
// product chosen lists its roles, and the filter's hits sit under their
// products' headings. The principals table has Google's Security insights
// column, which says Not available locally.
func TestIAMSelectARoleByProduct(t *testing.T) {
	needService(t, "iam")
	ctx := context.Background()
	projects, err := resourcemanager.NewProjectsClient(ctx, rmClientOptions(t)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = projects.Close() })
	project := uniqueProject(t)
	pol, err := projects.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: "projects/" + project})
	if err != nil {
		t.Fatal(err)
	}
	pol.Bindings = append(pol.Bindings, &iampb.Binding{Role: "roles/viewer", Members: []string{"user:ada@example.com"}})
	if _, err := projects.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: "projects/" + project, Policy: pol}); err != nil {
		t.Fatal(err)
	}

	p := open(t)
	p.navigate("/iam-admin/iam?project=" + project)
	p.waitFor(`document.querySelector('#iam-principals-table tr[data-member="user:ada@example.com"]') !== null`)
	var heads []string
	p.eval(`[...document.querySelectorAll("#iam-principals-table thead th")].map((h) => h.textContent)`, &heads)
	if strings.Join(heads, ",") != "Select,Type,Principal,Name,Role,Security insights,Inheritance,Edit" {
		t.Errorf("the principals table's columns are %v", heads)
	}
	var insight string
	p.eval(`document.querySelector('#iam-principals-table tr[data-member="user:ada@example.com"] .iam-insights').textContent`, &insight)
	if insight != "Not available locally" {
		t.Errorf("Security insights reads %q", insight)
	}

	p.run(chromedp.Click(chromedp.CSS(`#iam-grant-access`)))
	row := `.modal.is-open .iam-role-row[data-n="0"]`
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, row+" .iam-role-button"))
	p.run(chromedp.Click(chromedp.CSS(row + " .iam-role-button")))
	p.waitFor(fmt.Sprintf(`document.querySelectorAll(%q).length > 10`, row+" .iam-role-product"))
	var groups []string
	p.eval(fmt.Sprintf(`[...document.querySelectorAll(%q)].map((b) => b.textContent)`, row+" .iam-role-product"), &groups)
	if groups[0] != "Basic" || !contains(groups, "Cloud Storage") || !contains(groups, "Compute Engine") {
		t.Errorf("Select a role's groups start %v; want Basic first, then products such as Cloud Storage", groups[:min(5, len(groups))])
	}
	var basic []string
	p.eval(fmt.Sprintf(`[...document.querySelectorAll(%q)].map((o) => o.dataset.role)`, row+" .iam-role-option"), &basic)
	if !contains(basic, "roles/viewer") || contains(basic, "roles/storage.admin") {
		t.Errorf("the Basic group lists %v", basic)
	}
	p.eval(fmt.Sprintf(`(() => { [...document.querySelectorAll(%q)].find((b) => b.textContent === "Cloud Storage").click(); return true; })()`,
		row+" .iam-role-product"), nil)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, row+` .iam-role-option[data-role="roles/storage.objectViewer"]`))
	var storage []string
	p.eval(fmt.Sprintf(`[...document.querySelectorAll(%q)].map((o) => o.dataset.role)`, row+" .iam-role-option"), &storage)
	for _, r := range storage {
		if !strings.HasPrefix(r, "roles/storage") {
			t.Errorf("Cloud Storage lists %s", r)
			break
		}
	}
	// The filter searches every group; its hits are under their products.
	p.eval(fmt.Sprintf(`(() => { const s = document.querySelector(%q); s.value = "pubsub.viewer";
		s.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, row+" .iam-role-search"), nil)
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, row+` .iam-role-option[data-role="roles/pubsub.viewer"]`))
	var heading string
	p.eval(fmt.Sprintf(`document.querySelector(%q).textContent`, row+" .iam-role-group"), &heading)
	if heading != "Pub/Sub" {
		t.Errorf("roles/pubsub.viewer's heading is %q, want Pub/Sub", heading)
	}
	p.run(chromedp.Click(chromedp.CSS(row + ` .iam-role-option[data-role="roles/pubsub.viewer"]`)))
	p.waitFor(fmt.Sprintf(`document.querySelector(%q).title === "roles/pubsub.viewer"`, row+" .iam-role-button"))
}

// TestIAMAddPermissionsAllTypes (#2013): Add Permissions' All Services and
// All Types narrow the picker to a service's permissions on one resource
// type, the middle of service.resource.verb.
func TestIAMAddPermissionsAllTypes(t *testing.T) {
	needService(t, "iam-roles")
	project := uniqueProject(t)
	p := open(t)
	p.navigate("/iam-admin/roles/create?project=" + project)
	p.waitFor(`document.querySelector("#view #f-permissions-add") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#view #f-permissions-add`)))
	p.waitFor(`[...document.querySelectorAll("#f-permissions-service option")].some((o) => o.value === "storage")`)
	var first string
	p.eval(`document.querySelector("#f-permissions-type option").textContent`, &first)
	if first != "All Types" {
		t.Errorf("the type filter's first choice is %q, want All Types", first)
	}
	p.eval(`(() => { const s = document.querySelector("#f-permissions-service"); s.value = "storage";
		s.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, nil)
	p.waitFor(`[...document.querySelectorAll("#f-permissions-type option")].some((o) => o.value === "buckets")`)
	var types []string
	p.eval(`[...document.querySelectorAll("#f-permissions-type option")].map((o) => o.value).filter(Boolean)`, &types)
	if !contains(types, "objects") || contains(types, "topics") {
		t.Errorf("storage's types are %v", types)
	}
	p.eval(`(() => { const s = document.querySelector("#f-permissions-type"); s.value = "objects";
		s.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, nil)
	var shown []string
	p.eval(`[...document.querySelectorAll("#view .picklist-results input")].map((c) => c.dataset.value)`, &shown)
	if len(shown) == 0 {
		t.Fatal("storage's objects permissions are not listed")
	}
	for _, v := range shown {
		if !strings.HasPrefix(v, "storage.objects.") {
			t.Errorf("Service storage, Type objects lists %s", v)
		}
	}
}

// TestIAMAuditLogsInfoPanel (#2013): two services checked on Audit Logs
// open Google's info panel for both, its Log Types and Exempted Principals
// tabs; Save sets their log types and the exempted principal in one
// SetIamPolicy. One service checked opens its own configuration, the
// exemption read back with its Disabled Log Types, and Delete removes it.
func TestIAMAuditLogsInfoPanel(t *testing.T) {
	needService(t, "iam-audit")
	ctx := context.Background()
	projects, err := resourcemanager.NewProjectsClient(ctx, rmClientOptions(t)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = projects.Close() })
	project := uniqueProject(t)
	p := open(t)
	p.navigate("/iam-admin/audit?project=" + project)
	p.waitFor(`document.querySelectorAll("#view tbody tr").length > 0`)
	p.eval(`(() => { const f = document.querySelector("#view input.filter"); f.value = "access";
		f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, nil)
	for _, s := range []string{"accessapproval.googleapis.com", "accesscontextmanager.googleapis.com"} {
		sel := fmt.Sprintf(`#view input[aria-label="Select %s"]`, s)
		p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, sel))
		p.run(chromedp.Click(chromedp.CSS(sel)))
	}
	p.waitFor(`(document.querySelector("#action-panel-title") || {}).textContent === "Edit selected services: 2 services"`)
	var tabs []string
	p.eval(`[...document.querySelectorAll("#action-panel [role=tab]")].map((t) => t.textContent)`, &tabs)
	if strings.Join(tabs, ",") != "Log Types,Exempted Principals" {
		t.Errorf("the info panel's tabs are %v", tabs)
	}
	p.run(chromedp.Click(chromedp.CSS(`#action-panel #f-dataWrite`)))
	addAuditExemption(p, 1, "user:ada@example.com", "Data Read")
	p.run(chromedp.Click(chromedp.CSS(`#action-panel-save`)))
	p.waitFor(`document.querySelector("#action-panel") === null`)
	pol, err := projects.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: "projects/" + project})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"accessapproval.googleapis.com", "accesscontextmanager.googleapis.com"} {
		var got []string
		for _, lc := range auditConfigOf(pol, s).GetAuditLogConfigs() {
			got = append(got, lc.GetLogType().String()+"/"+strings.Join(lc.GetExemptedMembers(), ","))
		}
		// Exempting Ada from Data Read enables it, as Google documents.
		if strings.Join(got, " ") != "DATA_READ/user:ada@example.com DATA_WRITE/" {
			t.Errorf("%s's audit config is %v", s, got)
		}
	}

	// One service: its own configuration, the exemption read back.
	p.navigate("/iam-admin/audit?project=" + project)
	p.waitFor(`document.querySelectorAll("#view tbody tr").length > 0`)
	p.eval(`(() => { const f = document.querySelector("#view input.filter"); f.value = "accessapproval.googleapis.com";
		f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, nil)
	p.run(chromedp.Click(chromedp.CSS(`#view input[aria-label="Select accessapproval.googleapis.com"]`)))
	p.waitFor(`(document.querySelector("#action-panel-title") || {}).textContent === "Edit log types: accessapproval.googleapis.com"`)
	var checked []bool
	p.eval(`["#f-adminRead", "#f-dataRead", "#f-dataWrite"].map((s) => document.querySelector("#action-panel " + s).checked)`, &checked)
	if fmt.Sprint(checked) != "[false true true]" {
		t.Errorf("accessapproval's Log Types read %v; want Data Read and Data Write", checked)
	}
	p.run(chromedp.Click(chromedp.CSS(`#action-panel-exempted-principals-tab`)))
	var who string
	p.eval(`document.querySelector('#action-panel input[aria-label="Principal 1"]').value`, &who)
	if who != "user:ada@example.com" {
		t.Errorf("the exempted principal reads %q", who)
	}
	p.clickText("#action-panel button", "Delete")
	p.run(chromedp.Click(chromedp.CSS(`#action-panel-save`)))
	p.waitFor(`document.querySelector("#action-panel") === null`)
	pol, err = projects.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: "projects/" + project})
	if err != nil {
		t.Fatal(err)
	}
	for _, lc := range auditConfigOf(pol, "accessapproval.googleapis.com").GetAuditLogConfigs() {
		if len(lc.GetExemptedMembers()) > 0 {
			t.Errorf("after Delete accessapproval's %s still exempts %v", lc.GetLogType(), lc.GetExemptedMembers())
		}
	}
}

// TestIAMRound4ScreensMeetWCAG (#2013): Select a role open on its groups,
// Create custom role from selection with its permissions listed, Add
// Permissions with All Types, the Audit Logs info panel on each tab, and
// Edit policy's Custom value rows, audited as every other screen is.
func TestIAMRound4ScreensMeetWCAG(t *testing.T) {
	needService(t, "iam")
	needService(t, "iam-roles")
	needService(t, "iam-audit")
	needService(t, "iam-orgpolicies")
	project := uniqueProject(t)
	q := "?project=" + project
	auditScreens(t, []a11yPage{
		// Each ready clicks once (a mark on the control, which a new
		// render replaces), then waits for what opened.
		{"Grant access with Select a role open", "/iam-admin/iam" + q,
			`((b) => b && document.querySelector("#iam-principals-table, #iam-body .state") && !b.dataset.a11y &&
			   (b.dataset.a11y = "1", b.click()))(document.querySelector("#iam-grant-access")),
			  document.querySelector(".modal.is-open .iam-role-button") !== null`,
			// Open on its groups, focus back in the dialog, so Escape
			// closes the dialog as the keyboard audit expects.
			`document.querySelector(".modal.is-open .iam-role-button").click(); document.querySelector("#iam-new-principals").focus()`},
		{"Create custom role from selection", "/iam-admin/roles/create" + q + "&roles=predefined:storage.objectViewer",
			`document.querySelector('#view input[data-value="storage.objects.get"]') !== null`, ""},
		{"Add Permissions with All Types", "/iam-admin/roles/create" + q, `document.querySelector("#view #f-permissions-add") !== null`,
			`document.querySelector("#view #f-permissions-add").click()`},
		{"Audit Logs info panel, Log Types", "/iam-admin/audit" + q,
			`document.querySelector('#view input[aria-label="Select accessapproval.googleapis.com"]') !== null`,
			`document.querySelector('#view input[aria-label="Select accessapproval.googleapis.com"]').click()`},
		{"Audit Logs info panel, Exempted Principals", "/iam-admin/audit" + q,
			`((b) => b && !b.dataset.a11y && (b.dataset.a11y = "1", b.click()))(document.querySelector('#view input[aria-label="Select accessapproval.googleapis.com"]')),
			  document.querySelector("#action-panel-exempted-principals-tab") !== null`,
			`document.querySelector("#action-panel-exempted-principals-tab").click();
			 [...document.querySelectorAll("#action-panel button")].find((b) => b.textContent === "Add exempted principal").click()`},
		{"Set Default Configuration panel", "/iam-admin/audit" + q, `document.querySelector("#view .action-bar button") !== null`,
			`[...document.querySelectorAll("#view .action-bar button")].find((b) => b.textContent === "Set Default Configuration").click()`},
		{"Edit policy with Custom value", "/iam-admin/orgpolicies/" + "constraints%2Fgcp.resourceLocations/edit" + q,
			`document.querySelector("#orgpolicy-set") !== null`,
			`document.querySelector("#orgpolicy-source-override").click(); document.querySelector("#orgpolicy-add-rule").click();
			 document.querySelector("#orgpolicy-values-0-custom").click(); document.querySelector(".orgpolicy-add-value").click()`},
	})
}
