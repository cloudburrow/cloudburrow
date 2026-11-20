package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestConsoleScopesToAFolderAndTheOrganization (#2023): with ?folder= or
// ?organizationId= on the request, as the toolbar's resource selector puts
// them, the IAM page reads and changes that resource's policy through the
// Folders or Organizations API, with the organization's bindings inherited
// by a folder; Organization Policies lists, sets and edits the folder's own
// policies; Tags lists the organization's keys and makes new ones there;
// a folder that does not exist is refused, not silently replaced by the
// project.
func TestConsoleScopesToAFolderAndTheOrganization(t *testing.T) {
	t.Parallel()
	reg := resourcemanager.New(store.NewMemory())
	if _, err := reg.Create(resourcemanager.Project{ProjectID: "scope-proj", DisplayName: "Scope"}); err != nil {
		t.Fatal(err)
	}
	h := hierarchyProvider{registry: reg}
	id, err := h.Create(context.Background(), "", map[string]string{"displayName": "Engineering", "parent": resourcemanager.OrganizationName})
	if err != nil {
		t.Fatal(err)
	}
	folderCtx := console.WithScopeValue(context.Background(), scopeFolderParam, id)
	orgCtx := console.WithScopeValue(context.Background(), scopeOrgParam, resourcemanager.OrganizationNumber)
	iam := projectIAMProvider{registry: reg}

	// The organization grants a role a folder inherits.
	orgPol, err := iam.StudioCall(orgCtx, "scope-proj", "policy", nil)
	if err != nil {
		t.Fatal(err)
	}
	op := orgPol.(iamPagePolicy)
	if op.Kind != "organization" || op.Resource != "//cloudresourcemanager.googleapis.com/"+resourcemanager.OrganizationName || len(op.Inherited) != 0 {
		t.Fatalf("the organization's policy = %+v", op)
	}
	change, _ := json.Marshal(iamPageChange{Etag: op.Etag, Members: []string{"user:org@example.com"}, Add: []iamPageRoleChange{{Role: "roles/viewer"}}})
	if _, err := iam.StudioCall(orgCtx, "scope-proj", "change", change); err != nil {
		t.Fatal(err)
	}
	fp, err := iam.StudioCall(folderCtx, "scope-proj", "policy", nil)
	if err != nil {
		t.Fatal(err)
	}
	pol := fp.(iamPagePolicy)
	if pol.Kind != "folder" || pol.Display != "Engineering" || pol.Resource != "//cloudresourcemanager.googleapis.com/folders/"+id ||
		len(pol.Inherited) != 1 || pol.Inherited[0].Resource != resourcemanager.OrganizationName {
		t.Fatalf("the folder's policy = %+v", pol)
	}
	change, _ = json.Marshal(iamPageChange{Etag: pol.Etag, Members: []string{"user:eng@example.com"}, Add: []iamPageRoleChange{{Role: "roles/editor"}}})
	if _, err := iam.StudioCall(folderCtx, "scope-proj", "change", change); err != nil {
		t.Fatal(err)
	}
	got, err := resourcemanager.NewFoldersServer(reg).GetIamPolicy(context.Background(), &iampb.GetIamPolicyRequest{Resource: "folders/" + id})
	if err != nil || len(got.GetBindings()) != 1 || got.GetBindings()[0].GetMembers()[0] != "user:eng@example.com" {
		t.Errorf("the folder's policy through the Folders API = %v, %v", got, err)
	}
	if pp, err := resourcemanager.NewProjectsServer(reg).GetIamPolicy(context.Background(), &iampb.GetIamPolicyRequest{Resource: "projects/scope-proj"}); err != nil || len(pp.GetBindings()) != 0 {
		t.Errorf("the project's policy changed: %v, %v", pp, err)
	}
	if l, _ := iam.List(folderCtx, "scope-proj"); len(l.Items) != 1 || l.Items[0].Name != id || l.NameColumn != "Folder" {
		t.Errorf("IAM's list with the folder chosen = %+v", l)
	}
	if d, _ := iam.Detail(folderCtx, "scope-proj", []string{id}); !strings.Contains(d.Title, "folder Engineering") {
		t.Errorf("IAM's detail = %q %q", d.Title, d.Unavailable)
	}
	if _, err := iam.StudioCall(console.WithScopeValue(context.Background(), scopeFolderParam, "999"), "scope-proj", "policy", nil); err == nil {
		t.Error("a folder that does not exist was read")
	}

	// Organization Policies on the folder.
	op2 := iamOrgPoliciesProvider{registry: reg}
	c := resourcemanager.Constraints[0].Name
	if err := op2.ActAt(folderCtx, "scope-proj", []string{c}, orgPolicySet, map[string]string{"enforced": "true", "denyAll": "true"}); err != nil {
		t.Fatal(err)
	}
	if own, err := reg.OrgPolicies("folders/" + id); err != nil || len(own) != 1 {
		t.Errorf("the folder's own policies = %v, %v", own, err)
	}
	if own, _ := reg.OrgPolicies("projects/scope-proj"); len(own) != 0 {
		t.Errorf("the project got a policy: %v", own)
	}
	l, _ := op2.List(folderCtx, "scope-proj")
	var source string
	for _, it := range l.Items {
		if it.Name == c {
			source = it.Fields["Policy source"]
		}
	}
	if source != "Override parent's policy" {
		t.Errorf("the folder's listing says %q for %s", source, c)
	}
	d, _ := op2.Detail(folderCtx, "scope-proj", []string{c})
	if d.Summary[2].Value != "folders/"+id || d.Summary[3].Label != "Policy for this folder" {
		t.Errorf("the folder's policy details = %+v", d.Summary)
	}
	for _, a := range op2.DetailActions(folderCtx, "scope-proj", []string{c}) {
		if a.ID == "manage-policy" && !strings.Contains(a.Href, "folder="+id) {
			t.Errorf("Manage policy opens %s; want the folder kept", a.Href)
		}
	}
	view, err := op2.StudioCall(folderCtx, "scope-proj", "policy-get", json.RawMessage(`{"constraint":"`+c+`"}`))
	if err != nil || view.(orgPolicyEditView).Resource != "folders/"+id {
		t.Errorf("Edit policy reads %+v, %v; want the folder's", view, err)
	}

	// Tags with the organization chosen.
	tg := tagsProvider{registry: reg}
	if _, err := reg.CreateTagKey(&rmpb.TagKey{Parent: "projects/scope-proj", ShortName: "team"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.CreateTagKey(&rmpb.TagKey{Parent: resourcemanager.OrganizationName, ShortName: "env"}, false); err != nil {
		t.Fatal(err)
	}
	tl, _ := tg.List(orgCtx, "scope-proj")
	if len(tl.Items) != 1 || tl.Items[0].Fields["Short name"] != "env" {
		t.Errorf("Tags with the organization chosen = %+v", tl.Items)
	}
	if all, _ := tg.List(context.Background(), "scope-proj"); len(all.Items) != 2 {
		t.Errorf("Tags with the project = %+v", all.Items)
	}
	for _, f := range tg.CreateFormFor(folderCtx, "scope-proj") {
		if f.Name == "parent" && (len(f.Options) != 1 || f.Default != resourcemanager.OrganizationName) {
			t.Errorf("Create tag key's parent with a folder chosen = %+v", f)
		}
	}
}

// TestTagKeyDynamicValuesInTheConsole (#2023): Create tag key's Dynamic
// values, with Add value constraints and a validation regular expression,
// makes a key with that allowedValuesRegex; its page shows the regex
// instead of a values list, has no Create tag value, and edits the regex;
// a project's Bind dynamic tag and Manage resources' Tags panel take a typed
// value, which the regex validates.
func TestTagKeyDynamicValuesInTheConsole(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := resourcemanager.New(store.NewMemory())
	if _, err := reg.Create(resourcemanager.Project{ProjectID: "dyn-proj"}); err != nil {
		t.Fatal(err)
	}
	tg := tagsProvider{registry: reg}
	_, fields := tg.CreateForm()
	byName := map[string]console.Field{}
	for _, f := range fields {
		byName[f.Name] = f
	}
	if f := byName["valueConstraints"]; f.Label != "Add value constraints" || f.ShowWhen != "valueType=dynamic" {
		t.Errorf("Add value constraints = %+v", f)
	}
	if f := byName["allowedValuesRegex"]; f.Label != "Validation regular expression" || f.Default != ".*" {
		t.Errorf("the regex field = %+v", f)
	}
	if _, err := tg.Create(ctx, "", map[string]string{"parent": resourcemanager.OrganizationName, "shortName": "bad",
		"valueType": tagValuesDynamic, "valueConstraints": "true", "allowedValuesRegex": "(["}); err == nil {
		t.Error("an invalid regex was accepted")
	}
	id, err := tg.Create(ctx, "", map[string]string{"parent": resourcemanager.OrganizationName, "shortName": "env",
		"valueType": tagValuesDynamic, "valueConstraints": "true", "allowedValuesRegex": "env-[a-z]+"})
	if err != nil || reg.TagKeyAllowedValuesRegex("tagKeys/"+id) != "env-[a-z]+" {
		t.Fatalf("Create = %q, %v", id, err)
	}
	d, _ := tg.Detail(ctx, "", []string{id})
	var shown string
	for _, p := range d.Summary {
		if p.Label == "Validation regular expression" {
			shown = p.Value
		}
	}
	if shown != "env-[a-z]+" || len(d.Sections) != 1 || d.Sections[0].ID != "details" {
		t.Errorf("the key's page = %+v", d)
	}
	if acts := tg.DetailActions(ctx, "", []string{id}); len(acts) != 0 {
		t.Errorf("a dynamic key offers %+v", acts)
	}
	if err := tg.Edit(ctx, "", []string{id}, map[string]string{"description": "env", "allowedValuesRegex": "env-[a-z0-9]+"}); err != nil {
		t.Fatal(err)
	}
	if re := reg.TagKeyAllowedValuesRegex("tagKeys/" + id); re != "env-[a-z0-9]+" {
		t.Errorf("after Edit the regex is %q", re)
	}
	l, _ := tg.List(ctx, "")
	if len(l.Items) != 1 || l.Items[0].Fields["Values"] != "Dynamic: env-[a-z0-9]+" {
		t.Errorf("the list = %+v", l.Items)
	}

	target := projectTarget("dyn-proj")
	var bind *console.Action
	for _, a := range tagActions(reg, target) {
		if a.ID == tagBindDynamic {
			bind = &a
		}
	}
	if bind == nil || bind.Fields[0].Options[0] != resourcemanager.OrganizationNumber+"/env" {
		t.Fatalf("Bind dynamic tag = %+v", bind)
	}
	if _, err := tagAct(reg, target, tagBindDynamic, map[string]string{"key": resourcemanager.OrganizationNumber + "/env", "dynamicValue": "prod"}); err == nil {
		t.Error("a value that does not match was bound")
	}
	if _, err := tagAct(reg, target, tagBindDynamic, map[string]string{"key": resourcemanager.OrganizationNumber + "/env", "dynamicValue": "env-prod1"}); err != nil {
		t.Fatal(err)
	}
	if s := tagSection(reg, target); len(s.Listing.Items) != 1 || s.Listing.Items[0].Name != resourcemanager.OrganizationNumber+"/env/env-prod1" {
		t.Errorf("the project's tags = %+v", s.Listing.Items)
	}
	pp := projectsProvider{registry: reg}
	tree, err := pp.StudioCall(ctx, "dyn-proj", "tree", nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr := tree.(mrTree); tr.DynamicKeys[resourcemanager.OrganizationNumber+"/env"] != "env-[a-z0-9]+" {
		t.Errorf("the tree's dynamic keys = %v", tr.DynamicKeys)
	}
	body, _ := json.Marshal(map[string]string{"kind": "project", "id": "dyn-proj", "value": resourcemanager.OrganizationNumber + "/env/env-prod1"})
	if _, err := pp.StudioCall(ctx, "dyn-proj", "untag", body); err != nil {
		t.Errorf("untag the dynamic value: %v", err)
	}
}
