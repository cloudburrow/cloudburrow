//go:build browser

package browser

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	admin "cloud.google.com/go/iam/admin/apiv1"
	"cloud.google.com/go/iam/admin/apiv1/adminpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"github.com/chromedp/chromedp"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestIAMConditionEditorRunLinter (#1756): Run Linter in a folder's Edit
// condition dialog is the IAM API's lintPolicy of the folder's full
// resource name. A compile error shows an
// error result and Save is refused, the folder's policy unchanged; the
// linter runs again as the expression is edited, and once the error is
// gone Save stores the new expression, which the Folders API reads back.
func TestIAMConditionEditorRunLinter(t *testing.T) {
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
		Parent: org.GetName(), DisplayName: fmt.Sprintf("Lint %d", time.Now().UnixNano()%1e8)}})
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
	const role = "roles/resourcemanager.folderViewer"
	ada := "user:ada@example.com"
	start := `request.time.getHours("Europe/Berlin") >= 9`
	fixed := `request.time < timestamp("2099-01-01T00:00:00Z")`
	pol, err := folders.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: f.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := folders.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: f.GetName(), Policy: &iampb.Policy{Version: 3, Etag: pol.GetEtag(),
		Bindings: []*iampb.Binding{{Role: role, Members: []string{ada}, Condition: &expr.Expr{Title: "Office hours", Expression: start}}}}}); err != nil {
		t.Fatalf("SetIamPolicy: %v", err)
	}
	expression := func() string {
		pol, err := folders.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: f.GetName(),
			Options: &iampb.GetPolicyOptions{RequestedPolicyVersion: 3}})
		if err != nil {
			t.Fatalf("GetIamPolicy of %s: %v", f.GetName(), err)
		}
		for _, b := range pol.GetBindings() {
			if c := b.GetCondition(); c != nil {
				return c.GetExpression()
			}
		}
		return ""
	}

	p := open(t)
	p.navigate("/resource-hierarchy/" + f.GetName()[len("folders/"):])
	p.waitFor(`[...document.querySelectorAll("#view .tab-strip .tab")].some((t) => t.textContent === "Permissions")`)
	p.clickText("#view .tab-strip .tab", "Permissions")
	p.waitFor(`(document.querySelector("#permissions-table .permissions-condition") || {}).textContent?.includes("Office hours")`)
	p.run(chromedp.Click(chromedp.CSS(fmt.Sprintf(`button[aria-label=%q]`, "Edit the condition of "+ada+"'s "+role))))
	p.waitFor(`document.querySelector(".modal.is-open #f-conditionExpression") !== null`)
	setExpression := func(v string) {
		p.eval(fmt.Sprintf(`(() => { const e = document.querySelector(".modal.is-open #f-conditionExpression");
			e.value = %q; e.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, v), nil)
	}

	// The lint requests' bodies, to see the folder's full resource name in
	// each.
	p.eval(`(() => { window.__lintBodies = []; const f = window.fetch;
		window.fetch = (u, o) => { if (String(u).includes("/api/iam/lint")) window.__lintBodies.push(JSON.parse(o.body)); return f(u, o); };
		return true; })()`, nil)

	// A compile error: Run Linter shows it, and Save is refused.
	setExpression("1 <")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open #run-linter`)))
	p.waitFor(`document.querySelector("#lint-results .lint-error") !== null`)
	var resource string
	p.eval(`(window.__lintBodies[0] || {}).resource || ""`, &resource)
	if want := "//cloudresourcemanager.googleapis.com/" + f.GetName(); resource != want {
		t.Errorf("Run Linter sent LintPolicy the resource %q; want the folder's full resource name %q", resource, want)
	}
	var got string
	p.eval(`document.querySelector("#lint-results .lint-error").textContent`, &got)
	if !strings.Contains(got, "ERROR") || !strings.Contains(got, "ConditionCompileCheck") {
		t.Errorf("the linter's result for 1 < reads %q; want an ERROR of ConditionCompileCheck", got)
	}
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`(document.querySelector(".modal.is-open .form-error") || {}).textContent?.includes("linter found errors")`)
	if e := expression(); e != start {
		t.Errorf("after a refused Save the Folders API reads %q; want %q", e, start)
	}
	lints := len(p.posts("/api/iam/lint"))

	// Edited, the linter runs again by itself, and Save goes through.
	setExpression(fixed)
	p.waitFor(`document.querySelector("#lint-results .lint-error") === null && document.querySelector("#lint-results .lint-result") !== null`)
	if n := len(p.posts("/api/iam/lint")); n <= lints {
		t.Errorf("editing the expression sent %d lint requests in all, want more than %d", n, lints)
	}
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	if e := expression(); e != fixed {
		t.Errorf("after Save the Folders API reads %q; want %q", e, fixed)
	}
}

