//go:build browser

package browser

import (
	"context"
	"net/url"
	"strings"
	"testing"

	orgpolicy "cloud.google.com/go/orgpolicy/apiv2"
	"cloud.google.com/go/orgpolicy/apiv2/orgpolicypb"
	"github.com/chromedp/chromedp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// openOrgPolicyEdit opens Edit policy for constraint through its policy
// details' Manage policy, as Google's page does.
func openOrgPolicyEdit(p *tab, project, constraint string) {
	p.t.Helper()
	p.navigate("/iam-admin/orgpolicies/" + url.PathEscape(constraint) + "?project=" + project)
	p.clickText("#view button, #view a", "Manage policy")
	p.waitFor(`document.querySelector("#orgpolicy-set") !== null`)
}

// TestIAMOrgPolicyEditThroughThePage (#1889), in the storage shard: the
// Organization Policies page's Edit policy. Override parent's policy with
// Replace and a custom Allow rule under a tag condition for
// constraints/gcp.resourceLocations, then an Enforcement On rule for
// constraints/storage.uniformBucketLevelAccess, are the policies the
// official cloud.google.com/go/orgpolicy/apiv2 client reads; Merge with
// parent sets inheritFromParent; Google-managed default sets reset; the Dry
// run tab sets dryRunSpec alone; Inherit parent's policy deletes the
// policy.
func TestIAMOrgPolicyEditThroughThePage(t *testing.T) {
	needService(t, "iam-orgpolicies")
	ctx := context.Background()
	client, err := orgpolicy.NewClient(ctx, rmClientOptions(t)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	project := uniqueProject(t)
	get := func(c string) *orgpolicypb.Policy {
		t.Helper()
		pol, err := client.GetPolicy(ctx, &orgpolicypb.GetPolicyRequest{Name: "projects/" + project + "/policies/" + c})
		if err != nil {
			t.Fatalf("GetPolicy %s: %v", c, err)
		}
		return pol
	}

	// A list constraint: Override, Replace, a custom Allow rule with a
	// condition.
	p := open(t)
	openOrgPolicyEdit(p, project, "constraints/gcp.resourceLocations")
	p.run(chromedp.Click(chromedp.CSS(`#orgpolicy-source-override`)))
	p.run(chromedp.Click(chromedp.CSS(`#orgpolicy-enforcement-replace`)))
	p.run(chromedp.Click(chromedp.CSS(`#orgpolicy-add-rule`)))
	p.waitFor(`document.querySelector('.orgpolicy-rule[data-rule="0"]') !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#orgpolicy-values-0-custom`)))
	// Google's Custom value, then Add value for a second (#2013).
	setIn(p, `.orgpolicy-rule[data-rule="0"] .orgpolicy-custom-value`, "in:us-locations")
	p.run(chromedp.Click(chromedp.CSS(`.orgpolicy-rule[data-rule="0"] .orgpolicy-add-value`)))
	setIn(p, `.orgpolicy-rule[data-rule="0"] .orgpolicy-custom-line:nth-child(2) .orgpolicy-custom-value`, "europe-west1")
	p.run(chromedp.Click(chromedp.CSS(`.orgpolicy-rule[data-rule="0"] summary`)))
	setIn(p, `.orgpolicy-rule[data-rule="0"] .orgpolicy-condition-title`, "prod")
	setIn(p, `.orgpolicy-rule[data-rule="0"] .orgpolicy-condition`, "resource.matchTag('123456789012/env', 'prod')")
	p.run(chromedp.Click(chromedp.CSS(`#orgpolicy-set`)))
	p.waitFor(`document.querySelector("#orgpolicy-set") === null`)
	pol := get("gcp.resourceLocations")
	spec := pol.GetSpec()
	if spec.GetInheritFromParent() || len(spec.GetRules()) != 1 {
		t.Fatalf("the list policy's spec is %v", spec)
	}
	r := spec.GetRules()[0]
	if strings.Join(r.GetValues().GetAllowedValues(), ",") != "in:us-locations,europe-west1" || r.GetCondition().GetTitle() != "prod" ||
		!strings.Contains(r.GetCondition().GetExpression(), "matchTag") {
		t.Errorf("the list policy's rule is %v", r)
	}

	// Merge with parent.
	openOrgPolicyEdit(p, project, "constraints/gcp.resourceLocations")
	var checked bool
	p.eval(`document.querySelector("#orgpolicy-source-override").checked && document.querySelector("#orgpolicy-enforcement-replace").checked`, &checked)
	if !checked {
		t.Error("Edit policy does not read back Override parent's policy and Replace")
	}
	p.run(chromedp.Click(chromedp.CSS(`#orgpolicy-enforcement-merge`)))
	p.run(chromedp.Click(chromedp.CSS(`#orgpolicy-set`)))
	p.waitFor(`document.querySelector("#orgpolicy-set") === null`)
	if !get("gcp.resourceLocations").GetSpec().GetInheritFromParent() {
		t.Error("Merge with parent did not set inheritFromParent")
	}

	// A boolean constraint: Enforcement On.
	openOrgPolicyEdit(p, project, "constraints/compute.requireOsLogin")
	p.run(chromedp.Click(chromedp.CSS(`#orgpolicy-source-override`)))
	p.run(chromedp.Click(chromedp.CSS(`#orgpolicy-add-rule`)))
	p.waitFor(`document.querySelector('#orgpolicy-enforce-0-on') !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#orgpolicy-enforce-0-on`)))
	p.run(chromedp.Click(chromedp.CSS(`#orgpolicy-set`)))
	p.waitFor(`document.querySelector("#orgpolicy-set") === null`)
	if rules := get("compute.requireOsLogin").GetSpec().GetRules(); len(rules) != 1 || !rules[0].GetEnforce() {
		t.Errorf("the boolean policy's rules are %v", rules)
	}

	// The Dry run tab sets dryRunSpec and leaves spec.
	openOrgPolicyEdit(p, project, "constraints/compute.requireOsLogin")
	p.run(chromedp.Click(chromedp.CSS(`#orgpolicy-tab-dryrun`)))
	p.waitFor(`(document.querySelector("#orgpolicy-set") || {}).textContent === "Set dry run policy"`)
	p.run(chromedp.Click(chromedp.CSS(`#orgpolicy-source-override`)))
	p.run(chromedp.Click(chromedp.CSS(`#orgpolicy-add-rule`)))
	p.waitFor(`document.querySelector('#orgpolicy-enforce-0-off') !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#orgpolicy-enforce-0-off`)))
	p.run(chromedp.Click(chromedp.CSS(`#orgpolicy-set`)))
	p.waitFor(`document.querySelector("#orgpolicy-set") === null`)
	pol = get("compute.requireOsLogin")
	if d := pol.GetDryRunSpec().GetRules(); len(d) != 1 || d[0].GetEnforce() || !pol.GetSpec().GetRules()[0].GetEnforce() {
		t.Errorf("after the dry run the policy is %v", pol)
	}

	// Google-managed default, then Inherit parent's policy.
	openOrgPolicyEdit(p, project, "constraints/gcp.resourceLocations")
	p.run(chromedp.Click(chromedp.CSS(`#orgpolicy-source-default`)))
	p.run(chromedp.Click(chromedp.CSS(`#orgpolicy-set`)))
	p.waitFor(`document.querySelector("#orgpolicy-set") === null`)
	if !get("gcp.resourceLocations").GetSpec().GetReset_() {
		t.Error("Google-managed default did not set reset")
	}
	openOrgPolicyEdit(p, project, "constraints/gcp.resourceLocations")
	p.run(chromedp.Click(chromedp.CSS(`#orgpolicy-source-inherit`)))
	p.run(chromedp.Click(chromedp.CSS(`#orgpolicy-set`)))
	p.waitFor(`document.querySelector("#orgpolicy-set") === null`)
	if _, err := client.GetPolicy(ctx, &orgpolicypb.GetPolicyRequest{Name: "projects/" + project + "/policies/gcp.resourceLocations"}); status.Code(err) != codes.NotFound {
		t.Errorf("after Inherit parent's policy GetPolicy = %v; want NOT_FOUND", err)
	}
}
