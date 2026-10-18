//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	crmv1 "google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/genproto/googleapis/type/expr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Project, folder and organization IAM (#1368, ADR-0006 as amended on
// 2026-10-15): stored, never enforced, and inherited by nothing. These
// tests drive it with the official clients, gcloud, Terraform and the
// console, and assert nothing about access.

func rmProjects(t *testing.T, h *Harness) *resourcemanager.ProjectsClient {
	t.Helper()
	c, err := resourcemanager.NewProjectsClient(h.Context(), rmOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// iamProject registers a project of the test's own through v1, deleted at
// the end.
func iamProject(t *testing.T, h *Harness, prefix string) string {
	t.Helper()
	v1 := rmV1(t, h)
	id := v1ID(h, prefix)
	if _, err := v1.Projects.Create(&crmv1.Project{ProjectId: id, Name: "IAM"}).Context(h.Context()).Do(); err != nil {
		t.Fatalf("v1 projects.create %s: %v", id, err)
	}
	t.Cleanup(func() { _, _ = v1.Projects.Delete(id).Do() })
	return id
}

func rolesOf(p *iampb.Policy) map[string]string {
	out := map[string]string{}
	for _, b := range p.GetBindings() {
		m := append([]string(nil), b.GetMembers()...)
		slices.Sort(m)
		out[b.GetRole()] = strings.Join(m, ",")
	}
	return out
}

// TestResourceManagerProjectIAM: the official v3 ProjectsClient's
// GetIamPolicy, SetIamPolicy and TestIamPermissions, and v1's, on one
// policy. A set reads back with a new etag, a stale etag is ABORTED, a
// conditional binding is UNIMPLEMENTED (audit configs are stored since #1486,
// TestResourceManagerAuditConfigs), TestIamPermissions
// answers the permissions a project can hold, nothing inherits the policy (a
// bucket-less check: the instance project's policy is untouched), and a
// deleted project refuses a set.
// covers: google.cloud.resourcemanager.v3.Projects/GetIamPolicy, google.cloud.resourcemanager.v3.Projects/SetIamPolicy, google.cloud.resourcemanager.v3.Projects/TestIamPermissions, cloudresourcemanager.projects.getIamPolicy, cloudresourcemanager.projects.setIamPolicy, cloudresourcemanager.projects.testIamPermissions, cloudresourcemanager.projects.create, cloudresourcemanager.projects.delete
func TestResourceManagerProjectIAM(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	c := rmProjects(t, h)
	id := iamProject(t, h, "iam-")
	name := "projects/" + id

	empty, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
	if err != nil || len(empty.GetBindings()) != 0 || len(empty.GetEtag()) == 0 {
		t.Fatalf("GetIamPolicy of a new project = %v, %v; want an empty policy with an etag", empty, err)
	}
	set, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name, Policy: &iampb.Policy{Etag: empty.GetEtag(),
		Bindings: []*iampb.Binding{{Role: "roles/viewer", Members: []string{"user:ada@example.com"}}}}})
	if err != nil || string(set.GetEtag()) == string(empty.GetEtag()) {
		t.Fatalf("SetIamPolicy = %v, %v; want a new etag", set, err)
	}
	if _, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name, Policy: &iampb.Policy{Etag: empty.GetEtag()}}); status.Code(err) != codes.Aborted {
		t.Errorf("SetIamPolicy with a stale etag = %v; want ABORTED", err)
	}
	if _, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name, Policy: &iampb.Policy{Version: 3, Bindings: []*iampb.Binding{{
		Role: "roles/viewer", Members: []string{"user:ada@example.com"}, Condition: &expr.Expr{Expression: "true"}}}}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("a conditional binding = %v; want UNIMPLEMENTED", err)
	}
	tp, err := c.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: name,
		Permissions: []string{"storage.buckets.create", "resourcemanager.projects.get", "resourcemanager.organizations.get"}})
	if err != nil || !slices.Equal(tp.GetPermissions(), []string{"storage.buckets.create", "resourcemanager.projects.get"}) {
		t.Errorf("TestIamPermissions = %v, %v; want the two a project can hold", tp, err)
	}
	// One project's policy is not another's: nothing is inherited or shared.
	other := iamProject(t, h, "iam2-")
	if p, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: "projects/" + other}); err != nil || len(p.GetBindings()) != 0 {
		t.Errorf("another project's policy = %v, %v; want it empty", p, err)
	}

	// v1, which gcloud projects and Terraform call: the same policy.
	v1 := rmV1(t, h)
	p1, err := v1.Projects.GetIamPolicy(id, &crmv1.GetIamPolicyRequest{Options: &crmv1.GetPolicyOptions{RequestedPolicyVersion: 3}}).Context(ctx).Do()
	if err != nil || len(p1.Bindings) != 1 || p1.Bindings[0].Role != "roles/viewer" {
		t.Fatalf("v1 getIamPolicy = %+v, %v", p1, err)
	}
	p1.Bindings = append(p1.Bindings, &crmv1.Binding{Role: "roles/editor", Members: []string{"group:ops@example.com"}})
	if _, err := v1.Projects.SetIamPolicy(id, &crmv1.SetIamPolicyRequest{Policy: p1, UpdateMask: "bindings,etag,auditConfigs"}).Context(ctx).Do(); err != nil {
		t.Fatalf("v1 setIamPolicy with Terraform's mask: %v", err)
	}
	if r, err := v1.Projects.TestIamPermissions(id, &crmv1.TestIamPermissionsRequest{Permissions: []string{"pubsub.topics.create"}}).Context(ctx).Do(); err != nil || len(r.Permissions) != 1 {
		t.Errorf("v1 testIamPermissions = %+v, %v", r, err)
	}
	got, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
	if err != nil || fmt.Sprint(rolesOf(got)) != fmt.Sprint(map[string]string{"roles/editor": "group:ops@example.com", "roles/viewer": "user:ada@example.com"}) {
		t.Errorf("v3 after the v1 set = %v, %v", got, err)
	}

	if _, err := v1.Projects.Delete(id).Context(ctx).Do(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name, Policy: &iampb.Policy{}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("SetIamPolicy of a deleted project = %v; want FAILED_PRECONDITION", err)
	}
	if _, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: "projects/absent-" + id[:10]}); status.Code(err) != codes.NotFound {
		t.Errorf("GetIamPolicy of an absent project = %v; want NOT_FOUND", err)
	}
}

