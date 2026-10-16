//go:build compat

package compat

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	crmv1 "google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// localOrg is the instance's one organization (#1294), documented in
// docs/compatibility.md.
const localOrg = "organizations/100000000001"

// folderLabel is a display name unique to this run: folder names are unique
// among siblings, and the CI instance is shared.
func folderLabel(h *Harness, prefix string) string {
	s := prefix + strings.TrimPrefix(h.Project(), "cb-test-")
	if len(s) > 30 {
		s = s[:30]
	}
	return strings.TrimRight(s, "-_ ")
}

func foldersClients(t *testing.T, h *Harness) (*resourcemanager.FoldersClient, *resourcemanager.OrganizationsClient, *resourcemanager.ProjectsClient) {
	t.Helper()
	return foldersClientsAt(t, h, rmOptions(h))
}

// foldersClientsAt is foldersClients with opts, for a test that reads
// Resource Manager under its own shard's variable.
func foldersClientsAt(t *testing.T, h *Harness, opts []option.ClientOption) (*resourcemanager.FoldersClient, *resourcemanager.OrganizationsClient, *resourcemanager.ProjectsClient) {
	t.Helper()
	ctx := h.Context()
	fc, err := resourcemanager.NewFoldersClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	oc, err := resourcemanager.NewOrganizationsClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	pc, err := resourcemanager.NewProjectsClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fc.Close(); _ = oc.Close(); _ = pc.Close() })
	return fc, oc, pc
}

