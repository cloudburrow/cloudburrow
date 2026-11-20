package main

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/iamroles"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

func iamPageRegistry(t *testing.T) *resourcemanager.Registry {
	t.Helper()
	reg := resourcemanager.New(store.NewMemory())
	if _, err := reg.Create(resourcemanager.Project{ProjectID: "iam-page"}); err != nil {
		t.Fatal(err)
	}
	return reg
}

func readIAMPage(t *testing.T, p projectIAMProvider) iamPagePolicy {
	t.Helper()
	out, err := p.StudioCall(context.Background(), "iam-page", "policy", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	return out.(iamPagePolicy)
}

func iamPageRolesOf(pol iamPagePolicy, member string) []string {
	var out []string
	for _, b := range pol.Bindings {
		if slices.Contains(b.Members, member) {
			r := b.Role
			if b.Conditional != nil {
				r += "?" + b.Conditional.Title
			}
			out = append(out, r)
		}
	}
	slices.Sort(out)
	return out
}

// The IAM page's calls (#1864): Grant access of two roles, one with a
// condition, to two principals in one save; Edit principal replacing a
// principal's roles; a stale etag refused; the role picker's roles.
func TestConsoleIAMPageCalls(t *testing.T) {
	reg := iamPageRegistry(t)
	p := projectIAMProvider{registry: reg}
	ctx := context.Background()
	pol := readIAMPage(t, p)
	change := func(c iamPageChange) error {
		body, _ := json.Marshal(c)
		_, err := p.StudioCall(ctx, "iam-page", "change", body)
		return err
	}
	cond := &console.Condition{Title: "Until 2099", Expression: `request.time < timestamp("2099-01-01T00:00:00Z")`}
	if err := change(iamPageChange{Etag: pol.Etag, Members: []string{"user:ada@example.com", "user:bob@example.com"},
		Add: []iamPageRoleChange{{Role: "roles/viewer"}, {Role: "roles/storage.admin", Condition: cond}}}); err != nil {
		t.Fatalf("Grant access: %v", err)
	}
	stale := pol.Etag
	pol = readIAMPage(t, p)
	for _, m := range []string{"user:ada@example.com", "user:bob@example.com"} {
		if got := iamPageRolesOf(pol, m); strings.Join(got, ",") != "roles/storage.admin?Until 2099,roles/viewer" {
			t.Errorf("%s holds %v after Grant access", m, got)
		}
	}
	if err := change(iamPageChange{Etag: stale, Members: []string{"user:eve@example.com"}, Add: []iamPageRoleChange{{Role: "roles/viewer"}}}); err == nil {
		t.Error("a change with the etag of a policy changed since was applied")
	}
	// Edit principal: Ada's two roles become roles/editor.
	if err := change(iamPageChange{Etag: pol.Etag, Members: []string{"user:ada@example.com"},
		Add:    []iamPageRoleChange{{Role: "roles/editor"}},
		Remove: []iamPageRoleChange{{Role: "roles/viewer"}, {Role: "roles/storage.admin", Condition: cond}}}); err != nil {
		t.Fatalf("Edit principal: %v", err)
	}
	pol = readIAMPage(t, p)
	if got := iamPageRolesOf(pol, "user:ada@example.com"); strings.Join(got, ",") != "roles/editor" {
		t.Errorf("Ada holds %v after Edit principal; want roles/editor", got)
	}
	if got := iamPageRolesOf(pol, "user:bob@example.com"); len(got) != 2 {
		t.Errorf("Bob holds %v; Edit principal of Ada changed his roles", got)
	}
	if err := change(iamPageChange{Etag: pol.Etag}); err == nil {
		t.Error("a change naming no principal was applied")
	}
	roles, err := p.StudioCall(ctx, "iam-page", "roles", nil)
	if err != nil {
		t.Fatal(err)
	}
	var viewer *iamPageRole
	for _, r := range roles.([]iamPageRole) {
		if r.Name == "roles/viewer" {
			viewer = &r
		}
	}
	if viewer == nil || viewer.Type != "Basic" || viewer.Title != "Viewer" {
		t.Errorf("the role picker's roles/viewer is %+v; want the Basic role Viewer", viewer)
	}
}

// Create service account's three steps (#1864): the ID from the form, the
// project role granted to the new account, and the users and admins
// granted on it.
func TestConsoleIAMCreateServiceAccountSteps(t *testing.T) {
	reg := iamPageRegistry(t)
	p := iamServiceAccountsProvider{registry: reg}
	ctx := context.Background()
	_, fields := p.CreateForm()
	var names []string
	for _, f := range fields {
		names = append(names, f.Name)
	}
	if got := strings.Join(names, ","); got != "displayName,accountId,description,roles,users,admins" {
		t.Errorf("Create service account's fields are %s", got)
	}
	if _, err := p.Create(ctx, "iam-page", map[string]string{"accountId": "builder-one", "roles": "roles/no.such"}); err == nil {
		t.Error("an unknown project role was accepted")
	}
	email, err := p.Create(ctx, "iam-page", map[string]string{"accountId": "builder-one", "displayName": "Builder one",
		"roles": "roles/viewer\nroles/storage.objectViewer", "users": "user:ada@example.com", "admins": "user:bob@example.com"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	pol := readIAMPage(t, projectIAMProvider{registry: reg})
	if got := iamPageRolesOf(pol, "serviceAccount:"+email); strings.Join(got, ",") != "roles/storage.objectViewer,roles/viewer" {
		t.Errorf("the new account holds %v on the project", got)
	}
	sa, err := p.GetPolicy(ctx, "iam-page", []string{email})
	if err != nil {
		t.Fatal(err)
	}
	held := map[string]string{}
	for _, b := range sa.Bindings {
		for _, m := range b.Members {
			held[m] = b.Role
		}
	}
	if held["user:ada@example.com"] != "roles/iam.serviceAccountUser" || held["user:bob@example.com"] != "roles/iam.serviceAccountAdmin" {
		t.Errorf("the account's policy grants %v", held)
	}
}

// Labels, Organization Policies and Settings (#1864) over the registry.
func TestConsoleIAMResourcePages(t *testing.T) {
	reg := iamPageRegistry(t)
	ctx := context.Background()
	labels := iamLabelsProvider{registry: reg}
	if _, err := labels.Create(ctx, "iam-page", map[string]string{"key": "env", "value": "dev"}); err != nil {
		t.Fatal(err)
	}
	if _, err := labels.Create(ctx, "iam-page", map[string]string{"key": "Env", "value": "dev"}); err == nil {
		t.Error("an upper-case label key was accepted")
	}
	if err := labels.Edit(ctx, "iam-page", []string{"env"}, map[string]string{"value": "prod"}); err != nil {
		t.Fatal(err)
	}
	if pr, _ := reg.Get("iam-page"); pr.Labels["env"] != "prod" {
		t.Errorf("labels = %v", pr.Labels)
	}
	if err := labels.Delete(ctx, "iam-page", "env"); err != nil {
		t.Fatal(err)
	}
	if l, _ := labels.List(ctx, "iam-page"); len(l.Items) != 0 {
		t.Errorf("labels after Delete: %+v", l.Items)
	}

	op := iamOrgPoliciesProvider{registry: reg}
	l, _ := op.List(ctx, "iam-page")
	if len(l.Items) != len(resourcemanager.Constraints) {
		t.Fatalf("Organization Policies lists %d constraints; want %d", len(l.Items), len(resourcemanager.Constraints))
	}
	const c = "constraints/compute.requireOsLogin"
	acts := op.DetailActions(ctx, "iam-page", []string{c})
	if len(acts) == 0 || acts[0].Label != "Manage policy" || acts[0].Href != "/iam-admin/orgpolicies/constraints%2Fcompute.requireOsLogin/edit?project=iam-page" {
		t.Fatalf("a constraint's actions are %+v", acts)
	}
	if _, err := op.StudioCall(ctx, "iam-page", "policy-set", json.RawMessage(`{"constraint":"`+c+`","source":"override","rules":[{"enforce":true}]}`)); err != nil {
		t.Fatal(err)
	}
	l, _ = op.List(ctx, "iam-page")
	for _, r := range l.Items {
		if r.Name == c && (r.Fields["Policy source"] != "Override parent's policy" || r.Fields["Effective"] != "Enforced") {
			t.Errorf("%s reads %v after Manage policy", c, r.Fields)
		}
	}

	st := iamSettingsProvider{registry: reg}
	if err := st.Edit(ctx, "iam-page", []string{"iam-page"}, map[string]string{"displayName": "IAM page"}); err != nil {
		t.Fatal(err)
	}
	if l, _ := st.List(ctx, "iam-page"); len(l.Items) != 1 || l.Items[0].Fields["Project name"] != "IAM page" {
		t.Errorf("Settings = %+v", l.Items)
	}
}

// Create role from selection (#1864): two predefined roles combined into
// a project role holding their supported permissions.
func TestConsoleIAMCreateRoleFromSelection(t *testing.T) {
	reg := iamPageRegistry(t)
	p := iamRolesProvider{registry: reg}
	ctx := context.Background()
	l, _ := p.List(ctx, "iam-page")
	if len(l.SelectActions) != 2 || l.SelectActions[0].SelectionField != "roles" || l.SelectActions[1].Label != "Disable" {
		t.Fatalf("the Roles list's selection actions are %+v", l.SelectActions)
	}
	if err := p.ActAt(ctx, "iam-page", nil, roleFromSelection, map[string]string{
		"roles": "predefined:storage.objectViewer\npredefined:pubsub.viewer", "title": "Readers", "roleId": "readers", "stage": "GA"}); err != nil {
		t.Fatal(err)
	}
	r, err := reg.GetRole("projects/iam-page/roles/readers")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(r.GetIncludedPermissions(), "storage.objects.get") || !slices.Contains(r.GetIncludedPermissions(), "pubsub.topics.list") {
		t.Errorf("the role holds %v", r.GetIncludedPermissions())
	}
	_, fields := p.CreateForm()
	var names []string
	for _, f := range fields {
		names = append(names, f.Name)
	}
	if got := strings.Join(names, ","); got != "title,description,roleId,stage,permissions,parent" {
		t.Errorf("Create role's fields are %s; want Google's order", got)
	}
}

// TestConsoleIAMRolesPermissionPickerAndDisable (#1864): Create custom
// role's permissions are a picklist whose Add Permissions reads the
// supported permissions from the provider's Studio call; Edit role ends in
// Update; Disable over checked custom roles sets their stage to DISABLED
// and refuses a predefined role.
func TestConsoleIAMRolesPermissionPickerAndDisable(t *testing.T) {
	reg := iamPageRegistry(t)
	p := iamRolesProvider{registry: reg}
	ctx := context.Background()
	label, fields := p.CreateForm()
	if label != "Create custom role" {
		t.Errorf("the create label is %q", label)
	}
	var perms console.Field
	for _, f := range fields {
		if f.Name == "permissions" {
			perms = f
		}
	}
	if perms.Type != console.PicklistFieldType || perms.PickFrom != "iam-roles:permissions" || perms.AddLabel != "Add Permissions" {
		t.Fatalf("the permissions field is %+v", perms)
	}
	out, err := p.StudioCall(ctx, "iam-page", "permissions", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	items := out.(map[string]any)["items"].([]iamPickItem)
	has := func(v string) bool {
		return slices.ContainsFunc(items, func(it iamPickItem) bool { return it.Value == v })
	}
	if !has("storage.objects.get") || len(items) < 1000 {
		t.Errorf("the picker offers %d permissions, storage.objects.get %v", len(items), has("storage.objects.get"))
	}
	for _, it := range items {
		if l := iamroles.Level(it.Value); l != iamroles.Supported && l != iamroles.Testing {
			t.Fatalf("the picker offers %s, which custom roles do not support", it.Value)
		}
	}
	if _, err := p.StudioCall(ctx, "iam-page", "nope", nil); err == nil {
		t.Error("an unknown call was answered")
	}
	if _, err := p.Create(ctx, "iam-page", map[string]string{"title": "Getter", "roleId": "getter", "stage": "GA",
		"permissions": "storage.objects.get\nstorage.objects.list"}); err != nil {
		t.Fatal(err)
	}
	d, _ := p.Detail(ctx, "iam-page", []string{"getter"})
	if d.Edit == nil || d.Edit.Submit != "Update" || d.Edit.Label != "Edit role" {
		t.Fatalf("Edit role is %+v", d.Edit)
	}
	if err := p.ActAt(ctx, "iam-page", nil, rolesDisable, map[string]string{"roles": "predefined:viewer"}); err == nil {
		t.Error("a predefined role was disabled")
	}
	if err := p.ActAt(ctx, "iam-page", nil, rolesDisable, map[string]string{"roles": "getter"}); err != nil {
		t.Fatal(err)
	}
	r, err := reg.GetRole("projects/iam-page/roles/getter")
	if err != nil || r.GetStage().String() != "DISABLED" || len(r.GetIncludedPermissions()) != 2 {
		t.Errorf("after Disable the role is %v, %v", r, err)
	}
}