// TestIAMAuditLogsPage (#1756): IAM & Admin > Audit Logs lists the
// project's auditable services as the IAM API's queryAuditableServices
// answers them, with the log types the project's policy enables: Data
// Read on Cloud Storage, set through the Projects API, reads Enabled.
func TestIAMAuditLogsPage(t *testing.T) {
	needService(t, "iam-audit")
	opts := rmClientOptions(t)
	ctx := context.Background()
	project := uniqueProject(t)
	iamc, err := admin.NewIamClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	projects, err := resourcemanager.NewProjectsClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = iamc.Close(); _ = projects.Close() })
	want, err := iamc.QueryAuditableServices(ctx, &adminpb.QueryAuditableServicesRequest{
		FullResourceName: "//cloudresourcemanager.googleapis.com/projects/" + project})
	if err != nil || len(want.GetServices()) == 0 {
		t.Fatalf("QueryAuditableServices = %v, %v", want, err)
	}
	pol, err := projects.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: "projects/" + project})
	if err != nil {
		t.Fatal(err)
	}
	pol.AuditConfigs = []*iampb.AuditConfig{{Service: "storage.googleapis.com",
		AuditLogConfigs: []*iampb.AuditLogConfig{{LogType: iampb.AuditLogConfig_DATA_READ, ExemptedMembers: []string{"user:ada@example.com"}}}}}
	if _, err := projects.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: "projects/" + project, Policy: pol,
		// SetIamPolicy changes auditConfigs only when the mask names them.
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"bindings", "etag", "audit_configs"}}}); err != nil {
		t.Fatalf("SetIamPolicy: %v", err)
	}

	p := open(t)
	p.navigate("/iam-admin/audit?project=" + project)
	p.waitFor(`document.querySelectorAll("#view tbody tr").length > 0`)
	// The page shows 25 rows at a time: the first page is the first 25
	// services, in queryAuditableServices' order.
	var rows []string
	p.eval(`[...document.querySelectorAll("#view tbody tr")].map((r) => r.textContent)`, &rows)
	for i, r := range rows {
		if i >= len(want.GetServices()) || !strings.Contains(r, want.GetServices()[i].GetName()) {
			t.Errorf("row %d reads %q; queryAuditableServices answers %v", i, r, want.GetServices())
			break
		}
	}
	if len(rows) != min(25, len(want.GetServices())) {
		t.Errorf("the first page lists %d services; want %d", len(rows), min(25, len(want.GetServices())))
	}
	p.eval(`(() => { const f = document.querySelector("#view input.filter"); f.value = "storage.googleapis.com";
		f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, nil)
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].length < 25`)
	var storage string
	p.eval(`[...document.querySelectorAll("#view tbody tr")].map((r) => r.textContent).find((t) => t.startsWith("storage.googleapis.com")) || ""`, &storage)
	if !strings.Contains(storage, "storage.googleapis.com") || !strings.Contains(storage, "Enabled") || !strings.Contains(storage, "user:ada@example.com") {
		t.Errorf("storage.googleapis.com's row reads %q; want Data Read Enabled with user:ada@example.com exempted", storage)
	}
}