// covers: google.cloud.resourcemanager.v3.Organizations/GetOrganization, google.cloud.resourcemanager.v3.Organizations/SearchOrganizations
//
// TestResourceManagerV3Organizations (#1294): the one local organization
// through cloud.google.com/go/resourcemanager/apiv3 OrganizationsClient: get
// it, find it by domain, and another organization is NOT_FOUND. Its IAM and
// a folder's are stored since #1368 (TestResourceManagerFolderAndOrgIAM).
func TestResourceManagerV3Organizations(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	_, oc, _ := foldersClients(t, h)
	o, err := oc.GetOrganization(ctx, &rmpb.GetOrganizationRequest{Name: localOrg})
	if err != nil || o.GetState() != rmpb.Organization_ACTIVE || o.GetDisplayName() == "" {
		t.Fatalf("GetOrganization = %v, %v", o, err)
	}
	found, err := oc.SearchOrganizations(ctx, &rmpb.SearchOrganizationsRequest{Query: "domain:" + o.GetDisplayName()}).Next()
	if err != nil || found.GetName() != localOrg {
		t.Errorf("SearchOrganizations(domain) = %v, %v", found, err)
	}
	if _, err := oc.SearchOrganizations(ctx, &rmpb.SearchOrganizationsRequest{Query: "domain:example.com"}).Next(); err != iterator.Done {
		t.Errorf("SearchOrganizations(other domain) = %v; want none", err)
	}
	if _, err := oc.GetOrganization(ctx, &rmpb.GetOrganizationRequest{Name: "organizations/1"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetOrganization(organizations/1) = %v; want NotFound", err)
	}
}

// covers: google.cloud.resourcemanager.v3.Folders/CreateFolder, google.cloud.resourcemanager.v3.Folders/GetFolder, google.cloud.resourcemanager.v3.Folders/ListFolders, google.cloud.resourcemanager.v3.Folders/SearchFolders, google.cloud.resourcemanager.v3.Folders/UpdateFolder, google.cloud.resourcemanager.v3.Folders/MoveFolder, google.cloud.resourcemanager.v3.Folders/DeleteFolder, google.cloud.resourcemanager.v3.Folders/UndeleteFolder, google.cloud.resourcemanager.v3.Projects/MoveProject, google.cloud.resourcemanager.v3.Projects/ListProjects, cloudresourcemanager.projects.getAncestry
//
// TestResourceManagerV3Folders (#1294): folders through
// cloud.google.com/go/resourcemanager/apiv3 FoldersClient, each change a
// long-running operation waited on: create under the organization and under
// a folder, get, list, search, rename, a project created in a folder and
// listed there, moved to the organization, MoveFolder (refused under its own
// child), DeleteFolder (refused while not empty), UndeleteFolder, and folder
// IAM UNIMPLEMENTED. v1 getAncestry reports the project's folders and the
// organization, nearest first.
func TestResourceManagerV3Folders(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	fc, _, pc := foldersClients(t, h)
	create := func(parent, name string) *rmpb.Folder {
		t.Helper()
		op, err := fc.CreateFolder(ctx, &rmpb.CreateFolderRequest{Folder: &rmpb.Folder{Parent: parent, DisplayName: name}})
		if err != nil {
			t.Fatalf("CreateFolder(%s, %q): %v", parent, name, err)
		}
		f, err := op.Wait(ctx)
		if err != nil || f.GetParent() != parent || f.GetState() != rmpb.Folder_ACTIVE || !strings.HasPrefix(f.GetName(), "folders/") {
			t.Fatalf("CreateFolder result = %v, %v", f, err)
		}
		t.Cleanup(func() { _, _ = fc.DeleteFolder(context.Background(), &rmpb.DeleteFolderRequest{Name: f.GetName()}) })
		return f
	}
	top := create(localOrg, folderLabel(h, "top-"))
	child := create(top.GetName(), "child")
	if _, err := fc.CreateFolder(ctx, &rmpb.CreateFolderRequest{Folder: &rmpb.Folder{Parent: top.GetName(), DisplayName: "child"}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("a second sibling named child = %v; want FailedPrecondition", err)
	}

	if f, err := fc.GetFolder(ctx, &rmpb.GetFolderRequest{Name: child.GetName()}); err != nil || f.GetDisplayName() != "child" || f.GetParent() != top.GetName() {
		t.Errorf("GetFolder = %v, %v", f, err)
	}
	listed := false
	it := fc.ListFolders(ctx, &rmpb.ListFoldersRequest{Parent: localOrg})
	for {
		f, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListFolders: %v", err)
		}
		listed = listed || f.GetName() == top.GetName()
	}
	if !listed {
		t.Errorf("ListFolders(%s) does not list %s", localOrg, top.GetName())
	}
	if f, err := fc.SearchFolders(ctx, &rmpb.SearchFoldersRequest{Query: "displayName=" + top.GetDisplayName()}).Next(); err != nil || f.GetName() != top.GetName() {
		t.Errorf("SearchFolders(displayName) = %v, %v", f, err)
	}
	if f, err := fc.SearchFolders(ctx, &rmpb.SearchFoldersRequest{Query: "parent=" + top.GetName()}).Next(); err != nil || f.GetName() != child.GetName() {
		t.Errorf("SearchFolders(parent) = %v, %v", f, err)
	}
	uop, err := fc.UpdateFolder(ctx, &rmpb.UpdateFolderRequest{Folder: &rmpb.Folder{Name: child.GetName(), DisplayName: "renamed"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}})
	if err != nil {
		t.Fatalf("UpdateFolder: %v", err)
	}
	if f, err := uop.Wait(ctx); err != nil || f.GetDisplayName() != "renamed" {
		t.Errorf("UpdateFolder result = %v, %v", f, err)
	}

	// A project in the child folder.
	id := v1ID(h, "fp-")
	cop, err := pc.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: id, Parent: child.GetName()}})
	if err != nil {
		t.Fatalf("CreateProject in a folder: %v", err)
	}
	if p, err := cop.Wait(ctx); err != nil || p.GetParent() != child.GetName() {
		t.Fatalf("CreateProject in a folder = %v, %v", p, err)
	}
	t.Cleanup(func() {
		_, _ = pc.DeleteProject(context.Background(), &rmpb.DeleteProjectRequest{Name: "projects/" + id})
	})
	if p, err := pc.ListProjects(ctx, &rmpb.ListProjectsRequest{Parent: child.GetName()}).Next(); err != nil || p.GetProjectId() != id {
		t.Errorf("ListProjects(%s) = %v, %v", child.GetName(), p, err)
	}
	anc, err := rmV1(t, h).Projects.GetAncestry(id, &crmv1.GetAncestryRequest{}).Context(ctx).Do()
	if err != nil {
		t.Fatalf("v1 getAncestry: %v", err)
	}
	var chain []string
	for _, a := range anc.Ancestor {
		chain = append(chain, a.ResourceId.Type+"/"+a.ResourceId.Id)
	}
	want := fmt.Sprintf("project/%s folder/%s folder/%s organization/100000000001", id,
		strings.TrimPrefix(child.GetName(), "folders/"), strings.TrimPrefix(top.GetName(), "folders/"))
	if strings.Join(chain, " ") != want {
		t.Errorf("getAncestry = %v; want %s", chain, want)
	}

	if _, err := fc.MoveFolder(ctx, &rmpb.MoveFolderRequest{Name: top.GetName(), DestinationParent: child.GetName()}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("MoveFolder under its own child = %v; want InvalidArgument", err)
	}
	if _, err := fc.DeleteFolder(ctx, &rmpb.DeleteFolderRequest{Name: child.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("DeleteFolder of a folder holding a project = %v; want FailedPrecondition", err)
	}
	mop, err := pc.MoveProject(ctx, &rmpb.MoveProjectRequest{Name: "projects/" + id, DestinationParent: localOrg})
	if err != nil {
		t.Fatalf("MoveProject: %v", err)
	}
	if p, err := mop.Wait(ctx); err != nil || p.GetParent() != localOrg {
		t.Errorf("MoveProject result = %v, %v", p, err)
	}
	mf, err := fc.MoveFolder(ctx, &rmpb.MoveFolderRequest{Name: child.GetName(), DestinationParent: localOrg})
	if err != nil {
		t.Fatalf("MoveFolder: %v", err)
	}
	if f, err := mf.Wait(ctx); err != nil || f.GetParent() != localOrg {
		t.Errorf("MoveFolder result = %v, %v", f, err)
	}
	dop, err := fc.DeleteFolder(ctx, &rmpb.DeleteFolderRequest{Name: child.GetName()})
	if err != nil {
		t.Fatalf("DeleteFolder: %v", err)
	}
	if f, err := dop.Wait(ctx); err != nil || f.GetState() != rmpb.Folder_DELETE_REQUESTED {
		t.Errorf("DeleteFolder result = %v, %v", f, err)
	}
	if f, err := fc.GetFolder(ctx, &rmpb.GetFolderRequest{Name: child.GetName()}); err != nil || f.GetState() != rmpb.Folder_DELETE_REQUESTED {
		t.Errorf("GetFolder after delete = %v, %v; want DELETE_REQUESTED", f, err)
	}
	udop, err := fc.UndeleteFolder(ctx, &rmpb.UndeleteFolderRequest{Name: child.GetName()})
	if err != nil {
		t.Fatalf("UndeleteFolder: %v", err)
	}
	if f, err := udop.Wait(ctx); err != nil || f.GetState() != rmpb.Folder_ACTIVE {
		t.Errorf("UndeleteFolder result = %v, %v", f, err)
	}
	// Its IAM policy is stored since #1368, and a new folder has none.
	if p, err := fc.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: child.GetName()}); err != nil || len(p.GetBindings()) != 0 {
		t.Errorf("folder GetIamPolicy = %v, %v; want an empty policy", p, err)
	}
}

