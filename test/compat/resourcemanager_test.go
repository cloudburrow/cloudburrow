//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// EnvResourceManager is the Resource Manager v3 endpoint.
const EnvResourceManager = "CLOUDBURROW_TEST_RESOURCEMANAGER"

func rmOptions(h *Harness) []option.ClientOption {
	return []option.ClientOption{option.WithEndpoint(h.Endpoint(EnvResourceManager)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
}

// consoleProjects lists the console's project registry, as the selector does.
func consoleProjects(t *testing.T, addr, project string) []string {
	t.Helper()
	code, body := consoleDo(t, addr, http.MethodGet, "/api/resources/projects?project="+project, "")
	if code != http.StatusOK {
		t.Fatalf("console projects = %d: %s", code, body)
	}
	var l struct{ Items []struct{ Name string } }
	_ = json.Unmarshal([]byte(body), &l)
	var out []string
	for _, i := range l.Items {
		out = append(out, i.Name)
	}
	return out
}

// covers: google.cloud.resourcemanager.v3.Projects/CreateProject, google.cloud.resourcemanager.v3.Projects/GetProject, google.cloud.resourcemanager.v3.Projects/SearchProjects, google.cloud.resourcemanager.v3.Projects/UpdateProject, google.cloud.resourcemanager.v3.Projects/DeleteProject, google.cloud.resourcemanager.v3.Projects/UndeleteProject
//
// TestResourceManagerV3Projects.
//
// The v3 Projects API (#298) through cloud.google.com/go/resourcemanager/
// apiv3 against the CI instance: create, get, search, update labels,
// delete and undelete (#1216), with the console's project list agreeing in
// both directions, and the console's Undelete project.
func TestResourceManagerV3Projects(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	c, err := resourcemanager.NewProjectsClient(ctx, rmOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	console := consoleAddr(t, h)
	id := "rm-" + strings.TrimPrefix(h.Project(), "cb-test-")
	if len(id) > 30 {
		id = id[:30]
	}
	id = strings.TrimRight(id, "-")

	op, err := c.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: id, DisplayName: "From the API"}})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if p, err := op.Wait(ctx); err != nil || p.GetProjectId() != id {
		t.Fatalf("CreateProject result = %v, %v", p, err)
	}
	t.Cleanup(func() {
		_, _ = c.DeleteProject(context.Background(), &rmpb.DeleteProjectRequest{Name: "projects/" + id})
	})

	if p, err := c.GetProject(ctx, &rmpb.GetProjectRequest{Name: "projects/" + id}); err != nil || p.GetDisplayName() != "From the API" {
		t.Errorf("GetProject = %v, %v", p, err)
	}
	if !contains(consoleProjects(t, console, h.Project()), id) {
		t.Errorf("the API-created project %s is not in the console's project list", id)
	}

	// Update labels, then find it by label.
	uop, err := c.UpdateProject(ctx, &rmpb.UpdateProjectRequest{
		Project:    &rmpb.Project{Name: "projects/" + id, Labels: map[string]string{"owner": "compat"}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}})
	if err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	if p, err := uop.Wait(ctx); err != nil || p.GetLabels()["owner"] != "compat" {
		t.Errorf("UpdateProject result = %v, %v", p, err)
	}
	it := c.SearchProjects(ctx, &rmpb.SearchProjectsRequest{Query: "labels.owner:compat id:" + id})
	if p, err := it.Next(); err != nil || p.GetProjectId() != id {
		t.Errorf("SearchProjects by label = %v, %v", p, err)
	}

	// The reverse: a console-created project is listed by the API.
	cid := id[:len(id)-1] + "c"
	code, body := consoleDo(t, console, http.MethodPost, "/api/resources/projects?project="+h.Project(),
		fmt.Sprintf(`{"projectId":%q}`, cid))
	if code != http.StatusOK {
		t.Fatalf("console create project = %d: %s", code, body)
	}
	t.Cleanup(func() {
		_, _ = c.DeleteProject(context.Background(), &rmpb.DeleteProjectRequest{Name: "projects/" + cid})
	})
	found := false
	all := c.SearchProjects(ctx, &rmpb.SearchProjectsRequest{})
	for {
		p, err := all.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("SearchProjects: %v", err)
		}
		found = found || p.GetProjectId() == cid
	}
	if !found {
		t.Errorf("the console-created project %s is not listed by the API", cid)
	}

	// Delete, through the API; gone from the console too.
	dop, err := c.DeleteProject(ctx, &rmpb.DeleteProjectRequest{Name: "projects/" + id})
	if err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if p, err := dop.Wait(ctx); err != nil || p.GetState() != rmpb.Project_DELETE_REQUESTED {
		t.Errorf("DeleteProject result = %v, %v", p, err)
	}
	// Kept in DELETE_REQUESTED for 30 days, as Google keeps it (#1216):
	// GetProject reads it, and the console lists it in that state.
	if p, err := c.GetProject(ctx, &rmpb.GetProjectRequest{Name: "projects/" + id}); err != nil || p.GetState() != rmpb.Project_DELETE_REQUESTED {
		t.Errorf("GetProject after delete = %v, %v; want DELETE_REQUESTED", p, err)
	}
	if st := consoleProjectState(t, console, h.Project(), id); st != "DELETE_REQUESTED" {
		t.Errorf("the console lists the deleted project %s as %q, want DELETE_REQUESTED", id, st)
	}
	if _, err := c.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: id}}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("CreateProject reusing a deleted project's ID = %v, want AlreadyExists", err)
	}
	if _, err := c.UndeleteProject(ctx, &rmpb.UndeleteProjectRequest{Name: "projects/" + cid}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("UndeleteProject of an active project = %v, want FailedPrecondition", err)
	}
	undo, err := c.UndeleteProject(ctx, &rmpb.UndeleteProjectRequest{Name: "projects/" + id})
	if err != nil {
		t.Fatalf("UndeleteProject: %v", err)
	}
	if p, err := undo.Wait(ctx); err != nil || p.GetState() != rmpb.Project_ACTIVE {
		t.Errorf("UndeleteProject result = %v, %v; want ACTIVE", p, err)
	}
	if st := consoleProjectState(t, console, h.Project(), id); st != "ACTIVE" {
		t.Errorf("the console lists the undeleted project %s as %q, want ACTIVE", id, st)
	}

	// From the console: Delete, then Undelete project on its page.
	if code, body := consoleDo(t, console, http.MethodDelete, "/api/resources/projects?name="+cid+"&project="+h.Project(), ""); code != http.StatusOK {
		t.Fatalf("console delete project = %d: %s", code, body)
	}
	if p, err := c.GetProject(ctx, &rmpb.GetProjectRequest{Name: "projects/" + cid}); err != nil || p.GetState() != rmpb.Project_DELETE_REQUESTED {
		t.Errorf("GetProject after the console's delete = %v, %v; want DELETE_REQUESTED", p, err)
	}
	if code, body := consoleDo(t, console, http.MethodPost, "/api/actions/projects?project="+h.Project(),
		fmt.Sprintf(`{"Path":[%q],"Action":"undelete"}`, cid)); code != http.StatusOK {
		t.Fatalf("console Undelete project = %d: %s", code, body)
	}
	if p, err := c.GetProject(ctx, &rmpb.GetProjectRequest{Name: "projects/" + cid}); err != nil || p.GetState() != rmpb.Project_ACTIVE {
		t.Errorf("GetProject after the console's undelete = %v, %v; want ACTIVE", p, err)
	}
}

