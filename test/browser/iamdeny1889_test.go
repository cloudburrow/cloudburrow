//go:build browser

package browser

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"

	iam "cloud.google.com/go/iam/apiv2"
	"cloud.google.com/go/iam/apiv2/iampb"
	"github.com/chromedp/chromedp"
)

// setIn sets the value of the element sel matches, as typing would.
func setIn(p *tab, sel, v string) {
	p.t.Helper()
	p.eval(fmt.Sprintf(`(() => { const f = document.querySelector(%q); f.value = %q;
		f.dispatchEvent(new Event("input", { bubbles: true })); f.dispatchEvent(new Event("change", { bubbles: true }));
		return true; })()`, sel, v), nil)
}

// TestIAMDenyTabThroughThePage (#1889), in the storage shard: the IAM
// page's Deny tab, as Google lays it out. Create deny policy with a display
// name that fills in the ID and a rule with a denial condition is the policy
// the official cloud.google.com/go/iam/apiv2 client reads; the list shows it
// by Policy ID; its details' Edit renames it and adds a second rule with an
// exception principal; Delete removes it, as the client reads back.
func TestIAMDenyTabThroughThePage(t *testing.T) {
	needService(t, "iam")
	ctx := context.Background()
	client, err := iam.NewPoliciesClient(ctx, rmClientOptions(t)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	project := uniqueProject(t)
	parent := "policies/" + url.PathEscape("cloudresourcemanager.googleapis.com/projects/"+project) + "/denypolicies"

	p := open(t)
	p.navigate("/iam-admin/iam?project=" + project)
	p.waitFor(`document.querySelector("#iam-tab-deny") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#iam-tab-deny`)))
	p.waitFor(`document.querySelector("#iam-deny-create") !== null`)
	var options []string
	p.eval(`[...document.querySelectorAll("#iam-deny-resource option")].map((o) => o.value)`, &options)
	if len(options) < 2 || options[0] != "projects/"+project || !strings.HasPrefix(options[len(options)-1], "organizations/") {
		t.Errorf("the Deny tab's resource selector offers %v; want the project, then the organization", options)
	}

	// Create deny policy.
	p.run(chromedp.Click(chromedp.CSS(`#iam-deny-create`)))
	p.waitFor(`document.querySelector(".modal.is-open #iam-deny-display-name") !== null`)
	setIn(p, ".modal.is-open #iam-deny-display-name", "No project delete")
	var id string
	p.eval(`document.querySelector(".modal.is-open #iam-deny-id").value`, &id)
	if id != "no-project-delete" {
		t.Errorf("the display name filled in the ID %q; want no-project-delete", id)
	}
	rule := `.modal.is-open .iam-deny-rule-form:nth-of-type(%d) `
	setIn(p, fmt.Sprintf(rule, 1)+".iam-deny-denied-principals", "principal://goog/subject/ada@example.com")
	setIn(p, fmt.Sprintf(rule, 1)+".iam-deny-denied-permissions", "cloudresourcemanager.googleapis.com/projects.delete")
	p.run(chromedp.Click(chromedp.CSS(fmt.Sprintf(rule, 1) + "summary")))
	setIn(p, fmt.Sprintf(rule, 1)+".iam-deny-condition-title", "prod only")
	setIn(p, fmt.Sprintf(rule, 1)+".iam-deny-condition", "resource.matchTag('123456789012/env', 'prod')")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open #iam-deny-save`)))
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	got, err := client.GetPolicy(ctx, &iampb.GetPolicyRequest{Name: parent + "/no-project-delete"})
	if err != nil {
		t.Fatalf("GetPolicy after Create deny policy: %v", err)
	}
	if got.GetDisplayName() != "No project delete" || len(got.GetRules()) != 1 {
		t.Fatalf("the created policy is %v", got)
	}
	d := got.GetRules()[0].GetDenyRule()
	if strings.Join(d.GetDeniedPrincipals(), ",") != "principal://goog/subject/ada@example.com" ||
		strings.Join(d.GetDeniedPermissions(), ",") != "cloudresourcemanager.googleapis.com/projects.delete" ||
		d.GetDenialCondition().GetTitle() != "prod only" || !strings.Contains(d.GetDenialCondition().GetExpression(), "matchTag") {
		t.Errorf("the created rule is %v", d)
	}

	// The list, then the details' Edit.
	row := `#iam-deny-table tr[data-deny-policy="no-project-delete"]`
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, row))
	var text string
	p.eval(fmt.Sprintf(`document.querySelector(%q).textContent`, row), &text)
	if !strings.Contains(text, "No project delete") {
		t.Errorf("the policy's row reads %q", text)
	}
	p.run(chromedp.Click(chromedp.CSS(row + " button")))
	p.waitFor(`document.querySelector(".modal.is-open #iam-deny-edit") !== null`)
	p.eval(`document.querySelector(".modal.is-open #iam-deny-details").textContent`, &text)
	if !strings.Contains(text, "ada@example.com") || !strings.Contains(text, "prod only") {
		t.Errorf("the policy's details read %q", text)
	}
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open #iam-deny-edit`)))
	p.waitFor(`document.querySelector(".modal.is-open #iam-deny-add-rule") !== null`)
	setIn(p, ".modal.is-open #iam-deny-display-name", "Nobody deletes")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open #iam-deny-add-rule`)))
	p.waitFor(`document.querySelectorAll(".modal.is-open .iam-deny-rule-form").length === 2`)
	setIn(p, fmt.Sprintf(rule, 2)+".iam-deny-denied-principals", "principalSet://goog/public:all")
	setIn(p, fmt.Sprintf(rule, 2)+".iam-deny-exception-principals", "principal://goog/subject/bob@example.com")
	setIn(p, fmt.Sprintf(rule, 2)+".iam-deny-denied-permissions", "cloudresourcemanager.googleapis.com/projects.delete")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open #iam-deny-save`)))
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	got, err = client.GetPolicy(ctx, &iampb.GetPolicyRequest{Name: parent + "/no-project-delete"})
	if err != nil {
		t.Fatalf("GetPolicy after Edit: %v", err)
	}
	if got.GetDisplayName() != "Nobody deletes" || len(got.GetRules()) != 2 ||
		strings.Join(got.GetRules()[1].GetDenyRule().GetExceptionPrincipals(), ",") != "principal://goog/subject/bob@example.com" {
		t.Fatalf("the edited policy is %v", got)
	}

	// Delete.
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) !== null`, row))
	p.run(chromedp.Click(chromedp.CSS(row + " button")))
	p.waitFor(`document.querySelector(".modal.is-open #iam-deny-delete") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open #iam-deny-delete`)))
	p.waitFor(`document.querySelector(".modal.is-open #confirm-input") !== null`)
	setField(p, ".modal.is-open", "confirm-input", "no-project-delete")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(fmt.Sprintf(`document.querySelector(%q) === null && document.querySelector("#iam-deny-create") !== null`, row))
	list := client.ListPolicies(ctx, &iampb.ListPoliciesRequest{Parent: parent})
	if pol, err := list.Next(); err == nil {
		t.Errorf("after Delete the project still has deny policy %s", pol.GetName())
	}
}