// covers: google.cloud.resourcemanager.v3.Folders/CreateFolder, google.cloud.resourcemanager.v3.Folders/UpdateFolder, google.cloud.resourcemanager.v3.Folders/MoveFolder, google.cloud.resourcemanager.v3.Folders/DeleteFolder
//
// TestTerraformGoogleFolder (#1294): a google_folder under the local
// organization, a nested google_folder and a google_project in it, through
// `cloudburrow terraform` (resource_manager_v3_custom_endpoint and
// resource_manager_custom_endpoint point here). They apply and plan clean;
// renaming the nested folder and moving it to the organization apply in
// place (the folder number is kept) and plan clean; destroy deletes them.
func TestTerraformGoogleFolder(t *testing.T) {
	h := New(t)
	m := newTFModule(t)
	fc, _, _ := foldersClients(t, h)
	ctx := h.Context()
	top := folderLabel(h, "tf-")
	id := v1ID(h, "tff-")
	module := func(name, parent string) string {
		return fmt.Sprintf(`
resource "google_folder" "top" {
  display_name        = %q
  parent              = %q
  deletion_protection = false
}

resource "google_folder" "child" {
  display_name        = %q
  parent              = %s
  deletion_protection = false
}

resource "google_project" "p" {
  project_id      = %q
  name            = "In a folder"
  folder_id       = google_folder.child.folder_id
  deletion_policy = "DELETE"
}
`, top, localOrg, name, parent, id)
	}
	read := func(when, name string) *rmpb.Folder {
		t.Helper()
		f, err := fc.SearchFolders(ctx, &rmpb.SearchFoldersRequest{Query: fmt.Sprintf("displayName=%q", name)}).Next()
		if err != nil {
			t.Fatalf("%s, SearchFolders(%q): %v", when, name, err)
		}
		return f
	}

	m.write(module("tf child", "google_folder.top.name"))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	topF := read("after apply", top)
	child := read("after apply", "tf child")
	if child.GetParent() != topF.GetName() || topF.GetParent() != localOrg {
		t.Errorf("after apply, %s is under %s and %s under %s", child.GetName(), child.GetParent(), topF.GetName(), topF.GetParent())
	}
	if p, err := rmV1(t, h).Projects.Get(id).Context(ctx).Do(); err != nil || p.Parent == nil || "folders/"+p.Parent.Id != child.GetName() {
		t.Errorf("the project reads %+v, %v; want it in %s", p, err, child.GetName())
	}
	m.planClean()

	m.write(module("tf renamed", fmt.Sprintf("%q", localOrg)))
	m.apply()
	moved := read("after the rename and move", "tf renamed")
	if moved.GetName() != child.GetName() || moved.GetParent() != localOrg {
		t.Errorf("after the rename and move the folder is %v; want %s under %s", moved, child.GetName(), localOrg)
	}
	m.planClean()

	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	for _, name := range []string{child.GetName(), topF.GetName()} {
		if f, err := fc.GetFolder(ctx, &rmpb.GetFolderRequest{Name: name}); err != nil || f.GetState() != rmpb.Folder_DELETE_REQUESTED {
			t.Errorf("after destroy %s reads %v, %v; want DELETE_REQUESTED", name, f, err)
		}
	}
}