// TestResourceManagerFolderAndOrgIAM: the official FoldersClient's and
// OrganizationsClient's three IAM methods, stored as a project's are.
// covers: google.cloud.resourcemanager.v3.Folders/GetIamPolicy, google.cloud.resourcemanager.v3.Folders/SetIamPolicy, google.cloud.resourcemanager.v3.Folders/TestIamPermissions, google.cloud.resourcemanager.v3.Organizations/GetIamPolicy, google.cloud.resourcemanager.v3.Organizations/SetIamPolicy, google.cloud.resourcemanager.v3.Organizations/TestIamPermissions, google.cloud.resourcemanager.v3.Folders/CreateFolder, google.cloud.resourcemanager.v3.Folders/DeleteFolder, cloudresourcemanager.organizations.getIamPolicy, cloudresourcemanager.organizations.setIamPolicy, cloudresourcemanager.organizations.testIamPermissions
func TestResourceManagerFolderAndOrgIAM(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	fc, oc, _ := foldersClients(t, h)
	op, err := fc.CreateFolder(ctx, &rmpb.CreateFolderRequest{Folder: &rmpb.Folder{Parent: localOrg, DisplayName: "iam " + h.Project()[len(h.Project())-6:]}})
	if err != nil {
		t.Fatal(err)
	}
	f, err := op.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = fc.DeleteFolder(context.Background(), &rmpb.DeleteFolderRequest{Name: f.GetName()}) })

	b := []*iampb.Binding{{Role: "roles/resourcemanager.folderViewer", Members: []string{"user:ada@example.com"}}}
	if _, err := fc.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: f.GetName(), Policy: &iampb.Policy{Bindings: b}}); err != nil {
		t.Fatalf("folder SetIamPolicy: %v", err)
	}
	if p, err := fc.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: f.GetName()}); err != nil || len(p.GetBindings()) != 1 {
		t.Errorf("folder GetIamPolicy = %v, %v", p, err)
	}
	if r, err := fc.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: f.GetName(),
		Permissions: []string{"resourcemanager.folders.get", "resourcemanager.organizations.get"}}); err != nil || !slices.Equal(r.GetPermissions(), []string{"resourcemanager.folders.get"}) {
		t.Errorf("folder TestIamPermissions = %v, %v", r, err)
	}
	if _, err := fc.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: "folders/1"}); status.Code(err) != codes.NotFound {
		t.Errorf("an absent folder's GetIamPolicy = %v; want NOT_FOUND", err)
	}

	before, err := oc.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: localOrg})
	if err != nil {
		t.Fatalf("organization GetIamPolicy: %v", err)
	}
	t.Cleanup(func() {
		cur, err := oc.GetIamPolicy(context.Background(), &iampb.GetIamPolicyRequest{Resource: localOrg})
		if err == nil {
			_, _ = oc.SetIamPolicy(context.Background(), &iampb.SetIamPolicyRequest{Resource: localOrg,
				Policy: &iampb.Policy{Etag: cur.GetEtag(), Bindings: before.GetBindings()}})
		}
	})
	org := append(before.GetBindings(), &iampb.Binding{Role: "roles/browser", Members: []string{"group:ops@example.com"}})
	if _, err := oc.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: localOrg, Policy: &iampb.Policy{Etag: before.GetEtag(), Bindings: org}}); err != nil {
		t.Fatalf("organization SetIamPolicy: %v", err)
	}
	if _, err := oc.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: localOrg, Policy: &iampb.Policy{Etag: before.GetEtag()}}); status.Code(err) != codes.Aborted {
		t.Errorf("organization SetIamPolicy with a stale etag = %v; want ABORTED", err)
	}
	if r, err := oc.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: localOrg, Permissions: []string{"resourcemanager.organizations.get"}}); err != nil || len(r.GetPermissions()) != 1 {
		t.Errorf("organization TestIamPermissions = %v, %v", r, err)
	}
	v1 := rmV1(t, h)
	if p, err := v1.Organizations.GetIamPolicy(localOrg, &crmv1.GetIamPolicyRequest{}).Context(ctx).Do(); err != nil || !strings.Contains(fmt.Sprint(p.Bindings[len(p.Bindings)-1].Members), "group:ops@example.com") {
		t.Errorf("v1 organizations.getIamPolicy = %+v, %v", p, err)
	}
	if r, err := v1.Organizations.TestIamPermissions(localOrg, &crmv1.TestIamPermissionsRequest{Permissions: []string{"resourcemanager.organizations.get"}}).Context(ctx).Do(); err != nil || len(r.Permissions) != 1 {
		t.Errorf("v1 organizations.testIamPermissions = %+v, %v", r, err)
	}
	cur, _ := v1.Organizations.GetIamPolicy(localOrg, &crmv1.GetIamPolicyRequest{}).Context(ctx).Do()
	if _, err := v1.Organizations.SetIamPolicy(localOrg, &crmv1.SetIamPolicyRequest{Policy: cur}).Context(ctx).Do(); err != nil {
		t.Errorf("v1 organizations.setIamPolicy: %v", err)
	}
}

