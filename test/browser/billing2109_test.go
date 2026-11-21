//go:build browser

package browser

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestBillingThroughThePage (#2109): Billing in the drawer, Manage billing
// accounts' Create account (a local account: no payment details), the
// account's page and its Link a project, Account management listing the
// project as linked, Budgets & alerts' Create budget (an amount, alert
// thresholds and a Pub/Sub topic) with its actual spend of 0, and Pricing's
// SKUs at the local price of 0. The console API reads each result back.
// Every screen meets WCAG 2.1 AA, as the console's others do.
func TestBillingThroughThePage(t *testing.T) {
	needService(t, "billing")
	project := uniqueProject(t)
	accountName := fmt.Sprintf("Browser account %s", strings.TrimPrefix(project, "cb-browser-"))
	list := func(service string) string {
		t.Helper()
		code, body := consoleDo(t, http.MethodGet, "/api/resources/"+service+"?project="+url.QueryEscape(project), "")
		if code != http.StatusOK {
			t.Fatalf("list %s through the console API = %d: %s", service, code, body)
		}
		return body
	}

	p := open(t)
	p.navigateByDrawer("Billing", "/billing/manage", "Billing")
	p.navigate("/billing/manage?project=" + project)
	p.clickText("#view button", "Create account")
	p.submitForm(map[string]string{"displayName": accountName}, "displayName")
	p.waitFor(fmt.Sprintf(`location.pathname === "/billing/manage" &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes(%q))`, accountName))
	var accounts struct {
		Items []struct {
			Name   string
			Fields map[string]string
		}
	}
	if err := json.Unmarshal([]byte(list("billing")), &accounts); err != nil {
		t.Fatal(err)
	}
	account := ""
	for _, a := range accounts.Items {
		if a.Fields["Name"] == accountName {
			account = a.Name
		}
	}
	if account == "" || !strings.HasPrefix(account, "billingAccounts/") {
		t.Fatalf("the new account is not listed: %+v", accounts.Items)
	}
	p.follow(account)
	var accountPath string
	p.eval(`location.pathname + location.search`, &accountPath)
	p.pageAction("Link a project", map[string]string{"project": project}, "")
	if got := list("billing-projects"); !strings.Contains(got, accountName) {
		t.Errorf("Account management after Link a project: %s", got)
	}

	p.navigate("/billing/budgets?project=" + project)
	p.clickText("#view button", "Create budget")
	p.submitForm(map[string]string{"account": account, "displayName": "browser budget", "projects": "projects/" + project,
		"amount": "125", "thresholds": "50,90", "topic": "projects/" + project + "/topics/budget-alerts"}, "displayName")
	p.waitFor(`location.pathname === "/billing/budgets" &&
		[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("browser budget"))`)
	got := list("billing-budgets")
	if !strings.Contains(got, "125.00 USD") || !strings.Contains(got, `"Actual spend":"0.00 USD"`) || !strings.Contains(got, "50%, 90%") ||
		!strings.Contains(got, "/topics/budget-alerts") {
		t.Errorf("Budgets & alerts reads %s", got)
	}

	p.navigate("/billing/pricing?project=" + project)
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("0.00 USD"))`)

	auditScreens(t, []a11yPage{
		screen("Manage billing accounts", "/billing/manage", project),
		screen("create billing account", "/billing/manage/create", project),
		{"billing account", accountPath, settled, ""},
		screen("Account management", "/billing/projects", project),
		screen("Budgets & alerts", "/billing/budgets", project),
		screen("create budget", "/billing/budgets/create", project),
		screen("Pricing", "/billing/pricing", project),
	})
}
