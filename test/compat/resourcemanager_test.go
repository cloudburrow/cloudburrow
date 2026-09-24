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

// covers: google.cloud.resourcemanager.v3.Projects/CreateProject, google.cloud.resourcemanager.v3.Projects/GetProject, google.cloud.resourcemanager.v3.Projects/SearchProjects, google.cloud.resourcemanager.v3.Projects/UpdateProject, google.cloud.resourcemanager.v3.Projects/DeleteProject
//
// TestResourceManagerV3Projects.
//
// The v3 Projects API (#298) through cloud.google.com/go/resourcemanager/
// apiv3 against the CI instance: create, get, search, update labels and
// delete, with the console's project list agreeing in both directions, and
// the IAM and folder methods UNIMPLEMENTED.
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
	if contains(consoleProjects(t, console, h.Project()), id) {
		t.Errorf("the deleted project %s is still in the console", id)
	}

	// Not served, and said so.
	if _, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: "projects/" + cid}); status.Code(err) != codes.Unimplemented {
		t.Errorf("GetIamPolicy = %v, want Unimplemented", err)
	}
	if _, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: "projects/" + cid}); status.Code(err) != codes.Unimplemented {
		t.Errorf("SetIamPolicy = %v, want Unimplemented", err)
	}
	fc, err := resourcemanager.NewFoldersClient(ctx, rmOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	defer fc.Close()
	if _, err := fc.GetFolder(ctx, &rmpb.GetFolderRequest{Name: "folders/1"}); status.Code(err) != codes.Unimplemented {
		t.Errorf("GetFolder = %v, want Unimplemented", err)
	}
	fit := fc.ListFolders(ctx, &rmpb.ListFoldersRequest{Parent: "organizations/1"})
	if _, err := fit.Next(); status.Code(err) != codes.Unimplemented {
		t.Errorf("ListFolders = %v, want Unimplemented", err)
	}
}
