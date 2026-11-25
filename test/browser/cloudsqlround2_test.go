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

// TestCloudSQLConsoleStopStartConnectionsAndPasswordPolicy (#1860), in the
// sql shard, on a created PostgreSQL instance's page:
//   - Stop, beside the title and confirmed in Google's dialog, stops it
//     (activationPolicy NEVER, which the API answers), the page then shows
//     it STOPPED with Start beside the title, and Start starts it again;
//   - Connections has Google's Summary, Networking, Security and
//     Connectivity tests tabs, Create client certificate on Security;
//   - with the password policy enabled (Edit's Enable password policies,
//     then Set password policy's dialog),
//     Add user account with a short password is refused with the policy's
//     words, and one that meets it is taken;
//   - the create page has Enable password policies after the password,
//     and Data cache's on choice is not available.
func TestCloudSQLConsoleStopStartConnectionsAndPasswordPolicy(t *testing.T) {
	needService(t, "cloudsql-instances")
	svc := sqlAdminSDK(t)
	project := servedProject(t)
	name := fmt.Sprintf("browser-ss-%d", time.Now().UnixNano()%1e6)
	op, err := svc.Instances.Insert(project, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "POSTGRES_17", RootPassword: "Root-pass1",
		Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}}).Do()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if d, err := svc.Instances.Delete(project, name).Do(); err == nil {
			waitSQL(t, svc, project, d, 5*time.Minute)
		}
	})
	waitSQL(t, svc, project, op, 10*time.Minute)
	policy := func() string {
		t.Helper()
		in, err := svc.Instances.Get(project, name).Do()
		if err != nil {
			t.Fatal(err)
		}
		return in.Settings.ActivationPolicy
	}
	waitPolicy := func(want string) {
		t.Helper()
		for deadline := time.Now().Add(3 * time.Minute); time.Now().Before(deadline); time.Sleep(time.Second) {
			in, err := svc.Instances.Get(project, name).Do()
			if err == nil && in.Settings.ActivationPolicy == want && in.State == "RUNNABLE" {
				ops, _ := svc.Operations.List(project).Instance(name).Do()
				if ops == nil || len(ops.Items) == 0 || ops.Items[0].Status == "DONE" {
					return
				}
			}
		}
		t.Fatalf("activationPolicy %s, want %s", policy(), want)
	}

	p := open(t)
	page := "/cloudsql-instances/" + name + "?project=" + project
	p.navigate(page)
	p.waitFor(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent === "Stop")`)
	p.clickText("#view .page-actions button", "Stop")
	p.waitFor(`document.querySelector(".modal.is-open form[data-confirm=confirm]") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	waitPolicy("NEVER")
	p.navigate(page)
	p.waitFor(`[...document.querySelectorAll("#view .page-actions button")].some((b) => b.textContent === "Start")`)
	var stopped bool
	p.eval(`document.querySelector("#view").textContent.includes("STOPPED")`, &stopped)
	if !stopped {
		t.Error("the stopped instance's page does not say STOPPED")
	}
	p.clickText("#view .page-actions button", "Start")
	p.waitFor(`document.querySelector(".modal.is-open form[data-confirm=confirm]") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	waitPolicy("ALWAYS")

	// Connections: Google's tabs, the client certificates on Security.
	p.navigate(page)
	p.clickText("#view [role=tab]", "Connections")
	p.waitFor(`document.querySelector("#view .sub-tab-strip") !== null`)
	var tabs []string
	p.eval(`[...document.querySelectorAll("#view .sub-tab-strip [role=tab]")].map((t) => t.textContent)`, &tabs)
	if strings.Join(tabs, ",") != "Summary,Networking,Security,Connectivity tests" {
		t.Errorf("Connections' tabs are %q; want Summary, Networking, Security, Connectivity tests", tabs)
	}
	p.clickText("#view .sub-tab-strip [role=tab]", "Security")
	p.waitFor(`[...document.querySelectorAll("#view .section-actions button")].some((b) => b.textContent === "Create client certificate")`)

	// Edit's Enable password policies, then Add user account under it.
	p.navigate(page)
	p.clickText("#view .page-actions button", "Edit")
	p.waitFor(`document.querySelector(".modal.is-open #f-enablePasswordPolicy") !== null`)
	p.eval(`(() => { const f = document.querySelector(".modal.is-open #f-enablePasswordPolicy"); f.checked = true;
		f.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, nil)
	// The policy's options are in Google's Set password policy dialog.
	p.clickText(".modal.is-open button", "Set password policy")
	p.waitFor(`document.querySelector(".modal.is-open .field-panel-dialog #f-passwordMinLength") !== null`)
	p.setField(".modal.is-open .field-panel-dialog #f-passwordMinLength", "12")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open .field-panel-dialog button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal.is-open .field-panel-dialog") === null && document.querySelectorAll(".modal.is-open").length === 1`)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(time.Second) {
		in, err := svc.Instances.Get(project, name).Do()
		if err == nil && in.Settings.PasswordValidationPolicy != nil && in.Settings.PasswordValidationPolicy.EnablePasswordPolicy &&
			in.Settings.PasswordValidationPolicy.MinLength == 12 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after Edit, the password policy is %+v", in.Settings.PasswordValidationPolicy)
		}
	}
	p.navigate(page)
	where := p.findAction("Add user account")
	p.clickText(where, "Add user account")
	p.waitFor(`document.querySelector(".modal.is-open #f-userName") !== null`)
	p.eval(`(() => { for (const [id, v] of [["userName", "shorty"], ["password", "Sh0rt!"]]) {
		const f = document.querySelector(".modal.is-open #f-" + id); f.value = v;
		f.dispatchEvent(new Event("input", { bubbles: true })); } return true; })()`, nil)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	if why := p.modalRefusal(); !strings.Contains(why, "password validation policy") {
		t.Errorf("Add user account with a short password: %q; want the policy's refusal", why)
	}
	// The refusal's 400 was provoked on purpose.
	p.forgive()
	p.eval(`(() => { const f = document.querySelector(".modal.is-open #f-password"); f.value = "Long-enough-pass1";
		f.dispatchEvent(new Event("input", { bubbles: true })); return true; })()`, nil)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	if u, err := svc.Users.Get(project, name, "shorty").Do(); err != nil || u.Name != "shorty" {
		t.Errorf("users.get after Add user account = %+v, %v", u, err)
	}

	// The create page: Enable password policies after the password, and
	// Data cache's on choice not available locally.
	p.navigate("/cloudsql-instances?project=" + project)
	p.clickText("#view button", "Create instance")
	p.waitFor(`document.querySelector("#f-enablePasswordPolicy") !== null`)
	var order bool
	p.eval(`(() => { const ids = [...document.querySelectorAll("[id^=f-]")].map((e) => e.id);
		return ids.indexOf("f-enablePasswordPolicy") === ids.indexOf("f-rootPassword") + 1; })()`, &order)
	if !order {
		t.Error("Enable password policies does not follow the password on the create page")
	}
	var cacheOff bool
	p.eval(`[...document.querySelectorAll("input[type=radio]")].some((r) => r.name.includes("dataCache") && r.value === "on" && r.disabled)`, &cacheOff)
	if !cacheOff {
		t.Error("Data cache can be turned on, though it is not available locally")
	}
}
