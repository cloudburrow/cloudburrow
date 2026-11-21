//go:build compat

package compat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGcloudPrincipalAccessBoundaryPolicies (#2057): gcloud iam
// principal-access-boundary-policies create, describe, list, update,
// search-policy-bindings and delete, and gcloud iam policy-bindings create,
// describe, list and delete on a project, through gcloud-setup's iam
// override alone, as "Create and apply principal access boundary policies"
// runs them.
func TestGcloudPrincipalAccessBoundaryPolicies(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	g := newGcloudSession(t, h)
	project := iamProject(t, h, "pabg-")
	org := strings.TrimPrefix(localOrg, "organizations/")
	id := project
	file := filepath.Join(t.TempDir(), "rules.json")
	rules := `[{"description": "the project", "resources": ["//cloudresourcemanager.googleapis.com/projects/` + project + `"], "effect": "ALLOW"}]`
	if err := os.WriteFile(file, []byte(rules), 0o600); err != nil {
		t.Fatal(err)
	}
	where := []string{"--organization=" + org, "--location=global"}
	pab := func(args ...string) []string {
		return append(append([]string{"iam", "principal-access-boundary-policies"}, args...), where...)
	}
	g.must(pab("create", id, "--display-name=Project boundary", "--details-rules="+file, "--details-enforcement-version=1")...)
	t.Cleanup(func() { _, _ = g.run(nil, pab("delete", id, "--force", "--quiet")...) })
	var got struct {
		Name, DisplayName, Etag string
		Details                 struct {
			EnforcementVersion string
			Rules              []struct {
				Resources []string
				Effect    string
			}
		}
	}
	out := g.must(pab("describe", id, "--format=json")...)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("describe: %v\n%s", err, out)
	}
	if got.Name != localOrg+"/locations/global/principalAccessBoundaryPolicies/"+id || got.DisplayName != "Project boundary" ||
		got.Details.EnforcementVersion != "1" || len(got.Details.Rules) != 1 || got.Details.Rules[0].Effect != "ALLOW" {
		t.Fatalf("describe = %+v", got)
	}
	if out := g.must(pab("list", "--format=value(name)")...); !strings.Contains(out, "/principalAccessBoundaryPolicies/"+id) {
		t.Errorf("list = %q", out)
	}
	g.must(pab("update", id, "--display-name=Renamed", "--details-enforcement-version=latest")...)
	out = g.must(pab("describe", id, "--format=value(displayName,details.enforcementVersion)")...)
	if f := strings.Fields(out); len(f) != 2 || f[0] != "Renamed" || f[1] != "latest" {
		t.Errorf("after update = %q", out)
	}

	binding := "pabg-" + project[len(project)-8:]
	policy := localOrg + "/locations/global/principalAccessBoundaryPolicies/" + id
	bwhere := []string{"--project=" + project, "--location=global"}
	pb := func(args ...string) []string {
		return append(append([]string{"iam", "policy-bindings"}, args...), bwhere...)
	}
	g.must(pb("create", binding, "--policy="+policy, "--target-principal-set=//cloudresourcemanager.googleapis.com/projects/"+project,
		"--display-name=Service accounts", "--condition-title=sa", "--condition-expression=principal.type == 'iam.googleapis.com/ServiceAccount'")...)
	t.Cleanup(func() { _, _ = g.run(nil, pb("delete", binding, "--quiet")...) })
	out = g.must(pb("describe", binding, "--format=json")...)
	// gcloud names the parsed resource on stderr before the JSON.
	if i := strings.Index(out, "{"); i > 0 {
		out = out[i:]
	}
	var b struct {
		Policy, PolicyKind, DisplayName string
		Target                          struct{ PrincipalSet string }
		Condition                       struct{ Expression string }
	}
	if err := json.Unmarshal([]byte(out), &b); err != nil {
		t.Fatalf("policy-bindings describe: %v\n%s", err, out)
	}
	if b.Policy != policy || b.PolicyKind != "PRINCIPAL_ACCESS_BOUNDARY" || b.Target.PrincipalSet != "//cloudresourcemanager.googleapis.com/projects/"+project ||
		!strings.Contains(b.Condition.Expression, "ServiceAccount") {
		t.Fatalf("policy-bindings describe = %+v", b)
	}
	if out := g.must(pb("list", "--format=value(name)")...); !strings.Contains(out, "/policyBindings/"+binding) {
		t.Errorf("policy-bindings list = %q", out)
	}
	if out := g.must(pab("search-policy-bindings", id, "--format=value(name)")...); !strings.Contains(out, "/policyBindings/"+binding) {
		t.Errorf("search-policy-bindings = %q", out)
	}
	if out, err := g.run(nil, pab("delete", id, "--quiet")...); err == nil {
		t.Errorf("delete while bound succeeded: %s", out)
	}
	g.must(pb("delete", binding, "--quiet")...)
	g.must(pab("delete", id, "--quiet")...)
	if out, err := g.run(nil, pab("describe", id)...); err == nil {
		t.Errorf("describe after delete succeeded: %s", out)
	}
}
