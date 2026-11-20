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

// TestCloudSQLConsoleUserPasswordPolicyAndRetainedPassword (#2009), in the
// sql shard: on a MySQL 8.4 instance's page, Add user account's Set user
// password policy is no longer "not available": checked, it shows Lock
// after failed attempts (with the number of attempts), Set password to
// expire (with the days) and Require current password, and Add makes a
// user the official Admin client reads with that policy; the Users list
// draws its Password status; Change password's Retain current password
// keeps the old one (dualPasswordType DUAL_PASSWORD), shown as Old password
// retained, and Discard old password drops it; Set password policy changes
// the policy and Unlock answers. Add user account with the policy shown
// passes the console's WCAG 2.1 AA audit.
func TestCloudSQLConsoleUserPasswordPolicyAndRetainedPassword(t *testing.T) {
	needService(t, "cloudsql-instances")
	needService(t, "cloudsql-mysql")
	svc := sqlAdminSDK(t)
	project := servedProject(t)
	name := fmt.Sprintf("browser-upol-%d", time.Now().UnixNano()%1e6)
	createSQL(t, svc, project, &sqladmin.DatabaseInstance{Name: name, DatabaseVersion: "MYSQL_8_4", RootPassword: "Root-pass1",
		Region: "us-central1", Settings: &sqladmin.Settings{Tier: "db-custom-1-3840"}})

	p := open(t)
	page := "/cloudsql-instances/" + name + "?project=" + project
	submit := func() {
		t.Helper()
		p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
		p.waitFor(`document.querySelector(".modal.is-open") === null`)
	}
	check := func(id string, on bool) {
		t.Helper()
		p.eval(fmt.Sprintf(`(() => { const f = document.querySelector(".modal.is-open #f-%s"); f.checked = %v;
			f.dispatchEvent(new Event("change", { bubbles: true })); return true; })()`, id, on), nil)
	}
	shown := func(id string) bool {
		t.Helper()
		var on bool
		p.eval(fmt.Sprintf(`(() => { const f = document.querySelector(".modal.is-open #f-%s");
			return !!f && !f.closest(".form-row").hidden && !f.disabled; })()`, id), &on)
		return on
	}
	// rowMenu opens label from polly's row menu on the Users tab.
	rowMenu := func(label, field string) {
		t.Helper()
		p.navigate(page)
		p.clickText(`#view [role="tab"]`, "Users")
		find := `[...document.querySelectorAll("#view tbody tr")].find((r) => [...r.querySelectorAll("td, th")].some((c) => c.textContent.trim() === "polly"))`
		p.waitFor(find + ` !== undefined`)
		p.eval(find+`.querySelector(".overflow-trigger").click()`, nil)
		p.clickText("#view .overflow-item, .overflow-item", label)
		if field == "" {
			p.waitFor(`document.querySelector(".modal.is-open form[data-confirm=confirm]") !== null`)
			return
		}
		p.waitFor(fmt.Sprintf(`document.querySelector(".modal.is-open #f-%s") !== null`, field))
	}
	getUser := func(user string) *sqladmin.User {
		t.Helper()
		u, err := svc.Users.Get(project, name, user).Host("%").Do()
		if err != nil {
			t.Fatalf("users.get %s: %v", user, err)
		}
		return u
	}

	// Add user account with a user password policy.
	p.navigate(page)
	p.clickText(p.findAction("Add user account"), "Add user account")
	p.waitFor(`document.querySelector(".modal.is-open #f-userPasswordPolicy") !== null`)
	if !shown("userPasswordPolicy") {
		t.Fatal("Set user password policy is not offered")
	}
	if shown("policyLock") {
		t.Error("the policy's options are drawn before Set user password policy is checked")
	}
	check("userPasswordPolicy", true)
	for _, id := range []string{"policyLock", "policyExpire", "policyRequireCurrent"} {
		if !shown(id) {
			t.Errorf("%s is not drawn under Set user password policy", id)
		}
	}
	check("policyLock", true)
	check("policyExpire", true)
	check("policyRequireCurrent", true)
	p.setField(".modal.is-open #f-userName", "polly")
	p.setField(".modal.is-open #f-password", "Polly-pass-1")
	p.setField(".modal.is-open #f-policyAttempts", "4")
	p.setField(".modal.is-open #f-policyExpireDays", "3")
	submit()
	u := getUser("polly")
	if pp := u.PasswordPolicy; pp == nil || pp.AllowedFailedAttempts != 4 || !pp.EnableFailedAttemptsCheck ||
		pp.PasswordExpirationDuration != "259200s" || !pp.EnablePasswordVerification {
		t.Fatalf("the added user's policy %+v", pp)
	}

	// The Users list draws the Password status.
	statusOf := func(user string) string {
		t.Helper()
		p.navigate(page)
		p.clickText(`#view [role="tab"]`, "Users")
		find := fmt.Sprintf(`[...document.querySelectorAll("#view tbody tr")].find((r) => [...r.querySelectorAll("td, th")].some((c) => c.textContent.trim() === %q))`, user)
		p.waitFor(find + ` !== undefined`)
		var cells []string
		p.eval(`[...(`+find+`).querySelectorAll("td, th")].map((c) => c.textContent.trim())`, &cells)
		return strings.Join(cells, " | ")
	}
	if s := statusOf("polly"); !strings.Contains(s, "Expires ") {
		t.Errorf("polly's row %q; want its expiration", s)
	}

	// Change password with Retain current password.
	rowMenu("Change password", "retainCurrentPassword")
	if !shown("retainCurrentPassword") {
		t.Fatal("Retain current password is not available")
	}
	p.setField(".modal.is-open #f-password", "Polly-pass-2")
	check("retainCurrentPassword", true)
	submit()
	if u := getUser("polly"); u.DualPasswordType != "DUAL_PASSWORD" {
		t.Errorf("after Retain current password, dualPasswordType %q", u.DualPasswordType)
	}
	if s := statusOf("polly"); !strings.Contains(s, "Old password retained") {
		t.Errorf("polly's row %q; want Old password retained", s)
	}

	// Discard old password.
	rowMenu("Discard old password", "")
	submit()
	if u := getUser("polly"); u.DualPasswordType == "DUAL_PASSWORD" {
		t.Errorf("after Discard old password, dualPasswordType %q", u.DualPasswordType)
	}

	// Set password policy, then Unlock.
	rowMenu("Set password policy", "policyLock")
	check("policyLock", true)
	p.setField(".modal.is-open #f-policyAttempts", "6")
	check("policyExpire", false)
	submit()
	if pp := getUser("polly").PasswordPolicy; pp == nil || pp.AllowedFailedAttempts != 6 || pp.PasswordExpirationDuration != "" {
		t.Errorf("after Set password policy: %+v", pp)
	}
	rowMenu("Unlock", "")
	submit()
	if pp := getUser("polly").PasswordPolicy; pp == nil || pp.AllowedFailedAttempts != 6 || (pp.Status != nil && pp.Status.Locked) {
		t.Errorf("after Unlock: %+v", pp)
	}

	q := "?project=" + project
	auditScreens(t, []a11yPage{
		{"add user account with a password policy", "/cloudsql-instances/" + name + q + "&tab=users",
			settled + ` && [...document.querySelectorAll("#view .section-actions button")].some((b) => b.textContent === "Add user account")`,
			`(() => { [...document.querySelectorAll("#view .section-actions button")].find((b) => b.textContent === "Add user account").click();
			const on = () => { const f = document.querySelector(".modal.is-open #f-userPasswordPolicy");
				if (!f) return setTimeout(on, 20);
				f.checked = true; f.dispatchEvent(new Event("change", { bubbles: true })); };
			on(); })()`},
	})
}