// TestGcloudProjectIAM: gcloud projects add-iam-policy-binding,
// get-iam-policy and remove-iam-policy-binding through gcloud-setup alone,
// read back with the official v3 client. gcloud read-modify-writes with the
// etag, so a second add keeps the first.
// covers: cloudresourcemanager.projects.getIamPolicy, cloudresourcemanager.projects.setIamPolicy, google.cloud.resourcemanager.v3.Projects/GetIamPolicy
func TestGcloudProjectIAM(t *testing.T) {
	h := New(t)
	c := rmProjects(t, h)
	g := newGcloudSession(t, h)
	ctx := h.Context()
	id := iamProject(t, h, "giam-")
	const dev, ops = "user:dev@example.com", "group:ops@example.com"

	g.must("projects", "add-iam-policy-binding", id, "--member="+dev, "--role=roles/viewer", "--condition=None")
	g.must("projects", "add-iam-policy-binding", id, "--member="+ops, "--role=roles/editor", "--condition=None")
	read := func() map[string]string {
		t.Helper()
		p, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: "projects/" + id})
		if err != nil {
			t.Fatalf("GetIamPolicy: %v", err)
		}
		return rolesOf(p)
	}
	if r := read(); fmt.Sprint(r) != fmt.Sprint(map[string]string{"roles/editor": ops, "roles/viewer": dev}) {
		t.Errorf("after two adds the policy = %v", r)
	}
	var got struct {
		Etag     string `json:"etag"`
		Bindings []struct {
			Role    string   `json:"role"`
			Members []string `json:"members"`
		} `json:"bindings"`
	}
	if err := json.Unmarshal([]byte(g.must("projects", "get-iam-policy", id, "--format=json")), &got); err != nil {
		t.Fatalf("get-iam-policy is not JSON: %v", err)
	}
	if got.Etag == "" || len(got.Bindings) != 2 {
		t.Errorf("get-iam-policy = %+v; want both bindings and an etag", got)
	}
	g.must("projects", "remove-iam-policy-binding", id, "--member="+ops, "--role=roles/editor", "--condition=None")
	if r := read(); len(r) != 1 || r["roles/viewer"] != dev {
		t.Errorf("after remove-iam-policy-binding the policy = %v", r)
	}
}