// covers: cloudresourcemanager.organizations.get, cloudresourcemanager.organizations.search, cloudresourcemanager.projects.getAncestry
//
// TestGcloudFoldersAndOrganizations (#1294): gcloud, configured by
// `cloudburrow gcloud-setup` alone, lists and describes the local
// organization (Resource Manager v1), creates, lists, describes and deletes
// a folder under it (v2), creates a project in that folder, and prints the
// project's ancestors (v1 getAncestry). Every proxy variable points at the
// egress guard, so a request bound anywhere but loopback fails the test.
func TestGcloudFoldersAndOrganizations(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager)
	g := newGcloudSession(t, h)
	org := strings.TrimPrefix(localOrg, "organizations/")
	if out := g.must("organizations", "list", "--format=value(name)"); !strings.Contains(out, org) {
		t.Errorf("gcloud organizations list = %q; want %s", out, org)
	}
	if out := g.must("organizations", "describe", org, "--format=value(lifecycleState)"); !strings.Contains(out, "ACTIVE") {
		t.Errorf("gcloud organizations describe = %q", out)
	}
	name := folderLabel(h, "gc-")
	g.must("resource-manager", "folders", "create", "--display-name="+name, "--organization="+org, "--quiet")
	out := g.must("resource-manager", "folders", "list", "--organization="+org, "--filter=displayName="+name, "--format=value(name)")
	folder := strings.TrimPrefix(strings.TrimSpace(out), "folders/")
	if folder == "" {
		t.Fatalf("gcloud resource-manager folders list does not show %s", name)
	}
	if out := g.must("resource-manager", "folders", "describe", folder, "--format=value(displayName)"); strings.TrimSpace(out) != name {
		t.Errorf("gcloud resource-manager folders describe = %q; want %s", out, name)
	}
	id := v1ID(h, "gf-")
	g.must("projects", "create", id, "--folder="+folder, "--no-enable-cloud-apis", "--quiet")
	t.Cleanup(func() { _, _ = g.run(nil, "projects", "delete", id, "--quiet") })
	out = g.must("projects", "get-ancestors", id, "--format=value(id,type)")
	if !strings.Contains(out, folder) || !strings.Contains(out, org) {
		t.Errorf("gcloud projects get-ancestors = %q; want the folder and the organization", out)
	}
	g.must("projects", "delete", id, "--quiet")
	g.must("resource-manager", "folders", "delete", folder, "--quiet")
	if out := g.must("resource-manager", "folders", "describe", folder, "--format=value(lifecycleState)"); !strings.Contains(out, "DELETE_REQUESTED") {
		t.Errorf("after delete the folder reads %q; want DELETE_REQUESTED", out)
	}
}
