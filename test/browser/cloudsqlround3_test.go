//go:build browser

package browser

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	sqladmin "google.golang.org/api/sqladmin/v1"
)

// TestCloudSQLConsoleRound3DialogsAndCreateGroups (#1860), in the sql
// shard:
//   - the create page's Customize groups are Google's, in Google's order,
//     with Encryption, Connections, Security and Query insights each its
//     own;
//   - under Enable password policies, Set password policy opens Google's
//     dialog of the policy's options, ending in Save; Cancel puts back what
//     the options held;
//   - Create Instance with Minimum length 10 set in that dialog, an
//     authorized network and Query insights makes an instance the official
//     Admin client reads with them;
//   - Stop and Start are confirmed as Google's are, "Stop database
//     instance?" and "Start database instance?" with nothing typed;
//   - Create database opens "New database", ending in Create, the list has
//     Google's Collation type, Character set and Database type, and the
//     row's Delete asks for the name in "Delete database";
//   - Add user account opens "Add a user account to instance NAME" on
//     Built-in authentication, ending in Add, and the row's Remove asks for
//     nothing typed;
//   - Create backup ends in Create, and the row's Delete asks for the word
//     Delete in "Delete backup";
//   - the create page, its Set password policy dialog, the Stop dialog, Add
//     user account and New database pass the console's WCAG 2.1 AA audit.
func TestCloudSQLConsoleRound3DialogsAndCreateGroups(t *testing.T) {
	needService(t, "cloudsql-instances")
	svc := sqlAdminSDK(t)
	project := servedProject(t)
	name := fmt.Sprintf("browser-r3-%d", time.Now().UnixNano()%1e6)
	t.Cleanup(func() {
		if d, err := svc.Instances.Delete(project, name).Do(); err == nil {
			waitSQL(t, svc, project, d, 5*time.Minute)
		}
	})

	p := open(t)
	p.navigate("/cloudsql-instances?project=" + project)
	p.clickText("#view button", "Create instance")
	p.waitFor(`document.querySelector("#f-enablePasswordPolicy") !== null`)
	var steps []string
	p.eval(`[...document.querySelectorAll(".create-step-title")].map((s) => s.textContent)`, &steps)
	want := "Machine configuration,Data cache,Storage,Encryption,Connections,Security,Data protection,Maintenance,Flags,Query insights,Labels"
	if got := strings.Join(steps, ","); !strings.Contains(got, want) {
		t.Errorf("the create page's groups are %q; want %s in that order", got, want)
	}
	var placed struct{ Key, CA, Insights string }
	p.eval(`(() => { const step = (id) => { const s = document.querySelector(id).closest(".create-step");
		return s ? s.querySelector(".create-step-title").textContent : ""; };
		return { Key: step("#f-kmsKeyName"), CA: step("#f-serverCaMode"), Insights: step("#f-queryInsightsEnabled") }; })()`, &placed)
	if placed.Key != "Encryption" || placed.CA != "Security" || placed.Insights != "Query insights" {
		t.Errorf("CMEK, server CA mode and Query insights are under %+v", placed)
	}

	// Set password policy: drawn only under Enable password policies; its
	// dialog holds the options; Cancel restores, Save keeps.
	var shown bool
	p.eval(`document.querySelector("#panel-set-password-policy").closest(".form-row").hidden`, &shown)
	if !shown {
		t.Error("Set password policy is drawn while Enable password policies is unchecked")
	}
	p.eval(`(() => { const f = document.querySelector("#f-enablePasswordPolicy"); f.checked = true;
		f.dispatchEvent(new Event("change", { bubbles: true }));
		for (const h of document.querySelectorAll(".create-step-head[aria-expanded=false]")) h.click(); return true; })()`, nil)
	p.clickText("#view button", "Set password policy")
	p.waitFor(`document.querySelector(".modal.is-open .field-panel-dialog #f-passwordMinLength") !== null`)
	var title string
	p.eval(`document.querySelector(".modal.is-open h2").textContent`, &title)
	if title != "Set password policy" {
		t.Errorf("the dialog is titled %q; want Set password policy", title)
	}
	p.setField(".modal.is-open #f-passwordMinLength", "99")
	p.clickText(".modal.is-open button", "Cancel")
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	var minLen string
	p.eval(`document.querySelector("#f-passwordMinLength").value`, &minLen)
	if minLen != "" {
		t.Errorf("Cancel left Minimum length %q; want it as it was", minLen)
	}
	p.clickText("#view button", "Set password policy")
	p.waitFor(`document.querySelector(".modal.is-open .field-panel-dialog #f-passwordMinLength") !== null`)
	p.setField(".modal.is-open #f-passwordMinLength", "10")
	p.clickText(".modal.is-open button", "Save")
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	p.eval(`document.querySelector("#f-passwordMinLength").value`, &minLen)
	if minLen != "10" {
		t.Errorf("after Save, Minimum length is %q; want 10", minLen)
	}

	// Create Instance with what the groups set.
	p.eval(`(() => { for (const [id, on] of [["deletionProtection", false], ["pitr", false], ["queryInsightsEnabled", true]]) {
		const f = document.querySelector("#f-" + id); f.checked = on; f.dispatchEvent(new Event("change", { bubbles: true })); }
		return true; })()`, nil)
	p.submitForm(map[string]string{"name": name, "rootPassword": "Long-enough-pass1", "authorizedNetworks": "203.0.113.0/24"}, "name")
	var in *sqladmin.DatabaseInstance
	waitSQLInstance(t, svc, project, name, "RUNNABLE", func(i *sqladmin.DatabaseInstance) bool {
		in = i
		return i.State == "RUNNABLE"
	})
	st := in.Settings
	if st.PasswordValidationPolicy == nil || !st.PasswordValidationPolicy.EnablePasswordPolicy || st.PasswordValidationPolicy.MinLength != 10 {
		t.Errorf("the created instance's password policy is %+v; want enabled, minimum length 10", st.PasswordValidationPolicy)
	}
	if st.InsightsConfig == nil || !st.InsightsConfig.QueryInsightsEnabled {
		t.Errorf("the created instance's insightsConfig is %+v; want Query insights on", st.InsightsConfig)
	}
	if ic := st.IpConfiguration; ic == nil || !ic.Ipv4Enabled || len(ic.AuthorizedNetworks) != 1 || ic.AuthorizedNetworks[0].Value != "203.0.113.0/24" {
		t.Errorf("the created instance's ipConfiguration is %+v", ic)
	}
	if in.DiskEncryptionConfiguration != nil && in.DiskEncryptionConfiguration.KmsKeyName != "" {
		t.Errorf("a Google-managed instance has key %q", in.DiskEncryptionConfiguration.KmsKeyName)
	}
	idle := func(want string) func(*sqladmin.DatabaseInstance) bool {
		return func(i *sqladmin.DatabaseInstance) bool {
			if i.State != "RUNNABLE" || i.Settings.ActivationPolicy != want {
				return false
			}
			ops, _ := svc.Operations.List(project).Instance(name).Do()
			return ops == nil || len(ops.Items) == 0 || ops.Items[0].Status == "DONE"
		}
	}
	waitSQLInstance(t, svc, project, name, "idle", idle("ALWAYS"))

	// Stop and Start: Google's dialogs, nothing typed.
	page := "/cloudsql-instances/" + name + "?project=" + project
	confirmGoogle := func(action, title string) {
		t.Helper()
		p.navigate(page)
		p.clickText("#view .page-actions button", action)
		p.waitFor(`document.querySelector(".modal.is-open form[data-confirm=confirm]") !== null`)
		var d struct {
			Title string
			Typed bool
		}
		p.eval(`(() => { const m = document.querySelector(".modal.is-open");
			return { Title: m.querySelector("h2").textContent, Typed: m.querySelector("#confirm-input") !== null }; })()`, &d)
		if d.Title != title || d.Typed {
			t.Errorf("%s's confirmation is %+v; want %q with nothing typed", action, d, title)
		}
		p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	}
	confirmGoogle("Stop", "Stop database instance?")
	waitSQLInstance(t, svc, project, name, "stopped", idle("NEVER"))
	confirmGoogle("Start", "Start database instance?")
	waitSQLInstance(t, svc, project, name, "started", idle("ALWAYS"))

	// Databases: Google's New database, ending in Create; Google's
	// columns; Delete on the row, its name typed in "Delete database".
	dialog := func(label string) (title, submit string) {
		t.Helper()
		p.clickText(p.findAction(label), label)
		p.waitFor(`document.querySelectorAll(".modal").length === 1 && document.querySelector(".modal.is-open form") !== null`)
		var d struct{ Title, Submit string }
		p.eval(`(() => { const m = document.querySelector(".modal.is-open");
			return { Title: m.querySelector("h2").textContent, Submit: m.querySelector("button[type=submit]").textContent }; })()`, &d)
		return d.Title, d.Submit
	}
	submit := func() {
		t.Helper()
		p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
		p.waitFor(`document.querySelector(".modal.is-open") === null`)
	}
	p.navigate(page)
	if ti, sub := dialog("Create database"); ti != "New database" || sub != "Create" {
		t.Errorf("Create database opens %q ending in %q; want New database, Create", ti, sub)
	}
	p.setField(".modal.is-open #f-dbName", "r3db")
	submit()
	if _, err := svc.Databases.Get(project, name, "r3db").Do(); err != nil {
		t.Fatalf("databases.get after New database: %v", err)
	}
	p.navigate(page)
	p.clickText(`#view [role="tab"]`, "Databases")
	p.waitFor(`[...document.querySelectorAll("#view tbody td, #view tbody th")].some((c) => c.textContent.trim() === "r3db")`)
	var cols []string
	p.eval(`[...document.querySelectorAll("#view thead th")].map((h) => h.textContent.trim()).filter(Boolean)`, &cols)
	for _, w := range []string{"Name", "Collation type", "Character set", "Database type"} {
		if !contains(cols, w) {
			t.Errorf("the Databases columns are %q; want %s", cols, w)
		}
	}
	p.navigate(page)
	p.rowAction("Databases", "r3db", "Delete", nil, "r3db")
	waitSQLInstance(t, svc, project, name, "without r3db", func(*sqladmin.DatabaseInstance) bool {
		_, err := svc.Databases.Get(project, name, "r3db").Do()
		return err != nil
	})

	// Users: Google's Add a user account to instance NAME, Built-in
	// authentication by default, ending in Add; Remove with nothing typed.
	p.navigate(page)
	if ti, sub := dialog("Add user account"); ti != "Add a user account to instance "+name || sub != "Add" {
		t.Errorf("Add user account opens %q ending in %q", ti, sub)
	}
	var builtIn bool
	p.eval(`(() => { const r = [...document.querySelectorAll(".modal.is-open input[type=radio]")].find((r) => r.value === "BUILT_IN");
		return !!r && r.checked && document.querySelector(".modal.is-open #f-principal").closest(".form-row").hidden; })()`, &builtIn)
	if !builtIn {
		t.Error("Add user account does not start on Built-in authentication")
	}
	p.setField(".modal.is-open #f-userName", "r3user")
	p.setField(".modal.is-open #f-password", "Long-enough-pass2")
	submit()
	if _, err := svc.Users.Get(project, name, "r3user").Do(); err != nil {
		t.Fatalf("users.get after Add: %v", err)
	}
	p.navigate(page)
	p.clickText(`#view [role="tab"]`, "Users")
	find := `[...document.querySelectorAll("#view tbody tr")].find((r) => [...r.querySelectorAll("td, th")].some((c) => c.textContent.trim() === "r3user"))`
	p.waitFor(find + ` !== undefined`)
	p.eval(find+`.querySelector(".overflow-trigger").click()`, nil)
	p.clickText("#view .overflow-item, .overflow-item", "Remove")
	p.waitFor(`document.querySelector(".modal.is-open form[data-confirm=confirm]") !== null`)
	p.eval(`document.querySelector(".modal.is-open h2").textContent`, &title)
	if title != "Remove user account?" {
		t.Errorf("Remove's confirmation is %q", title)
	}
	submit()
	waitSQLInstance(t, svc, project, name, "without r3user", func(*sqladmin.DatabaseInstance) bool {
		_, err := svc.Users.Get(project, name, "r3user").Do()
		return err != nil
	})

	// Backups: Create backup, only Description, ending in Create; Delete on
	// the row asks for the word Delete in "Delete backup".
	p.navigate(page)
	if ti, sub := dialog("Create backup"); sub != "Create" {
		t.Errorf("Create backup (%q) ends in %q; want Create", ti, sub)
	}
	submit()
	var backupID string
	waitSQLInstance(t, svc, project, name, "backed up", func(*sqladmin.DatabaseInstance) bool {
		runs, err := svc.BackupRuns.List(project, name).Do()
		if err != nil || len(runs.Items) == 0 || runs.Items[0].Status != "SUCCESSFUL" {
			return false
		}
		backupID = fmt.Sprint(runs.Items[0].Id)
		return true
	})
	p.navigate(page)
	p.clickText(`#view [role="tab"]`, "Backups")
	find = fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].find((r) => [...r.querySelectorAll("td, th")].some((c) => c.textContent.trim() === %q))`, backupID)
	p.waitFor(find + ` !== undefined`)
	p.eval(find+`.querySelector(".overflow-trigger").click()`, nil)
	p.clickText("#view .overflow-item, .overflow-item", "Delete")
	p.waitFor(`document.querySelector(".modal.is-open #confirm-input") !== null`)
	p.eval(`document.querySelector(".modal.is-open h2").textContent`, &title)
	if title != "Delete backup" {
		t.Errorf("Delete backup's confirmation is %q", title)
	}
	p.confirmByName("Delete")
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	waitSQLInstance(t, svc, project, name, "backup deleted", func(*sqladmin.DatabaseInstance) bool {
		runs, err := svc.BackupRuns.List(project, name).Do()
		return err == nil && len(runs.Items) == 0
	})

	q := "?project=" + project
	createReady := settled + ` && document.querySelector("#f-enablePasswordPolicy") !== null`
	auditScreens(t, []a11yPage{
		{"Cloud SQL create instance", "/cloudsql-instances/create" + q, createReady, ""},
		{"set password policy dialog", "/cloudsql-instances/create" + q, createReady,
			`(() => { const f = document.querySelector("#f-enablePasswordPolicy"); f.checked = true;
			f.dispatchEvent(new Event("change", { bubbles: true }));
			document.querySelector("#panel-set-password-policy").click(); })()`},
		{"stop instance dialog", "/cloudsql-instances/" + name + q,
			settled + ` && [...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent === "Stop")`,
			`[...document.querySelectorAll("#view .page-actions button")].find((b) => b.textContent === "Stop").click()`},
		{"add user account dialog", "/cloudsql-instances/" + name + q + "&tab=users",
			settled + ` && [...document.querySelectorAll("#view .section-actions button")].some((b) => b.textContent === "Add user account")`,
			`[...document.querySelectorAll("#view .section-actions button")].find((b) => b.textContent === "Add user account").click()`},
		{"new database dialog", "/cloudsql-instances/" + name + q + "&tab=databases",
			settled + ` && [...document.querySelectorAll("#view .section-actions button")].some((b) => b.textContent === "Create database")`,
			`[...document.querySelectorAll("#view .section-actions button")].find((b) => b.textContent === "Create database").click()`},
	})
}
