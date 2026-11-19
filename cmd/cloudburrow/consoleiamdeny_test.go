package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

func denyCallOf(t *testing.T, p projectIAMProvider, call string, body any) (any, error) {
	t.Helper()
	raw, _ := json.Marshal(body)
	return p.StudioCall(context.Background(), "iam-page", call, raw)
}

// The IAM page's Deny tab (#1889): the project and the organization above
// it are offered; Create deny policy, its details, Edit with the etag read,
// a stale etag refused, the API's own validation, and Delete, each over the
// registry IAM v2's Policies API serves.
func TestConsoleIAMDenyTab(t *testing.T) {
	reg := iamPageRegistry(t)
	p := projectIAMProvider{registry: reg}
	out, err := denyCallOf(t, p, "deny-list", map[string]string{})
	if err != nil {
		t.Fatalf("deny-list: %v", err)
	}
	list := out.(denyPageList)
	if list.Resource != "projects/iam-page" || len(list.Policies) != 0 {
		t.Fatalf("deny-list = %+v", list)
	}
	offered := map[string]bool{}
	for _, r := range list.Resources {
		offered[r.Resource] = true
	}
	if !offered[resourcemanager.OrganizationName] {
		t.Errorf("the organization above the project is not offered: %+v", list.Resources)
	}
	if _, err := denyCallOf(t, p, "deny-list", map[string]string{"resource": "projects/elsewhere"}); err == nil {
		t.Error("deny-list of a resource not above the project was answered")
	}
	rule := denyPageRule{DeniedPrincipals: []string{"principal://goog/subject/ada@example.com"},
		DeniedPermissions: []string{"cloudresourcemanager.googleapis.com/projects.delete"},
		Condition:         &denyCondition{Title: "prod", Expression: "resource.matchTag('123456789012/env', 'prod')"}}
	out, err = denyCallOf(t, p, "deny-create", denyPageChange{ID: "no-delete", DisplayName: "No delete", Rules: []denyPageRule{rule}})
	if err != nil {
		t.Fatalf("deny-create: %v", err)
	}
	created := out.(denyPagePolicy)
	if created.ID != "no-delete" || created.RuleCount != 1 || created.Etag == "" {
		t.Fatalf("deny-create = %+v", created)
	}
	if _, err := denyCallOf(t, p, "deny-create", denyPageChange{ID: "Bad_ID", Rules: []denyPageRule{rule}}); err == nil {
		t.Error("an invalid policy ID was accepted")
	}
	if _, err := denyCallOf(t, p, "deny-create", denyPageChange{ID: "no-rules"}); err == nil {
		t.Error("a policy without rules was accepted")
	}
	out, _ = denyCallOf(t, p, "deny-list", map[string]string{})
	if l := out.(denyPageList); len(l.Policies) != 1 || l.Policies[0].ID != "no-delete" || l.Policies[0].RuleCount != 1 {
		t.Fatalf("deny-list after create = %+v", l.Policies)
	}
	out, err = denyCallOf(t, p, "deny-get", map[string]string{"name": created.Name})
	if err != nil {
		t.Fatalf("deny-get: %v", err)
	}
	got := out.(denyPagePolicy)
	if len(got.Rules) != 1 || got.Rules[0].Condition == nil || !strings.Contains(got.Rules[0].Condition.Expression, "matchTag") {
		t.Fatalf("deny-get rules = %+v", got.Rules)
	}
	rule2 := denyPageRule{DeniedPrincipals: []string{"principalSet://goog/public:all"}, ExceptionPrincipals: []string{"principal://goog/subject/bob@example.com"},
		DeniedPermissions: []string{"cloudresourcemanager.googleapis.com/projects.delete"}}
	out, err = denyCallOf(t, p, "deny-update", denyPageChange{Name: created.Name, Etag: got.Etag, DisplayName: "Renamed", Rules: []denyPageRule{rule, rule2}})
	if err != nil {
		t.Fatalf("deny-update: %v", err)
	}
	if u := out.(denyPagePolicy); u.DisplayName != "Renamed" || u.RuleCount != 2 {
		t.Fatalf("deny-update = %+v", u)
	}
	if _, err := denyCallOf(t, p, "deny-update", denyPageChange{Name: created.Name, Etag: got.Etag, Rules: []denyPageRule{rule}}); err == nil {
		t.Error("an edit with a stale etag was applied")
	}
	if _, err := denyCallOf(t, p, "deny-delete", map[string]string{"name": created.Name}); err != nil {
		t.Fatalf("deny-delete: %v", err)
	}
	out, _ = denyCallOf(t, p, "deny-list", map[string]string{})
	if l := out.(denyPageList); len(l.Policies) != 0 {
		t.Fatalf("deny-list after delete = %+v", l.Policies)
	}
	// On the organization.
	if _, err := denyCallOf(t, p, "deny-create", denyPageChange{Resource: resourcemanager.OrganizationName, ID: "org-wide", Rules: []denyPageRule{rule}}); err != nil {
		t.Fatalf("deny-create on the organization: %v", err)
	}
	out, _ = denyCallOf(t, p, "deny-list", map[string]string{"resource": resourcemanager.OrganizationName})
	if l := out.(denyPageList); len(l.Policies) != 1 || l.Policies[0].ID != "org-wide" {
		t.Fatalf("the organization's deny-list = %+v", l.Policies)
	}
}
