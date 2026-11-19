//go:build browser

package browser

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	orgpolicy "cloud.google.com/go/orgpolicy/apiv2"
	"cloud.google.com/go/orgpolicy/apiv2/orgpolicypb"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestIAMOrgPolicyCustomConstraintsThroughThePage (#1891), in the storage
// shard: the Organization Policies page's Custom constraints. Custom
// constraint creates one (display name, ID, description, resource type,
// enforcement on create and update, condition, Deny) that the official
// cloud.google.com/go/orgpolicy/apiv2 client reads; Edit changes its
// condition and action; Delete removes it. (A condition that is not CEL
// is refused by the call, TestConsoleOrgPolicyCustomConstraints: the
// suite fails a page that logs a 400.)
func TestIAMOrgPolicyCustomConstraintsThroughThePage(t *testing.T) {
	needService(t, "iam-orgpolicies")
	ctx := context.Background()
	client, err := orgpolicy.NewClient(ctx, rmClientOptions(t)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	project := uniqueProject(t)
	id := fmt.Sprintf("browser%d", time.Now().UnixNano()%1e12)
	name := "organizations/100000000001/customConstraints/custom." + id
	t.Cleanup(func() {
		_ = client.DeleteCustomConstraint(context.Background(), &orgpolicypb.DeleteCustomConstraintRequest{Name: name})
	})
	get := func() *orgpolicypb.CustomConstraint {
		t.Helper()
		c, err := client.GetCustomConstraint(ctx, &orgpolicypb.GetCustomConstraintRequest{Name: name})
		if err != nil {
			t.Fatalf("GetCustomConstraint: %v", err)
		}
		return c
	}

	p := open(t)
	p.navigate("/iam-admin/orgpolicies?project=" + project)
	p.waitFor(`document.querySelector("#orgpolicy-custom-constraints") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#orgpolicy-custom-constraints`)))
	p.waitFor(`document.querySelector("#orgpolicy-custom-create") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#orgpolicy-custom-create`)))
	p.waitFor(`document.querySelector("#custom-save") !== null`)
	setIn(p, "#custom-display-name", "Versioned buckets")
	setIn(p, "#custom-id", id)
	setIn(p, "#custom-description", "Buckets must have versioning enabled.")
	setIn(p, "#custom-resource-type", "storage.googleapis.com/Bucket")
	setIn(p, "#custom-condition", "resource.versioning.enabled == false")
	p.run(chromedp.Click(chromedp.CSS(`#custom-method-update`)))
	p.run(chromedp.Click(chromedp.CSS(`#custom-action-DENY`)))
	p.run(chromedp.Click(chromedp.CSS(`#custom-save`)))
	p.waitFor(`document.querySelector("#orgpolicy-custom-table tr[data-constraint='custom.` + id + `']") !== null`)
	c := get()
	if c.GetDisplayName() != "Versioned buckets" || c.GetActionType() != orgpolicypb.CustomConstraint_DENY ||
		strings.Join(c.GetResourceTypes(), ",") != "storage.googleapis.com/Bucket" || len(c.GetMethodTypes()) != 2 ||
		c.GetCondition() != "resource.versioning.enabled == false" || c.GetDescription() != "Buckets must have versioning enabled." {
		t.Errorf("the created custom constraint is %v", c)
	}

	// Edit: the condition as an ALLOW, on create only.
	p.navigate("/iam-admin/orgpolicies/custom-constraints/custom." + id + "/edit?project=" + project)
	p.waitFor(`(document.querySelector("#custom-condition") || {}).value === "resource.versioning.enabled == false"`)
	var disabled bool
	p.eval(`document.querySelector("#custom-id").disabled`, &disabled)
	if !disabled {
		t.Error("the edit page lets the constraint ID change")
	}
	setIn(p, "#custom-condition", "resource.versioning.enabled == true")
	p.run(chromedp.Click(chromedp.CSS(`#custom-action-ALLOW`)))
	p.run(chromedp.Click(chromedp.CSS(`#custom-method-create`)))
	p.run(chromedp.Click(chromedp.CSS(`#custom-save`)))
	p.waitFor(`document.querySelector("#orgpolicy-custom-table") !== null && document.querySelector("#custom-save") === null`)
	c = get()
	if c.GetActionType() != orgpolicypb.CustomConstraint_ALLOW || c.GetCondition() != "resource.versioning.enabled == true" || len(c.GetMethodTypes()) != 1 {
		t.Errorf("the edited custom constraint is %v", c)
	}

	// Delete.
	p.navigate("/iam-admin/orgpolicies/custom-constraints?project=" + project)
	p.waitFor(`document.querySelector("#orgpolicy-custom-table tr[data-constraint='custom.` + id + `'] button.danger") !== null`)
	p.run(chromedp.Click(chromedp.CSS(`#orgpolicy-custom-table tr[data-constraint='custom.` + id + `'] button.danger`)))
	p.waitFor(`document.activeElement === document.querySelector(".modal #confirm-input")`)
	p.run(chromedp.SendKeys(chromedp.CSS(`.modal #confirm-input`), "custom."+id), chromedp.KeyEvent(kb.Enter))
	p.waitFor(`document.querySelector(".modal") === null && document.querySelector("#orgpolicy-custom-table tr[data-constraint='custom.` + id + `']") === null`)
	if _, err := client.GetCustomConstraint(ctx, &orgpolicypb.GetCustomConstraintRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("after Delete GetCustomConstraint = %v; want NOT_FOUND", err)
	}
}
