//go:build compat

package compat

import (
	"strings"
	"testing"

	"cloud.google.com/go/iam/admin/apiv1/adminpb"
	iamv1 "google.golang.org/api/iam/v1"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// covers: google.iam.admin.v1.IAM/GetRole, google.iam.admin.v1.IAM/ListRoles, google.iam.admin.v1.IAM/QueryGrantableRoles, google.iam.admin.v1.IAM/CreateRole, google.iam.admin.v1.IAM/DeleteRole, google.iam.admin.v1.IAM/UndeleteRole
//
// TestIAMPredefinedRoles (#1746), with the official
// cloud.google.com/go/iam/admin/apiv1 client over gRPC and
// google.golang.org/api/iam/v1 over REST: GetRole of a predefined role
// (Google's title, description, stage, permissions and etag "AA=="), an
// unknown one NOT_FOUND, ListRoles without a parent paging every
// predefined role (BASIC without permissions, FULL with), and
// QueryGrantableRoles for a project (its custom role, project-grantable
// predefined roles, not an organization-only role) and the organization.
// The codes Google's own samples catch: reusing a deleted role's ID,
// deleting a deleted role and undeleting a live one are
// FAILED_PRECONDITION.
func TestIAMPredefinedRoles(t *testing.T) {
	h := New(t)
	c := iamAdmin(t, h)
	project := iamProject(t, h, "iamp-")
	ctx := h.Context()

	r, err := c.GetRole(ctx, &adminpb.GetRoleRequest{Name: "roles/storage.objectViewer"})
	if err != nil || r.GetTitle() != "Storage Object Viewer" || r.GetStage() != adminpb.Role_GA || string(r.GetEtag()) != "\x00" ||
		!strings.HasPrefix(r.GetDescription(), "Grants access to view objects and their metadata") ||
		!strings.Contains(","+strings.Join(r.GetIncludedPermissions(), ",")+",", ",storage.objects.get,") {
		t.Errorf("GetRole roles/storage.objectViewer = %v, %v", r, err)
	}
	if r, err := c.GetRole(ctx, &adminpb.GetRoleRequest{Name: "roles/aiplatform.expressUser"}); err != nil || r.GetStage() != adminpb.Role_BETA {
		t.Errorf("GetRole of a Beta role = %v, %v", r, err)
	}
	if _, err := c.GetRole(ctx, &adminpb.GetRoleRequest{Name: "roles/compat.noSuchRole"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetRole of an unknown role = %v, want NotFound", err)
	}
	n := 0
	for token := ""; ; {
		resp, err := c.ListRoles(ctx, &adminpb.ListRolesRequest{PageSize: 1000, PageToken: token})
		if err != nil {
			t.Fatalf("ListRoles: %v", err)
		}
		for _, r := range resp.GetRoles() {
			if len(r.GetIncludedPermissions()) != 0 || !strings.HasPrefix(r.GetName(), "roles/") {
				t.Fatalf("ListRoles BASIC returned %v", r)
			}
			n++
		}
		if token = resp.GetNextPageToken(); token == "" {
			break
		}
	}
	if n < 2400 {
		t.Errorf("ListRoles listed %d predefined roles", n)
	}
	if resp, err := c.ListRoles(ctx, &adminpb.ListRolesRequest{View: adminpb.RoleView_FULL, PageSize: 5}); err != nil || len(resp.GetRoles()) != 5 ||
		len(resp.GetRoles()[0].GetIncludedPermissions()) == 0 || resp.GetNextPageToken() == "" {
		t.Errorf("ListRoles FULL = %v, %v", resp, err)
	}

	mine, err := c.CreateRole(ctx, &adminpb.CreateRoleRequest{Parent: "projects/" + project, RoleId: "grantable", Role: &adminpb.Role{
		IncludedPermissions: []string{"datastore.entities.get"}}})
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	grantable := map[string]bool{}
	for token := ""; ; {
		resp, err := c.QueryGrantableRoles(ctx, &adminpb.QueryGrantableRolesRequest{
			FullResourceName: "//cloudresourcemanager.googleapis.com/projects/" + project, PageSize: 1000, PageToken: token})
		if err != nil {
			t.Fatalf("QueryGrantableRoles: %v", err)
		}
		for _, r := range resp.GetRoles() {
			grantable[r.GetName()] = true
		}
		if token = resp.GetNextPageToken(); token == "" {
			break
		}
	}
	for _, want := range []string{mine.GetName(), "roles/storage.objectViewer", "roles/viewer", "roles/iam.roleViewer"} {
		if !grantable[want] {
			t.Errorf("%s is not grantable on the project", want)
		}
	}
	if grantable["roles/iam.organizationRoleAdmin"] {
		t.Error("roles/iam.organizationRoleAdmin, an organization role, is grantable on a project")
	}

	// REST, as gcloud and Terraform call it.
	svc, err := iamv1.NewService(ctx, option.WithEndpoint("http://"+strings.TrimPrefix(h.Endpoint(EnvResourceManager), "http://")+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	if r, err := svc.Roles.Get("roles/iam.roleViewer").Context(ctx).Do(); err != nil || r.Title != "Role Viewer" || r.Etag != "AA==" || r.Stage != "GA" || len(r.IncludedPermissions) == 0 {
		t.Errorf("REST roles.get = %+v, %v", r, err)
	}
	if l, err := svc.Roles.List().PageSize(3).Context(ctx).Do(); err != nil || len(l.Roles) != 3 || l.NextPageToken == "" {
		t.Errorf("REST roles.list = %+v, %v", l, err)
	}
	q, err := svc.Roles.QueryGrantableRoles(&iamv1.QueryGrantableRolesRequest{
		FullResourceName: "//cloudresourcemanager.googleapis.com/" + localOrgRoles, PageSize: 1000}).Context(ctx).Do()
	if err != nil || len(q.Roles) != 1000 {
		t.Fatalf("REST roles.queryGrantableRoles = %v", err)
	}

	// The codes Google's samples catch.
	if _, err := c.DeleteRole(ctx, &adminpb.DeleteRoleRequest{Name: mine.GetName()}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeleteRole(ctx, &adminpb.DeleteRoleRequest{Name: mine.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("deleting a deleted role = %v, want FailedPrecondition", err)
	}
	if _, err := c.CreateRole(ctx, &adminpb.CreateRoleRequest{Parent: "projects/" + project, RoleId: "grantable", Role: &adminpb.Role{}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("reusing a deleted role's ID = %v, want FailedPrecondition", err)
	}
	if _, err := c.UndeleteRole(ctx, &adminpb.UndeleteRoleRequest{Name: mine.GetName()}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UndeleteRole(ctx, &adminpb.UndeleteRoleRequest{Name: mine.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("undeleting a live role = %v, want FailedPrecondition", err)
	}
}

// TestGcloudIAMPredefinedRoles (#1746): gcloud iam roles describe of a
// predefined role, gcloud iam roles list (every predefined role, paged),
// gcloud iam list-grantable-roles of a project and gcloud iam
// list-testable-permissions, through gcloud-setup's iam override.
func TestGcloudIAMPredefinedRoles(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager)
	project := iamProject(t, h, "iamgp-")
	g := newGcloudSession(t, h)
	if out := g.must("iam", "roles", "describe", "roles/storage.objectViewer", "--format=value(title,stage,etag)"); !strings.Contains(out, "Storage Object Viewer") ||
		!strings.Contains(out, "GA") || !strings.Contains(out, "AA==") {
		t.Errorf("describe roles/storage.objectViewer:\n%s", out)
	}
	if out := g.must("iam", "roles", "describe", "roles/storage.objectViewer", "--format=value(includedPermissions)"); !strings.Contains(out, "storage.objects.list") {
		t.Errorf("describe's permissions:\n%s", out)
	}
	out := g.must("iam", "roles", "list", "--format=value(name)")
	if lines := strings.Count(out, "\n"); lines < 2400 || !strings.Contains(out, "roles/iam.roleViewer\n") {
		t.Errorf("list: %d roles", lines)
	}
	out = g.must("iam", "list-grantable-roles", "//cloudresourcemanager.googleapis.com/projects/"+project, "--format=value(name)")
	if !strings.Contains(out, "roles/storage.admin\n") || strings.Contains(out, "roles/iam.organizationRoleAdmin\n") {
		t.Errorf("list-grantable-roles: %d roles", strings.Count(out, "\n"))
	}
	out = g.must("iam", "list-testable-permissions", "//cloudresourcemanager.googleapis.com/projects/"+project, "--format=value(name)")
	if !strings.Contains(out, "storage.objects.get\n") {
		t.Errorf("list-testable-permissions: %d permissions", strings.Count(out, "\n"))
	}
}

// TestTerraformIAMRoleDataSources (#1746): the google_iam_role data source
// reads a predefined role's title, stage and included permissions, and
// plans clean. (google_iam_testable_permissions is not covered: provider
// 8.x posts to https://iam.googleapis.com/v1/permissions:queryTestablePermissions,
// a URL it builds in, not from iam_custom_endpoint, so no instance can
// answer it.)
func TestTerraformIAMRoleDataSources(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager)
	m := newTFModule(t)
	m.write(`
data "google_iam_role" "r" {
  name = "roles/storage.objectViewer"
}

output "title" {
  value = data.google_iam_role.r.title
}

output "stage" {
  value = data.google_iam_role.r.stage
}

output "perms" {
  value = join(",", data.google_iam_role.r.included_permissions)
}
`)
	m.must("init", "-input=false", "-no-color")
	m.apply()
	if got := strings.TrimSpace(m.must("output", "-raw", "title")); got != "Storage Object Viewer" {
		t.Errorf("title = %q", got)
	}
	if got := strings.TrimSpace(m.must("output", "-raw", "stage")); got != "GA" {
		t.Errorf("stage = %q", got)
	}
	if got := m.must("output", "-raw", "perms"); !strings.Contains(","+got+",", ",storage.objects.get,") {
		t.Errorf("included_permissions = %q", got)
	}
	m.planClean()
}
