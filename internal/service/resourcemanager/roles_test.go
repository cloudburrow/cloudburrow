package resourcemanager

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	admin "cloud.google.com/go/iam/admin/apiv1"
	"cloud.google.com/go/iam/admin/apiv1/adminpb"
	"cloud.google.com/go/iam/apiv1/iampb"
	iamv1 "google.golang.org/api/iam/v1"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func startIAMAdmin(t *testing.T) (*Registry, *Server, *admin.IamClient) {
	t.Helper()
	reg, srv, _ := startAPI(t)
	c, err := admin.NewIamClient(context.Background(), option.WithEndpoint(srv.Addr()), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return reg, srv, c
}

// Custom roles through the official gRPC client (#1743): the lifecycle,
// etags, validation, deletion and its periods, and RolePermissions.
func TestCustomRolesThroughTheOfficialClient(t *testing.T) {
	reg, _, c := startIAMAdmin(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	reg.now = func() time.Time { return now }
	if _, err := reg.Create(Project{ProjectID: "roles-project"}); err != nil {
		t.Fatal(err)
	}
	const parent = "projects/roles-project"
	role, err := c.CreateRole(ctx, &adminpb.CreateRoleRequest{Parent: parent, RoleId: "reader.v1", Role: &adminpb.Role{
		Title: "Reader", IncludedPermissions: []string{"datastore.entities.list", "datastore.entities.get", "datastore.entities.get"}}})
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	if role.GetName() != parent+"/roles/reader.v1" || role.GetStage() != adminpb.Role_ALPHA || len(role.GetEtag()) == 0 ||
		strings.Join(role.GetIncludedPermissions(), ",") != "datastore.entities.get,datastore.entities.list" {
		t.Errorf("created role = %v", role)
	}
	// By the project's number too.
	if got, err := c.GetRole(ctx, &adminpb.GetRoleRequest{Name: "projects/" + projectNumberString("roles-project") + "/roles/reader.v1"}); err != nil || got.GetName() != role.GetName() {
		t.Errorf("GetRole by number = %v, %v", got, err)
	}
	for _, tc := range []struct {
		id    string
		role  *adminpb.Role
		code  codes.Code
		about string
	}{
		{"reader.v1", &adminpb.Role{}, codes.AlreadyExists, "a taken ID"},
		{"ab", &adminpb.Role{}, codes.InvalidArgument, "a 2-character ID"},
		{strings.Repeat("a", 65), &adminpb.Role{}, codes.InvalidArgument, "a 65-character ID"},
		{"has-dash", &adminpb.Role{}, codes.InvalidArgument, "a dash"},
		{"longtitle", &adminpb.Role{Title: strings.Repeat("t", 101)}, codes.InvalidArgument, "a 101-byte title"},
		{"longdesc", &adminpb.Role{Description: strings.Repeat("d", 301)}, codes.InvalidArgument, "a 301-byte description"},
		{"unknown", &adminpb.Role{IncludedPermissions: []string{"datastore.entities.frobnicate"}}, codes.InvalidArgument, "an unknown permission"},
		{"notsup", &adminpb.Role{IncludedPermissions: []string{"appengine.runtimes.actAsAdmin"}}, codes.InvalidArgument, "a NOT_SUPPORTED permission"},
		{"orgonly", &adminpb.Role{IncludedPermissions: []string{"resourcemanager.organizations.get"}}, codes.InvalidArgument, "an organization permission in a project role"},
	} {
		if _, err := c.CreateRole(ctx, &adminpb.CreateRoleRequest{Parent: parent, RoleId: tc.id, Role: tc.role}); status.Code(err) != tc.code {
			t.Errorf("%s: %v, want %v", tc.about, err, tc.code)
		}
	}
	if _, err := c.CreateRole(ctx, &adminpb.CreateRoleRequest{Parent: parent, RoleId: "unknown", Role: &adminpb.Role{
		IncludedPermissions: []string{"datastore.entities.frobnicate"}}}); err == nil || !strings.Contains(err.Error(), "Permission datastore.entities.frobnicate is not valid.") {
		t.Errorf("an unknown permission's message: %v", err)
	}
	// The organization's roles may hold an organization permission.
	org, err := c.CreateRole(ctx, &adminpb.CreateRoleRequest{Parent: OrganizationName, RoleId: "orgViewer", Role: &adminpb.Role{
		IncludedPermissions: []string{"resourcemanager.organizations.get"}, Stage: adminpb.Role_GA}})
	if err != nil || org.GetName() != OrganizationName+"/roles/orgViewer" {
		t.Fatalf("an organization role = %v, %v", org, err)
	}

	// Update: a mask, then a stale etag.
	up, err := c.UpdateRole(ctx, &adminpb.UpdateRoleRequest{Name: role.GetName(), Role: &adminpb.Role{Title: "Renamed", Etag: role.GetEtag()},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"title"}}})
	if err != nil || up.GetTitle() != "Renamed" || len(up.GetIncludedPermissions()) != 2 || string(up.GetEtag()) == string(role.GetEtag()) {
		t.Fatalf("UpdateRole title = %v, %v", up, err)
	}
	if _, err := c.UpdateRole(ctx, &adminpb.UpdateRoleRequest{Name: role.GetName(), Role: &adminpb.Role{Title: "x", Etag: role.GetEtag()},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"title"}}}); status.Code(err) != codes.Aborted {
		t.Errorf("a stale etag = %v, want Aborted", err)
	}
	if perms, ok := reg.RolePermissions(role.GetName()); !ok || len(perms) != 2 {
		t.Errorf("RolePermissions = %v, %v", perms, ok)
	}

	// A policy may bind it on its project, not on another, and not a role that does not exist.
	if _, err := reg.Create(Project{ProjectID: "other-project"}); err != nil {
		t.Fatal(err)
	}
	bind := func(res, r string) error {
		_, err := reg.SetIAMPolicy(res, &iampb.SetIamPolicyRequest{Resource: res, Policy: &iampb.Policy{
			Bindings: []*iampb.Binding{{Role: r, Members: []string{"user:ada@example.com"}}}}})
		return err
	}
	if err := bind(parent, role.GetName()); err != nil {
		t.Errorf("binding the role on its project: %v", err)
	}
	if err := bind("projects/other-project", role.GetName()); status.Code(err) != codes.InvalidArgument {
		t.Errorf("binding it on another project = %v, want InvalidArgument", err)
	}
	if err := bind(parent, parent+"/roles/nope"); status.Code(err) != codes.InvalidArgument {
		t.Errorf("binding a missing role = %v, want InvalidArgument", err)
	}

	// Delete: hidden from the list, still readable, grants nothing, not
	// bindable anew, undeletable within 7 days.
	del, err := c.DeleteRole(ctx, &adminpb.DeleteRoleRequest{Name: role.GetName()})
	if err != nil || !del.GetDeleted() {
		t.Fatalf("DeleteRole = %v, %v", del, err)
	}
	list := func(showDeleted bool) (names []string) {
		resp, err := c.ListRoles(ctx, &adminpb.ListRolesRequest{Parent: parent, ShowDeleted: showDeleted})
		if err != nil {
			t.Fatalf("ListRoles: %v", err)
		}
		for _, r := range resp.GetRoles() {
			if len(r.GetIncludedPermissions()) != 0 {
				t.Errorf("a BASIC list included permissions: %v", r)
			}
			names = append(names, r.GetName())
		}
		return names
	}
	if got := list(false); len(got) != 0 {
		t.Errorf("list without deleted = %v", got)
	}
	if got := list(true); len(got) != 1 {
		t.Errorf("list with deleted = %v", got)
	}
	if perms, _ := reg.RolePermissions(role.GetName()); len(perms) != 0 {
		t.Errorf("a deleted role grants %v", perms)
	}
	if err := bind("projects/other-project", parent+"/roles/reader.v1"); status.Code(err) != codes.InvalidArgument {
		t.Errorf("binding a deleted role = %v", err)
	}
	if _, err := c.UpdateRole(ctx, &adminpb.UpdateRoleRequest{Name: role.GetName(), Role: &adminpb.Role{Title: "x"}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("updating a deleted role = %v", err)
	}
	if _, err := c.CreateRole(ctx, &adminpb.CreateRoleRequest{Parent: parent, RoleId: "reader.v1", Role: &adminpb.Role{}}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("reusing a deleted role's ID = %v", err)
	}
	now = now.Add(6 * 24 * time.Hour)
	if got, err := c.UndeleteRole(ctx, &adminpb.UndeleteRoleRequest{Name: role.GetName()}); err != nil || got.GetDeleted() {
		t.Fatalf("UndeleteRole after 6 days = %v, %v", got, err)
	}
	if _, err := c.UndeleteRole(ctx, &adminpb.UndeleteRoleRequest{Name: role.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("undeleting a live role = %v", err)
	}
	if _, err := c.DeleteRole(ctx, &adminpb.DeleteRoleRequest{Name: role.GetName()}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(8 * 24 * time.Hour)
	if _, err := c.UndeleteRole(ctx, &adminpb.UndeleteRoleRequest{Name: role.GetName()}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("undeleting after 8 days = %v", err)
	}
	now = now.Add(37 * 24 * time.Hour)
	if _, err := c.GetRole(ctx, &adminpb.GetRoleRequest{Name: role.GetName()}); status.Code(err) != codes.NotFound {
		t.Errorf("a role 45 days after its deletion = %v, want NotFound", err)
	}
	if _, err := c.CreateRole(ctx, &adminpb.CreateRoleRequest{Parent: parent, RoleId: "reader.v1", Role: &adminpb.Role{}}); err != nil {
		t.Errorf("reusing the ID after the deletion completed: %v", err)
	}

	// A DISABLED role grants nothing.
	if _, err := c.UpdateRole(ctx, &adminpb.UpdateRoleRequest{Name: org.GetName(), Role: &adminpb.Role{Stage: adminpb.Role_DISABLED},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"stage"}}}); err != nil {
		t.Fatal(err)
	}
	if perms, ok := reg.RolePermissions(org.GetName()); !ok || len(perms) != 0 {
		t.Errorf("a DISABLED role grants %v", perms)
	}
	// Predefined roles are not served.
	if _, err := c.GetRole(ctx, &adminpb.GetRoleRequest{Name: "roles/viewer"}); status.Code(err) != codes.Unimplemented {
		t.Errorf("GetRole roles/viewer = %v, want Unimplemented", err)
	}
}

