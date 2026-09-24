package resourcemanager

import (
	"context"
	"net/http"
	"testing"
	"time"

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

	"github.com/cloudburrow/cloudburrow/internal/store"
)

func startAPI(t *testing.T) (*Registry, *Server, *resourcemanager.ProjectsClient) {
	t.Helper()
	reg := New(store.NewMemory())
	srv := NewServer("127.0.0.1:0", reg)
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Stop(ctx)
	})
	c, err := resourcemanager.NewProjectsClient(context.Background(),
		option.WithEndpoint(srv.Addr()), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return reg, srv, c
}

// The lifecycle through the official v3 client, against the registry the
// console reads: what the API creates the registry holds, and the reverse.
func TestProjectsLifecycleThroughTheOfficialClient(t *testing.T) {
	reg, _, c := startAPI(t)
	ctx := context.Background()

	op, err := c.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{
		ProjectId: "api-made-1", DisplayName: "API made", Labels: map[string]string{"team": "a"}}})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	p, err := op.Wait(ctx)
	if err != nil || p.GetName() != "projects/api-made-1" || p.GetState() != rmpb.Project_ACTIVE {
		t.Fatalf("created %v, %v", p, err)
	}
	if got, err := reg.Get("api-made-1"); err != nil || got.DisplayName != "API made" {
		t.Errorf("the registry does not hold the API-created project: %+v %v", got, err)
	}

	// Made through the registry, as the console does, and found by the API.
	if _, err := reg.Create(Project{ProjectID: "console-made-1"}); err != nil {
		t.Fatal(err)
	}
	if g, err := c.GetProject(ctx, &rmpb.GetProjectRequest{Name: "projects/console-made-1"}); err != nil || g.GetProjectId() != "console-made-1" {
		t.Errorf("GetProject(console-made-1) = %v, %v", g, err)
	}
	var ids []string
	it := c.SearchProjects(ctx, &rmpb.SearchProjectsRequest{})
	for {
		sp, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("SearchProjects: %v", err)
		}
		ids = append(ids, sp.GetProjectId())
	}
	if len(ids) != 2 || ids[0] != "api-made-1" || ids[1] != "console-made-1" {
		t.Errorf("SearchProjects = %v", ids)
	}
	it = c.SearchProjects(ctx, &rmpb.SearchProjectsRequest{Query: "labels.team:a"})
	if sp, err := it.Next(); err != nil || sp.GetProjectId() != "api-made-1" {
		t.Errorf("SearchProjects(labels.team:a) = %v, %v", sp, err)
	} else if _, err := it.Next(); err != iterator.Done {
		t.Errorf("SearchProjects(labels.team:a) returned more than one project")
	}

	uop, err := c.UpdateProject(ctx, &rmpb.UpdateProjectRequest{
		Project:    &rmpb.Project{Name: "projects/api-made-1", Labels: map[string]string{"team": "b", "env": "dev"}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}})
	if err != nil {
		t.Fatal(err)
	}
	if up, err := uop.Wait(ctx); err != nil || up.GetLabels()["team"] != "b" || up.GetDisplayName() != "API made" {
		t.Errorf("UpdateProject(labels) = %v, %v; want labels changed and the display name kept", up, err)
	}
	if _, err := c.UpdateProject(ctx, &rmpb.UpdateProjectRequest{
		Project: &rmpb.Project{Name: "projects/api-made-1"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"parent"}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("UpdateProject(parent) = %v, want InvalidArgument", err)
	}

	dop, err := c.DeleteProject(ctx, &rmpb.DeleteProjectRequest{Name: "projects/api-made-1"})
	if err != nil {
		t.Fatal(err)
	}
	if dp, err := dop.Wait(ctx); err != nil || dp.GetState() != rmpb.Project_DELETE_REQUESTED {
		t.Errorf("DeleteProject = %v, %v", dp, err)
	}
	if _, err := reg.Get("api-made-1"); err == nil {
		t.Error("the deleted project is still in the registry")
	}
	if _, err := c.GetProject(ctx, &rmpb.GetProjectRequest{Name: "projects/api-made-1"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetProject after delete = %v, want NotFound", err)
	}
}

func TestProjectsRefusesWhatItDoesNotServe(t *testing.T) {
	_, _, c := startAPI(t)
	ctx := context.Background()
	if _, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: "projects/x"}); status.Code(err) != codes.Unimplemented {
		t.Errorf("GetIamPolicy = %v", err)
	}
	if _, err := c.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: "projects/x"}); status.Code(err) != codes.Unimplemented {
		t.Errorf("SetIamPolicy = %v", err)
	}
	if _, err := c.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: "in-folder-1", Parent: "folders/123"}}); status.Code(err) != codes.Unimplemented {
		t.Errorf("CreateProject under a folder = %v", err)
	}
	if _, err := c.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: "BAD"}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("CreateProject(BAD) = %v", err)
	}
	it := c.SearchProjects(ctx, &rmpb.SearchProjectsRequest{Query: "id:a OR id:b"})
	if _, err := it.Next(); status.Code(err) != codes.InvalidArgument {
		t.Errorf("SearchProjects with OR = %v, want InvalidArgument rather than a partial match", err)
	}
}

func TestFoldersAreUnimplemented(t *testing.T) {
	_, srv, _ := startAPI(t)
	fc, err := resourcemanager.NewFoldersClient(context.Background(),
		option.WithEndpoint(srv.Addr()), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	defer fc.Close()
	if _, err := fc.GetFolder(context.Background(), &rmpb.GetFolderRequest{Name: "folders/1"}); status.Code(err) != codes.Unimplemented {
		t.Errorf("GetFolder = %v, want Unimplemented", err)
	}
}

func TestProjectsOverREST(t *testing.T) {
	_, srv, _ := startAPI(t)
	c, err := resourcemanager.NewProjectsRESTClient(context.Background(),
		option.WithEndpoint("http://"+srv.Addr()), option.WithoutAuthentication(), option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	op, err := c.CreateProject(ctx, &rmpb.CreateProjectRequest{Project: &rmpb.Project{ProjectId: "rest-made-1"}})
	if err != nil {
		t.Fatalf("REST CreateProject: %v", err)
	}
	if p, err := op.Wait(ctx); err != nil || p.GetProjectId() != "rest-made-1" {
		t.Fatalf("REST created %v, %v", p, err)
	}
	if p, err := c.GetProject(ctx, &rmpb.GetProjectRequest{Name: "projects/rest-made-1"}); err != nil || p.GetDisplayName() != "rest-made-1" {
		t.Errorf("REST GetProject = %v, %v", p, err)
	}
	it := c.SearchProjects(ctx, &rmpb.SearchProjectsRequest{Query: "id:rest-made-1"})
	if p, err := it.Next(); err != nil || p.GetProjectId() != "rest-made-1" {
		t.Errorf("REST SearchProjects = %v, %v", p, err)
	}
	if _, err := c.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: "projects/rest-made-1"}); err == nil {
		t.Error("REST GetIamPolicy succeeded")
	}
	dop, err := c.DeleteProject(ctx, &rmpb.DeleteProjectRequest{Name: "projects/rest-made-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dop.Wait(ctx); err != nil {
		t.Errorf("REST delete wait: %v", err)
	}
}