// addAuditExemption opens the Audit Logs info panel's Exempted Principals
// tab, adds exempted principal n with Add exempted principal, and checks
// logType in its Disabled Log Types (#2013).
func addAuditExemption(p *tab, n int, principal, logType string) {
	p.t.Helper()
	p.run(chromedp.Click(chromedp.CSS(`#action-panel-exempted-principals-tab`)))
	p.clickText("#action-panel button", "Add exempted principal")
	p.waitFor(fmt.Sprintf(`document.querySelector('#action-panel input[aria-label="Principal %d"]') !== null`, n))
	setIn(p, fmt.Sprintf(`#action-panel input[aria-label="Principal %d"]`, n), principal)
	p.run(chromedp.Click(chromedp.CSS(fmt.Sprintf(`#action-panel input[aria-label="%s %d"]`, logType, n))))
}

// auditConfigOf is the policy's audit config of service, or nil.
func auditConfigOf(pol *iampb.Policy, service string) *iampb.AuditConfig {
	for _, c := range pol.GetAuditConfigs() {
		if c.GetService() == service {
			return c
		}
	}
	return nil
}

// TestIAMAuditLogsFolder (#1756): picking a folder in the Audit Logs
// page's resource selector lists the folder's auditable services, and Edit
// log types on Cloud Storage's row saves Data Read with two exempted
// principals to the folder's policy, through SetIamPolicy with the
// auditConfigs mask: the Folders API reads the audit config back, and the
// folder's bindings are as they were.
func TestIAMAuditLogsFolder(t *testing.T) {
	needService(t, "iam-audit")
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
		Parent: org.GetName(), DisplayName: fmt.Sprintf("Audit %d", time.Now().UnixNano()%1e8)}})
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
	pol, err := folders.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: f.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	const kept = "user:kept@example.com"
	pol.Bindings = []*iampb.Binding{{Role: "roles/resourcemanager.folderViewer", Members: []string{kept}}}
	if _, err := folders.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: f.GetName(), Policy: pol}); err != nil {
		t.Fatalf("SetIamPolicy: %v", err)
	}

	p := open(t)
	p.navigate("/iam-admin/audit")
	p.waitFor(fmt.Sprintf(`[...(document.querySelector("#scope-auditResource") || {}).options || []].some((o) => o.value === %q)`, f.GetName()))
	p.eval(fmt.Sprintf(`(() => { const s = document.querySelector("#scope-auditResource"); s.value = %q;
		s.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, f.GetName()), nil)
	p.waitFor(fmt.Sprintf(`location.search.includes(%q) && document.querySelectorAll("#view tbody tr").length > 0`, "auditResource="+strings.ReplaceAll(f.GetName(), "/", "%2F")))
	p.eval(`(() => { const f = document.querySelector("#view input.filter"); f.value = "storage.googleapis.com";
		f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, nil)
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].length < 25`)
	// Google's info panel (#2013): checking the service opens it, Log
	// Types first; Exempted Principals' Add exempted principal takes each
	// principal with its Disabled Log Types; Save.
	p.run(chromedp.Click(chromedp.CSS(`#view input[aria-label="Select storage.googleapis.com"]`)))
	p.waitFor(`document.querySelector("#action-panel #f-dataRead") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#action-panel #f-dataRead`)))
	addAuditExemption(p, 1, "user:ada@example.com", "Data Read")
	addAuditExemption(p, 2, "user:bo@example.com", "Data Read")
	p.run(chromedp.Click(chromedp.CSS(`#action-panel-save`)))
	p.waitFor(`document.querySelector("#action-panel") === null`)
	for _, u := range p.posts("/api/actions/iam-audit") {
		if !strings.Contains(u, "auditResource="+strings.ReplaceAll(f.GetName(), "/", "%2F")) {
			t.Errorf("Edit log types was sent to %s; want the folder's resource in the address", u)
		}
	}
	got, err := folders.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: f.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	c := auditConfigOf(got, "storage.googleapis.com")
	if len(c.GetAuditLogConfigs()) != 1 || c.GetAuditLogConfigs()[0].GetLogType() != iampb.AuditLogConfig_DATA_READ ||
		strings.Join(c.GetAuditLogConfigs()[0].GetExemptedMembers(), ",") != "user:ada@example.com,user:bo@example.com" {
		t.Errorf("the folder's audit config of storage.googleapis.com = %v; want DATA_READ exempting ada and bo", c)
	}
	if len(got.GetBindings()) != 1 || got.GetBindings()[0].GetMembers()[0] != kept {
		t.Errorf("the folder's bindings after Edit log types = %v; want them as they were", got.GetBindings())
	}
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.startsWith("storage.googleapis.com") && r.textContent.includes("user:bo@example.com"))`)
}

// TestIAMAuditLogsOrganization (#1756): the organization's Audit Logs
// page lists the organization's auditable services, and Set default
// configuration saves Admin Read for allServices to the organization's
// policy, through SetIamPolicy with the auditConfigs mask: the
// Organizations API reads it back and every row reads Admin Read Enabled.
// The organization's audit configs are put back afterwards.
func TestIAMAuditLogsOrganization(t *testing.T) {
	needService(t, "iam-audit")
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
	before, err := orgs.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: org.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		now, err := orgs.GetIamPolicy(context.Background(), &iampb.GetIamPolicyRequest{Resource: org.GetName()})
		if err != nil {
			return
		}
		_, _ = orgs.SetIamPolicy(context.Background(), &iampb.SetIamPolicyRequest{Resource: org.GetName(),
			Policy:     &iampb.Policy{Version: 3, Etag: now.GetEtag(), AuditConfigs: before.GetAuditConfigs()},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"audit_configs"}}})
	})
	iamc, err := admin.NewIamClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = iamc.Close() })
	want, err := iamc.QueryAuditableServices(ctx, &adminpb.QueryAuditableServicesRequest{
		FullResourceName: "//cloudresourcemanager.googleapis.com/" + org.GetName()})
	if err != nil || len(want.GetServices()) == 0 {
		t.Fatalf("QueryAuditableServices = %v, %v", want, err)
	}

	p := open(t)
	p.navigate("/iam-admin/audit?auditResource=" + org.GetName())
	p.waitFor(`document.querySelectorAll("#view tbody tr").length > 0`)
	var selected string
	p.eval(`document.querySelector("#scope-auditResource").value`, &selected)
	if selected != org.GetName() {
		t.Errorf("the resource selector reads %q; want %s", selected, org.GetName())
	}
	var first string
	p.eval(`document.querySelector("#view tbody tr").textContent`, &first)
	if !strings.HasPrefix(first, want.GetServices()[0].GetName()) {
		t.Errorf("the first row reads %q; queryAuditableServices answers %s first", first, want.GetServices()[0].GetName())
	}
	p.clickText("#view button", "Set Default Configuration")
	p.waitFor(`document.querySelector("#action-panel #f-adminRead") !== null`)
	p.eval(`(() => { const b = document.querySelector("#action-panel #f-adminRead"); if (!b.checked) b.click(); return true; })()`, nil)
	p.run(chromedp.Click(chromedp.CSS(`#action-panel-save`)))
	p.waitFor(`document.querySelector("#action-panel") === null`)
	got, err := orgs.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: org.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	enabled := false
	for _, lc := range auditConfigOf(got, "allServices").GetAuditLogConfigs() {
		enabled = enabled || lc.GetLogType() == iampb.AuditLogConfig_ADMIN_READ
	}
	if !enabled {
		t.Errorf("the organization's allServices audit config = %v; want ADMIN_READ", auditConfigOf(got, "allServices"))
	}
	if len(got.GetBindings()) != len(before.GetBindings()) {
		t.Errorf("the organization's bindings went from %d to %d on Set default configuration", len(before.GetBindings()), len(got.GetBindings()))
	}
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].every((r) => r.textContent.includes("Enabled"))`)
}