// The limit of 300 roles a project.
func TestCustomRoleLimit(t *testing.T) {
	reg := New(newRegistry(t).st)
	if _, err := reg.Create(Project{ProjectID: "limit-project"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxRolesPerParent; i++ {
		if _, err := reg.CreateRole("projects/limit-project", "r"+strings.Repeat("x", 2)+itoa(i), &adminpb.Role{}); err != nil {
			t.Fatalf("role %d: %v", i, err)
		}
	}
	if _, err := reg.CreateRole("projects/limit-project", "onetoomany", &adminpb.Role{}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("role 301 = %v, want FailedPrecondition", err)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

// The REST surface through the official discovery client, as gcloud and
// Terraform call it, and queryTestablePermissions.
func TestCustomRolesOverREST(t *testing.T) {
	reg, srv, _ := startIAMAdmin(t)
	ctx := context.Background()
	if _, err := reg.Create(Project{ProjectID: "rest-roles"}); err != nil {
		t.Fatal(err)
	}
	svc, err := iamv1.NewService(ctx, option.WithEndpoint("http://"+srv.Addr()+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	role, err := svc.Projects.Roles.Create("projects/rest-roles", &iamv1.CreateRoleRequest{RoleId: "restRole",
		Role: &iamv1.Role{Title: "REST", IncludedPermissions: []string{"redis.clusters.connect"}, Stage: "BETA"}}).Do()
	if err != nil || role.Stage != "BETA" || role.Etag == "" {
		t.Fatalf("Create = %+v, %v", role, err)
	}
	got, err := svc.Projects.Roles.Patch(role.Name, &iamv1.Role{Description: "d", Etag: role.Etag}).UpdateMask("description").Do()
	if err != nil || got.Description != "d" || got.Title != "REST" {
		t.Fatalf("Patch = %+v, %v", got, err)
	}
	if _, err := svc.Projects.Roles.Patch(role.Name, &iamv1.Role{Description: "x", Etag: role.Etag}).UpdateMask("description").Do(); code(err) != 409 {
		t.Errorf("a stale etag over REST = %v, want 409", err)
	}
	list, err := svc.Projects.Roles.List("projects/rest-roles").View("FULL").Do()
	if err != nil || len(list.Roles) != 1 || len(list.Roles[0].IncludedPermissions) != 1 {
		t.Errorf("List FULL = %+v, %v", list, err)
	}
	if d, err := svc.Projects.Roles.Delete(role.Name).Do(); err != nil || !d.Deleted {
		t.Errorf("Delete = %+v, %v", d, err)
	}
	if u, err := svc.Projects.Roles.Undelete(role.Name, &iamv1.UndeleteRoleRequest{}).Do(); err != nil || u.Deleted {
		t.Errorf("Undelete = %+v, %v", u, err)
	}
	if _, err := svc.Projects.Roles.Create("projects/rest-roles", &iamv1.CreateRoleRequest{RoleId: "bad",
		Role: &iamv1.Role{IncludedPermissions: []string{"no.such.permission"}}}).Do(); code(err) != 400 {
		t.Errorf("an invalid permission over REST = %v, want 400", err)
	}
	q, err := svc.Permissions.QueryTestablePermissions(&iamv1.QueryTestablePermissionsRequest{
		FullResourceName: "//cloudresourcemanager.googleapis.com/projects/rest-roles", PageSize: 1000}).Do()
	if err != nil || len(q.Permissions) != 1000 || q.NextPageToken == "" {
		t.Fatalf("QueryTestablePermissions = %d, %q, %v", len(q.Permissions), q.NextPageToken, err)
	}
	if _, err := svc.Roles.QueryGrantableRoles(&iamv1.QueryGrantableRolesRequest{
		FullResourceName: "//cloudresourcemanager.googleapis.com/projects/rest-roles"}).Do(); code(err) != 501 {
		t.Errorf("QueryGrantableRoles = %v, want 501", err)
	}
}
