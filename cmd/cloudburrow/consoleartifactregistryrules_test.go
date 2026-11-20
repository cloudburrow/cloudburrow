package main

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"google.golang.org/grpc"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
)

// TestConsoleArtifactRegistryEditRuleAndCleanupPolicies (#1491): Edit rule
// reads the rule (GetRule) and changes only the fields set (UpdateRule);
// Edit cleanup policies opens on the repository's policies as JSON and
// replaces them and the dry run through UpdateRepository.
func TestConsoleArtifactRegistryEditRuleAndCleanupPolicies(t *testing.T) {
	ctx := context.Background()
	s := runadapter.NewServer(&runadapter.Knative{}, "t", time.Minute)
	g := grpc.NewServer()
	s.ArtifactRegistry().Register(g)
	runadapter.NewOperationsServer(s).Register(g)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(g.Stop)
	p := artifactRegistryProvider{defaultProject: "console-ar-rules", runAddr: func() string { return ln.Addr().String() }}
	t.Cleanup(func() { _ = p.Delete(ctx, "", "rules") })
	if _, err := p.Create(ctx, "", map[string]string{"id": "rules", "location": "us-east1"}); err != nil {
		t.Fatal(err)
	}
	ids := func() map[string]bool {
		out := map[string]bool{}
		for _, a := range p.DetailActions(ctx, "", []string{"rules"}) {
			out[a.ID] = true
		}
		return out
	}
	if acts := ids(); acts["edit-rule"] {
		t.Errorf("a repository with no rules offers %v; want no Edit rule", acts)
	}
	if _, err := runadapter.ConfigServer().CreateRule(ctx, &arpb.CreateRuleRequest{Parent: "projects/console-ar-rules/locations/us-east1/repositories/rules",
		RuleId: "deny-all", Rule: &arpb.Rule{Action: arpb.Rule_DENY, Operation: arpb.Rule_DOWNLOAD}}); err != nil {
		t.Fatal(err)
	}
	if !ids()["edit-rule"] {
		t.Fatal("a repository with a rule offers no Edit rule")
	}
	if err := p.ActAt(ctx, "", []string{"rules"}, "edit-rule", map[string]string{"id": "deny-all", "action": "ALLOW", "package": "web"}); err != nil {
		t.Fatal(err)
	}
	name := "projects/console-ar-rules/locations/us-east1/repositories/rules/rules/deny-all"
	r, err := runadapter.ConfigServer().GetRule(ctx, &arpb.GetRuleRequest{Name: name})
	if err != nil || r.GetAction() != arpb.Rule_ALLOW || r.GetPackageId() != "web" || r.GetOperation() != arpb.Rule_DOWNLOAD {
		t.Errorf("GetRule after Edit rule = %v, %v", r, err)
	}
	if err := p.ActAt(ctx, "", []string{"rules"}, "edit-rule", map[string]string{"id": "deny-all"}); err == nil {
		t.Error("Edit rule with nothing to change was sent")
	}

	// Cleanup policies are Edit Repository's rows (#1855): Conditional
	// delete with Untagged and Older than, Keep most recent versions with
	// Keep count, in Dry run.
	pols := `[{"name":"old","type":"Conditional delete","tagState":"Untagged","olderThan":"1d"},
		{"name":"recent","type":"Keep most recent versions","keepCount":"5","packagePrefixes":"web, api"}]`
	if err := p.Edit(ctx, "", []string{"rules"}, map[string]string{"cleanupPolicies": pols, "cleanupMode": arCleanupDryRun}); err != nil {
		t.Fatal(err)
	}
	repo, _, err := p.repository("console-ar-rules", "rules")
	if err != nil || len(repo.GetCleanupPolicies()) != 2 || !repo.GetCleanupPolicyDryRun() ||
		repo.GetCleanupPolicies()["old"].GetCondition().GetOlderThan().GetSeconds() != 86400 ||
		repo.GetCleanupPolicies()["old"].GetCondition().GetTagState() != arpb.CleanupPolicyCondition_UNTAGGED ||
		repo.GetCleanupPolicies()["recent"].GetMostRecentVersions().GetKeepCount() != 5 ||
		strings.Join(repo.GetCleanupPolicies()["recent"].GetMostRecentVersions().GetPackageNamePrefixes(), ",") != "web,api" {
		t.Fatalf("the repository after Edit Repository's cleanup policies = %v, %v", repo, err)
	}
	for _, f := range editRepositoryForm(repo).Fields {
		if f.Name == "cleanupPolicies" && (!strings.Contains(f.Default, `"keepCount":"5"`) || !strings.Contains(f.Default, `"olderThan":"1d"`)) {
			t.Errorf("Edit Repository's cleanup policies open on %q", f.Default)
		}
		if f.Name == "cleanupMode" && f.Default != arCleanupDryRun {
			t.Errorf("Edit Repository's cleanup mode opens on %q", f.Default)
		}
	}
	for _, bad := range []string{`[{"type":"Conditional keep"}]`, `[{"name":"a","tagState":"Untagged","tagPrefixes":"v"}]`,
		`[{"name":"a","type":"Keep most recent versions"}]`, `[{"name":"a"},{"name":"a"}]`, `[{"name":"a","olderThan":"3 weeks"}]`} {
		if err := p.Edit(ctx, "", []string{"rules"}, map[string]string{"cleanupPolicies": bad}); err == nil {
			t.Errorf("cleanup policies %s were sent", bad)
		}
	}
	if err := p.Edit(ctx, "", []string{"rules"}, map[string]string{"cleanupPolicies": "", "cleanupMode": arCleanupDelete}); err != nil {
		t.Fatal(err)
	}
	if repo, _, _ := p.repository("console-ar-rules", "rules"); len(repo.GetCleanupPolicies()) != 0 || repo.GetCleanupPolicyDryRun() {
		t.Errorf("no rows left %v", repo.GetCleanupPolicies())
	}
}