// consoleProjectState is a project's status in the console's project list,
// or "" when it is not listed.
func consoleProjectState(t *testing.T, addr, project, id string) string {
	t.Helper()
	code, body := consoleDo(t, addr, http.MethodGet, "/api/resources/projects?project="+project, "")
	if code != http.StatusOK {
		t.Fatalf("console projects = %d: %s", code, body)
	}
	var l struct {
		Items []struct{ Name, Status string }
	}
	_ = json.Unmarshal([]byte(body), &l)
	for _, i := range l.Items {
		if i.Name == id {
			return i.Status
		}
	}
	return ""
}

// covers: google.cloud.resourcemanager.v3.Projects/GetIamPolicy (unimplemented), google.cloud.resourcemanager.v3.Projects/SetIamPolicy (unimplemented), google.cloud.resourcemanager.v3.Projects/TestIamPermissions (unimplemented)
//
// TestResourceManagerV3RefusesIAM (#1216): a project's IAM methods stay
// UNIMPLEMENTED under ADR-0006, which stores policies only on the resources
// it names, and each says why. MoveProject, once refused here, is served
// since #1294 (TestResourceManagerV3Folders).
func TestResourceManagerV3RefusesIAM(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	c, err := resourcemanager.NewProjectsClient(ctx, rmOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	name := "projects/" + h.Project()
	for what, err := range map[string]error{
		"GetIamPolicy": func() error { _, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: name}); return err }(),
		"SetIamPolicy": func() error {
			_, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: name, Policy: &iampb.Policy{}})
			return err
		}(),
		"TestIamPermissions": func() error {
			_, err := c.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: name, Permissions: []string{"resourcemanager.projects.get"}})
			return err
		}(),
	} {
		if status.Code(err) != codes.Unimplemented || !strings.Contains(status.Convert(err).Message(), "ADR-0006") {
			t.Errorf("%s = %v, want UNIMPLEMENTED citing ADR-0006", what, err)
		}
	}
}

