//go:build browser

package browser

import (
	"context"
	"strings"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
)

// TestIAMPolicyAnalyzerThroughThePage (#2112), in the storage shard:
// Policy Analyzer is Google's two tabs, Analyze policies and Saved
// queries. A custom query for a role with List permissions inside roles
// runs Cloud Asset's analyzeIamPolicy and lists the principal with the
// role's permissions; a principal with List permissions and an access
// selector is the API's own refusal, shown in the form; Save query adds a
// row to Saved queries, whose Run answers the same and Delete removes it;
// and each state meets WCAG as the other screens do.
func TestIAMPolicyAnalyzerThroughThePage(t *testing.T) {
	needService(t, "iam")
	ctx := context.Background()
	projects, err := resourcemanager.NewProjectsClient(ctx, rmClientOptions(t)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = projects.Close() })
	project := uniqueProject(t)
	pol, err := projects.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: "projects/" + project})
	if err != nil {
		t.Fatalf("GetIamPolicy: %v", err)
	}
	pol.Bindings = append(pol.Bindings, &iampb.Binding{Role: "roles/secretmanager.secretAccessor", Members: []string{"user:dana@cloudburrow.local"}})
	if _, err := projects.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: "projects/" + project, Policy: pol}); err != nil {
		t.Fatalf("SetIamPolicy: %v", err)
	}

	p := open(t)
	p.navigate("/iam-admin/analyzer?project=" + project)
	p.waitFor(`document.querySelector("#analyzer-tab-analyze[aria-selected=true]") !== null && document.querySelector("#f-scope") !== null`)
	var tabs []string
	p.eval(`[...document.querySelectorAll("#view [role=tab]")].map((t) => t.textContent.trim())`, &tabs)
	if strings.Join(tabs, "|") != "Analyze policies|Saved queries" {
		t.Errorf("tabs = %v; want Google's Analyze policies and Saved queries", tabs)
	}
	var labels string
	p.eval(`[...document.querySelectorAll("#analyzer-analyze label")].map((l) => l.textContent.trim()).join("|")`, &labels)
	for _, want := range []string{"List resources within resource(s) matching your query", "List individual users inside groups",
		"List permissions inside roles", "Analyze service account impersonation"} {
		if !strings.Contains(labels, want) {
			t.Errorf("Advanced options lack %q: %s", want, labels)
		}
	}
	for _, u := range p.unnamedControls() {
		t.Errorf("Policy Analyzer: an unnamed %s", u)
	}
	for _, f := range p.contrastFailures() {
		t.Errorf("Policy Analyzer: %s", f)
	}

	// A resource query with expandRoles lists the role's permissions.
	setField(p, "#analyzer-analyze", "f-resource", "//cloudresourcemanager.googleapis.com/projects/"+project)
	setField(p, "#analyzer-analyze", "f-expandRoles", "true")
	p.clickText("#analyzer-analyze button", "Run query")
	p.waitFor(`document.querySelector("#analyze-table") !== null`)
	var table string
	p.eval(`document.querySelector("#analyze-table").textContent`, &table)
	if !strings.Contains(table, "user:dana@cloudburrow.local") || !strings.Contains(table, "secretmanager.versions.access") {
		t.Errorf("results = %q; want dana with secretmanager.versions.access", table)
	}
	for _, u := range p.unnamedControls() {
		t.Errorf("Policy Analyzer results: an unnamed %s", u)
	}

	// expandRoles with an access selector is the API's refusal.
	setField(p, "#analyzer-analyze", "f-roles", "roles/viewer")
	p.clickText("#analyzer-analyze button", "Run query")
	p.waitFor(`!document.querySelector("#analyzer-analyze .form-error").hidden`)
	var refusal string
	p.eval(`document.querySelector("#analyzer-analyze .form-error").textContent`, &refusal)
	if !strings.Contains(refusal, "expand_roles") {
		t.Errorf("refusal = %q; want the expand_roles error", refusal)
	}
	// The refusal's 400 is the one the test provoked.
	p.forgive()
	setField(p, "#analyzer-analyze", "f-roles", "")
	setField(p, "#analyzer-analyze", "f-expandRoles", "false")
	setField(p, "#analyzer-analyze", "f-resource", "")
	setField(p, "#analyzer-analyze", "f-identity", "user:dana@cloudburrow.local")

	// Save query, then Saved queries' Run and Delete.
	p.clickText("#analyzer-analyze button", "Save query")
	p.waitFor(`document.querySelector(".modal.is-open #f-id") !== null`)
	setField(p, ".modal.is-open", "f-id", "dana-access")
	setField(p, ".modal.is-open", "f-description", "What dana holds")
	for _, u := range p.unnamedControls() {
		t.Errorf("Save query: an unnamed %s", u)
	}
	p.clickText(".modal.is-open button", "Save")
	p.waitFor(`document.querySelector(".modal.is-open") === null`)
	p.clickText("#view [role=tab]", "Saved queries")
	p.waitFor(`document.querySelector("#saved-queries-table tr[data-name$='/savedQueries/dana-access']") !== null`)
	for _, f := range p.contrastFailures() {
		t.Errorf("Saved queries: %s", f)
	}
	p.clickText("#saved-queries-table button", "Run")
	p.waitFor(`document.querySelector("#analyze-table") !== null && document.querySelector("#analyze-table").textContent.includes("roles/secretmanager.secretAccessor")`)
	p.clickText("#view [role=tab]", "Saved queries")
	p.clickText("#saved-queries-table button", "Delete")
	p.waitFor(`document.querySelector("#saved-queries-empty") !== null`)
}
