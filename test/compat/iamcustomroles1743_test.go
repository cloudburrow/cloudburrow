//go:build compat

package compat

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	firestoreadminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	admin "cloud.google.com/go/iam/admin/apiv1"
	"cloud.google.com/go/iam/admin/apiv1/adminpb"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	iamv1 "google.golang.org/api/iam/v1"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// localOrgRoles is the local organization's custom roles parent.
const localOrgRoles = "organizations/100000000001"

// iamAdmin is the official IAM admin client (cloud.google.com/go/iam/admin/apiv1)
// at the Resource Manager endpoint, which serves the IAM API's roles (#1743).
func iamAdmin(t *testing.T, h *Harness) *admin.IamClient {
	t.Helper()
	c, err := admin.NewIamClient(h.Context(), rmOptionsAt(h.Endpoint(EnvResourceManager))...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// covers: google.iam.admin.v1.IAM/CreateRole, google.iam.admin.v1.IAM/GetRole, google.iam.admin.v1.IAM/ListRoles, google.iam.admin.v1.IAM/UpdateRole, google.iam.admin.v1.IAM/DeleteRole, google.iam.admin.v1.IAM/UndeleteRole, google.iam.admin.v1.IAM/QueryTestablePermissions
//
// TestIAMCustomRoles (#1743), with the official
// cloud.google.com/go/iam/admin/apiv1 client over gRPC and
// google.golang.org/api/iam/v1 over REST: a project role and an
// organization role are created (ALPHA by default, permissions sorted and
// deduplicated), read, listed (BASIC without permissions, FULL with),
// updated through a mask (a stale etag ABORTED), deleted (hidden from the
// list but with show_deleted, its ID not reusable, not updatable) and
// undeleted. Google's refusals: a role ID outside 3 to 64 letters, digits,
// underscores and periods, a title over 100 bytes, a description over 300,
// an unknown or NOT_SUPPORTED permission, an organization permission in a
// project role ("Permission P is not valid."), a taken ID (ALREADY_EXISTS).
// QueryTestablePermissions pages the catalog, with support levels.
func TestIAMCustomRoles(t *testing.T) {
	h := New(t)
	c := iamAdmin(t, h)
	project := iamProject(t, h, "iamr-")
	ctx := h.Context()
	parent := "projects/" + project

	role, err := c.CreateRole(ctx, &adminpb.CreateRoleRequest{Parent: parent, RoleId: "compat_reader.v1", Role: &adminpb.Role{
		Title: "Compat reader", Description: "Reads entities",
		IncludedPermissions: []string{"datastore.entities.list", "datastore.entities.get", "datastore.entities.get"}}})
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	if role.GetName() != parent+"/roles/compat_reader.v1" || role.GetStage() != adminpb.Role_ALPHA || len(role.GetEtag()) == 0 ||
		strings.Join(role.GetIncludedPermissions(), ",") != "datastore.entities.get,datastore.entities.list" {
		t.Errorf("CreateRole = %v", role)
	}
	orgID := fmt.Sprintf("compatOrg%d", time.Now().UnixNano()%1e8)
	org, err := c.CreateRole(ctx, &adminpb.CreateRoleRequest{Parent: localOrgRoles, RoleId: orgID, Role: &adminpb.Role{
		Title: "Org viewer", Stage: adminpb.Role_GA, IncludedPermissions: []string{"resourcemanager.organizations.get"}}})
	if err != nil {
		t.Fatalf("CreateRole on the organization: %v", err)
	}
	t.Cleanup(func() { _, _ = c.DeleteRole(context.Background(), &adminpb.DeleteRoleRequest{Name: org.GetName()}) })

	for _, tc := range []struct {
		id    string
		role  *adminpb.Role
		code  codes.Code
		about string
	}{
		{"compat_reader.v1", &adminpb.Role{}, codes.AlreadyExists, "a taken ID"},
		{"ab", &adminpb.Role{}, codes.InvalidArgument, "a 2-character ID"},
		{strings.Repeat("a", 65), &adminpb.Role{}, codes.InvalidArgument, "a 65-character ID"},
		{"has-dash", &adminpb.Role{}, codes.InvalidArgument, "a dash in the ID"},
		{"longTitle", &adminpb.Role{Title: strings.Repeat("t", 101)}, codes.InvalidArgument, "a 101-byte title"},
		{"longDesc", &adminpb.Role{Description: strings.Repeat("d", 301)}, codes.InvalidArgument, "a 301-byte description"},
		{"unknownPerm", &adminpb.Role{IncludedPermissions: []string{"datastore.entities.frobnicate"}}, codes.InvalidArgument, "an unknown permission"},
		{"notSupported", &adminpb.Role{IncludedPermissions: []string{"appengine.runtimes.actAsAdmin"}}, codes.InvalidArgument, "a NOT_SUPPORTED permission"},
		{"orgOnly", &adminpb.Role{IncludedPermissions: []string{"resourcemanager.organizations.get"}}, codes.InvalidArgument, "an organization permission in a project role"},
	} {
		_, err := c.CreateRole(ctx, &adminpb.CreateRoleRequest{Parent: parent, RoleId: tc.id, Role: tc.role})
		if status.Code(err) != tc.code {
			t.Errorf("%s: %v, want %v", tc.about, err, tc.code)
		}
		if tc.about == "an unknown permission" && !strings.Contains(err.Error(), "Permission datastore.entities.frobnicate is not valid.") {
			t.Errorf("%s: message %v", tc.about, err)
		}
	}

	got, err := c.GetRole(ctx, &adminpb.GetRoleRequest{Name: role.GetName()})
	if err != nil || got.GetTitle() != "Compat reader" || len(got.GetIncludedPermissions()) != 2 {
		t.Errorf("GetRole = %v, %v", got, err)
	}
	up, err := c.UpdateRole(ctx, &adminpb.UpdateRoleRequest{Name: role.GetName(), Role: &adminpb.Role{Title: "Renamed", Stage: adminpb.Role_BETA, Etag: role.GetEtag()},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"title", "stage"}}})
	if err != nil || up.GetTitle() != "Renamed" || up.GetStage() != adminpb.Role_BETA || up.GetDescription() != "Reads entities" {
		t.Fatalf("UpdateRole = %v, %v", up, err)
	}
	if _, err := c.UpdateRole(ctx, &adminpb.UpdateRoleRequest{Name: role.GetName(), Role: &adminpb.Role{Title: "x", Etag: role.GetEtag()},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"title"}}}); status.Code(err) != codes.Aborted {
		t.Errorf("UpdateRole with a stale etag = %v, want Aborted", err)
	}

	list := func(view adminpb.RoleView, showDeleted bool) []*adminpb.Role {
		resp, err := c.ListRoles(ctx, &adminpb.ListRolesRequest{Parent: parent, View: view, ShowDeleted: showDeleted})
		if err != nil {
			t.Fatalf("ListRoles: %v", err)
		}
		return resp.GetRoles()
	}
	if l := list(adminpb.RoleView_BASIC, false); len(l) != 1 || len(l[0].GetIncludedPermissions()) != 0 {
		t.Errorf("ListRoles BASIC = %v", l)
	}
	if l := list(adminpb.RoleView_FULL, false); len(l) != 1 || len(l[0].GetIncludedPermissions()) != 2 {
		t.Errorf("ListRoles FULL = %v", l)
	}

	del, err := c.DeleteRole(ctx, &adminpb.DeleteRoleRequest{Name: role.GetName(), Etag: up.GetEtag()})
	if err != nil || !del.GetDeleted() {
		t.Fatalf("DeleteRole = %v, %v", del, err)
	}
	if l := list(adminpb.RoleView_BASIC, false); len(l) != 0 {
		t.Errorf("ListRoles after the delete = %v", l)
	}
	if l := list(adminpb.RoleView_BASIC, true); len(l) != 1 || !l[0].GetDeleted() {
		t.Errorf("ListRoles show_deleted = %v", l)
	}
	if _, err := c.CreateRole(ctx, &adminpb.CreateRoleRequest{Parent: parent, RoleId: "compat_reader.v1", Role: &adminpb.Role{}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("reusing a deleted role's ID = %v, want FailedPrecondition", err)
	}
	if _, err := c.UpdateRole(ctx, &adminpb.UpdateRoleRequest{Name: role.GetName(), Role: &adminpb.Role{Title: "x"}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("updating a deleted role = %v, want FailedPrecondition", err)
	}
	if u, err := c.UndeleteRole(ctx, &adminpb.UndeleteRoleRequest{Name: role.GetName()}); err != nil || u.GetDeleted() {
		t.Errorf("UndeleteRole = %v, %v", u, err)
	}

	// REST, as gcloud and Terraform call it.
	svc, err := iamv1.NewService(ctx, option.WithEndpoint("http://"+strings.TrimPrefix(h.Endpoint(EnvResourceManager), "http://")+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	r, err := svc.Projects.Roles.Get(role.GetName()).Context(ctx).Do()
	if err != nil || r.Title != "Renamed" || r.Stage != "BETA" {
		t.Errorf("REST roles.get = %+v, %v", r, err)
	}
	if r, err := svc.Organizations.Roles.Get(org.GetName()).Context(ctx).Do(); err != nil || r.Stage != "GA" {
		t.Errorf("REST organizations.roles.get = %+v, %v", r, err)
	}
	q, err := c.QueryTestablePermissions(ctx, &adminpb.QueryTestablePermissionsRequest{
		FullResourceName: "//cloudresourcemanager.googleapis.com/projects/" + project, PageSize: 1000})
	if err != nil || len(q.GetPermissions()) != 1000 || q.GetNextPageToken() == "" {
		t.Fatalf("QueryTestablePermissions = %d, %v", len(q.GetPermissions()), err)
	}
	levels := map[adminpb.Permission_CustomRolesSupportLevel]bool{}
	token := ""
	for pages := 0; pages < 30; pages++ {
		resp, err := svc.Permissions.QueryTestablePermissions(&iamv1.QueryTestablePermissionsRequest{
			FullResourceName: "//cloudresourcemanager.googleapis.com/projects/" + project, PageSize: 1000, PageToken: token}).Context(ctx).Do()
		if err != nil {
			t.Fatalf("REST queryTestablePermissions: %v", err)
		}
		for _, p := range resp.Permissions {
			levels[adminpb.Permission_CustomRolesSupportLevel(adminpb.Permission_CustomRolesSupportLevel_value[p.CustomRolesSupportLevel])] = true
			if p.Name == "resourcemanager.organizations.get" {
				t.Errorf("a project's testable permissions include %s", p.Name)
			}
		}
		if token = resp.NextPageToken; token == "" {
			break
		}
	}
	if !levels[adminpb.Permission_SUPPORTED] || !levels[adminpb.Permission_TESTING] || !levels[adminpb.Permission_NOT_SUPPORTED] {
		t.Errorf("support levels seen = %v", levels)
	}
}

// TestGcloudIAMCustomRoles (#1743): gcloud iam roles create (with its
// queryTestablePermissions check), describe, list, update
// --add-permissions, delete, list --show-deleted and undelete, through
// gcloud-setup's iam override alone; an invalid permission is refused.
func TestGcloudIAMCustomRoles(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager)
	project := iamProject(t, h, "iamg-")
	g := newGcloudSession(t, h)
	g.must("iam", "roles", "create", "gcloudRole", "--project", project, "--title", "gcloud role",
		"--permissions", "datastore.entities.get,datastore.entities.list", "--stage", "BETA")
	if out := g.must("iam", "roles", "describe", "gcloudRole", "--project", project, "--format=value(stage,includedPermissions)"); !strings.Contains(out, "BETA") ||
		!strings.Contains(out, "datastore.entities.list") {
		t.Errorf("describe:\n%s", out)
	}
	g.must("iam", "roles", "update", "gcloudRole", "--project", project, "--add-permissions", "datastore.entities.create", "--quiet")
	if out := g.must("iam", "roles", "describe", "gcloudRole", "--project", project, "--format=value(includedPermissions)"); !strings.Contains(out, "datastore.entities.create") {
		t.Errorf("after update --add-permissions:\n%s", out)
	}
	if out := g.must("iam", "roles", "list", "--project", project, "--format=value(name)"); !strings.Contains(out, "projects/"+project+"/roles/gcloudRole") {
		t.Errorf("list:\n%s", out)
	}
	g.must("iam", "roles", "delete", "gcloudRole", "--project", project)
	if out := g.must("iam", "roles", "list", "--project", project, "--format=value(name)"); strings.Contains(out, "gcloudRole") {
		t.Errorf("list after delete:\n%s", out)
	}
	if out := g.must("iam", "roles", "list", "--project", project, "--show-deleted", "--format=value(name,deleted)"); !strings.Contains(out, "gcloudRole") {
		t.Errorf("list --show-deleted:\n%s", out)
	}
	g.must("iam", "roles", "undelete", "gcloudRole", "--project", project)
	if out, err := g.run(nil, "iam", "roles", "create", "badRole", "--project", project, "--permissions", "no.such.permission", "--quiet"); err == nil ||
		!strings.Contains(out, "Permission no.such.permission is not valid") {
		t.Errorf("an invalid permission = %v:\n%s", err, out)
	}
}

// TestTerraformIAMCustomRole (#1743): google_project_iam_custom_role and
// google_organization_iam_custom_role apply, plan clean, change in place,
// plan clean and destroy (Terraform's destroy deletes the role, as on
// Google), and google_project_iam_member grants the project role.
func TestTerraformIAMCustomRole(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvResourceManager)
	c := iamAdmin(t, h)
	project := iamProject(t, h, "iamt-")
	m := newTFModule(t)
	orgID := fmt.Sprintf("tfOrg%d", time.Now().UnixNano()%1e8)
	module := func(title, perms string) string {
		return fmt.Sprintf(`
resource "google_project_iam_custom_role" "r" {
  project     = %q
  role_id     = "tfRole"
  title       = %q
  permissions = [%s]
  stage       = "GA"
}

resource "google_organization_iam_custom_role" "o" {
  org_id      = "100000000001"
  role_id     = %q
  title       = "tf org"
  permissions = ["resourcemanager.organizations.get"]
}

resource "google_project_iam_member" "m" {
  project = %q
  role    = google_project_iam_custom_role.r.name
  member  = "user:tf-custom@example.com"
}
`, project, title, perms, orgID, project)
	}
	m.write(module("tf role", `"datastore.entities.get"`))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	name := "projects/" + project + "/roles/tfRole"
	r, err := c.GetRole(h.Context(), &adminpb.GetRoleRequest{Name: name})
	if err != nil || r.GetTitle() != "tf role" || r.GetStage() != adminpb.Role_GA {
		t.Fatalf("after apply GetRole = %v, %v", r, err)
	}
	m.planClean()
	m.write(module("tf role 2", `"datastore.entities.get", "datastore.entities.list"`))
	m.apply()
	if r, err := c.GetRole(h.Context(), &adminpb.GetRoleRequest{Name: name}); err != nil || r.GetTitle() != "tf role 2" || len(r.GetIncludedPermissions()) != 2 {
		t.Errorf("after the change GetRole = %v, %v", r, err)
	}
	m.planClean()
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if r, err := c.GetRole(h.Context(), &adminpb.GetRoleRequest{Name: name}); err != nil || !r.GetDeleted() {
		t.Errorf("after destroy GetRole = %v, %v; want deleted", r, err)
	}
}

// TestFirestoreMongoDBIAMCustomRole (#1743): a custom role holding
// datastore.entities.get and list lets a token connection read and not
// write; DISABLED, it grants nothing; and a custom role is evaluated as
// Google's IAM page lists each command's permissions.
func TestFirestoreMongoDBIAMCustomRole(t *testing.T) {
	h := New(t)
	ep, ca := mongoEndpoint(h)
	h.Endpoint(EnvFirestore)
	c := iamAdmin(t, h)
	project := iamProject(t, h, "fm-cr-")
	ctx := h.Context()
	role, err := c.CreateRole(ctx, &adminpb.CreateRoleRequest{Parent: "projects/" + project, RoleId: "mongoReader", Role: &adminpb.Role{
		IncludedPermissions: []string{"datastore.databases.get", "datastore.entities.get", "datastore.entities.list"}}})
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	reader := newIAMPrincipal(t, h, project, "fm-custom", role.GetName())
	writer := newIAMPrincipal(t, h, project, "fm-cwrite", "roles/datastore.user")

	fa := firestoreAdmin(t, h)
	id := fmt.Sprintf("mongo-cr-%d", time.Now().UnixNano()%1e8)
	op, err := fa.CreateDatabase(ctx, &firestoreadminpb.CreateDatabaseRequest{Parent: "projects/" + project, DatabaseId: id,
		Database: &firestoreadminpb.Database{LocationId: "nam5", Type: firestoreadminpb.Database_FIRESTORE_NATIVE, DatabaseEdition: firestoreadminpb.Database_ENTERPRISE}})
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	db, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	t.Cleanup(func() {
		if op, err := fa.DeleteDatabase(context.Background(), &firestoreadminpb.DeleteDatabaseRequest{Name: db.Name}); err == nil {
			_, _ = op.Wait(context.Background())
		}
	})
	items := func(token string) *mongo.Collection {
		return mongoConnect(t, mongoTokenURI(token, ep, id, ca)).Database(id).Collection("items")
	}
	if _, err := items(writer.token).InsertOne(ctx, bson.D{{Key: "_id", Value: "a"}}); err != nil {
		t.Fatalf("datastore.user's InsertOne: %v", err)
	}
	rc := items(reader.token)
	// The host pushes the roles every few seconds.
	deadline := time.Now().Add(20 * time.Second)
	for {
		err := rc.FindOne(ctx, bson.D{{Key: "_id", Value: "a"}}).Err()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the custom role's FindOne: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if _, err := rc.InsertOne(ctx, bson.D{{Key: "_id", Value: "b"}}); mongoCode(err) != 13 {
		t.Errorf("the custom role's InsertOne = %v: want Unauthorized (13)", err)
	}
	if _, err := c.UpdateRole(ctx, &adminpb.UpdateRoleRequest{Name: role.GetName(), Role: &adminpb.Role{Stage: adminpb.Role_DISABLED},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"stage"}}}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(20 * time.Second)
	for {
		err := rc.FindOne(ctx, bson.D{}).Err()
		if mongoCode(err) == 13 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("20 s after the role was DISABLED, FindOne = %v: want Unauthorized (13)", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