// covers: google.cloud.resourcemanager.v3.Projects/ListProjects
//
// TestResourceManagerV3ListProjects.
//
// ListProjects lists a parent's direct children. Through the official
// client, an empty parent is INVALID_ARGUMENT pointing at SearchProjects,
// as in Google's API; a folder or organization that does not exist is
// NOT_FOUND (#1294) rather than an empty list that would pass for "no
// children"; any other parent is INVALID_ARGUMENT. A top-level project is
// not listed under the local organization, and SearchProjects finds it.
func TestResourceManagerV3ListProjects(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	c, err := resourcemanager.NewProjectsClient(ctx, rmOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	id := v1ID(h, "rl-")
	op, err := c.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: id}})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = c.DeleteProject(context.Background(), &rmpb.DeleteProjectRequest{Name: "projects/" + id})
	})

	for parent, want := range map[string]codes.Code{
		"":                  codes.InvalidArgument,
		"folders/123":       codes.NotFound,
		"organizations/456": codes.NotFound,
		"projects/" + id:    codes.InvalidArgument,
	} {
		p, err := c.ListProjects(ctx, &rmpb.ListProjectsRequest{Parent: parent}).Next()
		if status.Code(err) != want {
			t.Errorf("ListProjects(parent %q) = %v, %v; want %v", parent, p, err, want)
		}
		if parent == "" && !strings.Contains(err.Error(), "SearchProjects") {
			t.Errorf("ListProjects with no parent does not point at SearchProjects: %v", err)
		}
	}
	for lit := c.ListProjects(ctx, &rmpb.ListProjectsRequest{Parent: localOrg}); ; {
		p, err := lit.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("ListProjects(%s): %v", localOrg, err)
		}
		if p.GetProjectId() == id {
			t.Errorf("ListProjects(%s) lists the top-level project %s", localOrg, id)
		}
	}
	it := c.SearchProjects(ctx, &rmpb.SearchProjectsRequest{Query: "id:" + id})
	if p, err := it.Next(); err != nil || p.GetProjectId() != id {
		t.Errorf("SearchProjects for the project ListProjects cannot list = %v, %v", p, err)
	}
}