// TestTerraformProjectIAM (#1368, #1304): google_project_iam_member,
// _binding and _policy through `cloudburrow terraform` with egress blocked:
// applied, a plan with no diff, changed (member and binding, then none,
// then a policy, then none) and destroyed, each read back with the official
// v3 client.
// covers: cloudresourcemanager.projects.getIamPolicy, cloudresourcemanager.projects.setIamPolicy, cloudresourcemanager.projects.get, google.cloud.resourcemanager.v3.Projects/GetIamPolicy
func TestTerraformProjectIAM(t *testing.T) {
	h := New(t)
	c := rmProjects(t, h)
	m := newTFModule(t)
	ctx := h.Context()
	id := iamProject(t, h, "tfiam-")
	runTFIAMPhases(t, m, "", [2]string{"roles/viewer", "roles/editor"}, []tfIAMTarget{{
		kind: "google_project", label: "p", policyData: true, ref: map[string]string{"project": fmt.Sprintf("%q", id)},
		read: func() map[string][]string {
			t.Helper()
			p, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: "projects/" + id})
			if err != nil {
				t.Fatalf("GetIamPolicy: %v", err)
			}
			out := map[string][]string{}
			for _, b := range p.GetBindings() {
				out[b.GetRole()] = b.GetMembers()
			}
			return out
		},
	}})
}

// TestConsoleProjectIAM: IAM & Admin > IAM, the selected project's policy
// on the Permissions tab of its row, read back with the official v3 client.
// covers: google.cloud.resourcemanager.v3.Projects/GetIamPolicy, google.cloud.resourcemanager.v3.Projects/SetIamPolicy, google.cloud.resourcemanager.v3.Projects/TestIamPermissions
func TestConsoleProjectIAM(t *testing.T) {
	h := New(t)
	c := rmProjects(t, h)
	addr := consoleAddr(t, h)
	id := iamProject(t, h, "ciam-")
	name := "projects/" + id
	consolePermissionsRoundTripIn(t, h, addr, "iam", id, []string{id}, policyHandle{
		get: func(ctx context.Context) (*iampb.Policy, error) {
			return c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name})
		},
		set: func(ctx context.Context, p *iampb.Policy) error {
			_, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name, Policy: p})
			return err
		},
		test: func(ctx context.Context, perms []string) ([]string, error) {
			r, err := c.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: name, Permissions: perms})
			return r.GetPermissions(), err
		},
	}, "roles/viewer", "storage.buckets.list")
}
