//go:build browser

package browser

import (
	"context"
	"fmt"
	"testing"
	"time"

	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
)

// TestCloudRunSourceManagerCodeOwnersThroughThePage (#1928), in the run
// shard, whose Run port serves Secure Source Manager:
//   - a CODEOWNERS file's page's Checks tab shows its status and its line-level checks:
//     an error for a line that does not parse, information for a rule of
//     another branch;
//   - Settings' Add branch rule's Require Code Owner Review on Pull
//     Requests makes the rule GetBranchRule reads with
//     require_code_owner_approval;
//   - a pull request's Code owners tab lists each requirement of its
//     changed files, by section, Pending until its owners approve, and the
//     page's summary counts them.
func TestCloudRunSourceManagerCodeOwnersThroughThePage(t *testing.T) {
	c := ssmClient(t)
	ctx := context.Background()
	project := uniqueProject(t)
	inst := fmt.Sprintf("browser-co-%d", time.Now().UnixNano()%1e6)
	parent := "projects/" + project + "/locations/us-central1"
	op, err := c.CreateInstance(ctx, &ssmpb.CreateInstanceRequest{Parent: parent, InstanceId: inst, Instance: &ssmpb.Instance{}})
	if err == nil {
		_, err = op.Wait(ctx)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(context.Background(), &ssmpb.DeleteInstanceRequest{Name: parent + "/instances/" + inst, Force: true}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	rop, err := c.CreateRepository(ctx, &ssmpb.CreateRepositoryRequest{Parent: parent, RepositoryId: "app",
		Repository: &ssmpb.Repository{Instance: parent + "/instances/" + inst}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := rop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ssmPush(t, r.GetUris().GetGitHttps(), "main", map[string]string{"README.md": "# app\n",
		"CODEOWNERS": "* lead@example.com\n* @team\nrelease-* /x/ rel@example.com\n[Docs][2]\n*.md doc1@example.com doc2@example.com\n"})
	ssmPush(t, r.GetUris().GetGitHttps(), "feature", map[string]string{"GUIDE.md": "# guide\n"})

	p := open(t)
	base := "/secure-source-manager/" + inst + "/app"
	p.navigate(base + "/tree/main/CODEOWNERS?project=" + project)
	p.clickText(`#view [role="tab"]`, "Checks")
	p.waitFor(`document.querySelector("#view [role=tabpanel]").textContent.includes("syntax errors")`)
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("@team") && r.textContent.includes("Error"))`)
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("release-*") && r.textContent.includes("does not match the branch main"))`)

	p.navigate(base + "?project=" + project + "&tab=settings")
	p.waitFor(`new URLSearchParams(location.search).get("tab") === "settings" && document.querySelector("#view [role=tabpanel]") !== null`)
	p.pageAction("Add branch rule", map[string]string{"id": "owners", "filter": "main", "requireCodeOwners": "true"}, "")
	br, err := c.GetBranchRule(ctx, &ssmpb.GetBranchRuleRequest{Name: r.GetName() + "/branchRules/owners"})
	if err != nil || !br.GetRequireCodeOwnerApproval() || br.GetIncludePattern() != "main" {
		t.Fatalf("GetBranchRule after Add branch rule = %v, %v", br, err)
	}

	pop, err := c.CreatePullRequest(ctx, &ssmpb.CreatePullRequestRequest{Parent: r.GetName(), PullRequest: &ssmpb.PullRequest{Title: "Guide",
		Base: &ssmpb.PullRequest_Branch{Ref: "main"}, Head: &ssmpb.PullRequest_Branch{Ref: "feature"}}})
	if err != nil {
		t.Fatal(err)
	}
	pr, err := pop.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p.navigate(base + "/pulls/" + lastPart(pr.GetName()) + "?project=" + project)
	p.waitFor(`document.querySelector("#view").textContent.includes("0 of 2 satisfied; required by branch rule owners")`)
	p.clickText(`#view [role="tab"]`, "Code owners")
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("lead@example.com") && r.textContent.includes("Pending") && r.textContent.includes("GUIDE.md"))`)
	p.waitFor(`[...document.querySelectorAll("#view tbody tr")].some((r) => r.textContent.includes("Docs") && r.textContent.includes("doc2@example.com") && r.textContent.includes("Pending"))`)
}
