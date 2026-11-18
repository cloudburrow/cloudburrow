//go:build browser

package browser

import (
	"context"
	"fmt"
	"testing"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"github.com/chromedp/chromedp"
)

// TestResourceHierarchyFolderIAMConditions (#1742), in the storage shard. A
// folder's Permissions tab grants a role under an IAM Condition (title,
// description, CEL expression), lists the condition on the principal's
// row, and Edit condition changes it: each is the v3 Folders API's
// SetIamPolicy, which its GetIamPolicy at version 3 reads back.
func TestResourceHierarchyFolderIAMConditions(t *testing.T) {
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
		Parent: org.GetName(), DisplayName: fmt.Sprintf("Cond %d", time.Now().UnixNano()%1e8)}})
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
	condition := func() string {
		pol, err := folders.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: f.GetName(),
			Options: &iampb.GetPolicyOptions{RequestedPolicyVersion: 3}})
		if err != nil {
			t.Fatalf("GetIamPolicy of %s: %v", f.GetName(), err)
		}
		for _, b := range pol.GetBindings() {
			if c := b.GetCondition(); c != nil {
				return c.GetTitle() + "|" + c.GetDescription() + "|" + c.GetExpression()
			}
		}
		return ""
	}
	const role = "roles/resourcemanager.folderViewer"
	ada := "user:ada@example.com"
	first := `request.time < timestamp("2099-01-01T00:00:00Z")`
	second := `request.time.getHours("Europe/Berlin") >= 9`

	p := open(t)
	p.navigate("/resource-hierarchy/" + f.GetName()[len("folders/"):])
	p.waitFor(`[...document.querySelectorAll("#view .tab-strip .tab")].some((t) => t.textContent === "Permissions")`)
	p.clickText("#view .tab-strip .tab", "Permissions")
	p.waitFor(`document.querySelector("#grant-access") !== null`)
	p.clickText("#view #grant-access", "Grant access")
	p.waitFor(`document.querySelector(".modal.is-open #f-conditionExpression") !== null`)
	p.eval(fmt.Sprintf(`(() => { const set = (s, v) => { const e = document.querySelector(".modal.is-open " + s);
		e.value = v; e.dispatchEvent(new Event("input", { bubbles: true })); };
		set("#f-members", %q); set("#f-role", %q); set("#f-conditionTitle", "Until 2099");
		set("#f-conditionDescription", "expires"); set("#f-conditionExpression", %q); return true; })()`, ada, role, first), nil)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	p.waitFor(`(document.querySelector("#permissions-table .permissions-condition") || {}).textContent?.includes("Until 2099")`)
	if got, want := condition(), "Until 2099|expires|"+first; got != want {
		t.Errorf("after Grant access with a condition the Folders API reads %q; want %q", got, want)
	}

	p.run(chromedp.Click(chromedp.CSS(fmt.Sprintf(`button[aria-label=%q]`, "Edit the condition of "+ada+"'s "+role))))
	p.waitFor(`document.querySelector(".modal.is-open #f-conditionExpression") !== null`)
	p.eval(fmt.Sprintf(`(() => { const set = (s, v) => { const e = document.querySelector(".modal.is-open " + s);
		e.value = v; e.dispatchEvent(new Event("input", { bubbles: true })); };
		set("#f-conditionTitle", "Office hours"); set("#f-conditionExpression", %q); return true; })()`, second), nil)
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	p.waitFor(`document.querySelector(".modal") === null`)
	p.waitFor(`(document.querySelector("#permissions-table .permissions-condition") || {}).textContent?.includes("Office hours")`)
	if got, want := condition(), "Office hours|expires|"+second; got != want {
		t.Errorf("after Edit condition the Folders API reads %q; want %q", got, want)
	}
	if sent := p.posts("/api/permissions/hierarchy"); len(sent) != 2 {
		t.Errorf("the grant and the edit sent %d requests, want 2: %v", len(sent), sent)
	}
}
